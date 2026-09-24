package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
		Paths    []string `json:"paths"`
		Roster   []string `json:"roster"`
		Roles    []string `json:"roles"`
		Risk     string   `json:"risk"`
		UI       bool     `json:"ui"`
		Security bool     `json:"security"`
	}{copyPaths, copyRoles, copyTaskRoles, task.Risk, task.UI, task.Security})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func reviewReusePaths(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, path := range paths {
		lower := strings.ToLower(strings.TrimSpace(path))
		if strings.HasPrefix(lower, ".aih/") || strings.Contains(lower, "auth") || strings.Contains(lower, "secret") || strings.Contains(lower, "permission") || strings.Contains(lower, "crypto") || strings.Contains(lower, "network") || strings.Contains(lower, "deserial") || strings.Contains(lower, "subprocess") {
			return false
		}
		// The first policy intentionally accepts plain Markdown only. CSS/SCSS can
		// import fonts or other resources through many equivalent syntaxes, and
		// MDX can execute JSX/JavaScript. Expand this only with a parser-backed,
		// separately reviewed classifier.
		if !strings.HasSuffix(lower, ".md") {
			return false
		}
	}
	return true
}

func reviewReuseDiffSafe(diff string, paths []string) bool {
	if !reviewReusePaths(paths) {
		return false
	}
	lower := strings.ToLower(diff)
	for _, marker := range []string{
		"binary files ", "similarity index ", "rename from ", "rename to ", "old mode ", "new mode ",
		"@import", "url(", "http://", "https://", "//", "curl ", "invoke-webrequest", "powershell", "cmd.exe", "bash -c", "sh -c",
	} {
		if strings.Contains(lower, marker) {
			return false
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
// only a clean security pass can be reused, and only across an intervening
// plain-Markdown documentation-only delta. QA, designer, reviewer and custom validators
// always run for the new head.
func (c *Controller) reusableReviewDispositions(ctx context.Context, effective config.Effective, task *model.Task, roster []string, scope string) map[string]model.ReviewDisposition {
	provenance, ok := task.ReviewProvenance["security"]
	if !ok || provenance.Role != "security" || provenance.Base != task.BaseSHA || provenance.Config != effective.Hash || provenance.Rules != roles.Hash() || provenance.Scope != scope || !sameRoster(provenance.Roster, roster) || provenance.Head == task.HeadSHA || !c.P.Git.Ancestor(ctx, task.BaseSHA, provenance.Head) || !c.P.Git.Ancestor(ctx, provenance.Head, task.HeadSHA) {
		return nil
	}
	diff, paths, err := c.P.Git.Diff(ctx, provenance.Head, task.HeadSHA)
	if err != nil || diff == "" || !reviewReuseDiffSafe(diff, paths) {
		return nil
	}
	return map[string]model.ReviewDisposition{"security": {
		Disposition: "reused",
		Reason:      "prior zero-finding security review reused for plain-Markdown documentation delta",
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
func (c *Controller) reviewEvidenceAccepted(ctx context.Context, effective config.Effective, task *model.Task) (bool, error) {
	if task == nil || task.Evidence == nil {
		return false, nil
	}
	_, paths, err := c.P.Git.Diff(ctx, task.BaseSHA, task.HeadSHA)
	if err != nil {
		return false, err
	}
	all, err := roles.Load(effective.Files)
	if err != nil {
		return false, err
	}
	required, err := roles.Required(all, task, paths, "review")
	if err != nil {
		return false, err
	}
	if visualRequirementMatches(task, effective) {
		visual, ok := all[task.VisualRequired.Role]
		if !ok || !designerReviewRole(visual) {
			return false, nil
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
	if task.Evidence.ReviewScope != reviewScope(task, paths, roster) || !validReviewDispositions(required, task.Evidence) {
		return false, nil
	}
	for name, disposition := range task.Evidence.ReviewDispositions {
		if disposition.Disposition != "reused" {
			continue
		}
		expected := c.reusableReviewDispositions(ctx, effective, task, roster, task.Evidence.ReviewScope)
		if expected == nil || expected[name].SourceHead != disposition.SourceHead || expected[name].Runtime != disposition.Runtime {
			return false, nil
		}
	}
	if visualRequirementMatches(task, effective) && (task.Evidence.Visual == nil || strings.TrimSpace(task.Evidence.Reviews[task.VisualRequired.Role]) == "") {
		return false, nil
	}
	return true, nil
}
