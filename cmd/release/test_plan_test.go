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
	inventory := []string{"TestZulu", "TestAlpha", "FuzzParser", "Example"}
	groups, err := releaseTestGroups([]string{"example.com/aih/cmd/release", engine, "example.com/aih/internal/platform"}, engine, inventory, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 3 || len(groups[0].Tests) != 0 || len(groups[0].Packages) != 2 {
		t.Fatalf("unexpected plan: %+v", groups)
	}
	seen := map[string]int{}
	for _, group := range groups[1:] {
		if len(group.Tests) != 2 || len(group.Packages) != 1 || group.Packages[0] != engine {
			t.Fatalf("invalid bounded integration group: %+v", group)
		}
		args := strings.Join(releaseGroupArgs(group), " ")
		if !strings.Contains(args, "-count=1 -failfast -timeout 15m") || !strings.Contains(args, "-run ^(") || !strings.Contains(args, ")$") {
			t.Fatalf("group weakened bounded uncached tests: %s", args)
		}
		for _, test := range group.Tests {
			seen[test]++
		}
	}
	for _, test := range inventory {
		if seen[test] != 1 {
			t.Fatalf("test %s covered %d times", test, seen[test])
		}
	}
	if inventory[0] != "TestZulu" {
		t.Fatal("planning mutated caller inventory")
	}
}

func TestReleaseTestGroupsRejectIncompleteOrUnsafeInventory(t *testing.T) {
	for _, tc := range []struct {
		packages []string
		tests    []string
		size     int
	}{
		{[]string{"engine"}, nil, 2},
		{[]string{"other"}, []string{"TestA"}, 2},
		{[]string{"engine", "engine"}, []string{"TestA"}, 2},
		{[]string{"engine"}, []string{"TestA", "TestA"}, 2},
		{[]string{"engine"}, []string{"TestA|.*"}, 2},
		{[]string{"engine"}, []string{"TestA"}, 0},
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
}
