package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

// providerAdmissionHeldError is returned before a provider call when a durable
// provider-owned failure already fences the same request scope. It is distinct
// from a task failure: callers must preserve their pre-provider checkpoint and
// never route it through source, Advisor, planning, or reader retry budgets.
type providerAdmissionHeldError struct {
	hold  model.ProviderAdmissionHold
	cause error
}

func (e *providerAdmissionHeldError) Error() string {
	if e == nil || e.cause == nil {
		return "provider admission is held"
	}
	return e.cause.Error()
}

func (e *providerAdmissionHeldError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func isProviderAdmissionHeld(err error) bool {
	_, held := providerAdmissionHeldErrorFor(err)
	return held
}

func providerAdmissionHeldErrorFor(err error) (*providerAdmissionHeldError, bool) {
	var held *providerAdmissionHeldError
	if !errors.As(err, &held) {
		return nil, false
	}
	return held, true
}

func providerAdmissionFailure(effective config.Effective, resolved config.ModelResolution, err error) (model.ProviderAdmissionHold, bool) {
	if code, ok := provider.RequestRejection(err); ok && code == provider.RejectionInvalidJSONSchema {
		return model.ProviderAdmissionHold{
			Provider: effective.Project.Provider, Class: model.ProviderAdmissionRequestRejected, Rejection: model.ProviderRejectionInvalidJSONSchema,
			SchemaSHA256: provider.SchemaSHA256(), OriginPolicy: effective.Hash, OriginRules: rolesHash(), OriginModel: resolved.EffectiveModel,
		}, true
	}
	if provider.IsAuthenticationFailure(err) {
		return model.ProviderAdmissionHold{
			Provider: effective.Project.Provider, Class: model.ProviderAdmissionAuthentication,
			OriginPolicy: effective.Hash, OriginRules: rolesHash(), OriginModel: resolved.EffectiveModel,
		}, true
	}
	return model.ProviderAdmissionHold{}, false
}

// rolesHash keeps the admission record's origin provenance together without
// making policy/model changes part of its suppression scope.
func rolesHash() string { return roles.Hash() }

func providerAdmissionHold(s *model.Snapshot, effective config.Effective) (model.ProviderAdmissionHold, bool) {
	return providerAdmissionHoldForSchema(s, effective, provider.SchemaSHA256())
}

func providerAdmissionHoldForSchema(s *model.Snapshot, effective config.Effective, schemaSHA256 string) (model.ProviderAdmissionHold, bool) {
	if s == nil || len(s.ProviderAdmissionHolds) == 0 {
		return model.ProviderAdmissionHold{}, false
	}
	for _, candidate := range []model.ProviderAdmissionHold{
		{Provider: effective.Project.Provider, Class: model.ProviderAdmissionSaturated},
		{Provider: effective.Project.Provider, Class: model.ProviderAdmissionAuthentication},
		{Provider: effective.Project.Provider, Class: model.ProviderAdmissionRequestRejected, Rejection: model.ProviderRejectionInvalidJSONSchema, SchemaSHA256: schemaSHA256},
	} {
		key, err := candidate.ScopeKey()
		if err != nil {
			continue
		}
		if hold, ok := s.ProviderAdmissionHolds[key]; ok {
			return hold, true
		}
	}
	return model.ProviderAdmissionHold{}, false
}

func (c *Controller) providerAdmissionHeld(effective config.Effective) (model.ProviderAdmissionHold, bool) {
	return providerAdmissionHold(c.Snapshot(), effective)
}

func (c *Controller) providerAdmissionGate(effective config.Effective, task *model.Task) error {
	hold, held := c.providerAdmissionHeld(effective)
	if !held {
		return nil
	}
	if task != nil {
		if err := c.mutate(func(s *model.Snapshot) error {
			providerAdmissionCheckpoint(s.Tasks[task.ID])
			return nil
		}); err != nil {
			return err
		}
	}
	return &providerAdmissionHeldError{hold: hold, cause: errors.New(providerAdmissionMessage(hold))}
}

// providerAdmissionActive resolves canonical configuration only while a
// durable hold exists. The scheduler uses it to avoid launching provider work;
// native verification and integration keep their ordinary paths.
func (c *Controller) providerAdmissionActive() bool {
	if len(c.Snapshot().ProviderAdmissionHolds) == 0 {
		return false
	}
	effective, err := c.effective(c.ctx)
	if err != nil {
		// A pre-existing hold must not become an admission permit merely because
		// canonical configuration cannot be resolved during recovery.
		return true
	}
	_, held := c.providerAdmissionHeld(effective)
	return held
}

// recordProviderAdmissionHold changes only portable provider admission state.
// Six exact records leave two non-evicting saturation slots, one for each
// supported provider. A saturation record is deliberately broader than a
// discarded exact hold: capacity pressure must fail closed.
func recordProviderAdmissionHold(s *model.Snapshot, hold model.ProviderAdmissionHold) (model.ProviderAdmissionHold, error) {
	if s.ProviderAdmissionHolds == nil {
		s.ProviderAdmissionHolds = map[string]model.ProviderAdmissionHold{}
	}
	key, err := hold.ScopeKey()
	if err != nil {
		return model.ProviderAdmissionHold{}, err
	}
	if current, ok := s.ProviderAdmissionHolds[key]; ok {
		return current, nil
	}
	exact := 0
	for _, current := range s.ProviderAdmissionHolds {
		if current.Class != model.ProviderAdmissionSaturated {
			exact++
		}
	}
	if hold.Class != model.ProviderAdmissionSaturated && exact >= model.MaxExactProviderAdmissionHolds {
		saturation := model.ProviderAdmissionHold{Provider: hold.Provider, Class: model.ProviderAdmissionSaturated, OriginPolicy: hold.OriginPolicy, OriginRules: hold.OriginRules, OriginModel: hold.OriginModel}
		saturationKey, saturationErr := saturation.ScopeKey()
		if saturationErr != nil {
			return model.ProviderAdmissionHold{}, saturationErr
		}
		if current, ok := s.ProviderAdmissionHolds[saturationKey]; ok {
			return current, nil
		}
		if len(s.ProviderAdmissionHolds) >= model.MaxProviderAdmissionHolds {
			return model.ProviderAdmissionHold{}, errors.New("provider admission hold capacity lacks reserved saturation slot")
		}
		s.ProviderAdmissionHolds[saturationKey] = saturation
		return saturation, nil
	}
	if len(s.ProviderAdmissionHolds) >= model.MaxProviderAdmissionHolds {
		return model.ProviderAdmissionHold{}, errors.New("provider admission hold capacity exhausted")
	}
	s.ProviderAdmissionHolds[key] = hold
	return hold, nil
}

func providerAdmissionCheckpoint(task *model.Task) {
	if task == nil {
		return
	}
	if task.State == model.Running {
		// Running is an admission reservation, not proof that a provider made
		// source changes. Preserve existing bounded FIX evidence when present.
		if task.Attempts != 0 || len(task.FixCycles) != 0 {
			task.State = model.Fix
		} else {
			task.State = model.Ready
		}
	}
	if task.Preflight != nil && (task.Preflight.Phase == "waiting" || task.Preflight.Phase == "running" || task.Preflight.Phase == "writing") {
		task.Preflight.Phase = "queued"
	}
}

func providerAdmissionMessage(hold model.ProviderAdmissionHold) string {
	if hold.Class == model.ProviderAdmissionSaturated {
		return "provider admission hold set is saturated"
	}
	if hold.Rejection != "" {
		return fmt.Sprintf("provider admission held: %s", hold.Rejection)
	}
	return fmt.Sprintf("provider admission held: %s", hold.Class)
}

var errProviderAdmissionProbeWaiting = errors.New("provider admission probe waits for active provider runs")

// providerAdmissionRetryTarget confirms that the operator selected the exact
// currently active hold. A historical schema hold whose canonical schema has
// changed is intentionally not releasable: it is already inactive without any
// deletion, while a policy or model change leaves the same active scope held.
func providerAdmissionRetryTarget(s *model.Snapshot, effective config.Effective, target string) (model.ProviderAdmissionHold, error) {
	hold, exists := s.ProviderAdmissionHolds[target]
	if !exists {
		return model.ProviderAdmissionHold{}, errors.New("provider admission hold is no longer present")
	}
	active, held := providerAdmissionHold(s, effective)
	activeKey, keyErr := active.ScopeKey()
	if !held || keyErr != nil || activeKey != target {
		return model.ProviderAdmissionHold{}, errors.New("provider admission retry target is not the current active hold")
	}
	return hold, nil
}

// retryProviderAdmission runs one supervisor-owned, detached, read-only
// protocol request for an exact active hold. The hold remains in force during
// the probe, so no source task can enter through the same provider admission
// path. Only a valid completed structured response releases the target key.
func (c *Controller) retryProviderAdmission(cmd store.Command) error {
	s := c.Snapshot()
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	effective, err := c.effective(ctx)
	if err != nil {
		return err
	}
	hold, err := providerAdmissionRetryTarget(s, effective, cmd.Target)
	if err != nil {
		return err
	}
	for _, run := range s.Runs {
		if run.Provider == hold.Provider && run.Outcome == "running" {
			return errProviderAdmissionProbeWaiting
		}
	}
	runID := "provider-retry-" + cmd.ID
	dir, err := c.P.ValidDisposableAnalysisWorktreePath(runID)
	if err != nil {
		return err
	}
	c.gitMu.Lock()
	err = c.P.Git.Detached(ctx, dir, effective.BaseSHA)
	c.gitMu.Unlock()
	if err != nil {
		return fmt.Errorf("create read-only provider retry checkout: %w", err)
	}
	defer func() {
		c.gitMu.Lock()
		if cleanupErr := c.P.RemoveDisposableAnalysisWorktree(context.Background(), runID); cleanupErr != nil {
			_ = c.P.DB.Event("", "", "provider-retry", hold.Provider, "provider_admission_probe_cleanup_failed", safety.Redact(cleanupErr.Error()))
		}
		c.gitMu.Unlock()
	}()
	p := c.P.Provider
	if p.Name() != effective.Project.Provider {
		p = provider.New(effective.Project.Provider)
	}
	reviewer := roles.Builtins()["reviewer"]
	resolved := effective.Project.ResolveModel(reviewer.Name, reviewer.Capability)
	runtimeDir := filepath.Join(c.P.Dir, "sessions", runID)
	result, err := p.Run(ctx, provider.Request{
		Directory: dir, Runtime: runtimeDir, Role: reviewer.Name, Model: resolved.RequestModel,
		Prompt: "AIH provider admission recovery probe. Inspect no source and return one valid completed structured result. Do not modify files.",
		Write:  false, Timeout: 20 * time.Second,
	})
	if err != nil {
		if provider.IsAuthenticationFailure(err) {
			return errors.New("provider authentication remains unavailable; restore the provider login before another retry")
		}
		return errors.New("provider admission probe did not complete successfully")
	}
	if result.Schema != 1 || result.Status != "completed" || strings.TrimSpace(result.Summary) == "" {
		return errors.New("provider admission probe returned no completed structured result")
	}
	status, err := (gitx.Git{Dir: dir}).Run(ctx, "", "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("provider admission probe modified its read-only checkout")
	}
	if err = c.save(c.ctx, func(next *model.Snapshot) error {
		current, ok := next.ProviderAdmissionHolds[cmd.Target]
		if !ok || current != hold {
			return errors.New("provider admission retry target changed during probe")
		}
		delete(next.ProviderAdmissionHolds, cmd.Target)
		next.Applied[cmd.ID] = true
		return nil
	}); err != nil {
		return err
	}
	_ = c.P.DB.Event("", "", "provider-retry", hold.Provider, "provider_admission_probe_released", cmd.ID+" "+cmd.Target)
	return nil
}
