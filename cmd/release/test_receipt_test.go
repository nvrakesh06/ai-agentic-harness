package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
)

func receiptFixture() (releaseReceiptIdentity, releaseTestGroup) {
	return releaseReceiptIdentity{Schema: releaseReceiptSchema, Head: "head", Tree: "tree", Inventory: "inventory", Toolchain: "toolchain", Environment: "environment", Resources: "resources"}, releaseTestGroup{Packages: []string{"example.com/engine"}, Tests: []string{"TestReceipt"}}
}

func TestReleaseGroupReceiptRequiresExactIdentityAndValidRecord(t *testing.T) {
	identity, group := receiptFixture()
	root := t.TempDir()
	if loadReleaseGroupReceipt(root, identity, group) {
		t.Fatal("missing receipt was accepted")
	}
	if err := saveReleaseGroupReceipt(context.Background(), root, identity, group); err != nil {
		t.Fatal(err)
	}
	if !loadReleaseGroupReceipt(root, identity, group) {
		t.Fatal("exact receipt was rejected")
	}
	changed := identity
	changed.Tree = "other-tree"
	if loadReleaseGroupReceipt(root, changed, group) {
		t.Fatal("changed source identity was accepted")
	}
	path := releaseReceiptPath(root, identity, group)
	partial := releaseGroupReceipt{Schema: releaseReceiptSchema, Identity: identity, Group: releaseGroupID(group)}
	data, err := json.Marshal(partial)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if loadReleaseGroupReceipt(root, identity, group) {
		t.Fatal("partial receipt was accepted")
	}
	if err := os.WriteFile(path, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if loadReleaseGroupReceipt(root, identity, group) {
		t.Fatal("malformed receipt was accepted")
	}
}

func TestReleaseReceiptRejectsDirtySourceAndInterruptedGroup(t *testing.T) {
	if err := releaseCleanWorktree(" M cmd/release/main.go"); err == nil {
		t.Fatal("dirty worktree was accepted")
	}
	identity, group := receiptFixture()
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := saveReleaseGroupReceipt(ctx, root, identity, group); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted group save = %v", err)
	}
	if loadReleaseGroupReceipt(root, identity, group) {
		t.Fatal("interrupted group produced a receipt")
	}
}

func TestReleaseBrowserGroupsNeverReuseReceipts(t *testing.T) {
	identity, group := receiptFixture()
	group.Tests = []string{"TestNativeVisualCapturePinsHeadAndStoresOutsideSource"}
	if !releaseBrowserSensitive(group) {
		t.Fatal("native visual group was reusable")
	}
	if err := saveReleaseGroupReceipt(context.Background(), t.TempDir(), identity, group); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIH_REAL_PLAYWRIGHT", "1")
	if releaseTerminalAccepted("TestNativeVisualCapturePinsHeadAndStoresOutsideSource", "skip") {
		t.Fatal("real browser opt-in accepted a skipped visual fixture")
	}
	if !releaseTerminalAccepted("TestNativeVisualCapturePinsHeadAndStoresOutsideSource", "pass") {
		t.Fatal("real browser opt-in rejected a passing visual fixture")
	}
	if !releaseTerminalAccepted("TestVisualPrepareUsesFreshDetachedCheckoutNotWriterRuntime", "skip") {
		t.Fatal("real browser opt-in rejected an unrelated capability unit skip")
	}
}

func TestReleaseWorkspaceAndModfileAuthorityAreRejected(t *testing.T) {
	if err := releaseWorkspaceAllowed(`C:\workspace\go.work`); err == nil {
		t.Fatal("active workspace was accepted")
	}
	if err := releaseWorkspaceAllowed("off"); err != nil {
		t.Fatal(err)
	}
	for _, flags := range []string{"-modfile=outside.mod", "-modfile outside.mod"} {
		if err := releaseGoFlagsAllowed(flags); err == nil {
			t.Fatalf("external modfile was accepted: %q", flags)
		}
	}
	t.Setenv("GOFLAGS", "")
	if err := releaseGoFlagsAllowed("-modfile=from-goenv.mod"); err == nil {
		t.Fatal("effective GOENV flags were accepted when process GOFLAGS was empty")
	}
}

func TestReleaseReceiptIdentityCombinesStructuredGoEnvironmentProbe(t *testing.T) {
	groups := []releaseTestGroup{{Packages: []string{"example.com/engine"}, Tests: []string{"TestReceipt"}}}
	t.Setenv("GOFLAGS", "-from-process")
	goEnv := releaseGoEnvJSON(t, "-from-goenv", "off")
	identity, calls, err := releaseReceiptIdentityForProbeFixture(groups, goEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 6 {
		t.Fatalf("identity probe launches = %d, want 6: %#v", len(calls), calls)
	}
	if got := strings.Join(calls[4], " "); got != "go env -json GOVERSION GOOS GOARCH CGO_ENABLED GOFLAGS GOTOOLCHAIN GOWORK" {
		t.Fatalf("combined go env query = %q", got)
	}
	t.Setenv("GOFLAGS", "-different-process-value")
	fromProcessChange, _, err := releaseReceiptIdentityForProbeFixture(groups, goEnv)
	if err != nil || fromProcessChange != identity {
		t.Fatalf("process GOFLAGS overrode effective go env: identity=%#v error=%v", fromProcessChange, err)
	}
	changedGoEnv, _, err := releaseReceiptIdentityForProbeFixture(groups, releaseGoEnvJSON(t, "-changed-in-goenv", "off"))
	if err != nil || changedGoEnv.Environment == identity.Environment || changedGoEnv.Toolchain == identity.Toolchain {
		t.Fatalf("effective go env change did not alter environment and toolchain identity: identity=%#v error=%v", changedGoEnv, err)
	}
	if _, _, err = releaseReceiptIdentityForProbeFixture(groups, releaseGoEnvJSON(t, "-modfile=outside.mod", "off")); err == nil {
		t.Fatal("GOENV -modfile authority was accepted")
	}
	if _, _, err = releaseReceiptIdentityForProbeFixture(groups, releaseGoEnvJSON(t, "", `C:\outside\go.work`)); err == nil {
		t.Fatal("active GOWORK authority was accepted")
	}
}

func TestReleaseGoEnvironmentJSONIsStrict(t *testing.T) {
	valid := releaseGoEnvJSON(t, "", "off")
	if _, err := releaseGoEnvironmentFromJSON(" \n\t" + valid + "\r\n "); err != nil {
		t.Fatalf("whitespace-wrapped go env JSON was rejected: %v", err)
	}
	if _, err := releaseGoEnvironmentFromJSON("not-json"); err == nil {
		t.Fatal("invalid go env JSON was accepted")
	}
	missing := map[string]any{"GOVERSION": "go1.24"}
	data, err := json.Marshal(missing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = releaseGoEnvironmentFromJSON(string(data)); err == nil {
		t.Fatal("missing go env fields were accepted")
	}
	invalid := map[string]any{"GOVERSION": "go1.24", "GOOS": "windows", "GOARCH": "amd64", "CGO_ENABLED": "1", "GOFLAGS": []string{"-race"}, "GOTOOLCHAIN": "auto", "GOWORK": "off"}
	data, err = json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = releaseGoEnvironmentFromJSON(string(data)); err == nil {
		t.Fatal("non-string go env field was accepted")
	}
	for _, value := range []string{
		strings.Replace(valid, `"GOFLAGS":""`, `"GOFLAGS":null`, 1),
		valid + ` {}`,
		strings.Replace(valid, `"GOWORK":"off"`, `"UNKNOWN":"off"`, 1),
	} {
		if _, err = releaseGoEnvironmentFromJSON(value); err == nil {
			t.Fatalf("invalid strict go env JSON was accepted: %q", value)
		}
	}
	for _, tc := range []struct {
		field, first, second string
	}{
		{"GOFLAGS", "-modfile=forbidden.mod", ""},
		{"GOFLAGS", "", "-modfile=forbidden.mod"},
		{"GOWORK", `C:\forbidden\go.work`, ""},
		{"GOWORK", "", `C:\forbidden\go.work`},
	} {
		if _, err = releaseGoEnvironmentFromJSON(releaseGoEnvJSONDuplicate(t, tc.field, tc.first, tc.second)); err == nil {
			t.Fatalf("duplicate %s was accepted: %q then %q", tc.field, tc.first, tc.second)
		}
	}
}

func releaseGoEnvJSON(t *testing.T, flags, work string) string {
	t.Helper()
	data, err := json.Marshal(map[string]string{"GOVERSION": "go1.24.0", "GOOS": "windows", "GOARCH": "amd64", "CGO_ENABLED": "1", "GOFLAGS": flags, "GOTOOLCHAIN": "auto", "GOWORK": work})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func releaseGoEnvJSONDuplicate(t *testing.T, field, first, second string) string {
	t.Helper()
	flags, work := "", "off"
	if field == "GOFLAGS" {
		flags = first
	} else {
		work = first
	}
	base := releaseGoEnvJSON(t, flags, work)
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	needle := `"` + field + `":` + string(firstJSON)
	replacement := needle + `,"` + field + `":` + string(secondJSON)
	result := strings.Replace(base, needle, replacement, 1)
	if result == base {
		t.Fatal("test fixture did not create duplicate JSON key")
	}
	return result
}

func releaseReceiptIdentityForProbeFixture(groups []releaseTestGroup, goEnv string) (releaseReceiptIdentity, [][]string, error) {
	var calls [][]string
	runner := func(_ context.Context, _ string, _ []string, _ string, name string, args ...string) (string, error) {
		call := append([]string{name}, args...)
		calls = append(calls, call)
		switch strings.Join(call, " ") {
		case "git status --porcelain":
			return "", nil
		case "git rev-parse HEAD":
			return "head", nil
		case "git rev-parse HEAD^{tree}":
			return "tree", nil
		case "go version":
			return "go version go1.24.0 windows/amd64", nil
		case "go env -json GOVERSION GOOS GOARCH CGO_ENABLED GOFLAGS GOTOOLCHAIN GOWORK":
			return goEnv, nil
		case "git --version":
			return "git version 2.0", nil
		default:
			return "", fmt.Errorf("unexpected identity probe %q", strings.Join(call, " "))
		}
	}
	identity, err := releaseReceiptIdentityWithRun(context.Background(), groups, runner)
	return identity, calls, err
}

func TestReleaseYieldCancellationPreservesOnlyYieldedCancellation(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errReleaseYielded)
	if got := releaseYieldCancellation(ctx, fmt.Errorf("wrapped: %w", context.Canceled)); !errors.Is(got, errReleaseYielded) {
		t.Fatalf("yielded cancellation = %v", got)
	}
	if got := releaseYieldCancellation(ctx, ctx.Err()); !errors.Is(got, errReleaseYielded) {
		t.Fatalf("yielded ctx.Err = %v", got)
	}
	if got := releaseYieldCancellation(ctx, errors.New("real package failure")); got != nil {
		t.Fatalf("real failure was converted into yield: %v", got)
	}
}

func TestReleaseYieldRetryReleasesPermitAndDoesNotRetryFailure(t *testing.T) {
	originalAcquire, originalRun := acquireReleaseMachinePermitFn, runCompleteReleaseTestsFn
	defer func() { acquireReleaseMachinePermitFn, runCompleteReleaseTestsFn = originalAcquire, originalRun }()
	var acquisitions, releases, runs atomic.Int32
	acquireReleaseMachinePermitFn = func() (func(), config.Machine, string, error) {
		acquisitions.Add(1)
		return func() { releases.Add(1) }, config.Machine{MaxHeavyChecks: 1}, filepath.Join(t.TempDir(), "verification"), nil
	}
	runCompleteReleaseTestsFn = func(context.Context) error {
		if runs.Add(1) == 1 {
			return errReleaseYielded
		}
		return nil
	}
	_, cancel, release, err := runReleaseTestsWithYieldRetry()
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	release()
	if acquisitions.Load() != 2 || releases.Load() != 2 || runs.Load() != 2 {
		t.Fatalf("yield retry did not release/reacquire exactly once: acquisitions=%d releases=%d runs=%d", acquisitions.Load(), releases.Load(), runs.Load())
	}
	runs.Store(0)
	acquisitions.Store(0)
	releases.Store(0)
	runCompleteReleaseTestsFn = func(context.Context) error { runs.Add(1); return errors.New("test failed") }
	if _, _, _, err = runReleaseTestsWithYieldRetry(); err == nil || runs.Load() != 1 || acquisitions.Load() != 1 || releases.Load() != 1 {
		t.Fatalf("actual test failure retried or leaked permit: err=%v acquisitions=%d releases=%d runs=%d", err, acquisitions.Load(), releases.Load(), runs.Load())
	}
}
