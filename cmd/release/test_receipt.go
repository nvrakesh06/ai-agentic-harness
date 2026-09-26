package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
)

const releaseReceiptSchema = 1

var releaseTestEnvironmentKeys = []string{
	"AIH_HOME", "AIH_PLAYWRIGHT_MODULE", "AIH_REAL_PLAYWRIGHT", "CGO_ENABLED",
	"GOARCH", "GOEXPERIMENT", "GOFLAGS", "GOMAXPROCS", "GOOS", "GOROOT", "GOTOOLCHAIN",
	"NODE_OPTIONS", "NODE_PATH", "PLAYWRIGHT_BROWSERS_PATH",
}

type releaseReceiptIdentity struct {
	Schema      int    `json:"schema"`
	Head        string `json:"head"`
	Tree        string `json:"tree"`
	Inventory   string `json:"inventory"`
	Toolchain   string `json:"toolchain"`
	Environment string `json:"environment"`
	Resources   string `json:"resources"`
}

type releaseGroupReceipt struct {
	Schema    int                    `json:"schema"`
	Identity  releaseReceiptIdentity `json:"identity"`
	Group     string                 `json:"group"`
	Completed int                    `json:"completed"`
}

func releaseHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func releaseGroupID(group releaseTestGroup) string {
	encoded, _ := json.Marshal(struct {
		Group releaseTestGroup `json:"group"`
		Args  []string         `json:"args"`
	}{group, releaseGroupArgs(group)})
	return releaseHash(string(encoded))
}

func releaseReceiptPath(root string, identity releaseReceiptIdentity, group releaseTestGroup) string {
	identityJSON, _ := json.Marshal(identity)
	return filepath.Join(root, releaseHash(string(identityJSON)), releaseGroupID(group)+".json")
}

func releaseReceiptMatches(receipt releaseGroupReceipt, identity releaseReceiptIdentity, group releaseTestGroup) bool {
	return receipt.Schema == releaseReceiptSchema && receipt.Identity == identity && receipt.Group == releaseGroupID(group) && receipt.Completed == releaseGroupCompletionCount(group)
}

func releaseGroupCompletionCount(group releaseTestGroup) int {
	if len(group.Tests) != 0 {
		return len(group.Tests)
	}
	return len(group.Packages)
}

func loadReleaseGroupReceipt(root string, identity releaseReceiptIdentity, group releaseTestGroup) bool {
	path := releaseReceiptPath(root, identity, group)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var receipt releaseGroupReceipt
	return json.Unmarshal(data, &receipt) == nil && releaseReceiptMatches(receipt, identity, group)
}

// saveReleaseGroupReceipt publishes only a completed group. A receipt has a
// unique identity/group path, so rename never replaces earlier evidence.
func saveReleaseGroupReceipt(ctx context.Context, root string, identity releaseReceiptIdentity, group releaseTestGroup) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := releaseReceiptPath(root, identity, group)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if loadReleaseGroupReceipt(root, identity, group) {
		return nil
	}
	data, err := json.MarshalIndent(releaseGroupReceipt{Schema: releaseReceiptSchema, Identity: identity, Group: releaseGroupID(group), Completed: releaseGroupCompletionCount(group)}, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".release-group-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err = temporary.Chmod(0600); err == nil {
		_, err = temporary.Write(append(data, '\n'))
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(temporaryName, path); err != nil {
		if loadReleaseGroupReceipt(root, identity, group) {
			return nil
		}
		return err
	}
	return nil
}

func releaseBrowserSensitive(group releaseTestGroup) bool {
	for _, test := range group.Tests {
		if releaseBrowserTest(test) {
			return true
		}
	}
	return false
}

func releaseBrowserTest(test string) bool {
	lower := strings.ToLower(test)
	return strings.Contains(lower, "browser") || strings.Contains(lower, "playwright") || strings.Contains(lower, "visual")
}

func releaseTerminalAccepted(test, action string) bool {
	if action == "pass" {
		return true
	}
	return action == "skip" && !(os.Getenv("AIH_REAL_PLAYWRIGHT") == "1" && releaseRealBrowserFixture(test))
}

func releaseRealBrowserFixture(test string) bool {
	switch test {
	case "TestNativeVisualCapturePinsHeadAndStoresOutsideSource", "TestNativeVisualCaptureReattestsIdenticalTree":
		return true
	default:
		return false
	}
}

func releaseReceiptIdentityFor(ctx context.Context, groups []releaseTestGroup) (releaseReceiptIdentity, error) {
	run := func(name string, args ...string) (string, error) {
		output, err := platform.Run(ctx, "", nil, "", name, args...)
		if err != nil {
			if yielded := releaseYieldCancellation(ctx, err); yielded != nil {
				return "", yielded
			}
			return "", fmt.Errorf("release receipt identity %s: %w", name, err)
		}
		return strings.TrimSpace(output), nil
	}
	status, err := run("git", "status", "--porcelain")
	if err != nil {
		return releaseReceiptIdentity{}, err
	}
	if err := releaseCleanWorktree(status); err != nil {
		return releaseReceiptIdentity{}, err
	}
	head, err := run("git", "rev-parse", "HEAD")
	if err != nil {
		return releaseReceiptIdentity{}, err
	}
	tree, err := run("git", "rev-parse", "HEAD^{tree}")
	if err != nil {
		return releaseReceiptIdentity{}, err
	}
	goVersion, err := run("go", "version")
	if err != nil {
		return releaseReceiptIdentity{}, err
	}
	goEnv, err := run("go", "env", "GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS", "GOTOOLCHAIN")
	if err != nil {
		return releaseReceiptIdentity{}, err
	}
	gitVersion, err := run("git", "--version")
	if err != nil {
		return releaseReceiptIdentity{}, err
	}
	goWork, err := run("go", "env", "GOWORK")
	if err != nil {
		return releaseReceiptIdentity{}, err
	}
	if err = releaseWorkspaceAllowed(goWork); err != nil {
		return releaseReceiptIdentity{}, err
	}
	if err = releaseGoFlagsAllowed(os.Getenv("GOFLAGS")); err != nil {
		return releaseReceiptIdentity{}, err
	}
	keys := append([]string(nil), releaseTestEnvironmentKeys...)
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+os.Getenv(key))
	}
	plan, err := json.Marshal(groups)
	if err != nil {
		return releaseReceiptIdentity{}, err
	}
	return releaseReceiptIdentity{
		Schema: releaseReceiptSchema, Head: head, Tree: tree,
		Inventory:   releaseHash(string(plan)),
		Toolchain:   releaseHash(goVersion + "\n" + goEnv + "\n" + gitVersion),
		Environment: releaseHash(strings.Join(environment, "\n")),
		Resources:   releaseHash(fmt.Sprintf("timeout=%s\nengine_group_size=%d\npackage_parallelism=1\nfailfast=true", releaseTestTimeout, releaseIntegrationGroupSize)),
	}, nil
}

func releaseWorkspaceAllowed(value string) error {
	if value != "" && value != "off" {
		return errors.New("release receipt reuse requires GOWORK=off; active workspaces can supply external replace authority")
	}
	return nil
}

func releaseGoFlagsAllowed(value string) error {
	for _, field := range strings.Fields(value) {
		if field == "-modfile" || strings.HasPrefix(field, "-modfile=") {
			return errors.New("release receipt reuse does not accept GOFLAGS -modfile authority")
		}
	}
	return nil
}

func releaseCleanWorktree(status string) error {
	if strings.TrimSpace(status) != "" {
		return errors.New("release receipt reuse requires a clean worktree")
	}
	return nil
}
