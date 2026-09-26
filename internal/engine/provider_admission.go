package engine

import (
	"errors"
	"fmt"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
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
