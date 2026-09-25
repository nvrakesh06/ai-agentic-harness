package engine

import (
	"context"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

// selectBatchAdmission collects immutable changed paths and filters candidates
// against the controller's current canonical identity before delegating the
// portable conflict decision to model. It is intentionally not wired into the
// merge loop yet: this slice reserves no integration behavior.
func (c *Controller) selectBatchAdmission(ctx context.Context, effective config.Effective, snapshot *model.Snapshot) (*model.IntegrationBatch, error) {
	if snapshot == nil || snapshot.IntegrationBatch != nil {
		return nil, nil
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
		paths[task.ID] = changed
	}
	return model.SelectIntegrationBatch(eligible, paths), nil
}

// batchAdmissionMatchesRuntime prevents a previously valid MERGE_READY task
// from being reserved after the canonical base, configuration, or rules moved.
func batchAdmissionMatchesRuntime(task *model.Task, effective config.Effective) bool {
	return task != nil && task.State == model.MergeReady && task.Evidence != nil &&
		task.BaseSHA == effective.BaseSHA && task.Evidence.Base == effective.BaseSHA &&
		task.Evidence.Head == task.HeadSHA && task.Evidence.Config == effective.Hash &&
		task.Evidence.Rules == roles.Hash()
}
