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
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

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
	if r.Name != "implementer" {
		select {
		case c.readers <- struct{}{}:
			defer func() { <-c.readers }()
		case <-ctx.Done():
			return provider.Result{}, ctx.Err()
		}
	}
	id := model.ID()
	taskID := ""
	if t != nil {
		taskID = t.ID
	}
	capability := r.Capability
	if configured := e.Project.Models[r.Name]; configured != "" {
		capability = configured
	}
	started := time.Now().UTC()
	if err := c.mutate(func(s *model.Snapshot) error {
		if t != nil {
			s.Tasks[t.ID].RunID = id
		}
		s.Runs = append(s.Runs, model.Run{ID: id, Task: taskID, Role: r.Name, Provider: e.Project.Provider, Capability: capability, Version: model.Version, RulesHash: roles.Hash(), Started: started, Epoch: s.Controller.Epoch, Outcome: "running"})
		return nil
	}); err != nil {
		return provider.Result{}, err
	}
	_ = c.P.DB.Event(taskID, id, r.Name, e.Project.Provider, "worker_start", capability)
	p := c.P.Provider
	if p.Name() != e.Project.Provider {
		p = provider.New(e.Project.Provider)
	}
	result, err := p.Run(ctx, provider.Request{Directory: dir, Runtime: filepath.Join(c.P.Dir, "sessions", id), Prompt: roles.Compile(e, r, runtime.GOOS, t, objective, diff, evidence), Role: r.Name, Model: e.Project.ProviderModels[capability], Write: r.Name == "implementer", Timeout: time.Duration(e.Project.WorkerSeconds) * time.Second})
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
			return nil
		})
		if saveErr != nil {
			return result, saveErr
		}
	}
	_ = c.P.DB.Event(taskID, id, r.Name, e.Project.Provider, "worker_exit", outcome)
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
func (c *Controller) ensureWorktree(id string) error {
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
			c.retry(id, "verification", e.Error())
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
	pre, e := roles.Required(all, t, t.Areas, "pre-implementation")
	if e != nil {
		c.block(id, "Correct required roles.", e.Error(), model.Ready)
		return false
	}
	if t.UI {
		pre = append(pre, all["designer"])
	}
	for _, r := range pre {
		result, e := c.role(c.ctx, effective, r, t, dir, "Provide pre-implementation guidance for the assigned task.", "", "")
		if e != nil {
			c.retry(id, "implementation", e.Error())
			return false
		}
		if result.Status == "blocked" {
			c.block(id, result.Question, result.Summary, model.Ready)
			return false
		}
		if result.Status != "completed" {
			c.retry(id, "implementation", r.Name+": "+result.Summary)
			return false
		}
		if r.Stage == "pre-implementation" && roles.Blocking(r, result.Findings) {
			c.block(id, "Resolve the pre-implementation specialist's blocking concerns.", result.Summary, model.Ready)
			return false
		}
		if e = c.mutate(func(s *model.Snapshot) error {
			s.Tasks[id].Decisions = append(s.Tasks[id].Decisions, r.Name+": "+result.Summary)
			return nil
		}); e != nil {
			return false
		}
	}
	t = c.Snapshot().Tasks[id]
	r, e := c.role(c.ctx, effective, all["implementer"], t, dir, t.Objective, "", "")
	if c.ctx.Err() != nil {
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
	_ = c.mutate(func(s *model.Snapshot) error {
		task := s.Tasks[id]
		if kind == "implementation" {
			task.Attempts = count
		} else {
			task.FixCycles[kind] = count
		}
		task.Findings = append(task.Findings, model.Finding{Severity: "high", Category: kind, Reason: reason, Role: kind})
		task.State = model.Fix
		return nil
	})
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
	var checked []string
	for _, check := range e.Project.Checks {
		applicable := len(check.Platforms) == 0
		for _, p := range check.Platforms {
			if p == runtime.GOOS {
				applicable = true
			}
		}
		if !applicable {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, time.Duration(check.Timeout)*time.Second)
		out, err := platform.Run(checkCtx, dir, cleanEnvironment(), "", check.Command[0], check.Command[1:]...)
		cancel()
		if err != nil {
			return checked, fmt.Errorf("%s failed: %w\n%s", check.Name, err, short(safety.Redact(out), 8000))
		}
		checked = append(checked, check.Name)
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
func (c *Controller) checks(ctx context.Context, e config.Effective, dir string) ([]string, error) {
	select {
	case c.readers <- struct{}{}:
		defer func() { <-c.readers }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return Verify(ctx, e, dir)
}
func (c *Controller) verifyReview(id string) error {
	effective, e := c.syncTask(c.ctx, id)
	if e != nil {
		return e
	}
	t := c.Snapshot().Tasks[id]
	dir := c.P.TaskPath(t)
	checks, e := c.checks(c.ctx, effective, dir)
	if e != nil {
		return e
	}
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
	if t.PR == 0 {
		pr, e := c.P.Hub.EnsurePR(c.ctx, t.Branch, "main", t.Title, c.prBody(t))
		if e != nil {
			return e
		}
		if e = c.mutate(func(s *model.Snapshot) error { s.Tasks[id].PR = pr; return nil }); e != nil {
			return e
		}
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
	evidence := &model.Evidence{Base: t.BaseSHA, Head: t.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Checks: checks, Reviews: map[string]string{}, At: time.Now().UTC()}
	if e = c.mutate(func(s *model.Snapshot) error {
		s.Tasks[id].State = model.Review
		s.Tasks[id].UI = t.UI
		s.Tasks[id].Security = t.Security
		s.Tasks[id].Findings = nil
		return nil
	}); e != nil {
		return e
	}
	for _, role := range required {
		serialized, _ := json.Marshal(evidence)
		result, e := c.role(c.ctx, effective, role, t, dir, t.Objective, diff, string(serialized))
		if e != nil {
			return e
		}
		if result.Status == "blocked" {
			c.block(id, result.Question, result.Summary, model.SyncRequired)
			return nil
		}
		if result.Status != "completed" {
			return fmt.Errorf("%s did not complete: %s", role.Name, result.Summary)
		}
		for i := range result.Findings {
			result.Findings[i].Role = role.Name
		}
		if e = c.mutate(func(s *model.Snapshot) error {
			s.Tasks[id].Findings = append(s.Tasks[id].Findings, result.Findings...)
			return nil
		}); e != nil {
			return e
		}
		if roles.Blocking(role, result.Findings) {
			c.retry(id, role.Name, result.Summary)
			return nil
		}
		evidence.Reviews[role.Name] = result.Summary
		for _, f := range result.Findings {
			if f.Severity == "medium" {
				key := fmt.Sprintf("%s-followup-%x", id, sha256.Sum256([]byte(f.Role+f.Location+f.Reason)))
				if _, e = c.P.Hub.EnsureIssue(c.ctx, key, "Follow-up: "+short(f.Reason, 90), fmt.Sprintf("From #%d\n\n%s\n\n%s", t.Issue, f.Reason, f.Resolution)); e != nil {
					return e
				}
			}
		}
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
	if e = c.P.Hub.UpdatePR(c.ctx, t.PR, c.prBody(t)); e != nil {
		return e
	}
	c.mirror(id)
	return nil
}
func (c *Controller) prBody(t *model.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nCloses #%d\n\n%s\n\nAcceptance criteria:\n", github.Marker(t.ID), t.Issue, t.Objective)
	for _, a := range t.Acceptance {
		mark := " "
		if t.Evidence != nil && t.Evidence.Reviews["qa"] != "" {
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
		fmt.Fprintf(&b, "\nVerification (base `%s`, head `%s`):\n", e.Base, e.Head)
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
	b.WriteString("\nRisks: inspect recorded findings and acceptance evidence.\nRollback: propose and verify a revert of the integration commit; no automatic production rollback.\n")
	return b.String()
}
