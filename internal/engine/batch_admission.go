package engine

import (
	"context"
	"sort"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

// selectBatchAdmission collects immutable changed paths and filters candidates
// against the controller's current canonical identity before delegating the
// portable conflict decision to model. It is intentionally not wired into the
// merge loop yet: this slice reserves no integration behavior.
func (c *Controller) selectBatchAdmission(ctx context.Context, effective config.Effective, snapshot *model.Snapshot) (*model.IntegrationBatch, error) {
	if snapshot == nil {
		return nil, nil
	}
	if snapshot.IntegrationBatch != nil {
		return c.reconcileBatchAdmission(ctx, effective, snapshot)
	}
	eligible := model.Clone(snapshot)
	paths := make(map[string][]string)
	for _, task := range model.Ordered(eligible) {
		if !batchAdmissionMatchesRuntime(task, effective) {
			task.State = model.SyncRequired
			continue
		}
		accepted, _, err := c.reviewEvidenceAccepted(ctx, effective, task)
		if err != nil {
			return nil, err
		}
		if !accepted {
			task.State = model.SyncRequired
			continue
		}
		_, changed, err := c.P.Git.Diff(ctx, task.BaseSHA, task.HeadSHA)
		if err != nil {
			return nil, err
		}
		if !c.batchValidationCurrent(ctx, effective, task, changed) {
			task.State = model.SyncRequired
			continue
		}
		paths[task.ID] = changed
	}
	return model.SelectIntegrationBatch(eligible, paths), nil
}

// reconcileBatchAdmission makes a durable reservation safe across attach or
// takeover. Every member is rechecked against current canonical identity,
// canonical review policy, and the actual base..head path set. An invalid
// reservation is cleared only if it is still the same durable manifest.
func (c *Controller) reconcileBatchAdmission(ctx context.Context, effective config.Effective, snapshot *model.Snapshot) (*model.IntegrationBatch, error) {
	batch := snapshot.IntegrationBatch
	if batch == nil {
		return nil, nil
	}
	current, err := c.batchReservationCurrent(ctx, effective, snapshot, batch)
	if err != nil {
		return nil, err
	}
	if current {
		return model.Clone(snapshot).IntegrationBatch, nil
	}
	staleID := batch.ID
	if err := c.mutate(func(next *model.Snapshot) error {
		if next.IntegrationBatch != nil && next.IntegrationBatch.ID == staleID {
			next.IntegrationBatch = nil
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// Do not leave an avoidable scheduler tick between clearing a stale
	// reservation and considering the now-current candidate set.
	return c.selectBatchAdmission(ctx, effective, c.Snapshot())
}

func (c *Controller) batchReservationCurrent(ctx context.Context, effective config.Effective, snapshot *model.Snapshot, batch *model.IntegrationBatch) (bool, error) {
	if !batchReservationMatchesRuntime(batch, effective) || model.ValidateIntegrationBatch(snapshot, batch) != nil {
		return false, nil
	}
	for _, member := range batch.Tasks {
		task := snapshot.Tasks[member.ID]
		if !batchAdmissionMatchesRuntime(task, effective) {
			return false, nil
		}
		accepted, _, err := c.reviewEvidenceAccepted(ctx, effective, task)
		if err != nil {
			return false, err
		}
		if !accepted {
			return false, nil
		}
		_, paths, err := c.P.Git.Diff(ctx, task.BaseSHA, task.HeadSHA)
		if err != nil {
			return false, err
		}
		if !sameBatchPaths(member.Paths, paths) {
			return false, nil
		}
		if !c.batchValidationCurrent(ctx, effective, task, paths) {
			return false, nil
		}
	}
	return true, nil
}

// batchValidationCurrent recomputes the policy-derived plan at the exact task
// head. It is shared by fresh admission and durable-reservation recovery so a
// changed toolchain, configuration, or gate never reaches batch selection.
func (c *Controller) batchValidationCurrent(ctx context.Context, effective config.Effective, task *model.Task, paths []string) bool {
	plan, err := c.taskValidationPlan(ctx, effective, task, c.P.TaskPath(task), paths)
	return err == nil && validationPlanMatchesEvidence(plan, task.Evidence)
}

func validationPlanMatchesEvidence(plan validationPlan, evidence *model.Evidence) bool {
	return evidence != nil && evidence.ValidationGate == plan.Gate && evidence.ValidationInput == plan.Input &&
		evidence.Toolchain == plan.Toolchain && evidence.TestInputs == plan.TestInputs
}

func batchReservationMatchesRuntime(batch *model.IntegrationBatch, effective config.Effective) bool {
	return batch != nil && batch.BaseSHA == effective.BaseSHA && batch.Config == effective.Hash && batch.Rules == roles.Hash()
}

func sameBatchPaths(expected, actual []string) bool {
	if len(expected) != len(actual) {
		return false
	}
	copy := append([]string(nil), actual...)
	sort.Strings(copy)
	for i := range expected {
		if expected[i] != copy[i] {
			return false
		}
	}
	return true
}

// batchAdmissionMatchesRuntime prevents a previously valid MERGE_READY task
// from being reserved after the canonical base, configuration, or rules moved.
func batchAdmissionMatchesRuntime(task *model.Task, effective config.Effective) bool {
	return task != nil && task.State == model.MergeReady && task.Evidence != nil &&
		task.BaseSHA == effective.BaseSHA && task.Evidence.Base == effective.BaseSHA &&
		task.Evidence.Head == task.HeadSHA && task.Evidence.Config == effective.Hash &&
		task.Evidence.Rules == roles.Hash()
}
