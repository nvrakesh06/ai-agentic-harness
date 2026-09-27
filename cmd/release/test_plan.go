package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
)

// No fixture is removed or made parallel: grouping prevents the aggregate
// package deadline from terminating a healthy growing test inventory.
const releaseIntegrationGroupSize = 4

var runnableTestName = regexp.MustCompile(`^(Test|Example|Fuzz)[A-Za-z0-9_]*$`)

type releaseTestGroup struct {
	Packages []string `json:"packages"`
	Tests    []string `json:"tests,omitempty"`
}

type releaseTestInventory struct {
	Schema        int                `json:"schema"`
	Head          string             `json:"reference_head"`
	Tree          string             `json:"reference_tree"`
	WorktreeDirty bool               `json:"working_tree_dirty"`
	Groups        []releaseTestGroup `json:"planned_groups"`
}

// releaseTestGroups preserves complete package coverage while bounding each
// runnable named-test invocation. Packages without runnable test names still
// receive one package group so their build and package-terminal event remain
// required release evidence.
func releaseTestGroups(packages []string, integration string, testsByPackage map[string][]string, size int) ([]releaseTestGroup, error) {
	if size < 1 || len(packages) == 0 {
		return nil, fmt.Errorf("release test plan requires packages and a positive group size")
	}
	seen := map[string]bool{}
	groups := make([]releaseTestGroup, 0, len(packages))
	foundIntegration := false
	for _, pkg := range packages {
		if pkg == "" || seen[pkg] {
			return nil, fmt.Errorf("release package inventory contains an empty or duplicate package")
		}
		seen[pkg] = true
		tests, found := testsByPackage[pkg]
		if !found {
			return nil, fmt.Errorf("release package %q has no named-test inventory", pkg)
		}
		if pkg == integration {
			foundIntegration = true
			if len(tests) == 0 {
				return nil, fmt.Errorf("release integration inventory missing runnable tests")
			}
		}
		testSeen := map[string]bool{}
		for _, test := range tests {
			if !runnableTestName.MatchString(test) || testSeen[test] {
				return nil, fmt.Errorf("invalid or duplicate release test %q for %s", test, pkg)
			}
			testSeen[test] = true
		}
		if len(tests) == 0 {
			groups = append(groups, releaseTestGroup{Packages: []string{pkg}})
			continue
		}
		names := append([]string(nil), tests...)
		sort.Strings(names)
		for start := 0; start < len(names); start += size {
			end := min(start+size, len(names))
			groups = append(groups, releaseTestGroup{Packages: []string{pkg}, Tests: names[start:end]})
		}
	}
	if !foundIntegration {
		return nil, fmt.Errorf("release integration inventory missing package")
	}
	for pkg := range testsByPackage {
		if !seen[pkg] {
			return nil, fmt.Errorf("release named-test inventory includes unknown package %q", pkg)
		}
	}
	return groups, nil
}

func releaseGroupArgs(group releaseTestGroup) []string {
	args := []string{"test", "-json", "-p=1", "-count=1", "-failfast", "-timeout", releaseTestTimeout}
	if len(group.Tests) > 0 {
		// Anchoring at the top-level test name includes all its subtests.
		args = append(args, "-run", "^("+strings.Join(group.Tests, "|")+")$")
	}
	return append(args, group.Packages...)
}

func runCompleteReleaseTests(ctx context.Context) error {
	discovery, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	output, err := platform.Run(discovery, "", nil, "", "go", "list", "./...")
	if err != nil {
		if yielded := releaseYieldCancellation(ctx, err); yielded != nil {
			return yielded
		}
		return fmt.Errorf("discover release package inventory: %w: %s", err, output)
	}
	packages := strings.Fields(output)
	integration := ""
	for _, pkg := range packages {
		if strings.HasSuffix(pkg, "/internal/engine") {
			integration = pkg
		}
	}
	if integration == "" {
		return fmt.Errorf("release package inventory has no engine integration package")
	}
	testsByPackage := make(map[string][]string, len(packages))
	for _, pkg := range packages {
		// Record packages with no runnable names too: they retain a package
		// group for compilation and terminal-package inventory evidence.
		testsByPackage[pkg] = nil
		output, err = platform.Run(discovery, "", nil, "", "go", "test", "-p=1", "-list", ".", pkg)
		if err != nil {
			if yielded := releaseYieldCancellation(ctx, err); yielded != nil {
				return yielded
			}
			return fmt.Errorf("discover release named-test inventory for %s: %w: %s", pkg, err, output)
		}
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if runnableTestName.MatchString(line) {
				testsByPackage[pkg] = append(testsByPackage[pkg], line)
			}
		}
	}
	groups, err := releaseTestGroups(packages, integration, testsByPackage, releaseIntegrationGroupSize)
	if err != nil {
		return err
	}
	// This local artifact records the complete planned inventory even if a
	// group fails. It is not evidence that unexecuted groups passed.
	identity, err := releaseReceiptIdentityFor(discovery, groups)
	if err != nil {
		if yielded := releaseYieldCancellation(ctx, err); yielded != nil {
			return yielded
		}
		return err
	}
	if boundary, ok := ctx.Value(releaseBoundaryKey{}).(releaseBoundary); ok && boundary.progress != nil {
		if err := boundary.progress.bindIdentity(identity, groups); err != nil {
			return err
		}
	}
	manifest, err := json.MarshalIndent(releaseTestInventory{Schema: 1, Head: identity.Head, Tree: identity.Tree, WorktreeDirty: false, Groups: groups}, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll("dist", 0755); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join("dist", "test-inventory.json"), append(manifest, '\n'), 0644); err != nil {
		return err
	}
	namedTests := 0
	for _, tests := range testsByPackage {
		namedTests += len(tests)
	}
	fmt.Printf("release test inventory: %d packages, %d named tests, %d serial groups\n", len(packages), namedTests, len(groups))
	receipts := filepath.Join("dist", "release-test-receipts")
	for index, group := range groups {
		fmt.Printf("release test group %d/%d\n", index+1, len(groups))
		unit, cancelUnit := context.WithTimeout(ctx, releaseUnitWatchdog)
		current, identityErr := releaseReceiptIdentityFor(unit, groups)
		if identityErr != nil || current != identity {
			if identityErr != nil {
				if yielded := releaseYieldCancellation(ctx, identityErr); yielded != nil {
					return yielded
				}
				return fmt.Errorf("release test group %d/%d identity: %w", index+1, len(groups), identityErr)
			}
			return fmt.Errorf("release test group %d/%d identity changed; refusing receipt reuse", index+1, len(groups))
		}
		// A browser-sensitive group has no durable receipt, but an already
		// validated completion belongs to this invocation's cursor. A fresh
		// release invocation creates a new cursor and reruns it as before.
		if boundary, ok := ctx.Value(releaseBoundaryKey{}).(releaseBoundary); ok && boundary.progress != nil && boundary.progress.completed[releaseGroupID(group)] {
			fmt.Printf("release test group %d/%d: completed before cooperative handoff\n", index+1, len(groups))
			cancelUnit()
			continue
		}
		if releaseBrowserSensitive(group) {
			fmt.Println("release test group browser/visual: rerunning; receipt reuse disabled")
		} else if loadReleaseGroupReceipt(receipts, identity, group) {
			fmt.Printf("release test group %d/%d: cached exact-identity receipt\n", index+1, len(groups))
			if handoff, handoffErr := releaseBoundaryHandoff(unit, releaseGroupID(group)); handoffErr != nil {
				return fmt.Errorf("release test group %d/%d boundary: %w", index+1, len(groups), handoffErr)
			} else if handoff {
				cancelUnit()
				return errReleaseYielded
			}
			cancelUnit()
			continue
		}
		err := runReleaseTestGroupCommand(unit, "go", releaseGroupArgs(group), group.Tests, group.Packages)
		if err != nil {
			cancelUnit()
			return fmt.Errorf("release test group %d/%d: %w", index+1, len(groups), err)
		}
		// An interrupted group reaches neither this line nor receipt handling.
		// Browser groups do not persist receipts, but still prove exact identity
		// before a cooperative handoff can expose the slot to product work.
		current, identityErr = releaseReceiptIdentityFor(unit, groups)
		if identityErr != nil || current != identity {
			if identityErr != nil {
				if yielded := releaseYieldCancellation(ctx, identityErr); yielded != nil {
					return yielded
				}
				return fmt.Errorf("release test group %d/%d final identity: %w", index+1, len(groups), identityErr)
			}
			return fmt.Errorf("release test group %d/%d changed source or runtime; refusing receipt", index+1, len(groups))
		}
		if !releaseBrowserSensitive(group) {
			if err := saveReleaseGroupReceipt(unit, receipts, identity, group); err != nil {
				if yielded := releaseYieldCancellation(ctx, err); yielded != nil {
					return yielded
				}
				return fmt.Errorf("record release test group %d/%d: %w", index+1, len(groups), err)
			}
		}
		if handoff, handoffErr := releaseBoundaryHandoff(unit, releaseGroupID(group)); handoffErr != nil {
			return fmt.Errorf("release test group %d/%d boundary: %w", index+1, len(groups), handoffErr)
		} else if handoff {
			cancelUnit()
			return errReleaseYielded
		}
		cancelUnit()
	}
	current, identityErr := releaseReceiptIdentityFor(ctx, groups)
	if identityErr != nil || current != identity {
		if identityErr != nil {
			if yielded := releaseYieldCancellation(ctx, identityErr); yielded != nil {
				return yielded
			}
			return fmt.Errorf("release test final identity: %w", identityErr)
		}
		return errors.New("release test final identity changed")
	}
	return nil
}
