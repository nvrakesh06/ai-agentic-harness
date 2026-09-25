package engine

import (
	"context"
	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
)

// PreflightScopeForTest lets black-box integration tests construct a durable
// completed preflight without copying the scope-fingerprint implementation.
func PreflightScopeForTest(t *model.Task, effective config.Effective) string {
	return preflightScope(t, effective)
}

func AcquireForTest(c *Controller, ctx context.Context) error {
	c.ctx = ctx
	return c.acquire(ctx)
}
func SyncTaskForTest(c *Controller, ctx context.Context, id string) error {
	_, err := c.syncTask(ctx, id)
	return err
}
func CheckpointForTest(c *Controller, ctx context.Context, id string) error {
	return c.checkpoint(ctx, id)
}
func RecoveredCheckpointForTest(c *Controller, ctx context.Context, id string, result provider.Result) error {
	return c.recoveredCheckpoint(ctx, id, result)
}
