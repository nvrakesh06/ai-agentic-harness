package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

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
