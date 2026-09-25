package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	pathpkg "path"
	"sort"
	"strings"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

// reviewScope binds a disposition to the selected roles and the full reviewed
// change scope. A later FIX must retain this fingerprint before reuse is even
// considered; unknown paths and roster changes therefore fail closed.
func reviewScope(task *model.Task, paths, roster []string) string {
	copyPaths := append([]string(nil), paths...)
	copyRoles := append([]string(nil), roster...)
	copyTaskRoles := append([]string(nil), task.Roles...)
	sort.Strings(copyPaths)
	sort.Strings(copyRoles)
	sort.Strings(copyTaskRoles)
	payload, _ := json.Marshal(struct {
		Paths        []string `json:"paths"`
		Roster       []string `json:"roster"`
		Roles        []string `json:"roles"`
		Title        string   `json:"title"`
		Objective    string   `json:"objective"`
		Acceptance   []string `json:"acceptance"`
		Dependencies []string `json:"dependencies"`
		Areas        []string `json:"areas"`
		Domains      []string `json:"domains"`
		Decisions    []string `json:"decisions"`
		Risk         string   `json:"risk"`
		UI           bool     `json:"ui"`
		Security     bool     `json:"security"`
	}{copyPaths, copyRoles, copyTaskRoles, task.Title, task.Objective, task.Acceptance, task.Dependencies, task.Areas, task.Domains, task.Decisions, task.Risk, task.UI, task.Security})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// acceptedReviewScope accepts only a fingerprint generated from the durable
// task contract. Historical snapshots do not retain the former contract, so a
// legacy fingerprint cannot safely survive an area removal or other narrowing.
func acceptedReviewScope(task *model.Task, paths, roster []string, scope string) bool {
	return scope == reviewScope(task, paths, roster)
}

func reviewReusePaths(paths, allowed []string) bool {
	if len(paths) == 0 || len(allowed) == 0 {
		return false
	}
	for _, path := range paths {
		lower := strings.ToLower(strings.TrimSpace(path))
		base := pathpkg.Base(lower)
		if strings.HasPrefix(lower, ".aih/") || strings.HasPrefix(lower, ".github/") || strings.HasPrefix(lower, ".codex/") || strings.HasPrefix(lower, ".claude/") || strings.HasPrefix(lower, ".cursor/") ||
			base == "agents.md" || base == "claude.md" || base == "copilot-instructions.md" || strings.Contains(base, "instruction") || strings.Contains(base, "policy") ||
			strings.Contains(lower, "auth") || strings.Contains(lower, "secret") || strings.Contains(lower, "permission") || strings.Contains(lower, "crypto") || strings.Contains(lower, "network") || strings.Contains(lower, "deserial") || strings.Contains(lower, "subprocess") {
			return false
		}
		if !strings.HasSuffix(lower, ".txt") || !matchesOne(allowed, lower) {
			return false
		}
	}
	return true
}

func matchesOne(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if roles.Matches(strings.ToLower(pattern), name) {
			return true
		}
	}
	return false
}

func reviewReuseDiffSafe(diff string, paths, allowed []string) bool {
	if !reviewReusePaths(paths, allowed) {
		return false
	}
	lower := strings.ToLower(diff)
	for _, marker := range []string{
		"binary files ", "similarity index ", "rename from ", "rename to ", "copy from ", "copy to ",
		"old mode ", "new mode ", "new file mode ", "deleted file mode ",
		"@import", "url(", "http://", "https://", "//", "curl ", "invoke-webrequest", "powershell", "cmd.exe", "bash -c", "sh -c",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "index ") {
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[2] != "100644" {
				return false
			}
		}
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		for _, r := range line[1:] {
			if !(r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '.' || r == ',' || r == ':' || r == ';' || r == '!' || r == '?' || r == '(' || r == ')' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
				return false
			}
		}
	}
	return true
}

func sameRoster(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// reusableReviewDispositions implements the first deliberately small policy:
// only a clean security pass can be reused, and only across a project-explicit
// inert text-data delta. QA, designer, reviewer and custom validators
// always run for the new head.
func (c *Controller) reusableReviewDispositions(ctx context.Context, effective config.Effective, task *model.Task, roster []string, scope string) map[string]model.ReviewDisposition {
	provenance, ok := task.ReviewProvenance["security"]
	if !ok || provenance.Role != "security" || provenance.Base != task.BaseSHA || provenance.Config != effective.Hash || provenance.Rules != roles.Hash() || provenance.Scope != scope || !sameRoster(provenance.Roster, roster) || provenance.Head == task.HeadSHA || !c.P.Git.Ancestor(ctx, task.BaseSHA, provenance.Head) || !c.P.Git.Ancestor(ctx, provenance.Head, task.HeadSHA) {
		return nil
	}
	diff, paths, err := c.P.Git.Diff(ctx, provenance.Head, task.HeadSHA)
	if err != nil || diff == "" || !reviewReuseDiffSafe(diff, paths, effective.Project.ReviewReuse.SecurityDataOnlyPaths) {
		return nil
	}
	return map[string]model.ReviewDisposition{"security": {
		Disposition: "reused",
		Reason:      "prior zero-finding security review reused for configured inert text-data delta",
		SourceHead:  provenance.Head,
		Runtime:     provenance.Runtime,
	}}
}

func roleRuntime(effective config.Effective, role roles.Role) string {
	resolved := effective.Project.ResolveModel(role.Name, role.Capability)
	return effective.Project.Provider + "/" + resolved.EffectiveModel + "/" + model.Version
}

func completedDisposition(task *model.Task, role roles.Role, runtime string) model.ReviewDisposition {
	return model.ReviewDisposition{Disposition: "completed", SourceHead: task.HeadSHA, Runtime: runtime}
}

func (c *Controller) persistReviewProvenance(id string, effective config.Effective, task *model.Task, roster []string, scope string, required []roles.Role, outcomes []reviewOutcome) error {
	return c.mutate(func(snapshot *model.Snapshot) error {
		current := snapshot.Tasks[id]
		if current.ReviewProvenance == nil {
			current.ReviewProvenance = map[string]model.ReviewProvenance{}
		}
		for index, role := range required {
			result := outcomes[index].result
			if outcomes[index].err != nil || result.Status != "completed" || len(result.Findings) != 0 {
				continue
			}
			current.ReviewProvenance[role.Name] = model.ReviewProvenance{
				Role: role.Name, Base: task.BaseSHA, Head: task.HeadSHA, Config: effective.Hash, Rules: roles.Hash(),
				Roster: append([]string(nil), roster...), Scope: scope, Provider: effective.Project.Provider,
				Runtime: roleRuntime(effective, role), Summary: result.Summary, CompletedAt: time.Now().UTC(),
			}
		}
		return nil
	})
}

func validReviewDispositions(required []roles.Role, evidence *model.Evidence) bool {
	if evidence == nil || len(evidence.ReviewRoster) == 0 || len(evidence.ReviewDispositions) != len(evidence.ReviewRoster) {
		return false
	}
	roster, _ := roles.ReviewRoster(required)
	if !sameRoster(roster, evidence.ReviewRoster) || evidence.ReviewScope == "" {
		return false
	}
	for _, name := range roster {
		disposition, ok := evidence.ReviewDispositions[name]
		if !ok || (disposition.Disposition != "completed" && disposition.Disposition != "reused") || disposition.SourceHead == "" || disposition.Runtime == "" {
			return false
		}
		if disposition.Disposition == "completed" && disposition.SourceHead != evidence.Head {
			return false
		}
		if disposition.Disposition == "reused" && name != "security" {
			return false
		}
	}
	return true
}

// reviewEvidenceAccepted is shared by MergeReady admission and final
// integration. It reconstructs the roster from canonical policy instead of
// trusting the earlier in-memory review attempt.
func (c *Controller) reviewEvidenceAccepted(ctx context.Context, effective config.Effective, task *model.Task) (bool, string, error) {
	if task == nil || task.Evidence == nil {
		return false, "no durable review evidence", nil
	}
	_, paths, err := c.P.Git.Diff(ctx, task.BaseSHA, task.HeadSHA)
	if err != nil {
		return false, "could not reconstruct exact-head changed paths", err
	}
	all, err := roles.Load(effective.Files)
	if err != nil {
		return false, "could not load canonical review roles", err
	}
	required, err := roles.Required(all, task, paths, "review")
	if err != nil {
		return false, "could not select the canonical review roster", err
	}
	if visualRequirementMatches(task, effective) {
		visual, ok := all[task.VisualRequired.Role]
		if !ok || !designerReviewRole(visual) {
			return false, "the required visual reviewer is no longer configured", nil
		}
		found := false
		for _, role := range required {
			found = found || role.Name == visual.Name
		}
		if !found {
			required = append(required, visual)
		}
	}
	roster, _ := roles.ReviewRoster(required)
	if !acceptedReviewScope(task, paths, roster, task.Evidence.ReviewScope) {
		return false, "review scope changed (task contract, changed paths, or selected roster differs)", nil
	}
	if !validReviewDispositions(required, task.Evidence) {
		return false, "a required review disposition is missing, malformed, or not exact-head", nil
	}
	for name, disposition := range task.Evidence.ReviewDispositions {
		if disposition.Disposition != "reused" {
			continue
		}
		expected := c.reusableReviewDispositions(ctx, effective, task, roster, task.Evidence.ReviewScope)
		if expected == nil || expected[name].SourceHead != disposition.SourceHead || expected[name].Runtime != disposition.Runtime {
			return false, "the bounded security-review reuse exception is no longer valid", nil
		}
	}
	if visualRequirementMatches(task, effective) && (task.Evidence.Visual == nil || strings.TrimSpace(task.Evidence.Reviews[task.VisualRequired.Role]) == "") {
		return false, "required exact-head visual capture or review summary is missing", nil
	}
	return true, "", nil
}
