package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
)

func TestReleaseInvocationCursorAvoidsRepeatedDiscoveryAndCompletedPrefixChecks(t *testing.T) {
	originalDiscover := discoverReleaseTestPlanFn
	originalIdentity := releaseReceiptIdentityForFn
	originalGroup := runReleaseTestGroupCommandFn
	originalDemand := releasePriorityDemandFn
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		discoverReleaseTestPlanFn = originalDiscover
		releaseReceiptIdentityForFn = originalIdentity
		runReleaseTestGroupCommandFn = originalGroup
		releasePriorityDemandFn = originalDemand
		_ = os.Chdir(originalDir)
	}()

	identity, _ := receiptFixture()
	groups := []releaseTestGroup{
		{Packages: []string{"example.com/one"}, Tests: []string{"TestOne"}},
		{Packages: []string{"example.com/two"}, Tests: []string{"TestTwo"}},
		{Packages: []string{"example.com/three"}, Tests: []string{"TestThree"}},
	}
	var discoveries, identities, commands, demands int
	discoverReleaseTestPlanFn = func(context.Context) (releaseTestPlan, error) {
		discoveries++
		current, err := releaseReceiptIdentityForFn(context.Background(), groups)
		return releaseTestPlan{groups: groups, identity: current, packages: 3, namedTests: 3}, err
	}
	releaseReceiptIdentityForFn = func(context.Context, []releaseTestGroup) (releaseReceiptIdentity, error) {
		identities++
		return identity, nil
	}
	runReleaseTestGroupCommandFn = func(context.Context, string, []string, []string, []string) error {
		commands++
		return nil
	}
	releasePriorityDemandFn = func(string, config.Machine) (bool, error) {
		demands++
		return demands <= 2, nil
	}

	progress := newReleaseInvocationProgress()
	for attempt := 0; attempt < 3; attempt++ {
		ctx := withReleaseBoundary(context.Background(), "unused", config.Machine{MaxHeavyChecks: 1}, progress)
		err := runCompleteReleaseTests(ctx)
		if attempt < 2 && !errors.Is(err, errReleaseYielded) {
			t.Fatalf("attempt %d = %v, want cooperative yield", attempt+1, err)
		}
		if attempt == 2 && err != nil {
			t.Fatalf("final attempt = %v", err)
		}
	}
	if discoveries != 1 {
		t.Fatalf("inventory discoveries = %d, want 1 per invocation", discoveries)
	}
	if commands != len(groups) || progress.nextGroup != len(groups) {
		t.Fatalf("group commands/cursor = %d/%d, want %d/%d", commands, progress.nextGroup, len(groups), len(groups))
	}
	// One initial identity, two checks around each executed group, one
	// reattestation at each reacquisition, and one final check. Completed
	// prefix groups contribute no extra identity probes after a handoff.
	if identities != 10 {
		t.Fatalf("identity probes = %d, want 10 without completed-prefix scans", identities)
	}
}

func TestReleaseInvocationCursorRejectsChangedIdentityBeforePendingGroup(t *testing.T) {
	originalDiscover := discoverReleaseTestPlanFn
	originalIdentity := releaseReceiptIdentityForFn
	originalGroup := runReleaseTestGroupCommandFn
	originalDemand := releasePriorityDemandFn
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		discoverReleaseTestPlanFn = originalDiscover
		releaseReceiptIdentityForFn = originalIdentity
		runReleaseTestGroupCommandFn = originalGroup
		releasePriorityDemandFn = originalDemand
		_ = os.Chdir(originalDir)
	}()

	identity, _ := receiptFixture()
	groups := []releaseTestGroup{
		{Packages: []string{"example.com/one"}, Tests: []string{"TestOne"}},
		{Packages: []string{"example.com/two"}, Tests: []string{"TestTwo"}},
	}
	var identities, commands int
	discoverReleaseTestPlanFn = func(context.Context) (releaseTestPlan, error) {
		return releaseTestPlan{groups: groups, identity: identity, packages: 2, namedTests: 2}, nil
	}
	releaseReceiptIdentityForFn = func(context.Context, []releaseTestGroup) (releaseReceiptIdentity, error) {
		identities++
		if identities >= 3 {
			changed := identity
			changed.Tree = "changed-tree"
			return changed, nil
		}
		return identity, nil
	}
	runReleaseTestGroupCommandFn = func(context.Context, string, []string, []string, []string) error {
		commands++
		return nil
	}
	releasePriorityDemandFn = func(string, config.Machine) (bool, error) { return true, nil }

	progress := newReleaseInvocationProgress()
	ctx := withReleaseBoundary(context.Background(), "unused", config.Machine{MaxHeavyChecks: 1}, progress)
	if err := runCompleteReleaseTests(ctx); !errors.Is(err, errReleaseYielded) {
		t.Fatalf("first group = %v, want cooperative yield", err)
	}
	ctx = withReleaseBoundary(context.Background(), "unused", config.Machine{MaxHeavyChecks: 1}, progress)
	err = runCompleteReleaseTests(ctx)
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("changed reacquisition identity = %v", err)
	}
	if commands != 1 || progress.nextGroup != 1 {
		t.Fatalf("changed identity ran pending work: commands/cursor = %d/%d, want 1/1", commands, progress.nextGroup)
	}
}

func TestReleaseFreshInvocationRediscoversAndRerunsBrowserGroup(t *testing.T) {
	originalDiscover := discoverReleaseTestPlanFn
	originalIdentity := releaseReceiptIdentityForFn
	originalGroup := runReleaseTestGroupCommandFn
	originalDemand := releasePriorityDemandFn
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		discoverReleaseTestPlanFn = originalDiscover
		releaseReceiptIdentityForFn = originalIdentity
		runReleaseTestGroupCommandFn = originalGroup
		releasePriorityDemandFn = originalDemand
		_ = os.Chdir(originalDir)
	}()

	identity, _ := receiptFixture()
	groups := []releaseTestGroup{{Packages: []string{"example.com/visual"}, Tests: []string{"TestVisualCursor"}}}
	var discoveries, commands int
	discoverReleaseTestPlanFn = func(context.Context) (releaseTestPlan, error) {
		discoveries++
		return releaseTestPlan{groups: groups, identity: identity, packages: 1, namedTests: 1}, nil
	}
	releaseReceiptIdentityForFn = func(context.Context, []releaseTestGroup) (releaseReceiptIdentity, error) { return identity, nil }
	runReleaseTestGroupCommandFn = func(context.Context, string, []string, []string, []string) error {
		commands++
		return nil
	}
	releasePriorityDemandFn = func(string, config.Machine) (bool, error) { return false, nil }

	for invocation := 0; invocation < 2; invocation++ {
		progress := newReleaseInvocationProgress()
		ctx := withReleaseBoundary(context.Background(), "unused", config.Machine{MaxHeavyChecks: 1}, progress)
		if err := runCompleteReleaseTests(ctx); err != nil {
			t.Fatalf("invocation %d = %v", invocation+1, err)
		}
	}
	if discoveries != 2 || commands != 2 {
		t.Fatalf("fresh browser invocations discoveries/commands = %d/%d, want 2/2", discoveries, commands)
	}
}

func TestReleaseTestGroupsCoverInventoryExactlyOnce(t *testing.T) {
	const engine = "example.com/aih/internal/engine"
	const release = "example.com/aih/cmd/release"
	const platform = "example.com/aih/internal/platform"
	inventory := map[string][]string{
		release:  {"TestReleaseZulu", "TestReleaseAlpha", "FuzzRelease"},
		engine:   {"TestZulu", "TestAlpha", "FuzzParser", "Example"},
		platform: nil,
	}
	groups, err := releaseTestGroups([]string{release, engine, platform}, engine, inventory, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 5 {
		t.Fatalf("unexpected plan: %+v", groups)
	}
	seen := map[string]map[string]int{}
	for _, group := range groups {
		if len(group.Packages) != 1 {
			t.Fatalf("group has invalid packages: %+v", group)
		}
		pkg := group.Packages[0]
		if len(group.Tests) > 2 {
			t.Fatalf("package %s exceeded named-test batch bound: %+v", pkg, group)
		}
		args := strings.Join(releaseGroupArgs(group), " ")
		if !strings.Contains(args, "-count=1 -failfast -timeout 15m") {
			t.Fatalf("group weakened bounded uncached tests: %s", args)
		}
		if len(group.Tests) == 0 {
			if pkg != platform {
				t.Fatalf("unexpected unnamed package group: %+v", group)
			}
			continue
		}
		if !strings.Contains(args, "-run ^(") || !strings.Contains(args, ")$") {
			t.Fatalf("named group lost exact test selection: %s", args)
		}
		if seen[pkg] == nil {
			seen[pkg] = map[string]int{}
		}
		for _, test := range group.Tests {
			seen[pkg][test]++
		}
	}
	for pkg, tests := range inventory {
		for _, test := range tests {
			if seen[pkg][test] != 1 {
				t.Fatalf("package %s test %s covered %d times", pkg, test, seen[pkg][test])
			}
		}
	}
	if inventory[engine][0] != "TestZulu" {
		t.Fatal("planning mutated caller inventory")
	}
}

func TestReleaseTestGroupsRejectIncompleteOrUnsafeInventory(t *testing.T) {
	for _, tc := range []struct {
		packages []string
		tests    map[string][]string
		size     int
	}{
		{[]string{"engine"}, nil, 2},
		{[]string{"engine"}, map[string][]string{"engine": nil}, 2},
		{[]string{"engine", "engine"}, map[string][]string{"engine": {"TestA"}}, 2},
		{[]string{"engine"}, map[string][]string{"engine": {"TestA", "TestA"}}, 2},
		{[]string{"engine"}, map[string][]string{"engine": {"TestA|.*"}}, 2},
		{[]string{"engine"}, map[string][]string{"engine": {"TestA"}}, 0},
		{[]string{"engine"}, map[string][]string{"other": {"TestA"}}, 2},
	} {
		if _, err := releaseTestGroups(tc.packages, "engine", tc.tests, tc.size); err == nil {
			t.Fatalf("accepted unsafe inventory: %+v", tc)
		}
	}
}

func TestReleaseGroupRejectsSuccessfulExitWithMissingTest(t *testing.T) {
	if os.Getenv("AIH_RELEASE_INVENTORY_HELPER") == "1" {
		fmt.Fprintln(os.Stdout, `{"Action":"pass","Package":"example.com/engine","Test":"TestActual"}`)
		return
	}
	t.Setenv("AIH_RELEASE_INVENTORY_HELPER", "1")
	args := []string{"-test.run=^TestReleaseGroupRejectsSuccessfulExitWithMissingTest$"}
	err := runReleaseTestCommand(context.Background(), os.Args[0], args, []string{"TestActual", "TestMissing"})
	if err == nil || !strings.Contains(err.Error(), "TestMissing") {
		t.Fatalf("successful process hid unexecuted inventory: %v", err)
	}
	if err := runReleaseTestCommand(context.Background(), os.Args[0], args, []string{"TestActual"}); err != nil {
		t.Fatalf("complete inventory rejected: %v", err)
	}
	if err := runReleaseTestGroupCommand(context.Background(), os.Args[0], args, []string{"TestActual"}, []string{"example.com/engine"}); err == nil || !strings.Contains(err.Error(), "package inventory incomplete") {
		t.Fatalf("successful process hid missing package terminal event: %v", err)
	}
}
