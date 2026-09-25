package engine

import (
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

// SupervisorHealth is local observability only. A healthy value does not grant
// lease authority; a stale value tells an operator that supervisor.lock alone
// is insufficient evidence of forward progress.
type SupervisorHealth struct {
	State         string    `json:"state"`
	Reason        string    `json:"reason,omitempty"`
	LastHeartbeat time.Time `json:"last_heartbeat,omitempty"`
	LastProgress  time.Time `json:"last_progress,omitempty"`
	Stage         string    `json:"stage,omitempty"`
	StageAt       time.Time `json:"stage_at,omitempty"`
}

// SupervisorHealthWindow is deliberately derived from the normal pulse cadence.
// It permits transient Git/SQLite latency, but detects a controller that has
// stopped completing lease pulses well before a normal lease expires.
func SupervisorHealthWindow(lease time.Duration) time.Duration {
	if lease <= 0 {
		lease = 3 * time.Minute
	}
	window := 2 * leasePulseInterval(lease)
	if window < 5*time.Second {
		return 5 * time.Second
	}
	if window > time.Minute {
		return time.Minute
	}
	return window
}

func runtimeTime(db *store.Store, key string) time.Time {
	value := db.Get(key)
	when, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return when.UTC()
}

// LocalSupervisorHealth reports whether the local process has recently
// completed a lease pulse. A fresh attempted heartbeat with stale progress is
// explicitly unhealthy: it is the signature of a pulse blocked in persistence.
func LocalSupervisorHealth(db *store.Store, lease time.Duration, now time.Time) SupervisorHealth {
	health := SupervisorHealth{
		LastHeartbeat: runtimeTime(db, LocalLeaseHeartbeatKey),
		LastProgress:  runtimeTime(db, LocalSupervisorProgressKey),
		Stage:         db.Get(LocalSupervisorStageKey),
		StageAt:       runtimeTime(db, LocalSupervisorStageAtKey),
	}
	window := SupervisorHealthWindow(lease)
	now = now.UTC()
	if health.LastHeartbeat.IsZero() || health.LastProgress.IsZero() {
		health.State = "unknown"
		health.Reason = "local heartbeat or completed-pulse evidence is unavailable"
		return health
	}
	if now.Sub(health.LastProgress) > window {
		health.State = "stalled"
		health.Reason = "no completed local lease pulse within " + window.String()
		return health
	}
	if now.Sub(health.LastHeartbeat) > window {
		health.State = "stalled"
		health.Reason = "no attempted local lease heartbeat within " + window.String()
		return health
	}
	health.State = "healthy"
	return health
}
