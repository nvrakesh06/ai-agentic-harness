package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
)

const (
	workerSoftDeadlineNumerator   = 4
	workerSoftDeadlineDenominator = 5
	workerActivityWindow          = 30 * time.Second
	handoffEvidenceTimeout        = 5 * time.Second
)

type deadlineHooks struct {
	active  func(error) bool
	event   func(string, string)
	recover func(error) provider.Result
}

func runWithCheckpoint(ctx context.Context, p provider.Provider, request provider.Request, checkpointPrompt string, hardLimit time.Duration, hooks deadlineHooks) (provider.Result, error) {
	hardCtx, cancelHard := context.WithTimeout(ctx, hardLimit)
	defer cancelHard()
	softLimit := hardLimit * workerSoftDeadlineNumerator / workerSoftDeadlineDenominator
	softCtx, cancelSoft := context.WithTimeout(hardCtx, softLimit)
	request.Timeout = softLimit
	result, err := p.Run(softCtx, request)
	cancelSoft()
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return result, err
	}
	if !hooks.active(err) {
		hooks.event("worker_soft_timeout_idle", "soft deadline reached without recent output or worktree changes; worker terminated without grace")
		return result, err
	}
	hooks.event("worker_checkpoint_requested", fmt.Sprintf("soft deadline reached after %s; starting one bounded checkpoint pass", softLimit.Round(time.Second)))
	remaining := time.Duration(0)
	if deadline, ok := hardCtx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	if remaining <= 0 {
		hooks.event("worker_hard_timeout", "hard deadline reached before checkpoint pass could start; synthetic handoff captured")
		return hooks.recover(err), nil
	}
	request.Runtime = filepath.Join(request.Runtime, "checkpoint")
	request.Prompt = checkpointPrompt
	request.Timeout = remaining
	result, checkpointErr := p.Run(hardCtx, request)
	if checkpointErr == nil {
		hooks.event("worker_checkpoint_completed", "bounded checkpoint pass returned a structured result")
		return result, nil
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if !errors.Is(checkpointErr, context.DeadlineExceeded) {
		hooks.event("worker_checkpoint_failed", "checkpoint pass failed before its deadline; returning through the ordinary implementation failure path")
		return result, checkpointErr
	}
	hooks.event("worker_hard_timeout", "hard deadline reached during checkpoint pass; synthetic handoff captured")
	return hooks.recover(checkpointErr), nil
}

func (c *Controller) workerActive(dir string, runErr error) bool {
	var invocation *provider.InvocationError
	if errors.As(runErr, &invocation) && invocation.OutputBytes > 0 && !invocation.LastActivity.IsZero() && time.Since(invocation.LastActivity) <= workerActivityWindow {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), handoffEvidenceTimeout)
	defer cancel()
	status, err := (gitx.Git{Dir: dir}).Run(ctx, "", "status", "--porcelain")
	return err == nil && status != ""
}

type timeoutHandoff struct {
	Schema            int      `json:"schema_version"`
	Reason            string   `json:"timeout_reason"`
	WorktreeStatus    []string `json:"worktree_status"`
	ChangedFiles      []string `json:"changed_files"`
	CompletedCommands []string `json:"last_completed_commands"`
}

func (c *Controller) syntheticHandoff(dir, runtimeDir string, runErr error) provider.Result {
	reason := "checkpoint pass failed before returning a structured result"
	if errors.Is(runErr, context.DeadlineExceeded) {
		reason = "worker hard deadline exhausted after a bounded checkpoint request"
	}
	handoff := timeoutHandoff{Schema: 1, Reason: reason}
	ctx, cancel := context.WithTimeout(context.Background(), handoffEvidenceTimeout)
	defer cancel()
	g := gitx.Git{Dir: dir}
	if status, err := g.Run(ctx, "", "status", "--short", "--untracked-files=all"); err == nil {
		for _, line := range strings.Split(strings.ReplaceAll(status, "\r\n", "\n"), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				handoff.WorktreeStatus = append(handoff.WorktreeStatus, c.safeHandoffText(line, 300))
			}
		}
	}
	paths := map[string]bool{}
	for _, args := range [][]string{{"diff", "--name-only", "-z", "HEAD"}, {"ls-files", "--others", "--exclude-standard", "-z"}} {
		if out, err := g.Run(ctx, "", args...); err == nil {
			for _, name := range strings.Split(out, "\x00") {
				name = strings.TrimSpace(name)
				if name != "" && safety.Path(name) == nil {
					paths[c.safeHandoffText(name, 300)] = true
				}
			}
		}
	}
	for name := range paths {
		handoff.ChangedFiles = append(handoff.ChangedFiles, name)
	}
	sort.Strings(handoff.ChangedFiles)
	handoff.CompletedCommands = c.completedCommandEvidence(filepath.Join(runtimeDir, "output.log"), filepath.Join(runtimeDir, "checkpoint", "output.log"))
	b, _ := json.MarshalIndent(handoff, "", "  ")
	if safety.Check(string(b)) == nil {
		_ = os.MkdirAll(runtimeDir, 0700)
		_ = os.WriteFile(filepath.Join(runtimeDir, "handoff.json"), b, 0600)
	}
	parts := []string{"Recovered worker timeout handoff."}
	if len(handoff.ChangedFiles) > 0 {
		parts = append(parts, "Changed files: "+strings.Join(handoff.ChangedFiles, ", ")+".")
	} else {
		parts = append(parts, "No changed files were detected.")
	}
	if len(handoff.WorktreeStatus) > 0 {
		parts = append(parts, "Worktree status: "+strings.Join(handoff.WorktreeStatus, "; ")+".")
	}
	if len(handoff.CompletedCommands) > 0 {
		parts = append(parts, "Last completed commands: "+strings.Join(handoff.CompletedCommands, "; ")+".")
	}
	return provider.Result{
		Schema:                   1,
		Status:                   "in_progress",
		Summary:                  c.safeHandoffText(strings.Join(parts, " "), 2000),
		ChangedAreas:             handoff.ChangedFiles,
		Tests:                    verificationCommands(handoff.CompletedCommands),
		Risks:                    []string{"The checkpoint worker did not return a structured result (" + reason + "); verify the recovered checkpoint before completing the task."},
		RecoveredDeadlineHandoff: true,
	}
}

func verificationCommands(commands []string) []string {
	var checks []string
	for _, command := range commands {
		lower := strings.ToLower(command)
		for _, marker := range []string{" test", "test ", "check", "lint", " vet", "vet ", " build", "build "} {
			if strings.Contains(lower, marker) {
				checks = append(checks, command)
				break
			}
		}
	}
	return checks
}

func (c *Controller) safeHandoffText(value string, limit int) string {
	value = c.portable(safety.Redact(value))
	if safety.Check(value) != nil {
		return "[redacted unsafe evidence]"
	}
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "..."
	}
	return value
}

func (c *Controller) completedCommandEvidence(paths ...string) []string {
	var commands []string
	for _, name := range paths {
		file, err := os.Open(name)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			var event any
			if json.Unmarshal(scanner.Bytes(), &event) == nil {
				for _, command := range completedCommands(event) {
					if command != "" && (len(commands) == 0 || commands[len(commands)-1] != command) {
						commands = append(commands, command)
					}
				}
			}
		}
		_ = file.Close()
	}
	if len(commands) > 5 {
		commands = commands[len(commands)-5:]
	}
	return commands
}

func completedCommands(value any) []string {
	var result []string
	switch current := value.(type) {
	case []any:
		for _, child := range current {
			result = append(result, completedCommands(child)...)
		}
	case map[string]any:
		typeName, _ := current["type"].(string)
		status, _ := current["status"].(string)
		_, exited := current["exit_code"]
		if strings.Contains(strings.ToLower(typeName), "command") && (exited || strings.EqualFold(status, "completed")) {
			if command := commandIdentifier(current["command"]); command != "" {
				result = append(result, command)
			}
		}
		for _, child := range current {
			result = append(result, completedCommands(child)...)
		}
	}
	return result
}

// commandIdentifier returns only fixed verification/check families. Provider
// command arguments may contain headers, credential URLs, or arbitrary secret
// flags and are never copied into portable state or events.
func commandIdentifier(value any) string {
	var parts []string
	switch command := value.(type) {
	case string:
		parts = strings.Fields(command)
	case []any:
		for _, part := range command {
			if text, ok := part.(string); ok {
				parts = append(parts, text)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	executable := commandWord(parts[0])
	subcommand := ""
	if len(parts) > 1 {
		subcommand = commandWord(parts[1])
	}
	switch executable {
	case "go":
		return fixedCommand("go", subcommand, "test", "vet", "build", "generate", "fmt")
	case "cargo":
		return fixedCommand("cargo", subcommand, "test", "check", "build", "clippy", "fmt")
	case "dotnet":
		return fixedCommand("dotnet", subcommand, "test", "build", "format")
	case "mvn", "mvnw":
		return fixedCommand("mvn", subcommand, "test", "verify", "package")
	case "gradle", "gradlew":
		return fixedCommand("gradle", subcommand, "test", "check", "build")
	case "npm", "pnpm", "yarn", "bun":
		if subcommand == "run" && len(parts) > 2 {
			if script := fixedCommand("run", commandWord(parts[2]), "test", "check", "lint", "build"); script != "" {
				return executable + " " + script
			}
		}
		return fixedCommand(executable, subcommand, "test", "check", "lint", "build")
	case "git":
		return fixedCommand("git", subcommand, "diff", "status")
	case "pytest", "ruff", "eslint", "tsc", "golangci-lint":
		return executable
	}
	return ""
}

func commandWord(value string) string {
	value = strings.Trim(value, "\"'")
	value = strings.ReplaceAll(value, "\\", "/")
	if slash := strings.LastIndex(value, "/"); slash >= 0 {
		value = value[slash+1:]
	}
	value = strings.ToLower(value)
	for _, suffix := range []string{".exe", ".cmd", ".bat"} {
		value = strings.TrimSuffix(value, suffix)
	}
	return value
}

func fixedCommand(executable, candidate string, allowed ...string) string {
	for _, value := range allowed {
		if candidate == value {
			return executable + " " + value
		}
	}
	return ""
}
