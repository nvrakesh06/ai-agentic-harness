package engine

import (
	"encoding/json"
	"os"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

const (
	// LocalPersistenceProfileKey is local observability only. It must never
	// become portable state or participate in publication fencing.
	LocalPersistenceProfileKey = "persistence_profile_v1"
	persistenceProfileEnv      = "AIH_PERSISTENCE_PROFILE"
)

// PersistencePhaseTiming retains a bounded aggregate for one publication
// phase. It intentionally has no samples, paths, revisions, or error text.
type PersistencePhaseTiming struct {
	Count     uint64 `json:"count"`
	TotalMS   int64  `json:"total_ms"`
	LastMS    int64  `json:"last_ms"`
	MaximumMS int64  `json:"maximum_ms"`
}

func (p PersistencePhaseTiming) MeanMS() int64 {
	if p.Count == 0 {
		return 0
	}
	return p.TotalMS / int64(p.Count)
}

// PersistenceProfile is local, opt-in timing evidence for the controller's
// existing publication critical section. A mean is TotalMS/Count; percentiles
// cannot be inferred from this bounded aggregate.
type PersistenceProfile struct {
	Version     int                    `json:"version"`
	Samples     uint64                 `json:"samples"`
	MutexWait   PersistencePhaseTiming `json:"mutex_wait"`
	Clone       PersistencePhaseTiming `json:"clone"`
	Redact      PersistencePhaseTiming `json:"redact"`
	StateCommit PersistencePhaseTiming `json:"state_commit"`
	Publish     PersistencePhaseTiming `json:"publish"`
	SQLiteSave  PersistencePhaseTiming `json:"sqlite_save"`
}

type persistencePublicationTiming struct {
	phases map[string]time.Duration
}

func newPersistencePublicationTiming() *persistencePublicationTiming {
	if os.Getenv(persistenceProfileEnv) != "1" {
		return nil
	}
	return &persistencePublicationTiming{phases: map[string]time.Duration{}}
}

func (p *persistencePublicationTiming) phase(name string, started time.Time) {
	if p == nil {
		return
	}
	p.phases[name] += time.Since(started)
}

// LocalPersistenceProfile returns only a valid bounded local aggregate. A
// malformed or unavailable record is treated as unavailable observability.
func LocalPersistenceProfile(db *store.Store) *PersistenceProfile {
	if db == nil {
		return nil
	}
	var profile PersistenceProfile
	if json.Unmarshal([]byte(db.Get(LocalPersistenceProfileKey)), &profile) != nil || profile.Version != 1 {
		return nil
	}
	return &profile
}

func (p *persistencePublicationTiming) record(db *store.Store) {
	if p == nil || db == nil {
		return
	}
	profile := LocalPersistenceProfile(db)
	if profile == nil {
		profile = &PersistenceProfile{Version: 1}
	}
	profile.Samples++
	for name, duration := range p.phases {
		addPersistencePhase(profile, name, duration)
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return
	}
	// This one opt-in local write is deliberately outside the measured phases;
	// it must never fail a source/state publication or lease renewal.
	_ = db.Set(LocalPersistenceProfileKey, string(encoded))
}

func addPersistencePhase(profile *PersistenceProfile, name string, duration time.Duration) {
	var phase *PersistencePhaseTiming
	switch name {
	case "mutex_wait":
		phase = &profile.MutexWait
	case "clone":
		phase = &profile.Clone
	case "redact":
		phase = &profile.Redact
	case "state_commit":
		phase = &profile.StateCommit
	case "publish":
		phase = &profile.Publish
	case "sqlite_save":
		phase = &profile.SQLiteSave
	default:
		return
	}
	ms := duration.Milliseconds()
	phase.Count++
	phase.TotalMS += ms
	phase.LastMS = ms
	if ms > phase.MaximumMS {
		phase.MaximumMS = ms
	}
}
