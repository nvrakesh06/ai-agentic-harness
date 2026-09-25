package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
	"path/filepath"
)

func (c *Controller) integrate(id string) {
	t := c.Snapshot().Tasks[id]
	if t == nil {
		return
	}
	if t.State == model.PostVerify {
		c.postVerify(id)
		return
	}
	if !dependenciesComplete(c.Snapshot(), t) {
		if c.mutate(func(s *model.Snapshot) error { s.Tasks[id].State = model.SyncRequired; return nil }) == nil {
			_ = c.updatePR(id, true)
			c.mirror(id)
		}
		return
	}
	effective, e := c.effective(c.ctx)
	if e != nil {
		c.block(id, "Restore access before integration.", e.Error(), model.SyncRequired)
		return
	}
	base := effective.BaseSHA
	rosterAccepted, reviewReason, rosterErr := c.reviewEvidenceAccepted(c.ctx, effective, t)
	if rosterErr != nil {
		c.block(id, "Restore canonical review state before integration.", rosterErr.Error(), model.SyncRequired)
		return
	}
	if t.Evidence == nil || t.Evidence.Base != base || t.Evidence.Head != t.HeadSHA || t.Evidence.Config != effective.Hash || t.Evidence.Rules != roles.Hash() || !rosterAccepted {
		if reviewReason == "" {
			reviewReason = "base, head, configuration, or rules hash no longer matches exact-head review evidence"
		}
		_ = c.P.DB.Event(id, t.RunID, "verification", "native", "review_evidence_invalidated", reviewReason)
		if e = c.verifyReview(id); e != nil {
			c.handleVerificationError(id, e)
			return
		}
		t = c.Snapshot().Tasks[id]
		if t.State != model.MergeReady {
			return
		}
		effective, e = c.effective(c.ctx)
		if e != nil {
			c.fail(e)
			return
		}
		base = t.BaseSHA
		rosterAccepted, reviewReason, rosterErr = c.reviewEvidenceAccepted(c.ctx, effective, t)
		if rosterErr != nil || effective.BaseSHA != base || effective.Hash != t.Evidence.Config || !rosterAccepted {
			if reviewReason != "" {
				_ = c.P.DB.Event(id, t.RunID, "verification", "native", "review_evidence_invalidated", reviewReason)
			}
			if c.mutate(func(s *model.Snapshot) error { s.Tasks[id].State = model.SyncRequired; return nil }) == nil {
				_ = c.updatePR(id, true)
				c.mirror(id)
			}
			return
		}
	}
	if e = c.validateTaskScope(t); e != nil {
		c.block(id, "Correct or replan the immutable task-area assignment before integration.", e.Error(), model.SyncRequired)
		return
	}
	pr, e := c.P.Hub.Pull(c.ctx, t.PR)
	if e != nil {
		c.block(id, "Check PR access and retry.", e.Error(), model.SyncRequired)
		return
	}
	if pr.State != "open" || pr.Merged || pr.Draft || pr.Head.SHA != t.HeadSHA || pr.Base.Ref != "main" {
		_ = c.updatePR(id, true)
		c.block(id, "The PR changed outside AIH. Reconcile it before retrying.", "PR state/head/base/readiness differs from verified evidence", model.SyncRequired)
		return
	}
	merge, e := c.P.Git.MergeCommit(c.ctx, base, t.HeadSHA, fmt.Sprintf("Merge AIH task #%d: %s", t.Issue, t.Title))
	if e != nil {
		c.retry(id, "verification", e.Error())
		return
	}
	dir := filepath.Join(c.P.Dir, "integration", model.ID())
	if e = c.P.Git.Detached(c.ctx, dir, merge); e != nil {
		c.fail(e)
		return
	}
	defer c.P.Git.RemoveWorktree(context.Background(), dir)
	if e = c.mutate(func(s *model.Snapshot) error { return model.Transition(s.Tasks[id], model.MergeTrain) }); e != nil {
		return
	}
	plan, e := fullValidationPlan(c.ctx, effective, dir, merge, "exact integrated merge-train head")
	if e != nil {
		// Merge-train checks run on the candidate merge, which is exactly where
		// an owned-but-unmerged sibling defect can surface. Give the bounded
		// canonical-base reproduction route the same chance it has during the
		// ordinary verification pass before falling back to a local FIX retry.
		if failure, ok := nativeCheckFailure(e); ok {
			c.verificationFailure(id, failure)
			return
		}
		c.retry(id, "verification", e.Error())
		return
	}
	checks, e := c.checksForPlan(c.ctx, dir, id, plan)
	if e != nil {
		c.retry(id, "verification", fmt.Sprintf("exact integrated merge head %s: %v", merge, e))
		return
	}
	// The task ref is advanced to the actual merge commit as well. This is an
	// intentional non-no-op guard: git push omits unchanged refs, so merely
	// including HEAD:task would not protect the reviewed task head.
	e = c.save(c.ctx, func(s *model.Snapshot) error {
		task := s.Tasks[id]
		if task.HeadSHA != t.HeadSHA || task.Evidence == nil {
			return fmt.Errorf("task changed during integration")
		}
		task.MergeSHA = merge
		task.HeadSHA = merge
		if err := applyValidationEvidence(task.Evidence, plan, checks); err != nil {
			return err
		}
		task.Evidence.IntegrationSHA = merge
		task.Evidence.IntegrationOwner = c.owner
		return model.Transition(task, model.PostVerify)
	}, gitx.Update{Branch: "main", Old: base, New: merge}, gitx.Update{Branch: t.Branch, Old: t.HeadSHA, New: merge})
	if e != nil {
		remote, h, re := c.P.Git.Load(c.ctx)
		if re != nil {
			c.fail(e)
			return
		}
		c.mu.Lock()
		owned := h == c.head && remote.Controller.Owner == c.owner
		c.mu.Unlock()
		if !owned {
			c.fail(fmt.Errorf("integration lost controller authority: %w", e))
			return
		}
		newMain, re := c.P.Git.RemoteHead(c.ctx, "main")
		if re == nil && newMain != base {
			if c.mutate(func(s *model.Snapshot) error {
				task := s.Tasks[id]
				task.FixCycles["integration_races"]++
				if task.FixCycles["integration_races"] > 5 {
					model.Block(task, "Main changed repeatedly during final verification. Retry when ready.", e.Error(), model.SyncRequired)
				} else {
					task.State = model.SyncRequired
					task.Evidence = nil
				}
				return nil
			}) == nil {
				_ = c.updatePR(id, true)
				c.mirror(id)
			}
			return
		}
		c.block(id, "Atomic integration was rejected. Check repository permissions/atomic support.", e.Error(), model.SyncRequired)
		return
	}
	c.postVerify(id)
}

func nativeCheckFailure(err error) (*checkFailure, bool) {
	var failure *checkFailure
	if !errors.As(err, &failure) {
		return nil, false
	}
	return failure, true
}
func (c *Controller) postVerify(id string) {
	t := c.Snapshot().Tasks[id]
	effective, e := c.effective(c.ctx)
	if e != nil {
		c.fail(e)
		return
	}
	dir := filepath.Join(c.P.Dir, "post-verify", model.ID())
	target := t.MergeSHA
	recovering := t.RecoveryRequired || c.Snapshot().IntegrationBlocked == id
	if recovering && effective.BaseSHA != target {
		if !c.P.Git.Ancestor(c.ctx, target, effective.BaseSHA) {
			c.block(id, "Main history no longer contains the recorded integration; reconcile manually.", "Refusing to accept a rewritten main as recovery.", model.PostVerify)
			return
		}
		target = effective.BaseSHA
	}
	plan, planErr := fullValidationPlan(c.ctx, effective, c.P.Root, target, "exact integrated merge-train head")
	reused := planErr == nil && !recovering && effective.BaseSHA == target && t.Evidence != nil &&
		t.Evidence.IntegrationSHA == target && t.Evidence.IntegrationOwner == c.owner &&
		t.Evidence.Config == effective.Hash && t.Evidence.Rules == roles.Hash() && t.Evidence.ValidationGate == "full" && t.Evidence.ValidationInput == plan.Input && len(t.Evidence.Checks) > 0
	if !reused {
		if e = c.P.Git.Detached(c.ctx, dir, target); e != nil {
			c.fail(e)
			return
		}
		defer c.P.Git.RemoveWorktree(context.Background(), dir)
		plan, e = fullValidationPlan(c.ctx, effective, dir, target, "exact integrated merge-train head")
		var checks []string
		if e == nil {
			checks, e = c.checksForPlan(c.ctx, dir, id, plan)
		}
		if c.ctx.Err() != nil {
			return
		}
		if e != nil {
			_ = c.mutate(func(s *model.Snapshot) error {
				s.IntegrationBlocked = id
				s.Tasks[id].RecoveryRequired = true
				model.Block(s.Tasks[id], "Post-merge verification failed. Repair or authorize a revert, then answer to recheck.", c.portable(e.Error()), model.PostVerify)
				return nil
			})
			c.mirror(id)
			return
		}
		if e = c.mutate(func(s *model.Snapshot) error {
			return applyValidationEvidence(s.Tasks[id].Evidence, plan, checks)
		}); e != nil {
			return
		}
	}
	if recovering && target != t.MergeSHA {
		result, err := c.roleAtRef(c.ctx, effective, roles.Builtins()["qa"], t, dir, "Verify acceptance criteria after the human-directed repair/revert on main.", "", "Native checks passed on "+target, target)
		if err != nil {
			c.block(id, "Recovery QA could not run; restore access and retry.", err.Error(), model.PostVerify)
			return
		}
		if result.Status != "completed" || roles.Blocking(roles.Builtins()["qa"], result.Findings) {
			_ = c.mutate(func(s *model.Snapshot) error {
				s.IntegrationBlocked = ""
				task := s.Tasks[id]
				task.RecoveryRequired = true
				task.PostVerifySHA = target
				task.Findings = append(task.Findings, result.Findings...)
				model.Block(task, "Main is healthy but acceptance remains unresolved. Submit a repair objective, then answer this task after the repair merges.", c.portable(result.Summary), model.PostVerify)
				return nil
			})
			c.mirror(id)
			return
		}
	}
	// Keep the durable task in POST_VERIFY until its local-only scratch has
	// been removed. A transient cleanup failure is retried on the next pass and
	// cannot turn a successfully integrated task into a failed worker task.
	if e = c.P.RemoveTaskScratch(t); e != nil {
		_ = c.P.DB.Event(id, "", "", "", "scratch_cleanup_failed", safety.Redact(e.Error()))
		return
	}
	if e = c.mutate(func(s *model.Snapshot) error {
		if s.IntegrationBlocked == id {
			s.IntegrationBlocked = ""
		}
		s.Tasks[id].PostVerifySHA = target
		s.Tasks[id].RecoveryRequired = false
		return model.Transition(s.Tasks[id], model.Done)
	}); e != nil {
		return
	}
	c.mirror(id)
	t = c.Snapshot().Tasks[id]
	verification := "Post-merge verification passed on"
	if reused {
		verification = "Exact-SHA integration verification reused on"
	}
	_ = c.P.Hub.UpdatePR(c.ctx, t.PR, c.prBody(t)+"\nIntegration commit: `"+t.MergeSHA+"`\n"+verification+": `"+target+"`\n")
	s := c.Snapshot()
	complete := true
	for _, other := range s.Tasks {
		if other.ObjectiveID == t.ObjectiveID && other.State != model.Done {
			complete = false
		}
	}
	if complete {
		if o := s.Objectives[t.ObjectiveID]; o != nil {
			_ = c.P.Hub.UpdateIssue(c.ctx, o.Issue, githubObjectiveBody(o), true)
		}
	}
}
func githubObjectiveBody(o *model.Objective) string {
	return "<!-- aih:" + o.ID + " -->\n\n" + o.Text + "\n\nAll AIH tasks completed with post-merge verification.\n"
}
