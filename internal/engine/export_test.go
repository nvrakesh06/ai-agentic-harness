package engine

import (
	"context"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

func ExportAcquire(c *Controller, ctx context.Context) error {
	c.ctx, c.cancel = context.WithCancel(ctx)
	return c.acquire(ctx)
}

func ExportReserveBatch(c *Controller) (*model.IntegrationBatch, error) {
	return c.reserveBatchIntegration()
}
func ExportIntegrateBatch(c *Controller, id string)                    { c.integrateBatch(id) }
func ExportMutate(c *Controller, fn func(*model.Snapshot) error) error { return c.save(c.ctx, fn) }
func PreflightScopeForTest(t *model.Task, e config.Effective) string   { return preflightScope(t, e) }

func ExportAcceptedEvidence(c *Controller, task *model.Task, paths []string) (*model.Evidence, error) {
	effective, err := c.effective(c.ctx)
	if err != nil {
		return nil, err
	}
	plan, err := c.taskValidationPlan(c.ctx, effective, task, c.P.TaskPath(task), paths)
	if err != nil {
		return nil, err
	}
	all, err := roles.Load(effective.Files)
	if err != nil {
		return nil, err
	}
	required, err := roles.Required(all, task, paths, "review")
	if err != nil {
		return nil, err
	}
	roster, reason := roles.ReviewRoster(required)
	scope := reviewScope(task, paths, roster)
	dispositions := map[string]model.ReviewDisposition{}
	for _, role := range required {
		dispositions[role.Name] = completedDisposition(task, role, roleRuntime(effective, role))
	}
	return &model.Evidence{Base: task.BaseSHA, Head: task.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Checks: []string{passedCheckEvidence(plan.Checks[0], "")}, ValidationGate: plan.Gate, ValidationReason: plan.Reason, ValidationInput: plan.Input, Toolchain: plan.Toolchain, TestInputs: plan.TestInputs, ReviewRoster: roster, ReviewRosterReason: reason, ReviewScope: scope, ReviewDispositions: dispositions, Reviews: map[string]string{}, At: time.Now().UTC()}, nil
}
