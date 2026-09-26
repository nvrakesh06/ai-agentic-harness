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
// package deadline from terminating a healthy growing integration inventory.
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

func releaseTestGroups(packages []string, integration string, tests []string, size int) ([]releaseTestGroup, error) {
	if size < 1 || len(packages) == 0 {
		return nil, fmt.Errorf("release test plan requires packages and a positive group size")
	}
	seen := map[string]bool{}
	normal := []string{}
	found := false
	for _, pkg := range packages {
		if pkg == "" || seen[pkg] {
			return nil, fmt.Errorf("release package inventory contains an empty or duplicate package")
		}
		seen[pkg] = true
		if pkg == integration {
			found = true
		} else {
			normal = append(normal, pkg)
		}
	}
	if !found || len(tests) == 0 {
		return nil, fmt.Errorf("release integration inventory missing package or runnable tests")
	}
	seen = map[string]bool{}
	for _, test := range tests {
		if !runnableTestName.MatchString(test) || seen[test] {
			return nil, fmt.Errorf("invalid or duplicate release integration test %q", test)
		}
		seen[test] = true
	}
	names := append([]string(nil), tests...)
	sort.Strings(names)
	groups := make([]releaseTestGroup, 0, len(normal)+(len(names)+size-1)/size)
	for _, pkg := range normal {
		groups = append(groups, releaseTestGroup{Packages: []string{pkg}})
	}
	for start := 0; start < len(names); start += size {
		end := min(start+size, len(names))
		groups = append(groups, releaseTestGroup{Packages: []string{integration}, Tests: names[start:end]})
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
	output, err = platform.Run(discovery, "", nil, "", "go", "test", "-p=1", "-list", ".", integration)
	if err != nil {
		if yielded := releaseYieldCancellation(ctx, err); yielded != nil {
			return yielded
		}
		return fmt.Errorf("discover release integration inventory: %w: %s", err, output)
	}
	tests := []string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if runnableTestName.MatchString(line) {
			tests = append(tests, line)
		}
	}
	groups, err := releaseTestGroups(packages, integration, tests, releaseIntegrationGroupSize)
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
	fmt.Printf("release test inventory: %d packages, %d engine tests, %d serial groups\n", len(packages), len(tests), len(groups))
	receipts := filepath.Join("dist", "release-test-receipts")
	for index, group := range groups {
		fmt.Printf("release test group %d/%d\n", index+1, len(groups))
		current, identityErr := releaseReceiptIdentityFor(ctx, groups)
		if identityErr != nil || current != identity {
			if identityErr != nil {
				if yielded := releaseYieldCancellation(ctx, identityErr); yielded != nil {
					return yielded
				}
				return fmt.Errorf("release test group %d/%d identity: %w", index+1, len(groups), identityErr)
			}
			return fmt.Errorf("release test group %d/%d identity changed; refusing receipt reuse", index+1, len(groups))
		}
		if releaseBrowserSensitive(group) {
			fmt.Println("release test group browser/visual: rerunning; receipt reuse disabled")
		} else if loadReleaseGroupReceipt(receipts, identity, group) {
			fmt.Printf("release test group %d/%d: cached exact-identity receipt\n", index+1, len(groups))
			continue
		}
		if err := runReleaseTestGroupCommand(ctx, "go", releaseGroupArgs(group), group.Tests, group.Packages); err != nil {
			return fmt.Errorf("release test group %d/%d: %w", index+1, len(groups), err)
		}
		// An interrupted group reaches neither this line nor the receipt write.
		if !releaseBrowserSensitive(group) {
			current, identityErr = releaseReceiptIdentityFor(ctx, groups)
			if identityErr != nil || current != identity {
				if identityErr != nil {
					if yielded := releaseYieldCancellation(ctx, identityErr); yielded != nil {
						return yielded
					}
					return fmt.Errorf("release test group %d/%d final identity: %w", index+1, len(groups), identityErr)
				}
				return fmt.Errorf("release test group %d/%d changed source or runtime; refusing receipt", index+1, len(groups))
			}
			if err := saveReleaseGroupReceipt(ctx, receipts, identity, group); err != nil {
				if yielded := releaseYieldCancellation(ctx, err); yielded != nil {
					return yielded
				}
				return fmt.Errorf("record release test group %d/%d: %w", index+1, len(groups), err)
			}
		}
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
