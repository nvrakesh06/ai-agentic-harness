package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

func preflightMatches(p *model.Preflight, t *model.Task, effective config.Effective) bool {
	return p != nil && p.BaseSHA == effective.BaseSHA && p.HeadSHA == t.HeadSHA &&
		p.Config == effective.Hash && p.Rules == roles.Hash() &&
		(p.Scope == "" || p.Scope == preflightScope(t, effective))
}

// reusablePreflightForFix deliberately accepts a changed task head only after
// every required pre-implementation role completed and the task is returning
// through the bounded FIX route. The scope/base/config/rules fingerprint keeps
// a changed contract or role policy from inheriting old advice.
func reusablePreflightForFix(p *model.Preflight, t *model.Task, effective config.Effective, required []roles.Role) bool {
	if p == nil || t == nil || t.State != model.Fix || p.Phase != "writing" || p.HeadSHA == t.HeadSHA ||
		p.BaseSHA != effective.BaseSHA || p.Config != effective.Hash || p.Rules != roles.Hash() ||
		p.Scope == "" || p.Scope != preflightScope(t, effective) || p.ReuseCount >= effective.Policy.ImplementationRetries {
		return false
	}
	for _, role := range required {
		if !slices.Contains(p.Completed, role.Name) {
			return false
		}
	}
	return true
}

func reusePreflight(p *model.Preflight, t *model.Task) {
	p.HeadSHA = t.HeadSHA
	p.Phase = "ready"
	p.ReuseCount++
	p.ReuseReason = "reused completed pre-implementation guidance for bounded FIX: scope, base, policy, and role rules unchanged"
}

func reusePreflightForFix(p *model.Preflight, t *model.Task, effective config.Effective, required []roles.Role) bool {
	if !reusablePreflightForFix(p, t, effective, required) {
		return false
	}
	reusePreflight(p, t)
	return true
}

func findingsFingerprint(findings []model.Finding) string {
	copy := append([]model.Finding(nil), findings...)
	sort.Slice(copy, func(i, j int) bool {
		left, _ := json.Marshal(copy[i])
		right, _ := json.Marshal(copy[j])
		return string(left) < string(right)
	})
	b, _ := json.Marshal(copy)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var directFixLocation = regexp.MustCompile(`^[A-Za-z0-9_./-]+\.(?:tsx|jsx|css|scss|html|vue|svelte):[1-9][0-9]*$`)

func directFixSensitive(text string) bool {
	text = strings.ToLower(text)
	for _, marker := range []string{"schema", "migration", "security", "auth", "secret", "permission", "credential", "architecture", "policy", "dependency", "package", "lockfile", "api contract", "database", "serialize", "network", "subprocess"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func directFixFinding(finding model.Finding) bool {
	category := strings.ToLower(strings.TrimSpace(finding.Category))
	if finding.Role != "designer" || (category != "visual" && category != "layout" && category != "text-layout" && category != "text layout") ||
		!directFixLocation.MatchString(strings.TrimSpace(finding.Location)) {
		return false
	}
	detail := strings.ToLower(strings.Join([]string{finding.Category, finding.Location, finding.Reason, finding.Resolution}, " "))
	if directFixSensitive(detail) || (!strings.Contains(detail, "text") && !strings.Contains(detail, "layout") && !strings.Contains(detail, "caption") && !strings.Contains(detail, "label") && !strings.Contains(detail, "overlap") && !strings.Contains(detail, "typograph")) {
		return false
	}
	for _, ambiguous := range []string{"?", "maybe", "might", "consider", "investigate", "unclear", "unknown"} {
		if strings.Contains(detail, ambiguous) {
			return false
		}
	}
	reason := strings.ToLower(finding.Reason)
	resolution := strings.ToLower(finding.Resolution)
	if !containsAny(reason, "overlap", "overflow", "clip", "truncat", "non-breaking", "nbsp", "text-fit", "wrap", "line break") ||
		!containsAny(resolution, "use ", "wrap", "replace", "apply", "add", "remove", "set ", "adjust", "ensure", "render", "measure", "validate", "test") ||
		!containsAny(resolution, "text-fit", "wrap", "white-space", "nbsp", "non-breaking", "line-break", "overflow", "width", "caption", "validation", "test") {
		return false
	}
	return true
}

func completedReviewRoster(evidence *model.Evidence) bool {
	if evidence == nil || len(evidence.ReviewRoster) == 0 {
		return false
	}
	for _, role := range evidence.ReviewRoster {
		if strings.TrimSpace(evidence.Reviews[role]) == "" {
			return false
		}
	}
	return true
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func directFixFindings(t *model.Task) ([]model.Finding, bool) {
	if t == nil || !t.UI || t.Security || len(t.Findings) == 0 || len(t.Findings) > 2 {
		return nil, false
	}
	guidance := make([]string, 0, len(model.TaskGuidance(t)))
	for _, item := range model.TaskGuidance(t) {
		guidance = append(guidance, item.Text)
	}
	// Acceptance criteria and assigned role names can require an architecture
	// review without making this narrowly located UI repair architecture-sensitive.
	// Those specialists remain required below; source-scope sensitivity comes from
	// the objective, affected areas/domains, dependencies, guidance, and findings.
	if directFixSensitive(strings.Join(append(append(append(append([]string{t.Objective}, t.Areas...), t.Domains...), t.Dependencies...), guidance...), " ")) {
		return nil, false
	}
	seenLocations := map[string]bool{}
	for _, finding := range t.Findings {
		if !directFixFinding(finding) || seenLocations[finding.Location] {
			return nil, false
		}
		seenLocations[finding.Location] = true
	}
	return append([]model.Finding(nil), t.Findings...), true
}

// directFixWaiver is deliberately narrower than normal FIX guidance reuse. It
// records only a completed built-in designer review of at most two exact visual repairs.
func directFixWaiver(t *model.Task, effective config.Effective, required []roles.Role) *model.Preflight {
	if t == nil || t.State != model.Review || t.Evidence == nil || t.Evidence.Base != effective.BaseSHA || t.Evidence.Head != t.HeadSHA ||
		t.Evidence.Config != effective.Hash || t.Evidence.Rules != roles.Hash() || len(t.Evidence.Checks) == 0 || !completedReviewRoster(t.Evidence) || t.Evidence.Reviews["designer"] == "" {
		return nil
	}
	if !slices.Contains(t.Evidence.ReviewRoster, "designer") || !slices.ContainsFunc(required, func(role roles.Role) bool { return role.Name == "designer" && role.Stage == "review" }) {
		return nil
	}
	findings, ok := directFixFindings(t)
	if !ok {
		return nil
	}
	scope := preflightScope(t)
	return &model.Preflight{Phase: "queued", BaseSHA: effective.BaseSHA, HeadSHA: t.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Scope: scope,
		ReuseReason: "direct FIX route: waived built-in designer preflight after exact-head native checks and up to two located text-layout review findings",
		DirectFix:   &model.DirectFixWaiver{Role: "designer", Disposition: "waived", Reason: "completed exact-head designer review identified a bounded set of specific visual/text-layout repairs", BaseSHA: effective.BaseSHA, HeadSHA: t.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Scope: scope, Findings: findingsFingerprint(findings)}}
}

func directFixWaiverMatches(p *model.Preflight, t *model.Task, effective config.Effective) bool {
	if p == nil || p.DirectFix == nil || !preflightMatches(p, t, effective) || t == nil || t.Evidence == nil || t.Evidence.Base != effective.BaseSHA ||
		t.Evidence.Head != t.HeadSHA || t.Evidence.Config != effective.Hash || t.Evidence.Rules != roles.Hash() || len(t.Evidence.Checks) == 0 || !completedReviewRoster(t.Evidence) || t.Evidence.Reviews["designer"] == "" {
		return false
	}
	findings, ok := directFixFindings(t)
	w := p.DirectFix
	return ok && w.Role == "designer" && w.Disposition == "waived" && w.BaseSHA == effective.BaseSHA && w.HeadSHA == t.HeadSHA &&
		w.Config == effective.Hash && w.Rules == roles.Hash() && w.Scope == preflightScope(t) && w.Findings == findingsFingerprint(findings)
}

func preflightRoleSatisfied(p *model.Preflight, t *model.Task, effective config.Effective, role roles.Role) bool {
	return slices.Contains(p.Completed, role.Name) || (role.Name == "designer" && role.Stage == "review" && directFixWaiverMatches(p, t, effective))
}

type preflightScopeInput struct {
	Objective  string           `json:"objective"`
	Acceptance []string         `json:"acceptance"`
	Areas      []string         `json:"areas"`
	Domains    []string         `json:"domains"`
	Risk       string           `json:"risk"`
	UI         bool             `json:"ui"`
	Security   bool             `json:"security"`
	Roles      []string         `json:"roles"`
	DependsOn  []string         `json:"depends_on"`
	Guidance   []model.Guidance `json:"guidance,omitempty"`
}

// preflightScope records inputs that define the task's specialist contract.
// Source-head changes are intentionally excluded: exact-head checks and final
// review cover those changes after the bounded repair.
func preflightScope(t *model.Task, effective config.Effective) string {
	input := preflightScopeInput{Objective: t.Objective, Acceptance: append([]string(nil), t.Acceptance...), Areas: append([]string(nil), t.Areas...), Domains: append([]string(nil), t.Domains...), Risk: t.Risk, UI: t.UI, Security: t.Security, Roles: append([]string(nil), t.Roles...), DependsOn: append([]string(nil), t.Dependencies...)}
	input.Guidance = model.EligibleGuidance(t, effective.BaseSHA, effective.Hash, roles.Hash())
	for _, values := range [][]string{input.Acceptance, input.Areas, input.Domains, input.Roles, input.DependsOn} {
		sort.Strings(values)
	}
	b, _ := json.Marshal(input)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// resetInterruptedPreflight releases reader ownership without discarding the
// completed guidance marker needed by a later FIX after verification recovery.
func resetInterruptedPreflight(p *model.Preflight) {
	if p != nil && p.Phase != "ready" && p.Phase != "writing" {
		p.Phase = "queued"
	}
}

func requiredPreflightRoles(effective config.Effective, t *model.Task) ([]roles.Role, error) {
	all, err := roles.Load(effective.Files)
	if err != nil {
		return nil, err
	}
	pre, err := roles.Required(all, t, t.Areas, "pre-implementation")
	if err != nil {
		return nil, err
	}
	if t.UI && !slices.ContainsFunc(pre, func(r roles.Role) bool { return r.Name == "designer" }) {
		pre = append(pre, all["designer"])
	}
	return pre, nil
}

type preflightDispatch struct {
	task   *model.Task
	guided bool
}

// selectPreflights bounds both worktree preparation and reader waiters. A
// separate fast allowance keeps tasks without guidance moving when readers are
// occupied by unrelated reviews or UI preflights.
func selectPreflights(s *model.Snapshot, active map[string]bool, guidedActive map[string]bool, maxReaders, maxWriters int, configured map[string]roles.Role) []preflightDispatch {
	fastActive, guidedCount := 0, 0
	for id, writing := range active {
		if writing {
			continue
		}
		if guidedActive[id] {
			guidedCount++
		} else {
			fastActive++
		}
	}
	fastSlots, guidedSlots := maxWriters-fastActive, maxReaders+1-guidedCount
	if fastSlots < 0 {
		fastSlots = 0
	}
	if guidedSlots < 0 {
		guidedSlots = 0
	}
	var fastReady, guidedReady []preflightDispatch
	for _, t := range model.Ordered(s) {
		if hasActive(active, t.ID) || (t.State != model.Ready && t.State != model.Fix) || (t.Preflight != nil && t.Preflight.Phase == "ready") {
			continue
		}
		waiting := false
		for _, dep := range t.Dependencies {
			if s.Tasks[dep] == nil || s.Tasks[dep].State != model.Done {
				waiting = true
				break
			}
		}
		if waiting {
			continue
		}
		pre, err := roles.Required(configured, t, t.Areas, "pre-implementation")
		needsReader := err != nil || len(pre) > 0 || t.UI
		if needsReader && len(guidedReady) < guidedSlots {
			guidedReady = append(guidedReady, preflightDispatch{task: t, guided: true})
		}
		if !needsReader && len(fastReady) < fastSlots {
			fastReady = append(fastReady, preflightDispatch{task: t})
		}
	}
	return append(fastReady, guidedReady...)
}

// preflight runs reader guidance without occupying a writer slot. Each completed
// role is committed before the next one starts, so takeover resumes at the first
// unfinished role without depending on provider conversation history.
func (c *Controller) preflight(id string) {
	if c.ctx.Err() != nil {
		return
	}
	effective, err := c.effective(c.ctx)
	if err != nil {
		c.block(id, "Restore Git access and retry.", err.Error(), model.Ready)
		return
	}
	t := c.Snapshot().Tasks[id]
	if t == nil || (t.State != model.Ready && t.State != model.Fix) {
		return
	}
	pre, err := requiredPreflightRoles(effective, t)
	if err != nil {
		c.block(id, "Correct role configuration or required roles.", err.Error(), model.Ready)
		return
	}
	if err = c.ensureWorktree(id); err != nil {
		c.block(id, "Repair the task worktree and retry.", err.Error(), model.Ready)
		return
	}
	if t.SyncBase != "" {
		if err = c.P.Git.PrepareMerge(c.ctx, c.P.TaskPath(t), t.SyncBase); err != nil {
			c.block(id, "Repair task synchronization and retry.", err.Error(), model.Fix)
			return
		}
	}
	if err = c.mutate(func(s *model.Snapshot) error {
		task := s.Tasks[id]
		if task.State != model.Ready && task.State != model.Fix {
			return errors.New("task left preflight eligibility")
		}
		if !preflightMatches(task.Preflight, task, effective) {
			if !reusePreflightForFix(task.Preflight, task, effective, pre) {
				task.Preflight = &model.Preflight{Phase: "queued", BaseSHA: effective.BaseSHA, HeadSHA: task.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Scope: preflightScope(task, effective)}
			}
		}
		return nil
	}); err != nil {
		return
	}
	for _, r := range pre {
		t = c.Snapshot().Tasks[id]
		if t == nil || !preflightMatches(t.Preflight, t, effective) {
			return
		}
		if preflightRoleSatisfied(t.Preflight, t, effective, r) {
			continue
		}
		if err = c.mutate(func(s *model.Snapshot) error {
			task := s.Tasks[id]
			if preflightMatches(task.Preflight, task, effective) {
				task.Preflight.Phase = "waiting"
			}
			return nil
		}); err != nil {
			return
		}
		t = c.Snapshot().Tasks[id]
		if t == nil || t.Preflight == nil || t.Preflight.Phase != "waiting" {
			return
		}
		result, roleErr := c.roleWithCompletion(c.ctx, effective, r, t, c.P.TaskPath(t), "Provide pre-implementation guidance for the assigned task.", "", "", func(s *model.Snapshot, result provider.Result, runErr error) error {
			task := s.Tasks[id]
			if task == nil || !preflightMatches(task.Preflight, task, effective) || runErr != nil || result.Status != "completed" || (r.Stage == "pre-implementation" && roles.Blocking(r, result.Findings)) {
				return nil
			}
			task.Decisions = append(task.Decisions, r.Name+": "+result.Summary)
			task.Preflight.Completed = append(task.Preflight.Completed, r.Name)
			task.Preflight.Phase = "queued"
			return nil
		})
		if c.ctx.Err() != nil {
			return
		}
		current := c.Snapshot().Tasks[id]
		if current == nil || !preflightMatches(current.Preflight, current, effective) {
			return
		}
		if roleErr != nil {
			c.retry(id, "implementation", roleErr.Error())
			return
		}
		if result.Status == "blocked" {
			question := strings.TrimSpace(result.Question)
			if question == "" {
				question = "Resolve the pre-implementation guidance blocker."
			}
			c.block(id, question, result.Summary, model.Ready)
			return
		}
		if result.Status != "completed" {
			c.retry(id, "implementation", r.Name+": "+result.Summary)
			return
		}
		if r.Stage == "pre-implementation" && roles.Blocking(r, result.Findings) {
			c.block(id, "Resolve the pre-implementation specialist's blocking concerns.", result.Summary, model.Ready)
			return
		}
	}
	_ = c.mutate(func(s *model.Snapshot) error {
		task := s.Tasks[id]
		if preflightMatches(task.Preflight, task, effective) {
			task.Preflight.Phase = "ready"
		}
		return nil
	})
}

// admitWriter runs on the scheduler goroutine. It checks the current canonical
// base and writer conflicts after reader guidance and before writer reservation.
func (c *Controller) admitWriter(id string, active map[string]bool) (bool, error) {
	effective, err := c.effective(c.ctx)
	if err != nil {
		return false, err
	}
	s := c.Snapshot()
	t := s.Tasks[id]
	if t == nil || (t.State != model.Ready && t.State != model.Fix) || t.Preflight == nil || t.Preflight.Phase != "ready" {
		return false, nil
	}
	if !preflightMatches(t.Preflight, t, effective) {
		return false, c.mutate(func(s *model.Snapshot) error { s.Tasks[id].Preflight = nil; return nil })
	}
	pre, err := requiredPreflightRoles(effective, t)
	if err != nil {
		return false, err
	}
	for _, role := range pre {
		if !preflightRoleSatisfied(t.Preflight, t, effective, role) {
			return false, c.mutate(func(s *model.Snapshot) error { s.Tasks[id].Preflight = nil; return nil })
		}
	}
	for _, dep := range t.Dependencies {
		if s.Tasks[dep] == nil || s.Tasks[dep].State != model.Done {
			return false, nil
		}
	}
	count := 0
	for peer, writing := range active {
		if !writing {
			continue
		}
		other := s.Tasks[peer]
		if other == nil || other.State != model.Running {
			continue
		}
		count++
		for _, d := range t.Domains {
			if slices.Contains(other.Domains, d) {
				return false, nil
			}
		}
	}
	if count >= c.P.Config.Project.MaxWriters {
		return false, nil
	}
	admitted := true
	err = c.mutate(func(s *model.Snapshot) error {
		task := s.Tasks[id]
		if task == nil || task.Preflight == nil || task.Preflight.Phase != "ready" {
			admitted = false
			return nil
		}
		if !preflightMatches(task.Preflight, task, effective) {
			task.Preflight = nil
			admitted = false
			return nil
		}
		for _, role := range pre {
			if !preflightRoleSatisfied(task.Preflight, task, effective, role) {
				task.Preflight = nil
				admitted = false
				return nil
			}
		}
		if err := model.Transition(task, model.Running); err != nil {
			return err
		}
		task.Preflight.Phase = "writing"
		return nil
	})
	return admitted, err
}
