package engine

import (
	"fmt"
	"path/filepath"

	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

// reserveBatchIntegration makes a selected admission durable before any
// candidate objects are created. A singleton deliberately leaves serial
// integration available immediately.
func (c *Controller) reserveBatchIntegration() (*model.IntegrationBatch, error) {
	effective, err := c.effective(c.ctx)
	if err != nil {
		return nil, err
	}
	batch, err := c.selectBatchAdmission(c.ctx, effective, c.Snapshot())
	if err != nil || batch == nil {
		return batch, err
	}
	if c.Snapshot().IntegrationBatch == nil {
		if err = c.mutate(func(s *model.Snapshot) error { return model.ReserveIntegrationBatch(s, batch) }); err != nil {
			return nil, err
		}
	}
	return c.Snapshot().IntegrationBatch, nil
}

// failBatch breaks deterministic re-admission after a merge conflict, failed
// integrated check, or fenced publication rejection. The lexical first member
// must refresh from main; the remaining members can proceed through the normal
// serial path on the next scheduler tick.
func (c *Controller) failBatch(id, diagnostic string) {
	_ = c.mutate(func(s *model.Snapshot) error {
		if s.IntegrationBatch == nil || s.IntegrationBatch.ID != id {
			return nil
		}
		members := s.IntegrationBatch.Tasks
		s.IntegrationBatch = nil
		if len(members) != 0 {
			if task := s.Tasks[members[0].ID]; task != nil && task.State == model.MergeReady && task.HeadSHA == members[0].HeadSHA {
				task.State = model.SyncRequired
				task.Evidence = nil
				task.Summary = "Batch integration deferred: " + diagnostic
			}
		}
		return nil
	})
}

// batchCandidateCount is deliberately cheap. It avoids running Git diffs,
// policy plans, and review reconciliation every scheduler tick when only a
// singleton can possibly be admitted.
func batchCandidateCount(snapshot *model.Snapshot) int {
	count := 0
	for _, task := range model.Ordered(snapshot) {
		if task.State == model.MergeReady && task.Risk == "low" && task.Evidence != nil {
			count++
		}
	}
	return count
}

// integrateBatch validates the durable admission again immediately before it
// builds a candidate. It never advances a source ref until all members and the
// new state snapshot can be published in one fenced transaction.
func (c *Controller) integrateBatch(id string) {
	snapshot := c.Snapshot()
	batch := snapshot.IntegrationBatch
	if batch == nil || batch.ID != id {
		return
	}
	effective, err := c.effective(c.ctx)
	if err != nil {
		c.failBatch(id, "canonical integration identity could not be read")
		return
	}
	current, err := c.batchReservationCurrent(c.ctx, effective, snapshot, batch)
	if err != nil || !current {
		c.failBatch(id, "reserved members are no longer current")
		return
	}
	heads := make([]string, 0, len(batch.Tasks))
	for _, member := range batch.Tasks {
		task := snapshot.Tasks[member.ID]
		if task == nil || c.validateTaskScope(task) != nil {
			c.failBatch(id, "member scope is no longer valid")
			return
		}
		pr, pullErr := c.P.Hub.Pull(c.ctx, task.PR)
		if pullErr != nil || pr.State != "open" || pr.Merged || pr.Draft || pr.Base.Ref != "main" || pr.Head.SHA != member.HeadSHA {
			c.failBatch(id, "member pull request is no longer exact")
			return
		}
		heads = append(heads, member.HeadSHA)
	}
	merge, err := c.P.Git.MergeHeads(c.ctx, batch.BaseSHA, heads, "Merge AIH batch "+batch.ID)
	if err != nil {
		c.failBatch(id, "combined tree could not be verified")
		return
	}
	dir := filepath.Join(c.P.Dir, "integration", model.ID())
	if err = c.P.Git.Detached(c.ctx, dir, merge); err != nil {
		c.failBatch(id, "candidate checkout could not be created")
		return
	}
	defer c.P.Git.RemoveWorktree(c.ctx, dir)
	plan, err := fullValidationPlan(c.ctx, effective, dir, merge, "exact integrated merge-train head")
	if err == nil {
		checks, checkErr := c.checksForPlan(c.ctx, dir, batch.Tasks[0].ID, plan)
		err = checkErr
		if err == nil {
			err = c.publishBatch(batch, merge, plan, checks)
		}
	}
	if err != nil {
		c.failBatch(id, "integrated validation or fenced publication failed")
		return
	}
	// The publish stored full exact-SHA evidence for every member, so these
	// calls complete cleanup and PR closure without repeating native checks.
	for _, member := range batch.Tasks {
		c.postVerify(member.ID)
	}
}

func (c *Controller) publishBatch(batch *model.IntegrationBatch, merge string, plan validationPlan, checks []string) error {
	updates := make([]gitx.Update, 0, len(batch.Tasks)+1)
	updates = append(updates, gitx.Update{Branch: "main", Old: batch.BaseSHA, New: merge})
	for _, member := range batch.Tasks {
		task := c.Snapshot().Tasks[member.ID]
		if task == nil {
			return fmt.Errorf("batch task %s disappeared", member.ID)
		}
		updates = append(updates, gitx.Update{Branch: task.Branch, Old: member.HeadSHA, New: merge})
	}
	return c.save(c.ctx, func(s *model.Snapshot) error {
		if s.IntegrationBatch == nil || s.IntegrationBatch.ID != batch.ID {
			return fmt.Errorf("batch reservation changed during integration")
		}
		for _, member := range batch.Tasks {
			task := s.Tasks[member.ID]
			if task == nil || task.State != model.MergeReady || task.HeadSHA != member.HeadSHA || task.Evidence == nil {
				return fmt.Errorf("batch task %s changed during integration", member.ID)
			}
			task.MergeSHA = merge
			task.HeadSHA = merge
			if err := applyValidationEvidence(task.Evidence, plan, checks); err != nil {
				return err
			}
			task.Evidence.IntegrationSHA = merge
			task.Evidence.IntegrationOwner = c.owner
			if err := model.Transition(task, model.PostVerify); err != nil {
				return err
			}
		}
		// ValidateIntegrationBatch requires MERGE_READY members. Clear the
		// reservation in this same publication that changes all member states.
		s.IntegrationBatch = nil
		return nil
	}, updates...)
}
