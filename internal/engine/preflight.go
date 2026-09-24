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
var actionableSourceLocation = regexp.MustCompile(`^[A-Za-z0-9_./-]+\.[A-Za-z0-9_+-]+:[1-9][0-9]*$`)
var renderedFrameEvidenceLocation = regexp.MustCompile(`^Rendered-frame evidence for head [a-f0-9]{40}$`)

func directFixSensitive(text string) bool {
	text = strings.ToLower(text)
	for _, marker := range []string{"schema", "migration", "security", "auth", "secret", "permission", "credential", "architecture", "policy", "dependency", "package", "lockfile", "api contract", "database", "serialize", "network", "subprocess"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// preflightEvidenceFix is intentionally narrower than ordinary guidance. It
// permits one implementer pass only when a pre-implementation specialist has
// located a concrete source defect but cannot inspect the exact rendered frame
// in its restricted environment. It does not make a visual approval claim:
// final review must still receive supervisor-captured visual evidence.
func preflightEvidenceFix(role roles.Role, result provider.Result) bool {
	_, ok := preflightEvidenceSourceFindings(role, result)
	return ok
}

func preflightVisualEvidenceDeferral(role roles.Role, result provider.Result) bool {
	preflightSpecialist := role.Stage == "pre-implementation" || role.Name == "designer"
	if !preflightSpecialist || result.Status != "in_progress" || preflightHumanDecision(result) || !supervisorEvidenceRequest(result) || !visualEvidenceRequest(result) || len(result.Findings) == 0 {
		return false
	}
	for _, finding := range result.Findings {
		if !preflightVisualEvidenceOnlyFinding(finding) {
			return false
		}
	}
	return true
}

// preflightEvidenceSourceFindings separates an evidence-only visual finding
// from source repairs. A specialist may report that it cannot inspect the
// rendered frame and still identify one or two exact source changes. Only the
// latter are passed to the implementer; the former remains a final-review
// requirement and can never count as visual approval.
func preflightEvidenceSourceFindings(role roles.Role, result provider.Result) ([]model.Finding, bool) {
	// UI tasks currently inject the built-in designer into preflight even though
	// that reusable role is also configured as a review-stage validator.
	preflightSpecialist := role.Stage == "pre-implementation" || role.Name == "designer"
	if !preflightSpecialist || result.Status != "in_progress" ||
		preflightHumanDecision(result) || !supervisorEvidenceRequest(result) || !visualEvidenceRequest(result) || len(result.Findings) == 0 {
		return nil, false
	}
	sources := make([]model.Finding, 0, 2)
	for _, finding := range result.Findings {
		if actionablePreflightSourceFinding(finding) {
			sources = append(sources, finding)
			continue
		}
		if !preflightVisualEvidenceOnlyFinding(finding) {
			return nil, false
		}
	}
	return sources, len(sources) > 0 && len(sources) <= 2
}

func actionablePreflightSourceFinding(finding model.Finding) bool {
	if !actionableSourceLocation.MatchString(strings.TrimSpace(finding.Location)) {
		return false
	}
	severity := strings.ToLower(strings.TrimSpace(finding.Severity))
	if severity != "high" && severity != "medium" {
		return false
	}
	detail := strings.ToLower(strings.Join([]string{finding.Category, finding.Location, finding.Reason, finding.Resolution}, " "))
	if strings.TrimSpace(finding.Category) == "" || strings.Contains(detail, "verification") || preflightEvidenceSensitive(detail) {
		return false
	}
	for _, vague := range []string{"?", "maybe", "might", "consider", "investigate", "unclear", "unknown", "looks wrong", "make it better"} {
		if strings.Contains(detail, vague) {
			return false
		}
	}
	return strings.TrimSpace(finding.Reason) != "" &&
		containsAny(strings.ToLower(finding.Resolution), "use ", "replace", "apply", "add", "remove", "set ", "adjust", "ensure", "render", "measure", "validate", "test", "wrap")
}

func preflightVisualEvidenceOnlyFinding(finding model.Finding) bool {
	category := strings.ToLower(strings.TrimSpace(finding.Category))
	if !containsAny(category, "visual", "verification") {
		return false
	}
	// The enclosing result has already been classified by the shared
	// supervisorEvidenceRequest and visualEvidenceRequest helpers. Do not make
	// this partition depend on a second copy of their request vocabulary: a
	// finding can accurately say "have the supervisor supply captures" without
	// repeating the word "provide". It must still name visual evidence and avoid
	// a source location or a consequential scope marker.
	text := strings.ToLower(strings.Join([]string{finding.Reason, finding.Resolution}, " "))
	location := strings.TrimSpace(finding.Location)
	return (location == "" || renderedFrameEvidenceLocation.MatchString(location)) &&
		containsAny(text, "rendered", "frame", "browser", "capture", "screenshot", "playwright", "visual evidence") &&
		!preflightEvidenceSensitive(text)
}

func preflightEvidenceSensitive(text string) bool {
	for _, marker := range []string{"security", "auth", "secret", "permission", "credential", "product decision", "choose whether", "accept risk", "authorize", "production access", "destructive migration", "api contract", "database", "network", "subprocess"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func preflightHumanDecision(result provider.Result) bool {
	if result.Status != "blocked" && result.Status != "in_progress" {
		return false
	}
	text := strings.ToLower(strings.Join([]string{result.Question, result.Summary, strings.Join(result.Risks, " ")}, " "))
	return containsAny(text, "product decision", "choose whether", "accept risk", "authorize an exception", "approve an exception", "provide credentials", "threat model", "security boundary")
}

func directFixFinding(finding model.Finding, visualRoles map[string]bool) bool {
	category := strings.ToLower(strings.TrimSpace(finding.Category))
	if !visualRoles[finding.Role] || (category != "visual" && category != "layout" && category != "text-layout" && category != "text layout") ||
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

func visualReviewRoles(effective config.Effective, evidence *model.Evidence) map[string]bool {
	if !completedReviewRoster(evidence) {
		return nil
	}
	all, err := roles.Load(effective.Files)
	if err != nil {
		return nil
	}
	visual := map[string]bool{}
	for _, name := range evidence.ReviewRoster {
		role, ok := all[name]
		if !ok {
			return nil
		}
		if designerReviewRole(role) {
			visual[name] = true
		}
	}
	return visual
}

func designerReviewRole(role roles.Role) bool {
	return role.Stage == "review" && role.Mode == "validator" && (role.Name == "designer" || role.Extends == "designer")
}

func isDesignerReviewRole(effective config.Effective, name string) bool {
	all, err := roles.Load(effective.Files)
	return err == nil && designerReviewRole(all[name])
}

// directFixRoute is the retry admission decision. The retry counter remains
// keyed to the reporting role; this only decides whether that role belongs to
// the completed designer review family.
func directFixRoute(effective config.Effective, retryRole string, task *model.Task, required []roles.Role) *model.Preflight {
	if !isDesignerReviewRole(effective, retryRole) {
		return nil
	}
	return directFixWaiver(task, effective, required)
}

func directFixSensitivePath(value string) bool {
	value = strings.ReplaceAll(value, `\`, "/")
	return strings.Contains(value, "/") && directFixSensitive(value)
}

func directFixFindings(t *model.Task, visualRoles map[string]bool) ([]model.Finding, bool) {
	if t == nil || !t.UI || t.Security || len(t.Findings) == 0 || len(t.Findings) > 2 {
		return nil, false
	}
	for _, path := range append(append(append([]string(nil), t.Areas...), t.Domains...), t.Dependencies...) {
		if directFixSensitivePath(path) {
			return nil, false
		}
	}
	seenLocations := map[string]bool{}
	for _, finding := range t.Findings {
		if !directFixFinding(finding, visualRoles) || seenLocations[finding.Location] {
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
		t.Evidence.Config != effective.Hash || t.Evidence.Rules != roles.Hash() || len(t.Evidence.Checks) == 0 || !completedReviewRoster(t.Evidence) {
		return nil
	}
	if !slices.ContainsFunc(required, func(role roles.Role) bool { return role.Name == "designer" && role.Stage == "review" }) {
		return nil
	}
	visualRoles := visualReviewRoles(effective, t.Evidence)
	findings, ok := directFixFindings(t, visualRoles)
	if !ok {
		return nil
	}
	scope := preflightScope(t, effective)
	return &model.Preflight{Phase: "queued", BaseSHA: effective.BaseSHA, HeadSHA: t.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Scope: scope,
		ReuseReason: "direct FIX route: waived built-in designer preflight after exact-head native checks and up to two located text-layout review findings",
		DirectFix:   &model.DirectFixWaiver{Role: "designer", Disposition: "waived", Reason: "completed exact-head designer review identified a bounded set of specific visual/text-layout repairs", BaseSHA: effective.BaseSHA, HeadSHA: t.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Scope: scope, Findings: findingsFingerprint(findings)}}
}

func directFixWaiverMatches(p *model.Preflight, t *model.Task, effective config.Effective) bool {
	if p == nil || p.DirectFix == nil || !preflightMatches(p, t, effective) || t == nil || t.Evidence == nil || t.Evidence.Base != effective.BaseSHA ||
		t.Evidence.Head != t.HeadSHA || t.Evidence.Config != effective.Hash || t.Evidence.Rules != roles.Hash() || len(t.Evidence.Checks) == 0 || !completedReviewRoster(t.Evidence) {
		return false
	}
	findings, ok := directFixFindings(t, visualReviewRoles(effective, t.Evidence))
	w := p.DirectFix
	return ok && w.Role == "designer" && w.Disposition == "waived" && w.BaseSHA == effective.BaseSHA && w.HeadSHA == t.HeadSHA &&
		w.Config == effective.Hash && w.Rules == roles.Hash() && w.Scope == preflightScope(t, effective) && w.Findings == findingsFingerprint(findings)
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
			if task == nil || !preflightMatches(task.Preflight, task, effective) || runErr != nil {
				return nil
			}
			completed := result.Status == "completed" && !(r.Stage == "pre-implementation" && roles.Blocking(r, result.Findings))
			evidenceFix := preflightEvidenceFix(r, result)
			evidenceDeferral := preflightVisualEvidenceDeferral(r, result)
			if !completed && !evidenceFix && !evidenceDeferral {
				return nil
			}
			task.Decisions = append(task.Decisions, r.Name+": "+result.Summary)
			if evidenceFix || evidenceDeferral {
				task.VisualRequired = &model.VisualRequirement{Role: r.Name, Base: effective.BaseSHA, Head: task.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Reason: "pre-implementation specialist requested supervisor-owned exact-head rendered evidence; final visual review remains required"}
			}
			if evidenceFix {
				findings, _ := preflightEvidenceSourceFindings(r, result)
				findings = append([]model.Finding(nil), findings...)
				for index := range findings {
					findings[index].Role = r.Name
				}
				task.Findings = appendUniqueFindings(task.Findings, findings)
				task.Decisions = append(task.Decisions, r.Name+": supervisor-owned exact-head visual evidence requested; preserve the located source repairs for one bounded implementer pass. Final visual review must still use rendered evidence.")
			}
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
		if preflightEvidenceFix(r, result) || (preflightVisualEvidenceDeferral(r, result) && effective.Project.VisualCapture != nil) {
			_ = c.P.DB.Event(id, current.RunID, r.Name, effective.Project.Provider, "preflight_visual_evidence_fix_admitted", "exact-head visual evidence remains required for final review; concrete source findings preserved for one bounded implementer pass")
			continue
		}
		if result.Status == "blocked" {
			question := strings.TrimSpace(result.Question)
			if question == "" {
				question = "Resolve the pre-implementation guidance blocker."
			}
			c.block(id, question, result.Summary, model.Ready)
			return
		}
		if preflightHumanDecision(result) {
			question := strings.TrimSpace(result.Question)
			if question == "" {
				question = "Resolve the pre-implementation product decision."
			}
			c.block(id, question, result.Summary, model.Ready)
			return
		}
		if result.Status == "in_progress" && supervisorEvidenceRequest(result) && visualEvidenceRequest(result) {
			reason := "No concrete actionable source finding accompanied this exact-head visual evidence request. The specialist will not be rerun until visual capture is available."
			_ = c.P.DB.Event(id, current.RunID, r.Name, effective.Project.Provider, "preflight_visual_evidence_unavailable", reason)
			c.block(id, "Exact-head visual evidence is unavailable for pre-implementation guidance. Configure supervisor visual capture, then retry.", reason, model.Ready)
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
