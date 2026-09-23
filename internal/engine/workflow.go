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
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type checkFailure struct {
	name, command, output string
	err                   error
}

func (e *checkFailure) Error() string {
	return fmt.Sprintf("%s failed: %v\n%s", e.name, e.err, e.output)
}
func (e *checkFailure) Unwrap() error { return e.err }

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
	return fmt.Sprintf("check=%q command=%q exit=0 stdout=%s stdout_bytes=%d stdout_lines=%d", check.Name, filepath.Base(check.Command[0]), state, len([]byte(output)), lines)
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
	for _, marker := range []string{"verification evidence", "native verification", "native check", "native logs", "test output", "validation output", "check output", "runtime evidence", "review evidence", "peer review", "peer approval", "independent review", "exact-head", "current-head", "cannot run", "could not run", "tool unavailable"} {
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

func assessReviews(required []roles.Role, outcomes []reviewOutcome) reviewAssessment {
	assessment := reviewAssessment{blocking: -1, human: -1}
	for i, role := range required {
		result := outcomes[i].result
		retained := make([]model.Finding, 0, len(result.Findings))
		for _, finding := range result.Findings {
			if supervisorEvidenceOnlyFinding(result, finding) {
				continue
			}
			finding.Role = role.Name
			assessment.findings = append(assessment.findings, finding)
			retained = append(retained, finding)
		}
		if assessment.blocking == -1 && roles.Blocking(role, retained) {
			assessment.blocking = i
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
	}{Evidence: &copy, Attempt: attempt, Policy: "Peer reviews are concurrent and independent; the empty reviews map is intentional. Supervisor check evidence is exact-head metadata; successful stdout content, command arguments, and environment values are intentionally omitted."}
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
	return c.roleWithCompletion(ctx, e, r, t, dir, objective, diff, evidence, nil)
}
func (c *Controller) roleWithCompletion(ctx context.Context, e config.Effective, r roles.Role, t *model.Task, dir, objective, diff, evidence string, complete func(*model.Snapshot, provider.Result, error) error) (provider.Result, error) {
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
	prompt := roles.Compile(e, r, runtime.GOOS, t, objective, diff, evidence)
	request := provider.Request{Directory: dir, Runtime: runtimeDir, Prompt: prompt, Role: r.Name, Model: resolved.RequestModel, Write: r.Name == "implementer", Timeout: time.Duration(e.Project.WorkerSeconds) * time.Second}
	var result provider.Result
	var err error
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
	}
	outcome := result.Status
	if err != nil {
		outcome = "failed"
	}
	if ctx.Err() == nil {
		saveErr := c.mutate(func(s *model.Snapshot) error {
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
	dir := filepath.Join(c.P.Dir, "analysis", model.ID())
	if e = c.P.Git.Detached(c.ctx, dir, "refs/remotes/origin/main"); e != nil {
		c.planFailure(id, e)
		return
	}
	defer c.P.Git.RemoveWorktree(context.Background(), dir)
	r, e := c.role(c.ctx, effective, roles.Builtins()["orchestrator"], nil, dir, o.Text, "", "")
	if e != nil {
		c.planFailure(id, e)
		return
	}
	if r.Status != "completed" {
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
	e = c.mutate(func(s *model.Snapshot) error {
		for _, p := range r.Plan {
			taskID := id + "-" + p.Key
			deps := []string{}
			for _, d := range p.Dependencies {
				deps = append(deps, id+"-"+d)
			}
			s.Tasks[taskID] = &model.Task{ID: taskID, ObjectiveID: id, Title: p.Title, Objective: p.Objective, Acceptance: p.Acceptance, Dependencies: deps, Areas: p.Areas, Domains: p.Domains, Risk: p.Risk, UI: p.UI, Security: p.Security, Roles: p.Roles, State: model.Planned, FixCycles: map[string]int{}}
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
	if c.ctx.Err() != nil {
		return
	}
	if c.mutate(func(s *model.Snapshot) error {
		model.Block(s.Tasks[id], c.portable(question), c.portable(reason), resume)
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
	sha, e := c.P.Git.Checkpoint(ctx, c.P.TaskPath(t), id)
	if e != nil {
		return e
	}
	if sha == t.HeadSHA {
		return nil
	}
	return c.save(ctx, func(s *model.Snapshot) error { s.Tasks[id].HeadSHA = sha; return nil }, gitx.Update{Branch: t.Branch, Old: t.HeadSHA, New: sha})
}

func (c *Controller) recoveredCheckpoint(ctx context.Context, id string, result provider.Result) error {
	if !result.RecoveredDeadlineHandoff || result.Status != "in_progress" {
		return errors.New("invalid recovered deadline handoff")
	}
	t := c.Snapshot().Tasks[id]
	sha, err := c.P.Git.Checkpoint(ctx, c.P.TaskPath(t), id)
	if err != nil {
		return err
	}
	updates := []gitx.Update{}
	if sha != t.HeadSHA {
		updates = append(updates, gitx.Update{Branch: t.Branch, Old: t.HeadSHA, New: sha})
	}
	return c.save(ctx, func(s *model.Snapshot) error {
		task := s.Tasks[id]
		task.HeadSHA = sha
		task.Summary = c.portable(result.Summary)
		task.ReportedTests = portableStrings(c, result.Tests)
		task.Risks = portableStrings(c, result.Risks)
		task.Rotations++
		task.Decisions = append(task.Decisions, "Checkpoint: "+task.Summary)
		if task.Rotations >= 24 {
			model.Block(task, "Task reached 24 checkpoint slices. Refine or authorize further work.", task.Summary, model.Ready)
			return nil
		}
		return model.Transition(task, model.Ready)
	}, updates...)
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
		from = "refs/remotes/origin/main"
	}
	if e := c.P.Git.Worktree(c.ctx, c.P.TaskPath(t), t.Branch, from); e != nil {
		return e
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
			var checkErr *checkFailure
			if errors.As(e, &checkErr) {
				c.verificationFailure(id, checkErr)
			} else {
				c.retry(id, "verification", e.Error())
			}
		}
		return
	}
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
	dir := c.P.TaskPath(t)
	if t.SyncBase != "" {
		if e = c.P.Git.PrepareMerge(c.ctx, dir, t.SyncBase); e != nil {
			c.block(id, "Repair task synchronization and retry.", e.Error(), model.Fix)
			return false
		}
	}
	t = c.Snapshot().Tasks[id]
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
			if c.mutate(func(s *model.Snapshot) error {
				task := s.Tasks[id]
				task.Verification = &model.Verification{Environment: native, SourceEnvironment: source, HeadSHA: task.HeadSHA, Fingerprint: verificationFingerprint(source, reason), NativeOnly: true}
				task.Decisions = append(task.Decisions, "Implementation complete; supervisor-native verification requested because the worker environment lacked a required verification capability.")
				return model.Transition(task, model.Implemented)
			}) != nil {
				return false
			}
			_ = c.P.DB.Event(id, c.Snapshot().Tasks[id].RunID, "implementer", effective.Project.Provider, "verification_rerouted", source+" -> "+native)
			return true
		}
		c.block(id, r.Question, r.Summary, model.Ready)
		return false
	case "in_progress":
		_ = c.mutate(func(s *model.Snapshot) error {
			t := s.Tasks[id]
			t.Rotations++
			if t.Rotations >= 24 {
				model.Block(t, "Task reached 24 checkpoint slices. Refine or authorize further work.", r.Summary, model.Ready)
				return nil
			}
			t.Decisions = append(t.Decisions, "Checkpoint: "+r.Summary)
			return model.Transition(t, model.Ready)
		})
		return false
	case "failed":
		c.retry(id, "implementation", r.Summary)
		return false
	case "completed":
		if c.mutate(func(s *model.Snapshot) error { return model.Transition(s.Tasks[id], model.Implemented) }) != nil {
			return false
		}
		return true
	default:
		c.retry(id, "implementation", "invalid result status")
		return false
	}
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
	environment := nativeEnvironment(effective)
	task := c.Snapshot().Tasks[id]
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
			model.Block(t, question, reason, resume)
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
		task.Findings = append(task.Findings, model.Finding{Severity: "high", Category: kind, Reason: reason, Role: kind})
		task.State = model.Fix
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
	if e = c.checkpoint(ctx, id); e != nil {
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
	var checked []string
	for _, check := range e.Project.Checks {
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
		checkCtx, cancel := context.WithTimeout(ctx, time.Duration(check.Timeout)*time.Second)
		out, err := platform.Run(checkCtx, dir, cleanEnvironment(), "", check.Command[0], check.Command[1:]...)
		cancel()
		release()
		if err != nil {
			return checked, &checkFailure{name: check.Name, command: filepath.Base(check.Command[0]), err: err, output: short(safety.Redact(out), 8000)}
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
func (c *Controller) checks(ctx context.Context, e config.Effective, dir, taskID string) ([]string, error) {
	return verifyWithPermit(ctx, e, dir, func(ctx context.Context, check config.Check) (func(), error) {
		return c.checkPermit(ctx, taskID, check)
	})
}

func (c *Controller) runReviewAttempt(effective config.Effective, task *model.Task, dir, diff string, evidence *model.Evidence, attempt int, required []roles.Role) []reviewOutcome {
	outcomes := make([]reviewOutcome, len(required))
	payload := reviewEvidencePayload(evidence, attempt)
	var reviews sync.WaitGroup
	for i, role := range required {
		reviews.Add(1)
		go func() {
			defer reviews.Done()
			outcomes[i].result, outcomes[i].err = c.role(c.ctx, effective, role, task, dir, task.Objective, diff, payload)
		}()
	}
	reviews.Wait()
	return outcomes
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

func (c *Controller) reviewFollowups(task *model.Task, findings []model.Finding) error {
	for _, finding := range findings {
		if finding.Severity != "medium" {
			continue
		}
		key := fmt.Sprintf("%s-followup-%x", task.ID, sha256.Sum256([]byte(finding.Role+finding.Location+finding.Reason)))
		if _, err := c.P.Hub.EnsureIssue(c.ctx, key, "Follow-up: "+short(finding.Reason, 90), fmt.Sprintf("From #%d\n\n%s\n\n%s", task.Issue, finding.Reason, finding.Resolution)); err != nil {
			return err
		}
	}
	return nil
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
	if e = safety.Check(diff); e != nil {
		return e
	}
	if e = c.ensureDraftPR(id); e != nil {
		return e
	}
	t = c.Snapshot().Tasks[id]
	checks, e := c.checks(c.ctx, effective, dir, id)
	if e != nil {
		return e
	}
	all, e := roles.Load(effective.Files)
	if e != nil {
		return e
	}
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
	t.Areas = append(t.Areas, paths...)
	required, e := roles.Required(all, t, paths, "review")
	if e != nil {
		return e
	}
	roster, rosterReason := roles.ReviewRoster(required)
	evidence := &model.Evidence{Base: t.BaseSHA, Head: t.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Checks: checks, Reviews: map[string]string{}, ReviewRoster: roster, ReviewRosterReason: rosterReason, At: time.Now().UTC()}
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
	if e = c.updatePR(id, true); e != nil {
		return e
	}
	c.mirror(id)
	outcomes := c.runReviewAttempt(effective, t, dir, diff, evidence, 1, required)
	assessment := assessReviews(required, outcomes)
	if e = c.preserveReviewFindings(id, assessment.findings); e != nil {
		return e
	}
	for i, role := range required {
		if outcomes[i].result.Status == "completed" {
			evidence.Reviews[role.Name] = outcomes[i].result.Summary
		}
	}
	if e = c.publishReviewProgress(id, evidence); e != nil {
		return e
	}
	if e = c.reviewFollowups(t, assessment.findings); e != nil {
		return e
	}
	if assessment.blocking >= 0 {
		role := required[assessment.blocking]
		_ = c.P.DB.Event(id, t.RunID, role.Name, effective.Project.Provider, "review_finding_fix", outcomes[assessment.blocking].result.Summary)
		c.retry(id, role.Name, outcomes[assessment.blocking].result.Summary)
		return c.refreshDraftPR(id)
	}
	if assessment.human >= 0 {
		role := required[assessment.human]
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
		for _, index := range assessment.evidence {
			refreshRoles = append(refreshRoles, required[index])
			names = append(names, required[index].Name)
		}
		_ = c.P.DB.Event(id, t.RunID, "verification", "native", "review_evidence_refresh_requested", "roles="+strings.Join(names, ",")+" head="+t.HeadSHA)
		checks, checkErr := c.checks(c.ctx, effective, dir, id)
		if checkErr != nil {
			_ = c.P.DB.Event(id, t.RunID, "verification", "native", "review_evidence_refresh_failed", short(checkErr.Error(), 500))
			return checkErr
		}
		evidence.Checks = checks
		evidence.At = time.Now().UTC()
		if e = c.publishReviewProgress(id, evidence); e != nil {
			return e
		}
		refreshed := c.runReviewAttempt(effective, t, dir, diff, evidence, 2, refreshRoles)
		refreshAssessment := assessReviews(refreshRoles, refreshed)
		if e = c.preserveReviewFindings(id, refreshAssessment.findings); e != nil {
			return e
		}
		for i, role := range refreshRoles {
			if refreshed[i].result.Status == "completed" {
				evidence.Reviews[role.Name] = refreshed[i].result.Summary
			}
		}
		if e = c.publishReviewProgress(id, evidence); e != nil {
			return e
		}
		if e = c.reviewFollowups(t, refreshAssessment.findings); e != nil {
			return e
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
	if t == nil || t.Evidence == nil || t.Evidence.Config == "" || t.Evidence.Rules == "" || len(t.Evidence.Checks) == 0 {
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
		fmt.Fprintf(&b, "\nPolicy hash: `%s`\nRules hash: `%s`\n", e.Config, e.Rules)
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
