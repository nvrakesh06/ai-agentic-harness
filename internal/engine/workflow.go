package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/github"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type checkFailure struct {
	name, command, output string
	err                   error
	check                 config.Check
	report                *nativeFailureReport
}

func (e *checkFailure) Error() string {
	return fmt.Sprintf("%s failed: %v\n%s", e.name, e.err, e.output)
}
func (e *checkFailure) Unwrap() error { return e.err }

const (
	maxVerificationEvidenceScan   = 64 << 10
	maxVerificationPassCountItems = 8
	maxFailureDiagnosticBytes     = 8 << 10
	failureDiagnosticHeadBytes    = 2 << 10
	failureDiagnosticTailBytes    = 5 << 10
)

var verificationPassCount = regexp.MustCompile(`(?i)\b(test files|tests|test suites|suites|specs?)\s*:?\s*(\d{1,9})\s+(?:passed|passing)\b`)
var verificationBarePassCount = regexp.MustCompile(`(?i)\b(\d{1,9})\s+passed\b`)

// verificationPassCounts extracts only fixed labels and decimal counts from
// successful command output. Review evidence must be useful without copying
// arbitrary stdout, which can contain source paths, fixture data, or secrets.
func verificationPassCounts(output string) string {
	output = boundedVerificationOutput(output)
	counts := make([]string, 0, 4)
	seen := map[string]bool{}
	add := func(item string) bool {
		if seen[item] || len(counts) == maxVerificationPassCountItems {
			return len(counts) < maxVerificationPassCountItems
		}
		seen[item] = true
		counts = append(counts, item)
		return len(counts) < maxVerificationPassCountItems
	}
	named := verificationPassCount.FindAllStringSubmatchIndex(output, -1)
	for _, match := range named {
		label := strings.ToLower(strings.ReplaceAll(output[match[2]:match[3]], " ", "_"))
		item := label + "=" + output[match[4]:match[5]] + "_passed"
		if !add(item) {
			break
		}
	}
	// Playwright commonly reports only "N passed". Preserve it alongside
	// named test-suite counts, while still never copying adjacent free text.
	if len(counts) < maxVerificationPassCountItems {
		for _, match := range verificationBarePassCount.FindAllStringSubmatchIndex(output, -1) {
			overlapsNamed := false
			for _, namedMatch := range named {
				if match[0] < namedMatch[1] && namedMatch[0] < match[1] {
					overlapsNamed = true
					break
				}
			}
			if overlapsNamed {
				continue
			}
			if !add("passed=" + output[match[2]:match[3]]) {
				break
			}
		}
	}
	if len(counts) == 0 {
		return "none"
	}
	return strings.Join(counts, ",")
}

func boundedVerificationOutput(output string) string {
	if len(output) <= maxVerificationEvidenceScan {
		return output
	}
	return output[len(output)-maxVerificationEvidenceScan:]
}

// boundedFailureDiagnostic preserves the beginning and, especially, the end of
// a failed check's output. Test runners commonly print many successful results
// before writing the actionable failure at the end. The caller redacts before
// invoking this helper, so both retained slices are safe to persist.
func boundedFailureDiagnostic(output string) string {
	if len(output) <= maxFailureDiagnosticBytes {
		return output
	}
	omitted := len(output) - failureDiagnosticHeadBytes - failureDiagnosticTailBytes
	return output[:failureDiagnosticHeadBytes] + fmt.Sprintf("\n[... %d bytes omitted; showing first and last diagnostic output ...]\n", omitted) + output[len(output)-failureDiagnosticTailBytes:]
}

func passedCheckEvidence(check config.Check, output string) string {
	state := "captured"
	lines := 0
	if output == "" {
		state = "empty"
	} else {
		lines = strings.Count(output, "\n")
		if !strings.HasSuffix(output, "\n") {
			lines++
		}
	}
	commandID := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(check.Command, "\x00"))))[:12]
	return fmt.Sprintf("stage=native check=%q command=%q command_id=%s exit=0 pass_counts=%q stdout=%s stdout_bytes=%d stdout_lines=%d", check.Name, filepath.Base(check.Command[0]), commandID, verificationPassCounts(output), state, len([]byte(output)), lines)
}

type reviewOutcome struct {
	result provider.Result
	err    error
}

type reviewAssessment struct {
	findings []model.Finding
	blocking int
	evidence []int
	human    int
	failure  error
}

func supervisorEvidenceText(text string) bool {
	text = strings.ToLower(text)
	for _, decision := range []string{"product decision", "choose whether", "accept risk", "authorize an exception", "approve an exception", "production access", "destructive migration", "provide credentials", "threat model", "security boundary", "trusted workspace", "race condition"} {
		if strings.Contains(text, decision) {
			return false
		}
	}
	evidence := false
	for _, marker := range []string{"verification evidence", "native verification", "native check", "native logs", "test output", "validation output", "check output", "runtime evidence", "review evidence", "peer review", "peer approval", "independent review", "exact-head", "current-head", "cannot run", "could not run", "tool unavailable", "screenshot", "visual evidence", "browser capture", "playwright"} {
		if strings.Contains(text, marker) {
			evidence = true
			break
		}
	}
	toolMissing := strings.Contains(text, "node") || strings.Contains(text, "npm") || strings.Contains(text, "bun")
	environmentMissing := strings.Contains(text, "missing") || strings.Contains(text, "lack") || strings.Contains(text, "unavailable") || strings.Contains(text, "cannot") || strings.Contains(text, "could not")
	evidence = evidence || (toolMissing && environmentMissing)
	if !evidence {
		return false
	}
	for _, request := range []string{"provide", "rerun", "run the", "missing", "lack", "unavailable", "cannot", "can't", "could not", "need"} {
		if strings.Contains(text, request) {
			return true
		}
	}
	return false
}
func visualEvidenceRequest(result provider.Result) bool {
	if !supervisorEvidenceRequest(result) {
		return false
	}
	text := strings.ToLower(strings.Join([]string{result.Question, result.Summary, strings.Join(result.Risks, " ")}, " "))
	for _, marker := range []string{"screenshot", "visual evidence", "browser capture", "playwright", "rendered frame"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
func sourceEvidenceRequest(result provider.Result) bool {
	text := strings.ToLower(strings.Join([]string{result.Question, result.Summary, strings.Join(result.Risks, " ")}, " "))
	for _, marker := range []string{"native check", "test output", "validation output", "check output", "npm", "node", "bun", "peer approval"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func supervisorEvidenceRequest(result provider.Result) bool {
	if result.Status != "blocked" && result.Status != "in_progress" {
		return false
	}
	return supervisorEvidenceText(strings.Join([]string{result.Question, result.Summary, strings.Join(result.Risks, " ")}, " "))
}

func supervisorEvidenceOnlyFinding(result provider.Result, finding model.Finding) bool {
	if result.Status != "completed" || strings.ToLower(finding.Category) != "verification" {
		return false
	}
	switch strings.ToLower(finding.Severity) {
	case "medium", "low", "nit":
		return supervisorEvidenceText(strings.Join([]string{finding.Reason, finding.Resolution}, " "))
	default:
		return false
	}
}

func assessReviews(required []roles.Role, outcomes []reviewOutcome, blockers ...func(model.Finding) bool) reviewAssessment {
	blocksOrigin := func(finding model.Finding) bool { return findingBlocks(required, finding) }
	if len(blockers) != 0 && blockers[0] != nil {
		blocksOrigin = blockers[0]
	}
	assessment := reviewAssessment{blocking: -1, human: -1}
	for i, role := range required {
		result := outcomes[i].result
		retained := make([]model.Finding, 0, len(result.Findings))
		for _, finding := range result.Findings {
			if supervisorEvidenceOnlyFinding(result, finding) {
				continue
			}
			finding.Role = role.Name
			finding = normalizedReviewFinding(finding)
			assessment.findings = append(assessment.findings, finding)
			retained = append(retained, finding)
		}
		if assessment.blocking == -1 {
			for _, finding := range retained {
				if blocksOrigin(finding) {
					assessment.blocking = i
					break
				}
			}
		}
		if outcomes[i].err != nil && assessment.failure == nil {
			assessment.failure = outcomes[i].err
		}
	}
	for i, role := range required {
		if outcomes[i].err != nil {
			continue
		}
		result := outcomes[i].result
		switch result.Status {
		case "completed":
		case "blocked", "in_progress":
			if supervisorEvidenceRequest(result) {
				assessment.evidence = append(assessment.evidence, i)
			} else if result.Status == "blocked" && assessment.human == -1 {
				assessment.human = i
			} else if assessment.failure == nil {
				assessment.failure = fmt.Errorf("%s did not complete: %s", role.Name, result.Summary)
			}
		default:
			if assessment.failure == nil {
				assessment.failure = fmt.Errorf("%s did not complete: %s", role.Name, result.Summary)
			}
		}
	}
	return assessment
}

func reviewEvidencePayload(evidence *model.Evidence, attempt int) string {
	copy := *evidence
	copy.Reviews = map[string]string{}
	payload := struct {
		*model.Evidence
		Attempt int    `json:"review_attempt"`
		Policy  string `json:"review_policy"`
	}{Evidence: &copy, Attempt: attempt, Policy: "Peer reviews are concurrent and independent; the empty reviews map is intentional. Supervisor check evidence is exact-head metadata. It includes the AIH-observed native check stage, executable, opaque command ID, exit status, and parsed pass counts, but omits successful stdout content, command arguments, and environment values."}
	serialized, _ := json.Marshal(payload)
	return string(serialized)
}

func appendUniqueFindings(existing []model.Finding, additions []model.Finding) []model.Finding {
	for _, addition := range additions {
		duplicate := false
		for _, current := range existing {
			if current.Severity == addition.Severity && current.Category == addition.Category && current.Location == addition.Location && current.Reason == addition.Reason && current.Role == addition.Role {
				duplicate = true
				break
			}
		}
		if !duplicate {
			existing = append(existing, addition)
		}
	}
	return existing
}

func applicable(check config.Check) bool {
	if len(check.Platforms) == 0 {
		return true
	}
	for _, platform := range check.Platforms {
		if platform == runtime.GOOS {
			return true
		}
	}
	return false
}

func nativeEnvironment(e config.Effective) string {
	commands := []string{}
	for _, check := range e.Project.Checks {
		if applicable(check) {
			commands = append(commands, check.Name+"="+filepath.Base(check.Command[0]))
		}
	}
	sort.Strings(commands)
	hash := e.Hash
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return strings.Join([]string{runtime.GOOS, "native", hash, strings.Join(commands, ",")}, "/")
}

func workerEnvironment(e config.Effective, role roles.Role) string {
	resolved := e.Project.ResolveModel(role.Name, role.Capability)
	return strings.Join([]string{runtime.GOOS, e.Project.Provider, role.Name, resolved.EffectiveModel, "workspace-write"}, "/")
}

func verificationFingerprint(environment, reason string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(reason), " "))
	hash := sha256.Sum256([]byte(environment + "\n" + normalized))
	return fmt.Sprintf("%x", hash)
}

func (c *Controller) effective(ctx context.Context) (config.Effective, error) {
	if e := c.fetch(ctx); e != nil {
		return config.Effective{}, e
	}
	e, eErr := Canonical(ctx, c.P.Git)
	if eErr == nil && e.Project.ID != c.P.Config.Project.ID {
		eErr = errors.New("canonical project identity changed; refusing to reuse old orchestration state")
	}
	return e, eErr
}
func (c *Controller) role(ctx context.Context, e config.Effective, r roles.Role, t *model.Task, dir, objective, diff, evidence string) (provider.Result, error) {
	return c.roleWithCompletionAtRef(ctx, e, r, t, dir, objective, diff, evidence, nil, "")
}
func (c *Controller) roleWithCompletion(ctx context.Context, e config.Effective, r roles.Role, t *model.Task, dir, objective, diff, evidence string, complete func(*model.Snapshot, provider.Result, error) error) (provider.Result, error) {
	return c.roleWithCompletionAtRef(ctx, e, r, t, dir, objective, diff, evidence, complete, "")
}

// roleAtRef runs a read-only role against an explicit immutable revision. It is
// used when the durable task head intentionally differs from the source under
// review, such as post-verify recovery after a repair on main.
func (c *Controller) roleAtRef(ctx context.Context, e config.Effective, r roles.Role, t *model.Task, dir, objective, diff, evidence, readRef string) (provider.Result, error) {
	return c.roleWithCompletionAtRef(ctx, e, r, t, dir, objective, diff, evidence, nil, readRef)
}

func (c *Controller) roleWithCompletionAtRef(ctx context.Context, e config.Effective, r roles.Role, t *model.Task, dir, objective, diff, evidence string, complete func(*model.Snapshot, provider.Result, error) error, explicitReadRef string) (provider.Result, error) {
	if r.Name != "implementer" {
		select {
		case c.readers <- struct{}{}:
			defer func() { <-c.readers }()
		case <-ctx.Done():
			return provider.Result{}, ctx.Err()
		}
		if t != nil && t.Preflight != nil && t.Preflight.Phase == "waiting" {
			if err := c.mutate(func(s *model.Snapshot) error {
				if p := s.Tasks[t.ID].Preflight; p != nil {
					p.Phase = "running"
				}
				return nil
			}); err != nil {
				return provider.Result{}, err
			}
		}
	}
	id := model.ID()
	taskID := ""
	if t != nil {
		taskID = t.ID
	}
	resolved := e.Project.ResolveModel(r.Name, r.Capability)
	started := time.Now().UTC()
	if err := c.mutate(func(s *model.Snapshot) error {
		if t != nil {
			s.Tasks[t.ID].RunID = id
		}
		s.Runs = append(s.Runs, model.Run{ID: id, Task: taskID, Role: r.Name, Provider: e.Project.Provider, Capability: resolved.Capability, EffectiveModel: resolved.EffectiveModel, Version: model.Version, RulesHash: roles.Hash(), Started: started, Epoch: s.Controller.Epoch, Outcome: "running"})
		return nil
	}); err != nil {
		return provider.Result{}, err
	}
	_ = c.P.DB.Event(taskID, id, r.Name, e.Project.Provider, "worker_start", fmt.Sprintf("capability=%s effective_model=%s", resolved.Capability, resolved.EffectiveModel))
	p := c.P.Provider
	if p.Name() != e.Project.Provider {
		p = provider.New(e.Project.Provider)
	}
	runtimeDir := filepath.Join(c.P.Dir, "sessions", id)
	runDir := dir
	if r.Name != "implementer" && t != nil {
		// Preflight runs before an implementer has made a task head. Use the
		// immutable planning base in that case so an advisory command can never
		// write into the writer worktree merely because it is early in the task.
		readRef, readErr := readOnlyCheckoutRef(t, e, explicitReadRef)
		if readErr != nil {
			return provider.Result{}, readErr
		}
		// Git worktree metadata is shared by all reader roles. Serialize only
		// creation/removal, never the provider invocation, so independent reviews
		// retain bounded parallelism without racing worktree add/remove locks.
		c.gitMu.Lock()
		runDir, readErr = c.P.ValidDisposableReviewWorktreePath(id)
		if readErr != nil {
			c.gitMu.Unlock()
			return provider.Result{}, readErr
		}
		if err := c.P.Git.Detached(ctx, runDir, readRef); err != nil {
			c.gitMu.Unlock()
			return provider.Result{}, fmt.Errorf("create disposable read-only review worktree: %w", err)
		}
		c.gitMu.Unlock()
		// Non-writer roles run in an AIH-created detached checkout. Force removal
		// is safe here and prevents a diagnostic's generated files from leaving a
		// stranded worktree after either success or provider failure.
		defer func() {
			c.gitMu.Lock()
			if cleanupErr := c.P.RemoveDisposableReviewWorktree(context.Background(), id); cleanupErr != nil {
				_ = c.P.DB.Event(taskID, id, r.Name, e.Project.Provider, "read_only_worktree_cleanup_failed", safety.Redact(cleanupErr.Error()))
			}
			c.gitMu.Unlock()
		}()
	}
	prompt := roles.Compile(e, r, runtime.GOOS, t, objective, diff, evidence)
	var operatorGuidance []model.Guidance
	if r.Name == "implementer" && t != nil {
		operatorGuidance = model.EligibleGuidance(t, e.BaseSHA, e.Hash, roles.Hash())
	}
	var err error
	scratch := ""
	if t != nil {
		scratch, err = c.P.PrepareTaskScratch(t)
		if err != nil {
			return provider.Result{}, err
		}
		prompt += "\nWORKER SCRATCH\nUse the supplied external scratch directory for temporary tooling, package-manager caches, downloads, and generated diagnostics. Do not create worker caches or downloaded tools inside the source worktree. Scratch is local-only and is never checkpointed: " + scratch + "\n"
	}
	request := provider.Request{Directory: runDir, Runtime: runtimeDir, Scratch: scratch, Prompt: prompt, Role: r.Name, Model: resolved.RequestModel, Write: r.Name == "implementer", Timeout: time.Duration(e.Project.WorkerSeconds) * time.Second}
	readonlyStatus := ""
	if !request.Write && dir != "" {
		readonlyStatus, err = (gitx.Git{Dir: dir}).Run(ctx, "", "status", "--porcelain")
		if err != nil {
			return provider.Result{}, err
		}
	}
	var result provider.Result
	if r.Name == "implementer" {
		checkpointPrompt := prompt + "\n\nSOFT DEADLINE CHECKPOINT\nStop expanding scope. Inspect and preserve the existing worktree edits, run only the smallest relevant verification that fits, and immediately return the required structured result. Use completed only if the assigned acceptance criteria are satisfied; otherwise use in_progress and report the exact handoff, tests, and remaining risks. Do not undo safe existing work or begin unrelated improvements."
		result, err = runWithCheckpoint(ctx, p, request, checkpointPrompt, request.Timeout, deadlineHooks{
			active: func(runErr error) bool { return c.workerActive(dir, runErr) },
			event: func(kind, message string) {
				_ = c.P.DB.Event(taskID, id, r.Name, e.Project.Provider, kind, message)
			},
			recover: func(runErr error) provider.Result {
				return c.syntheticHandoff(dir, runtimeDir, runErr)
			},
		})
	} else {
		result, err = p.Run(ctx, request)
		if err == nil && dir != "" {
			after, statusErr := (gitx.Git{Dir: dir}).Run(ctx, "", "status", "--porcelain")
			if statusErr != nil {
				err = statusErr
			} else if after != readonlyStatus {
				err = fmt.Errorf("read-only %s run modified task worktree; preserve and repair these paths before review completion: %s", r.Name, short(after, 1000))
			}
		}
	}
	outcome := result.Status
	if err != nil {
		outcome = "failed"
	}
	if ctx.Err() == nil {
		saveErr := c.mutate(func(s *model.Snapshot) error {
			// A failed provider call has no durable acceptance acknowledgement, so
			// retain the record for at-least-once recovery. A structured result is
			// the bounded invocation acknowledgement used for one-time delivery.
			if err == nil && t != nil {
				model.MarkOperatorGuidanceDelivered(s.Tasks[t.ID], operatorGuidance)
			}
			for i := range s.Runs {
				if s.Runs[i].ID == id {
					s.Runs[i].DurationMS = time.Since(started).Milliseconds()
					s.Runs[i].Outcome = outcome
				}
			}
			if complete != nil {
				return complete(s, result, err)
			}
			return nil
		})
		if saveErr != nil {
			return result, saveErr
		}
	}
	_ = c.P.DB.Event(taskID, id, r.Name, e.Project.Provider, "worker_exit", fmt.Sprintf("outcome=%s capability=%s effective_model=%s", outcome, resolved.Capability, resolved.EffectiveModel))
	return result, err
}

func readOnlyCheckoutRef(t *model.Task, e config.Effective, explicit string) (string, error) {
	if t == nil {
		return "", errors.New("read-only role has no task for disposable checkout")
	}
	if explicit != "" {
		return explicit, nil
	}
	if t.HeadSHA != "" {
		return t.HeadSHA, nil
	}
	if t.BaseSHA != "" {
		return t.BaseSHA, nil
	}
	if e.BaseSHA != "" {
		return e.BaseSHA, nil
	}
	return "", errors.New("read-only role has no immutable source revision for disposable checkout")
}

func needsProvision(s *model.Snapshot, id string) bool {
	for _, t := range s.Tasks {
		if t.ObjectiveID == id && t.State == model.Planned {
			return true
		}
	}
	return false
}
func (c *Controller) plan(id string) {
	s := c.Snapshot()
	o := s.Objectives[id]
	if o == nil {
		return
	}
	if o.Planned {
		if e := c.provision(id); e != nil {
			c.planFailure(id, e)
		}
		return
	}
	if o.Issue == 0 {
		issue, e := c.P.Hub.EnsureIssue(c.ctx, id, "AIH: "+short(o.Text, 100), o.Text)
		if e != nil {
			c.planFailure(id, e)
			return
		}
		if e = c.mutate(func(s *model.Snapshot) error { s.Objectives[id].Issue = issue; return nil }); e != nil {
			return
		}
		o = c.Snapshot().Objectives[id]
	}
	effective, e := c.effective(c.ctx)
	if e != nil {
		c.planFailure(id, e)
		return
	}
	runID := model.ID()
	dir, e := c.P.ValidDisposableAnalysisWorktreePath(runID)
	if e != nil {
		c.planFailure(id, e)
		return
	}
	if e = c.P.Git.Detached(c.ctx, dir, "refs/remotes/origin/main"); e != nil {
		c.planFailure(id, e)
		return
	}
	defer func() {
		if cleanupErr := c.P.RemoveDisposableAnalysisWorktree(context.Background(), runID); cleanupErr != nil {
			_ = c.P.DB.Event("", "", "orchestrator", effective.Project.Provider, "analysis_worktree_cleanup_failed", safety.Redact(cleanupErr.Error()))
		}
	}()
	r, e := c.role(c.ctx, effective, roles.Builtins()["orchestrator"], nil, dir, o.Text, "", "")
	if e != nil {
		c.planFailure(id, e)
		return
	}
	if r.Status != "completed" {
		if r.Status == "blocked" && strings.TrimSpace(r.Question) != "" {
			c.planHumanBlock(id, r.Question)
			return
		}
		c.planFailure(id, fmt.Errorf("%s %s", r.Summary, r.Question))
		return
	}
	if e = model.ValidatePlan(r.Plan); e != nil {
		c.planFailure(id, e)
		return
	}
	all, e := roles.Load(effective.Files)
	if e != nil {
		c.planFailure(id, e)
		return
	}
	for _, p := range r.Plan {
		for _, n := range p.Roles {
			if _, ok := all[n]; !ok {
				c.planFailure(id, fmt.Errorf("unknown planned role %s", n))
				return
			}
		}
	}
	plannedAreas := make(map[string][]gitx.Area, len(r.Plan))
	for _, p := range r.Plan {
		classified, classifyErr := c.P.Git.ClassifyAreasAtRef(c.ctx, effective.BaseSHA, p.Areas)
		if classifyErr != nil {
			c.planFailure(id, fmt.Errorf("classify planned areas for %s: %w", p.Key, classifyErr))
			return
		}
		plannedAreas[p.Key] = classified
	}
	e = c.mutate(func(s *model.Snapshot) error {
		for _, p := range r.Plan {
			taskID := id + "-" + p.Key
			deps := []string{}
			for _, d := range p.Dependencies {
				deps = append(deps, id+"-"+d)
			}
			classified := plannedAreas[p.Key]
			s.Tasks[taskID] = &model.Task{ID: taskID, ObjectiveID: id, Title: p.Title, Objective: p.Objective, Acceptance: p.Acceptance, Dependencies: deps, Areas: p.Areas, AssignedAreas: canonicalAssignedAreas(classified), AssignedAreaKinds: normalizeAreaKinds(classified), Domains: p.Domains, Risk: p.Risk, UI: p.UI, Security: p.Security, Roles: p.Roles, State: model.Planned, FixCycles: map[string]int{}}
		}
		s.Objectives[id].Planned = true
		return nil
	})
	if e != nil {
		return
	}
	if e = c.provision(id); e != nil {
		c.planFailure(id, e)
	}
}
func (c *Controller) planHumanBlock(id, question string) {
	if c.ctx.Err() != nil {
		return
	}
	_ = c.mutate(func(s *model.Snapshot) error {
		if o := s.Objectives[id]; o != nil {
			markObjectivePlanningBlocked(o, c.portable(question))
		}
		return nil
	})
}

func markObjectivePlanningBlocked(o *model.Objective, question string) {
	o.Blocker = "Planning needs input: " + strings.TrimSpace(question)
}

func validateObjectiveAnswer(answer string) error {
	if strings.TrimSpace(answer) == "" {
		return errors.New("answer cannot be empty")
	}
	return nil
}

func applyObjectiveAnswer(o *model.Objective, answer string) error {
	if err := validateObjectiveAnswer(answer); err != nil {
		return err
	}
	o.Text += "\nHuman answer: " + answer
	o.Blocker = ""
	o.Attempts = 0
	return nil
}

func (c *Controller) planFailure(id string, e error) {
	if c.ctx.Err() != nil {
		return
	}
	_ = c.mutate(func(s *model.Snapshot) error {
		o := s.Objectives[id]
		o.Attempts++
		if o.Attempts > c.P.Config.Policy.ImplementationRetries {
			o.Blocker = "Planning/provisioning failed: " + c.portable(e.Error()) + ". Correct the cause, then answer this objective to retry."
		}
		return nil
	})
}
func (c *Controller) provision(id string) error {
	for _, t := range model.Ordered(c.Snapshot()) {
		if t.ObjectiveID != id || t.State != model.Planned {
			continue
		}
		if t.Issue == 0 {
			issue, e := c.P.Hub.EnsureIssue(c.ctx, t.ID, t.Title, c.issueBody(t))
			if e != nil {
				return e
			}
			if e = c.mutate(func(s *model.Snapshot) error {
				task := s.Tasks[t.ID]
				task.Issue = issue
				task.Branch = fmt.Sprintf("aih/%d-%s", issue, task.ID)
				return nil
			}); e != nil {
				return e
			}
		}
		if e := c.mutate(func(s *model.Snapshot) error { return model.Transition(s.Tasks[t.ID], model.Ready) }); e != nil {
			return e
		}
	}
	s := c.Snapshot()
	o := s.Objectives[id]
	var body strings.Builder
	fmt.Fprintf(&body, "%s\n\n%s\n\nTasks:\n", github.Marker(id), o.Text)
	for _, t := range model.Ordered(s) {
		if t.ObjectiveID == id {
			c.mirror(t.ID)
			fmt.Fprintf(&body, "- [ ] #%d %s\n", t.Issue, t.Title)
		}
	}
	if e := c.P.Hub.UpdateIssue(c.ctx, o.Issue, body.String(), false); e != nil {
		return e
	}
	return nil
}
func short(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
func (c *Controller) issueBody(t *model.Task) string {
	var b strings.Builder
	b.WriteString(github.Marker(t.ID) + "\n\n" + t.Objective + "\n\nAcceptance criteria:\n")
	for _, a := range t.Acceptance {
		b.WriteString("- " + a + "\n")
	}
	s := c.Snapshot()
	if o := s.Objectives[t.ObjectiveID]; o != nil {
		fmt.Fprintf(&b, "\nParent: #%d\n", o.Issue)
	}
	for _, d := range t.Dependencies {
		if dep := s.Tasks[d]; dep != nil {
			fmt.Fprintf(&b, "Depends on: #%d (%s)\n", dep.Issue, d)
		}
	}
	fmt.Fprintf(&b, "\nAIH state: %s\nRisk: %s\n", t.State, t.Risk)
	if t.Blocker != nil {
		b.WriteString("\nNeeds human input: " + t.Blocker.Question + "\nReason: " + t.Blocker.Reason + "\n")
	}
	if t.Verification != nil {
		fmt.Fprintf(&b, "\nVerification retry guard: environment `%s`, head `%s`, attempt %d, native-only `%t`.\n", t.Verification.Environment, t.Verification.HeadSHA, t.Verification.Attempts, t.Verification.NativeOnly)
	}
	for _, a := range t.Decisions {
		b.WriteString("\nDecision: " + a + "\n")
	}
	for _, finding := range t.Findings {
		if finding.Location == "" {
			continue
		}
		fmt.Fprintf(&b, "\nPending finding: [%s] %s — %s\n", finding.Severity, finding.Location, safety.Redact(finding.Reason))
	}
	return b.String()
}
func (c *Controller) mirror(id string) {
	c.mirrorWith(c.ctx, id)
}
func (c *Controller) mirrorWith(ctx context.Context, id string) {
	t := c.Snapshot().Tasks[id]
	if t == nil || t.Issue == 0 {
		return
	}
	if e := c.P.Hub.UpdateIssue(ctx, t.Issue, c.issueBody(t), t.State == model.Done); e != nil {
		_ = c.P.DB.Event(id, "", "", "", "github_sync_pending", safety.Redact(e.Error()))
	}
	if t.PR != 0 {
		if e := c.P.Hub.UpdatePR(ctx, t.PR, c.prBody(t)); e != nil {
			_ = c.P.DB.Event(id, "", "", "", "github_sync_pending", safety.Redact(e.Error()))
		}
	}
}
func (c *Controller) block(id, question, reason string, resume model.State) {
	c.blockWithOrigin(id, question, reason, resume, "")
}

func (c *Controller) blockWithOrigin(id, question, reason string, resume model.State, origin string) {
	if c.ctx.Err() != nil {
		return
	}
	if c.mutate(func(s *model.Snapshot) error {
		model.BlockWithOrigin(s.Tasks[id], c.portable(question), c.portable(reason), resume, origin)
		return nil
	}) == nil {
		c.mirror(id)
	}
}
func (c *Controller) portable(text string) string {
	return safety.Portable(text, c.P.Dir, c.P.Root, c.P.Home, os.TempDir())
}
func (c *Controller) checkpoint(ctx context.Context, id string) error {
	t := c.Snapshot().Tasks[id]
	return c.checkpointAtBase(ctx, id, t.BaseSHA)
}

// checkpointAtBase validates only the task delta after a rebase. A task's
// former base may contain unrelated changes that have since reached main;
// comparing that old base to a rebased head would incorrectly attribute those
// changes to the task. The new base and rewritten branch head publish together.
func (c *Controller) checkpointAtBase(ctx context.Context, id, immutableBase string) error {
	t := c.Snapshot().Tasks[id]
	areas, ok := immutableScope(t)
	if !ok || immutableBase == "" {
		return &gitx.ScopeError{}
	}
	if e := c.P.Git.ValidateFullCheckpointScope(ctx, c.P.TaskPath(t), immutableBase, areas); e != nil {
		return e
	}
	sha, e := c.P.Git.Checkpoint(ctx, c.P.TaskPath(t), id)
	if e != nil {
		return e
	}
	if sha == t.HeadSHA && immutableBase == t.BaseSHA {
		return nil
	}
	return c.save(ctx, func(s *model.Snapshot) error {
		task := s.Tasks[id]
		task.HeadSHA = sha
		task.BaseSHA = immutableBase
		if task.VisualRequired != nil {
			task.VisualRequired.Head = sha
		}
		return nil
	}, gitx.Update{Branch: t.Branch, Old: t.HeadSHA, New: sha})
}

func (c *Controller) recoveredCheckpoint(ctx context.Context, id string, result provider.Result) error {
	if !result.RecoveredDeadlineHandoff || result.Status != "in_progress" {
		return errors.New("invalid recovered deadline handoff")
	}
	t := c.Snapshot().Tasks[id]
	effective, effectiveErr := c.effective(ctx)
	preflightRoles, preflightErr := requiredPreflightRoles(effective, t)
	areas, scoped := immutableScope(t)
	if !scoped || t.BaseSHA == "" {
		return &gitx.ScopeError{}
	}
	if err := c.P.Git.ValidateFullCheckpointScope(ctx, c.P.TaskPath(t), t.BaseSHA, areas); err != nil {
		return err
	}
	sha, err := c.P.Git.Checkpoint(ctx, c.P.TaskPath(t), id)
	if err != nil {
		return err
	}
	updates := []gitx.Update{}
	if sha != t.HeadSHA {
		updates = append(updates, gitx.Update{Branch: t.Branch, Old: t.HeadSHA, New: sha})
	}
	resumed := false
	err = c.save(ctx, func(s *model.Snapshot) error {
		task := s.Tasks[id]
		task.HeadSHA = sha
		if task.VisualRequired != nil {
			task.VisualRequired.Head = sha
		}
		task.Summary = c.portable(result.Summary)
		task.ReportedTests = portableStrings(c, result.Tests)
		task.Risks = portableStrings(c, result.Risks)
		task.Rotations++
		task.Decisions = append(task.Decisions, "Checkpoint: "+task.Summary)
		if task.Rotations >= 24 {
			model.Block(task, "Task reached 24 checkpoint slices. Refine or authorize further work.", task.Summary, model.Ready)
			return nil
		}
		if err := model.Transition(task, model.Ready); err != nil {
			return err
		}
		if effectiveErr == nil && preflightErr == nil && continuationPreflightReusable(result) {
			resumed = reusePreflightForContinuation(task.Preflight, task, effective, preflightRoles)
		}
		if resumed {
			task.Decisions = append(task.Decisions, "Recovered checkpoint continuation: reused completed pre-implementation guidance at "+task.HeadSHA+".")
		}
		return nil
	}, updates...)
	if err == nil && resumed {
		current := c.Snapshot().Tasks[id]
		_ = c.P.DB.Event(id, current.RunID, "implementer", effective.Project.Provider, "checkpoint_continuation_admitted", "recovered checkpoint="+current.HeadSHA+"; prior guidance reused without another preflight")
	}
	return err
}

func portableStrings(c *Controller, values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = c.portable(value)
	}
	return out
}
func (c *Controller) ensureWorktree(id string) error {
	c.gitMu.Lock()
	defer c.gitMu.Unlock()
	t := c.Snapshot().Tasks[id]
	from := t.HeadSHA
	if from == "" {
		from = t.BaseSHA
		if from == "" {
			from = "refs/remotes/origin/main"
		}
	}
	if e := c.P.Git.Worktree(c.ctx, c.P.TaskPath(t), t.Branch, from); e != nil {
		return e
	}
	if t.HeadSHA == "" {
		if t.State != model.Planned && t.State != model.Ready {
			return &gitx.ScopeError{}
		}
		head, e := (gitx.Git{Dir: c.P.TaskPath(t)}).SHA(c.ctx, "HEAD")
		if e != nil {
			return e
		}
		if t.BaseSHA != "" && head != t.BaseSHA {
			return fmt.Errorf("task %s local branch does not match its durable base", id)
		}
		var classified []gitx.Area
		if t.BaseSHA == "" {
			assigned, assignedOK := model.ImmutableAreas(t)
			if !assignedOK {
				return &gitx.ScopeError{}
			}
			classified, e = c.P.Git.ClassifyAreasAtRef(c.ctx, head, assigned)
			if e != nil {
				return e
			}
		}
		err := c.save(c.ctx, func(s *model.Snapshot) error {
			task := s.Tasks[id]
			if task.HeadSHA == "" {
				task.HeadSHA = head
				if task.BaseSHA == "" {
					task.BaseSHA = head
					task.AssignedAreas = canonicalAssignedAreas(classified)
					task.AssignedAreaKinds = normalizeAreaKinds(classified)
				}
			}
			return nil
		}, gitx.Update{Branch: t.Branch, New: head})
		if err != nil {
			c.fail(err)
		}
		return err
	}
	return nil
}
func (c *Controller) work(id string, write bool) {
	if c.ctx.Err() != nil {
		return
	}
	if e := c.ensureWorktree(id); e != nil {
		c.block(id, "Repair the task worktree and retry.", e.Error(), model.Ready)
		return
	}
	if write {
		if !c.implement(id) {
			return
		}
	}
	if c.ctx.Err() != nil {
		return
	}
	if e := c.verifyReview(id); e != nil {
		if c.ctx.Err() == nil {
			c.handleVerificationError(id, e)
		}
		return
	}
}

func (c *Controller) handleVerificationError(id string, err error) {
	if scopeError(err) {
		c.block(id, "Correct or replan the immutable task-area assignment; local work is preserved.", err.Error(), model.Ready)
		return
	}
	var unavailable *visualCaptureUnavailableError
	if errors.As(err, &unavailable) {
		t := c.Snapshot().Tasks[id]
		reason := c.portable(unavailable.Error())
		if c.mutate(func(s *model.Snapshot) error {
			model.Block(s.Tasks[id], "Visual capture is unavailable on this supervisor. Repair the configured capture tool or choose a verification path, then retry.", reason, model.SyncRequired)
			return nil
		}) == nil {
			_ = c.P.DB.Event(id, t.RunID, "verification", "native", "visual_capture_unavailable", reason)
			c.mirror(id)
		}
		return
	}
	var checkErr *checkFailure
	if errors.As(err, &checkErr) {
		c.verificationFailure(id, checkErr)
		return
	}
	c.retry(id, "verification", err.Error())
}
func (c *Controller) implement(id string) bool {
	effective, e := c.effective(c.ctx)
	if e != nil {
		c.block(id, "Restore Git access and retry.", e.Error(), model.Ready)
		return false
	}
	all, e := roles.Load(effective.Files)
	if e != nil {
		c.block(id, "Correct role configuration.", e.Error(), model.Ready)
		return false
	}
	t := c.Snapshot().Tasks[id]
	if _, ok := immutableScope(t); !ok {
		c.block(id, "This legacy task has no provable immutable area assignment. Replan it before work starts.", "AIH refuses to guess a started task's historical write scope.", model.Ready)
		return false
	}
	dir := c.P.TaskPath(t)
	if t.SyncBase != "" {
		if e = c.P.Git.PrepareMerge(c.ctx, dir, t.SyncBase); e != nil {
			c.block(id, "Repair task synchronization and retry.", e.Error(), model.Fix)
			return false
		}
	}
	t = c.Snapshot().Tasks[id]
	guidanceAtStart := len(model.TaskGuidance(t))
	r, e := c.role(c.ctx, effective, all["implementer"], t, dir, t.Objective, "", "")
	if c.ctx.Err() != nil {
		return false
	}
	if e == nil && r.RecoveredDeadlineHandoff {
		if ce := c.recoveredCheckpoint(c.ctx, id, r); ce != nil {
			c.block(id, "Resolve recovered checkpoint publication failure; local work is preserved.", ce.Error(), model.Ready)
		}
		return false
	}
	if ce := c.checkpoint(c.ctx, id); ce != nil {
		c.block(id, "Resolve checkpoint failure; local work is preserved.", ce.Error(), model.Ready)
		return false
	}
	if e != nil {
		c.retry(id, "implementation", e.Error())
		return false
	}
	if c.mutate(func(s *model.Snapshot) error {
		task := s.Tasks[id]
		task.Summary = c.portable(r.Summary)
		task.ReportedTests = r.Tests
		task.Risks = r.Risks
		return nil
	}) != nil {
		return false
	}
	switch r.Status {
	case "blocked":
		if strings.TrimSpace(r.Question) == "" {
			native := nativeEnvironment(effective)
			source := workerEnvironment(effective, all["implementer"])
			reason := c.portable(r.Summary + " " + strings.Join(r.Risks, " "))
			replay := false
			if c.mutate(func(s *model.Snapshot) error {
				task := s.Tasks[id]
				guard := &model.Verification{Environment: native, SourceEnvironment: source, HeadSHA: task.HeadSHA, Fingerprint: verificationFingerprint(source, reason), NativeOnly: true}
				var err error
				replay, err = completeNativeOnlyImplementation(task, guidanceAtStart, guard)
				return err
			}) != nil {
				return false
			}
			if replay {
				_ = c.P.DB.Event(id, c.Snapshot().Tasks[id].RunID, "implementer", effective.Project.Provider, "task_guidance_replay", "new guidance requires one fresh implementer invocation after safe source checkpoint")
				return false
			}
			_ = c.P.DB.Event(id, c.Snapshot().Tasks[id].RunID, "implementer", effective.Project.Provider, "verification_rerouted", source+" -> "+native)
			return true
		}
		c.blockWithOrigin(id, r.Question, r.Summary, model.Ready, model.BlockerOriginImplementerDecision)
		return false
	case "in_progress":
		preflightRoles, preflightErr := requiredPreflightRoles(effective, c.Snapshot().Tasks[id])
		resumed := false
		_ = c.mutate(func(s *model.Snapshot) error {
			t := s.Tasks[id]
			t.Rotations++
			if t.Rotations >= 24 {
				model.Block(t, "Task reached 24 checkpoint slices. Refine or authorize further work.", r.Summary, model.Ready)
				return nil
			}
			t.Decisions = append(t.Decisions, "Checkpoint: "+r.Summary)
			if err := model.Transition(t, model.Ready); err != nil {
				return err
			}
			if preflightErr == nil && continuationPreflightReusable(r) {
				resumed = reusePreflightForContinuation(t.Preflight, t, effective, preflightRoles)
			}
			if resumed {
				t.Decisions = append(t.Decisions, "Checkpoint continuation: reused completed pre-implementation guidance at "+t.HeadSHA+".")
			}
			return nil
		})
		if resumed {
			current := c.Snapshot().Tasks[id]
			_ = c.P.DB.Event(id, current.RunID, "implementer", effective.Project.Provider, "checkpoint_continuation_admitted", "checkpoint="+current.HeadSHA+"; prior guidance reused without another preflight")
		}
		return false
	case "failed":
		c.retry(id, "implementation", r.Summary)
		return false
	case "completed":
		replay := false
		if c.mutate(func(s *model.Snapshot) error {
			var err error
			replay, err = completeImplementation(s.Tasks[id], guidanceAtStart)
			return err
		}) != nil {
			return false
		}
		if replay {
			_ = c.P.DB.Event(id, c.Snapshot().Tasks[id].RunID, "implementer", effective.Project.Provider, "task_guidance_replay", "new guidance requires one fresh implementer invocation after safe source checkpoint")
			return false
		}
		return true
	default:
		c.retry(id, "implementation", "invalid result status")
		return false
	}
}

func completeImplementation(task *model.Task, guidanceAtStart int) (bool, error) {
	replay, err := replayLateGuidance(task, guidanceAtStart)
	if replay || err != nil {
		return replay, err
	}
	return false, model.Transition(task, model.Implemented)
}

func completeNativeOnlyImplementation(task *model.Task, guidanceAtStart int, guard *model.Verification) (bool, error) {
	replay, err := replayLateGuidance(task, guidanceAtStart)
	if replay || err != nil {
		return replay, err
	}
	task.Verification = guard
	task.Decisions = append(task.Decisions, "Implementation complete; supervisor-native verification requested because the worker environment lacked a required verification capability.")
	return false, model.Transition(task, model.Implemented)
}

// advancePreflightHead binds completed pre-implementation guidance to the
// durable writer checkpoint only after the supervisor has identified a
// verification-only human blocker. Ordinary check failures keep their old
// preflight identity and continue through bounded FIX reuse semantics.
func advancePreflightHead(task *model.Task) {
	if task != nil && task.Preflight != nil && task.Preflight.Phase == "writing" {
		task.Preflight.HeadSHA = task.HeadSHA
	}
}

func replayLateGuidance(task *model.Task, guidanceAtStart int) (bool, error) {
	if len(model.TaskGuidance(task)) > guidanceAtStart {
		model.CarryLateOperatorGuidance(task, guidanceAtStart)
		task.Decisions = append(task.Decisions, "Checkpoint: task guidance arrived while this implementer invocation was running; next bounded pass must reconcile it.")
		return true, model.Transition(task, model.Ready)
	}
	return false, nil
}

func (c *Controller) verificationFailure(id string, failure *checkFailure) {
	if c.ctx.Err() != nil {
		return
	}
	reason := c.portable(failure.Error())
	effective, err := c.effective(c.ctx)
	if err != nil {
		c.block(id, "Restore canonical policy access before retrying verification.", err.Error(), model.SyncRequired)
		return
	}
	task := c.Snapshot().Tasks[id]
	if task == nil {
		return
	}
	if routed, routeErr := c.reproduceAndRouteNativeFailure(id, failure, effective); routeErr != nil {
		c.block(id, "Canonical-base failure reproduction could not complete. Retry verification when the local check resource is available.", routeErr.Error(), model.SyncRequired)
		return
	} else if routed {
		_ = c.P.DB.Event(id, task.RunID, "verification", "native", "native_failure_routed", failure.check.Name+" id="+failure.report.ID+" path="+failure.report.Path)
		_ = c.updatePR(id, true)
		c.mirror(id)
		return
	}
	environment := nativeEnvironment(effective)
	guard := task.Verification
	// A NativeOnly handoff is a request for the first supervisor-owned check,
	// not a prior native failure. Only a guard with a recorded native attempt can
	// suppress another check at the same revision.
	repeated := guard != nil && guard.Attempts > 0 && guard.Environment == environment && guard.HeadSHA == task.HeadSHA
	nativeOnly := guard != nil && guard.NativeOnly
	capabilityMissing := errors.Is(failure, exec.ErrNotFound)
	attempts := 1
	source := ""
	if guard != nil {
		source = guard.SourceEnvironment
		if repeated {
			attempts = guard.Attempts + 1
		}
	}
	next := &model.Verification{Environment: environment, SourceEnvironment: source, HeadSHA: task.HeadSHA, Fingerprint: verificationFingerprint(environment+"/"+failure.command, reason), Attempts: attempts, NativeOnly: nativeOnly || capabilityMissing}
	// NativeOnly records why the supervisor, rather than the worker, owns this
	// check. It does not turn a check that launched and exited non-zero into an
	// environment failure. A launched check has actionable source evidence and
	// must give the implementer one bounded FIX attempt.
	if capabilityMissing || repeated {
		question := "Native verification cannot complete in the current environment. Repair its tools or environment, then answer to retry verification."
		resume := model.SyncRequired
		if repeated && !capabilityMissing {
			question = "Native verification failed again at the same source revision and environment. Diagnose the persistent source failure, then answer to run one bounded implementer fix."
			resume = model.Fix
		}
		if c.mutate(func(s *model.Snapshot) error {
			t := s.Tasks[id]
			t.Verification = next
			origin := ""
			if capabilityMissing {
				origin = model.BlockerOriginVerificationOnly
				advancePreflightHead(t)
			}
			model.BlockWithOrigin(t, question, reason, resume, origin)
			return nil
		}) == nil {
			_ = c.P.DB.Event(id, task.RunID, "verification", "native", "retry_suppressed", environment+" head="+task.HeadSHA)
			c.mirror(id)
		}
		return
	}
	if c.mutate(func(s *model.Snapshot) error {
		s.Tasks[id].Verification = next
		return nil
	}) != nil {
		return
	}
	_ = c.P.DB.Event(id, task.RunID, "verification", "native", "verification_retry_guarded", environment+" head="+task.HeadSHA)
	c.retry(id, "verification", reason)
}
func (c *Controller) retry(id, kind, reason string) {
	if c.ctx.Err() != nil {
		return
	}
	reason = c.portable(reason)
	t := c.Snapshot().Tasks[id]
	effective, err := c.effective(c.ctx)
	if err != nil {
		c.block(id, "Restore canonical policy access before retrying.", err.Error(), model.Fix)
		return
	}
	preflightRoles, preflightErr := requiredPreflightRoles(effective, t)
	limit := effective.Policy.ReviewCycles
	count := t.FixCycles[kind] + 1
	if kind == "implementation" {
		count = t.Attempts + 1
		limit = effective.Policy.ImplementationRetries
	}
	if kind == "qa" {
		limit = effective.Policy.QACycles
	}
	if kind == "designer" {
		limit = effective.Policy.DesignCycles
	}
	if count > limit {
		if !t.AdvisorUsed {
			if c.mutate(func(s *model.Snapshot) error { s.Tasks[id].AdvisorUsed = true; return nil }) != nil {
				return
			}
			effective, e := c.effective(c.ctx)
			if e == nil {
				result, re := c.role(c.ctx, effective, roles.Builtins()["advisor"], t, c.P.TaskPath(t), "Investigate failure and recommend one final bounded approach: "+reason, "", "")
				if re == nil && result.Status == "completed" {
					_ = c.mutate(func(s *model.Snapshot) error {
						task := s.Tasks[id]
						task.Decisions = append(task.Decisions, "Advisor: "+result.Summary)
						task.State = model.Fix
						task.Findings = append(task.Findings, model.Finding{Severity: "high", Category: kind, Reason: reason, Resolution: result.Summary, Role: "advisor"})
						return nil
					})
					return
				}
			}
		}
		c.block(id, "Recovery budget exhausted. Choose how to proceed.", reason, model.Fix)
		return
	}
	if c.mutate(func(s *model.Snapshot) error {
		task := s.Tasks[id]
		if kind == "implementation" {
			task.Attempts = count
		} else {
			task.FixCycles[kind] = count
		}
		if preflightErr == nil {
			if direct := directFixRoute(effective, kind, task, preflightRoles); direct != nil {
				task.Preflight = direct
				allSatisfied := true
				for _, role := range preflightRoles {
					if !preflightRoleSatisfied(direct, task, effective, role) {
						allSatisfied = false
						break
					}
				}
				if allSatisfied {
					direct.Phase = "ready"
				}
				task.State = model.Fix
				return nil
			}
		}
		task.Findings = append(task.Findings, model.Finding{Severity: "high", Category: kind, Reason: reason, Role: kind})
		task.State = model.Fix
		if preflightErr == nil {
			reusePreflightForFix(task.Preflight, task, effective, preflightRoles)
		}
		return nil
	}) == nil {
		c.mirror(id)
	}
}
func (c *Controller) syncTask(ctx context.Context, id string) (config.Effective, error) {
	effective, e := c.effective(ctx)
	if e != nil {
		return effective, e
	}
	base := effective.BaseSHA
	t := c.Snapshot().Tasks[id]
	if e = c.P.Git.Rebase(ctx, c.P.TaskPath(t), base); e != nil {
		if ctx.Err() == nil {
			if pe := c.P.Git.PrepareMerge(ctx, c.P.TaskPath(t), base); pe == nil {
				_ = c.mutate(func(s *model.Snapshot) error { s.Tasks[id].SyncBase = base; return nil })
			}
		}
		return effective, e
	}
	if e = c.checkpointAtBase(ctx, id, base); e != nil {
		return effective, e
	}
	head, e := (gitx.Git{Dir: c.P.TaskPath(t)}).SHA(ctx, "HEAD")
	if e != nil {
		return effective, e
	}
	e = c.mutate(func(s *model.Snapshot) error {
		task := s.Tasks[id]
		task.BaseSHA = base
		task.SyncBase = ""
		task.HeadSHA = head
		if task.VisualRequired != nil {
			task.VisualRequired.Base = base
			task.VisualRequired.Head = head
			task.VisualRequired.Config = effective.Hash
			task.VisualRequired.Rules = roles.Hash()
		}
		task.State = model.Verifying
		task.Evidence = nil
		return nil
	})
	return effective, e
}
func cleanEnvironment() []string {
	env := []string{}
	for _, v := range os.Environ() {
		k := strings.ToUpper(strings.SplitN(v, "=", 2)[0])
		if strings.Contains(k, "TOKEN") || strings.Contains(k, "SECRET") || strings.Contains(k, "API_KEY") || strings.Contains(k, "PASSWORD") {
			continue
		}
		env = append(env, v)
	}
	return env
}
func Verify(ctx context.Context, e config.Effective, dir string) ([]string, error) {
	return verifyWithPermit(ctx, e, dir, nil)
}
func verifyWithPermit(ctx context.Context, e config.Effective, dir string, permit func(context.Context, config.Check) (func(), error)) ([]string, error) {
	return verifyChecksWithPermit(ctx, e.Project.Checks, dir, permit)
}
func verifyChecksWithPermit(ctx context.Context, configured []config.Check, dir string, permit func(context.Context, config.Check) (func(), error)) ([]string, error) {
	var checked []string
	for _, check := range configured {
		if !applicable(check) {
			continue
		}
		release := func() {}
		if permit != nil {
			var err error
			release, err = permit(ctx, check)
			if err != nil {
				return checked, err
			}
		}
		out, err, report := runVerificationCheck(ctx, check, dir)
		release()
		if err != nil {
			return checked, &checkFailure{name: check.Name, command: filepath.Base(check.Command[0]), err: err, output: boundedFailureDiagnostic(safety.Redact(out)), check: check, report: report}
		}
		checked = append(checked, passedCheckEvidence(check, out))
	}
	if len(checked) == 0 {
		return nil, errors.New("no applicable verification checks; configure .aih/project.yaml on main")
	}
	dirty, err := (gitx.Git{Dir: dir}).Run(ctx, "", "status", "--porcelain")
	if err != nil {
		return checked, err
	}
	if dirty != "" {
		return checked, errors.New("verification modified source or created unignored files; evidence invalid")
	}
	return checked, nil
}

func runVerificationCheck(ctx context.Context, check config.Check, dir string) (string, error, *nativeFailureReport) {
	reportPath, cleanup, setupErr := nativeFailureReportPath(check)
	if setupErr != nil {
		return "", setupErr, nil
	}
	defer cleanup()
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(check.Timeout)*time.Second)
	out, err := platform.Run(checkCtx, dir, nativeFailureEnvironment(cleanEnvironment(), reportPath), "", check.Command[0], check.Command[1:]...)
	cancel()
	if err == nil || reportPath == "" {
		return out, err, nil
	}
	report, reportErr := parseNativeFailureReport(reportPath)
	if reportErr != nil {
		// A malformed child diagnostic must not hide the check failure or widen
		// its routing authority. The ordinary bounded local recovery remains.
		return out, err, nil
	}
	return out, err, report
}

func (c *Controller) checks(ctx context.Context, e config.Effective, dir, taskID string) ([]string, error) {
	return verifyWithPermit(ctx, e, dir, func(ctx context.Context, check config.Check) (func(), error) {
		return c.checkPermit(ctx, taskID, check)
	})
}
func (c *Controller) checksForPlan(ctx context.Context, dir, taskID string, plan validationPlan) ([]string, error) {
	return verifyChecksWithPermit(ctx, plan.Checks, dir, func(ctx context.Context, check config.Check) (func(), error) {
		return c.checkPermit(ctx, taskID, check)
	})
}

func reviewPromptTask(task *model.Task, paths []string) *model.Task {
	// Prompt scoping must include actual changed paths so nested AGENTS.md
	// instructions apply even when a worker changed outside its planned Areas.
	// This copy is deliberately never used for review-scope hashing or durable
	// task state: the canonical task contract remains immutable during review.
	promptTask := *task
	promptTask.Areas = append(append([]string(nil), task.Areas...), paths...)
	return &promptTask
}

func (c *Controller) runReviewAttempt(effective config.Effective, task *model.Task, paths []string, dir, diff string, evidence *model.Evidence, attempt int, required []roles.Role) []reviewOutcome {
	outcomes := make([]reviewOutcome, len(required))
	payload := reviewEvidencePayload(evidence, attempt)
	promptTask := reviewPromptTask(task, paths)
	if evidence.Visual != nil {
		payload += "\nVISUAL ARTIFACT ROOT (local, read-only): " + filepath.Join(c.P.Dir, filepath.FromSlash(filepath.Dir(evidence.Visual.Manifest))) + "\nInspect the screenshot and diagnostics listed in visual.artifacts. A capture artifact is evidence, not a visual pass.\n"
	}
	var reviews sync.WaitGroup
	for i, role := range required {
		reviews.Add(1)
		go func() {
			defer reviews.Done()
			outcomes[i].result, outcomes[i].err = c.role(c.ctx, effective, role, promptTask, dir, task.Objective, diff, payload)
		}()
	}
	reviews.Wait()
	return outcomes
}

func visualRequirementMatches(task *model.Task, effective config.Effective) bool {
	if task == nil || task.VisualRequired == nil {
		return false
	}
	requirement := task.VisualRequired
	return requirement.Base == effective.BaseSHA && requirement.Head == task.HeadSHA && requirement.Config == effective.Hash && requirement.Rules == roles.Hash()
}

func (c *Controller) preserveReviewFindings(id string, findings []model.Finding) error {
	if len(findings) == 0 {
		return nil
	}
	return c.mutate(func(s *model.Snapshot) error {
		s.Tasks[id].Findings = appendUniqueFindings(s.Tasks[id].Findings, findings)
		return nil
	})
}

func (c *Controller) verifyReview(id string) error {
	effective, e := c.syncTask(c.ctx, id)
	if e != nil {
		return e
	}
	t := c.Snapshot().Tasks[id]
	dir := c.P.TaskPath(t)
	diff, paths, e := c.P.Git.Diff(c.ctx, t.BaseSHA, t.HeadSHA)
	if e != nil {
		return e
	}
	if diff == "" {
		return errors.New("implementation has no source changes")
	}
	if e = c.validateTaskScope(t); e != nil {
		return e
	}
	if e = safety.Check(diff); e != nil {
		return e
	}
	if e = c.ensureDraftPR(id); e != nil {
		return e
	}
	t = c.Snapshot().Tasks[id]
	for _, p := range paths {
		if e = safety.Path(p); e != nil {
			return e
		}
		if strings.HasPrefix(p, ".aih/") {
			return errors.New("worker changed AIH policy; change canonical policy through a separate human-reviewed commit")
		}
		lower := strings.ToLower(p)
		for _, sensitive := range []string{"auth", "secret", "permission", "crypto", "network", "deserial", "subprocess", "package-lock", "go.sum", "cargo.lock"} {
			if strings.Contains(lower, sensitive) {
				t.Security = true
			}
		}
		for _, suffix := range []string{".tsx", ".jsx", ".css", ".scss", ".html", ".vue", ".svelte", ".swiftui"} {
			if strings.HasSuffix(lower, suffix) {
				t.UI = true
			}
		}
	}
	plan, e := c.taskValidationPlan(c.ctx, effective, t, dir, paths)
	if e != nil {
		return e
	}
	checks, e := c.checksForPlan(c.ctx, dir, id, plan)
	if e != nil {
		return e
	}
	all, e := roles.Load(effective.Files)
	if e != nil {
		return e
	}
	// Changed paths are already bound into the review scope below. Do not append
	// them to the task contract here: this local copy is not persisted, so doing
	// so makes a completed MERGE_READY review scope differ from the scope rebuilt
	// by final integration and forces an unnecessary full re-verification.
	required, e := roles.Required(all, t, paths, "review")
	if e != nil {
		return e
	}
	visualRequired := visualRequirementMatches(t, effective)
	if visualRequired {
		name := t.VisualRequired.Role
		visualRole, ok := all[name]
		if !ok || !designerReviewRole(visualRole) {
			return errors.New("durable visual requirement has no configured visual reviewer")
		}
		if !slices.ContainsFunc(required, func(role roles.Role) bool { return role.Name == name }) {
			required = append(required, visualRole)
		}
	}
	roster, rosterReason := roles.ReviewRoster(required)
	scope := reviewScope(t, paths, roster)
	dispositions := c.reusableReviewDispositions(c.ctx, effective, t, roster, scope)
	if dispositions == nil {
		dispositions = map[string]model.ReviewDisposition{}
	}
	activeRequired := make([]roles.Role, 0, len(required))
	for _, role := range required {
		if _, reused := dispositions[role.Name]; !reused {
			activeRequired = append(activeRequired, role)
		}
	}
	evidence := &model.Evidence{Base: t.BaseSHA, Head: t.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Checks: checks, ValidationGate: plan.Gate, ValidationReason: plan.Reason, ValidationInput: plan.Input, Toolchain: plan.Toolchain, TestInputs: plan.TestInputs, Reviews: map[string]string{}, ReviewRoster: roster, ReviewRosterReason: rosterReason, ReviewScope: scope, ReviewDispositions: dispositions, At: time.Now().UTC()}
	if e = c.mutate(func(s *model.Snapshot) error {
		task := s.Tasks[id]
		task.State = model.Review
		task.UI = t.UI
		task.Security = t.Security
		task.Findings = nil
		task.Verification = nil
		task.Evidence = evidence
		return nil
	}); e != nil {
		return e
	}
	if visualRequired {
		visual, visualErr := c.captureVisual(c.ctx, effective, t, dir)
		if visualErr != nil {
			return visualErr
		}
		evidence.Visual = visual
		if e = c.publishReviewProgress(id, evidence); e != nil {
			return e
		}
	}
	if e = c.updatePR(id, true); e != nil {
		return e
	}
	c.mirror(id)
	outcomes := c.runReviewAttempt(effective, t, paths, dir, diff, evidence, 1, activeRequired)
	assessment := assessReviews(activeRequired, outcomes, reviewFindingBlocksOrigin(t, paths, activeRequired))
	if e = c.preserveReviewFindings(id, assessment.findings); e != nil {
		return e
	}
	for i, role := range activeRequired {
		if outcomes[i].result.Status == "completed" {
			evidence.Reviews[role.Name] = outcomes[i].result.Summary
			evidence.ReviewDispositions[role.Name] = completedDisposition(t, role, roleRuntime(effective, role))
		}
	}
	if e = c.persistReviewProvenance(id, effective, t, roster, scope, activeRequired, outcomes); e != nil {
		return e
	}
	if e = c.publishReviewProgress(id, evidence); e != nil {
		return e
	}
	routed, local := routableReviewFindings(assessment.findings)
	route, routeErr := c.routeCrossTaskFindings(id, routed, reviewFindingBlocksOrigin(t, paths, activeRequired))
	if routeErr != nil {
		return routeErr
	}
	route.local = append(route.local, local...)
	if e = c.reviewFollowups(t, route.local); e != nil {
		return e
	}
	if route.gated {
		return c.refreshDraftPR(id)
	}
	if assessment.blocking >= 0 {
		role := activeRequired[assessment.blocking]
		_ = c.P.DB.Event(id, t.RunID, role.Name, effective.Project.Provider, "review_finding_fix", outcomes[assessment.blocking].result.Summary)
		c.retry(id, role.Name, outcomes[assessment.blocking].result.Summary)
		return c.refreshDraftPR(id)
	}
	if assessment.human >= 0 {
		role := activeRequired[assessment.human]
		result := outcomes[assessment.human].result
		_ = c.P.DB.Event(id, t.RunID, role.Name, effective.Project.Provider, "human_decision_required", result.Question)
		c.block(id, result.Question, result.Summary, model.SyncRequired)
		return c.refreshDraftPR(id)
	}
	if assessment.failure != nil {
		return assessment.failure
	}
	if len(assessment.evidence) > 0 {
		refreshRoles := make([]roles.Role, 0, len(assessment.evidence))
		names := make([]string, 0, len(assessment.evidence))
		visualRequested := false
		sourceRequested := false
		for _, index := range assessment.evidence {
			refreshRoles = append(refreshRoles, activeRequired[index])
			names = append(names, activeRequired[index].Name)
			visualRequested = visualRequested || visualEvidenceRequest(outcomes[index].result)
			sourceRequested = sourceRequested || sourceEvidenceRequest(outcomes[index].result)
		}
		_ = c.P.DB.Event(id, t.RunID, "verification", "native", "review_evidence_refresh_requested", "roles="+strings.Join(names, ",")+" head="+t.HeadSHA)
		if visualRequested {
			_ = c.P.DB.Event(id, t.RunID, "verification", "native", "visual_capture_queued", "head="+t.HeadSHA)
		}
		if !visualRequested || sourceRequested {
			plan, planErr := fullValidationPlan(c.ctx, effective, dir, t.HeadSHA, "reviewer requested source evidence refresh")
			if planErr != nil {
				return planErr
			}
			checks, checkErr := c.checksForPlan(c.ctx, dir, id, plan)
			if checkErr != nil {
				_ = c.P.DB.Event(id, t.RunID, "verification", "native", "review_evidence_refresh_failed", short(checkErr.Error(), 500))
				return checkErr
			}
			if e = applyValidationEvidence(evidence, plan, checks); e != nil {
				return e
			}
		}
		if visualRequested {
			_ = c.P.DB.Event(id, t.RunID, "verification", "native", "visual_capture_running", "head="+t.HeadSHA)
			visual, visualErr := c.captureVisual(c.ctx, effective, t, dir)
			if visualErr != nil {
				kind := "visual_capture_failed"
				if effective.Project.VisualCapture == nil {
					kind = "visual_capture_unavailable"
				}
				_ = c.P.DB.Event(id, t.RunID, "verification", "native", kind, short(safety.Redact(visualErr.Error()), 500))
				return visualErr
			}
			evidence.Visual = visual
			_ = c.P.DB.Event(id, t.RunID, "verification", "native", "visual_capture_completed", "head="+t.HeadSHA+" manifest="+visual.Manifest)
		}
		evidence.At = time.Now().UTC()
		if e = c.publishReviewProgress(id, evidence); e != nil {
			return e
		}
		refreshed := c.runReviewAttempt(effective, t, paths, dir, diff, evidence, 2, refreshRoles)
		refreshAssessment := assessReviews(refreshRoles, refreshed, reviewFindingBlocksOrigin(t, paths, refreshRoles))
		if e = c.preserveReviewFindings(id, refreshAssessment.findings); e != nil {
			return e
		}
		for i, role := range refreshRoles {
			if refreshed[i].result.Status == "completed" {
				evidence.Reviews[role.Name] = refreshed[i].result.Summary
				evidence.ReviewDispositions[role.Name] = completedDisposition(t, role, roleRuntime(effective, role))
			}
		}
		if e = c.persistReviewProvenance(id, effective, t, roster, scope, refreshRoles, refreshed); e != nil {
			return e
		}
		if e = c.publishReviewProgress(id, evidence); e != nil {
			return e
		}
		refreshedRouted, refreshedLocal := routableReviewFindings(refreshAssessment.findings)
		refreshRoute, routeErr := c.routeCrossTaskFindings(id, refreshedRouted, reviewFindingBlocksOrigin(t, paths, refreshRoles))
		if routeErr != nil {
			return routeErr
		}
		refreshRoute.local = append(refreshRoute.local, refreshedLocal...)
		if e = c.reviewFollowups(t, refreshRoute.local); e != nil {
			return e
		}
		if refreshRoute.gated {
			return c.refreshDraftPR(id)
		}
		if refreshAssessment.blocking >= 0 {
			role := refreshRoles[refreshAssessment.blocking]
			_ = c.P.DB.Event(id, t.RunID, role.Name, effective.Project.Provider, "review_finding_fix", refreshed[refreshAssessment.blocking].result.Summary)
			c.retry(id, role.Name, refreshed[refreshAssessment.blocking].result.Summary)
			return c.refreshDraftPR(id)
		}
		if refreshAssessment.human >= 0 {
			role := refreshRoles[refreshAssessment.human]
			result := refreshed[refreshAssessment.human].result
			_ = c.P.DB.Event(id, t.RunID, role.Name, effective.Project.Provider, "human_decision_required", result.Question)
			c.block(id, result.Question, result.Summary, model.SyncRequired)
			return c.refreshDraftPR(id)
		}
		if refreshAssessment.failure != nil {
			return refreshAssessment.failure
		}
		if len(refreshAssessment.evidence) > 0 {
			reason := "reviewer requested supervisor-owned evidence again after one exact-head native refresh: " + strings.Join(names, ",")
			_ = c.P.DB.Event(id, t.RunID, "verification", "native", "review_evidence_refresh_failed", reason)
			c.retry(id, "verification", reason)
			return c.refreshDraftPR(id)
		}
		_ = c.P.DB.Event(id, t.RunID, "verification", "native", "review_evidence_refresh_completed", "roles="+strings.Join(names, ",")+" head="+t.HeadSHA)
	}
	if !validReviewDispositions(required, evidence) {
		return errors.New("final review roster has no valid disposition for every required role")
	}
	if visualRequired && (evidence.Visual == nil || strings.TrimSpace(evidence.Reviews[t.VisualRequired.Role]) == "") {
		c.block(id, "Exact-head visual evidence and the designated visual review are required before merge.", "The durable preflight visual requirement was not satisfied; capture or reviewer completion is missing.", model.SyncRequired)
		return c.refreshDraftPR(id)
	}
	if !dependenciesComplete(c.Snapshot(), t) {
		// A cross-task route can arrive while final reviews are running. Do not
		// advertise a merge-ready PR until every durable dependency is complete;
		// after its owner merges this task must obtain fresh integration evidence.
		if e = c.mutate(func(s *model.Snapshot) error {
			task := s.Tasks[id]
			task.Evidence = evidence
			task.State = model.SyncRequired
			return nil
		}); e != nil {
			return e
		}
		return c.refreshDraftPR(id)
	}
	if e = c.mutate(func(s *model.Snapshot) error {
		task := s.Tasks[id]
		task.Evidence = evidence
		task.State = model.MergeReady
		return nil
	}); e != nil {
		return e
	}
	t = c.Snapshot().Tasks[id]
	if e = c.updatePR(id, false); e != nil {
		return e
	}
	c.mirror(id)
	return nil
}

func (c *Controller) publishReviewProgress(id string, evidence *model.Evidence) error {
	if err := c.mutate(func(s *model.Snapshot) error {
		s.Tasks[id].Evidence = evidence
		return nil
	}); err != nil {
		return err
	}
	return c.refreshDraftPR(id)
}

func (c *Controller) refreshDraftPR(id string) error {
	if err := c.updatePR(id, true); err != nil {
		return err
	}
	c.mirror(id)
	return nil
}

func (c *Controller) ensureDraftPR(id string) error {
	t := c.Snapshot().Tasks[id]
	if t.PR == 0 {
		pr, e := c.P.Hub.EnsurePR(c.ctx, t.Branch, "main", t.Title, c.prBody(t))
		if e != nil {
			return e
		}
		if e = c.mutate(func(s *model.Snapshot) error { s.Tasks[id].PR = pr; return nil }); e != nil {
			return e
		}
	}
	return c.updatePR(id, true)
}
func (c *Controller) updatePR(id string, draft bool) error {
	t := c.Snapshot().Tasks[id]
	if t == nil || t.PR == 0 {
		return nil
	}
	if draft {
		if e := c.P.Hub.SetPRDraft(c.ctx, t.PR, true); e != nil {
			return e
		}
	}
	if e := c.P.Hub.UpdatePR(c.ctx, t.PR, c.prBody(t)); e != nil {
		return e
	}
	if !draft {
		if e := c.P.Hub.SetPRDraft(c.ctx, t.PR, false); e != nil {
			return e
		}
	}
	return nil
}
func acceptedEvidence(t *model.Task) bool {
	if t == nil || t.Evidence == nil || t.Evidence.Config == "" || t.Evidence.Rules == "" || t.Evidence.ValidationGate == "" || t.Evidence.ValidationInput == "" || len(t.Evidence.Checks) == 0 {
		return false
	}
	switch t.State {
	case model.MergeReady, model.MergeTrain:
		return t.Evidence.Base == t.BaseSHA && t.Evidence.Head == t.HeadSHA
	case model.PostVerify, model.Done:
		return t.MergeSHA != "" && t.Evidence.IntegrationSHA == t.MergeSHA
	default:
		return false
	}
}
func (c *Controller) prBody(t *model.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nCloses #%d\n\n%s\n", github.Marker(t.ID), t.Issue, t.Objective)
	accepted := acceptedEvidence(t)
	if accepted {
		fmt.Fprintf(&b, "\nAIH review status: exact-head verification and required independent reviews passed.\nAIH state: `%s`\nCheckpoint head: `%s`\n", t.State, t.HeadSHA)
	} else {
		fmt.Fprintf(&b, "\nAIH work in progress: this draft PR is visible for inspection but is not verified or merge-ready.\nAIH state: `%s`\nCheckpoint head: `%s`\n", t.State, t.HeadSHA)
	}
	b.WriteString("\nAcceptance criteria:\n")
	for _, a := range t.Acceptance {
		mark := " "
		if accepted {
			mark = "x"
		}
		fmt.Fprintf(&b, "- [%s] %s\n", mark, a)
	}
	if t.Summary != "" {
		fmt.Fprintf(&b, "\nImplementation: %s\n", t.Summary)
	}
	for _, risk := range t.Risks {
		fmt.Fprintf(&b, "\nReported risk: %s\n", risk)
	}
	if e := t.Evidence; e != nil {
		label := "Verification progress; acceptance is not complete"
		if accepted {
			label = "Accepted verification"
		}
		fmt.Fprintf(&b, "\n%s (base `%s`, head `%s`):\n", label, e.Base, e.Head)
		if len(e.ReviewRoster) > 0 {
			fmt.Fprintf(&b, "- review roster: %s\n- roster reason: %s\n", strings.Join(e.ReviewRoster, ", "), e.ReviewRosterReason)
		}
		for _, n := range e.Checks {
			b.WriteString("- passed: " + n + "\n")
		}
		names := []string{}
		for n := range e.Reviews {
			names = append(names, n)
		}
		sort.Strings(names)
		b.WriteString("\nIndependent reviews:\n")
		for _, n := range names {
			b.WriteString("- " + n + ": " + e.Reviews[n] + "\n")
		}
		dispositionNames := make([]string, 0, len(e.ReviewDispositions))
		for name := range e.ReviewDispositions {
			dispositionNames = append(dispositionNames, name)
		}
		sort.Strings(dispositionNames)
		if len(dispositionNames) > 0 {
			b.WriteString("\nReview dispositions:\n")
			for _, name := range dispositionNames {
				d := e.ReviewDispositions[name]
				fmt.Fprintf(&b, "- %s: %s from `%s` via %s", name, d.Disposition, d.SourceHead, d.Runtime)
				if d.Reason != "" {
					fmt.Fprintf(&b, " (%s)", d.Reason)
				}
				b.WriteString("\n")
			}
		}
		fmt.Fprintf(&b, "\nPolicy hash: `%s`\nRules hash: `%s`\n", e.Config, e.Rules)
		fmt.Fprintf(&b, "Validation gate: `%s` (%s)\nValidation identity: `%s`\n", e.ValidationGate, e.ValidationReason, e.ValidationInput)
	}
	verificationFindings := false
	for _, finding := range t.Findings {
		if finding.Category != "verification" {
			continue
		}
		if !verificationFindings {
			b.WriteString("\nNative verification findings:\n")
			verificationFindings = true
		}
		fmt.Fprintf(&b, "- %s: %s\n", finding.Severity, short(safety.Redact(finding.Reason), 8000))
	}
	b.WriteString("\nRisks: inspect recorded findings and acceptance evidence.\nRollback: propose and verify a revert of the integration commit; no automatic production rollback.\n")
	return b.String()
}
