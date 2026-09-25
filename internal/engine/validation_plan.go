package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// validationPlan is deliberately conservative. A focused gate is possible only
// for a low-risk Go change contained in one ordinary package. Every other task
// keeps the complete configured gate before its PR is merge-ready.
type validationPlan struct {
	Gate, Reason, Package, Toolchain, TestInputs, Input string
	Checks                                              []config.Check
}

func fullValidationPlan(ctx context.Context, e config.Effective, dir, head, reason string) (validationPlan, error) {
	return fullValidationPlanWithGitDir(ctx, e, dir, dir, head, reason)
}

// fullValidationPlanWithGitDir keeps path-sensitive verification-tool identity
// in a checkout while resolving the validation tree from the Git store that
// owns head. Post-verify normally has a source checkout that predates the
// integrated merge, so its control repository is the only safe object lookup.
func fullValidationPlanWithGitDir(ctx context.Context, e config.Effective, toolDir, gitDir, head, reason string) (validationPlan, error) {
	return makeValidationPlan(ctx, e, toolDir, gitDir, head, "full", reason, "", e.Project.Checks)
}

func postVerifyValidationReason(recovering bool, target, merge string) string {
	if recovering && target != merge {
		return "exact repaired main recovery target"
	}
	return "exact integrated merge-train head"
}

func (c *Controller) taskValidationPlan(ctx context.Context, e config.Effective, t *model.Task, dir string, paths []string) (validationPlan, error) {
	if t.Risk != "low" {
		return fullValidationPlan(ctx, e, dir, t.HeadSHA, "task risk is not low")
	}
	if t.Security {
		return fullValidationPlan(ctx, e, dir, t.HeadSHA, "task is security-sensitive")
	}
	pkg, reason := focusedPackage(paths)
	if reason != "" {
		return fullValidationPlan(ctx, e, dir, t.HeadSHA, reason)
	}
	if !focusedPackagePresent(dir, pkg) {
		return fullValidationPlan(ctx, e, dir, t.HeadSHA, "focused package is absent at validation head")
	}
	focused := make([]config.Check, 0, len(e.Project.Checks))
	for _, check := range e.Project.Checks {
		if !applicable(check) {
			continue
		}
		if candidate, ok := focusedGoCheck(check, pkg); ok {
			focused = append(focused, candidate)
		}
	}
	if len(focused) == 0 {
		return fullValidationPlan(ctx, e, dir, t.HeadSHA, "no configured focused Go test or static check")
	}
	return makeValidationPlan(ctx, e, dir, dir, t.HeadSHA, "focused", "low-risk change is confined to "+pkg, pkg, focused)
}

func focusedPackage(paths []string) (string, string) {
	if len(paths) == 0 {
		return "", "changed paths could not be inferred"
	}
	pkg := ""
	for _, path := range paths {
		path = filepath.ToSlash(path)
		lower := strings.ToLower(path)
		if strings.HasPrefix(path, ".aih/") || path == "AGENTS.md" || path == "go.mod" || path == "go.sum" || path == "SECURITY.md" ||
			strings.HasPrefix(path, "cmd/") || strings.HasPrefix(path, "internal/cli/") || strings.HasPrefix(path, "internal/engine/") || strings.HasPrefix(path, "internal/gitx/") || strings.HasPrefix(path, "internal/model/") || strings.HasPrefix(path, "internal/provider/") || strings.HasPrefix(path, "internal/roles/") || strings.HasPrefix(path, "internal/store/") || strings.HasPrefix(path, "internal/config/") || strings.HasPrefix(path, "internal/platform/") || strings.HasPrefix(path, "internal/safety/") || strings.HasPrefix(path, "internal/github/") ||
			strings.Contains(lower, "security") || strings.Contains(lower, "auth") || strings.Contains(lower, "secret") || strings.Contains(lower, "crypto") || strings.Contains(lower, "permission") || strings.Contains(lower, "network") || strings.Contains(lower, "deserial") || strings.Contains(lower, "subprocess") {
			return "", "scheduler, schema, security, configuration, or toolchain input changed"
		}
		if !strings.HasSuffix(path, ".go") || strings.Contains(path, "/testdata/") {
			return "", "changed paths are not confined to one Go package"
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		if dir == "." {
			dir = ""
		}
		if pkg == "" {
			pkg = dir
		} else if pkg != dir {
			return "", "change spans more than one Go package"
		}
	}
	if pkg == "" {
		return ".", ""
	}
	return "./" + pkg, ""
}

// focusedPackagePresent prevents a deleted final package file from becoming a
// focused test-input lookup error. The full gate still covers that deletion.
func focusedPackagePresent(dir, pkg string) bool {
	path := dir
	if pkg != "." {
		path = filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(pkg, "./")))
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			return true
		}
	}
	return false
}

func focusedGoCheck(check config.Check, pkg string) (config.Check, bool) {
	if filepath.Base(check.Command[0]) != "go" || len(check.Command) < 2 || (check.Command[1] != "test" && check.Command[1] != "vet") {
		return config.Check{}, false
	}
	copy := check
	copy.Command = append([]string(nil), check.Command...)
	found := false
	for i, arg := range copy.Command {
		if arg == "./..." {
			copy.Command[i] = pkg
			found = true
		}
	}
	return copy, found
}

func makeValidationPlan(ctx context.Context, e config.Effective, toolDir, gitDir, head, gate, reason, pkg string, checks []config.Check) (validationPlan, error) {
	toolchain, err := toolchainIdentity(toolDir, checks)
	if err != nil {
		return validationPlan{}, err
	}
	inputs, err := testInputIdentity(ctx, gitDir, head, pkg)
	if err != nil {
		return validationPlan{}, err
	}
	command := make([][]string, len(checks))
	for i := range checks {
		command[i] = append([]string(nil), checks[i].Command...)
	}
	payload, _ := json.Marshal(struct {
		Head, Config, Gate, Reason, Package, Toolchain, TestInputs string
		Commands                                                   [][]string
	}{head, e.Hash, gate, reason, pkg, toolchain, inputs, command})
	hash := sha256.Sum256(payload)
	return validationPlan{Gate: gate, Reason: reason, Package: pkg, Toolchain: toolchain, TestInputs: inputs, Input: hex.EncodeToString(hash[:]), Checks: checks}, nil
}

func toolchainIdentity(dir string, checks []config.Check) (string, error) {
	seen := map[string]bool{}
	identities := make([]string, 0, len(checks))
	for _, check := range checks {
		if len(check.Command) == 0 || seen[check.Command[0]] {
			continue
		}
		seen[check.Command[0]] = true
		path := check.Command[0]
		if filepath.IsAbs(path) || strings.ContainsAny(path, `/\\`) {
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
		} else {
			var err error
			path, err = exec.LookPath(path)
			if err != nil {
				return "", fmt.Errorf("resolve verification tool %q: %w", check.Command[0], err)
			}
		}
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("read verification tool %q: %w", check.Command[0], err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return "", fmt.Errorf("hash verification tool %q: %w", check.Command[0], errors.Join(copyErr, closeErr))
		}
		identities = append(identities, filepath.Base(check.Command[0])+"="+hex.EncodeToString(hash.Sum(nil)))
	}
	sort.Strings(identities)
	return strings.Join(identities, ","), nil
}

func testInputIdentity(ctx context.Context, dir, head, pkg string) (string, error) {
	if head == "" {
		return "", errors.New("validation head is empty")
	}
	path := "."
	if strings.HasPrefix(pkg, "./") {
		path = strings.TrimPrefix(pkg, "./")
	}
	ref := head + "^{tree}"
	if path != "." {
		ref = head + ":" + path
	}
	out, err := (gitx.Git{Dir: dir}).Run(ctx, "", "rev-parse", "--verify", ref)
	if err != nil {
		return "", fmt.Errorf("capture test input identity: %w", err)
	}
	return out, nil
}

// applyValidationEvidence replaces only evidence that the current native gate
// actually covered. Integration provenance stays attached to the original
// published merge, while recovery verification can name a later repaired main
// target truthfully.
func applyValidationEvidence(evidence *model.Evidence, plan validationPlan, checks []string) error {
	if evidence == nil {
		return errors.New("missing validation evidence")
	}
	evidence.Checks = checks
	evidence.ValidationGate = plan.Gate
	evidence.ValidationReason = plan.Reason
	evidence.ValidationInput = plan.Input
	evidence.Toolchain = plan.Toolchain
	evidence.TestInputs = plan.TestInputs
	evidence.At = time.Now().UTC()
	return nil
}
