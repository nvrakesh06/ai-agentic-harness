package engine

import (
	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

// PreflightScopeForTest lets black-box integration tests construct a durable
// completed preflight without copying the scope-fingerprint implementation.
func PreflightScopeForTest(t *model.Task, effective config.Effective) string {
	return preflightScope(t, effective)
}
