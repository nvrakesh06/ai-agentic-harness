package engine

import (
	"errors"
	"slices"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

func preflightMatches(p *model.Preflight, t *model.Task, effective config.Effective) bool {
	return p != nil && p.BaseSHA == effective.BaseSHA && p.HeadSHA == t.HeadSHA &&
		p.Config == effective.Hash && p.Rules == roles.Hash()
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
			task.Preflight = &model.Preflight{Phase: "queued", BaseSHA: effective.BaseSHA, HeadSHA: task.HeadSHA, Config: effective.Hash, Rules: roles.Hash()}
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
		if slices.Contains(t.Preflight.Completed, r.Name) {
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
			c.block(id, result.Question, result.Summary, model.Ready)
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
		if !slices.Contains(t.Preflight.Completed, role.Name) {
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
	return true, c.mutate(func(s *model.Snapshot) error {
		task := s.Tasks[id]
		if task == nil || task.Preflight == nil || task.Preflight.Phase != "ready" {
			return errors.New("preflight changed during writer admission")
		}
		if err := model.Transition(task, model.Running); err != nil {
			return err
		}
		task.Preflight.Phase = "writing"
		return nil
	})
}
