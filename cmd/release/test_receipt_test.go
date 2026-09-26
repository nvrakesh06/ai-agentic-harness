package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
