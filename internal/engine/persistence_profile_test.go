package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

func TestPersistenceProfileIsOptInAndAggregatesOnlyFixedPhases(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if timing := newPersistencePublicationTiming(); timing != nil {
		t.Fatal("persistence profiling enabled without its explicit environment option")
	}
	t.Setenv(persistenceProfileEnv, "1")
	first := newPersistencePublicationTiming()
	first.phases["clone"] = 12 * time.Millisecond
	first.phases["publish"] = 9 * time.Millisecond
	first.phases["unrecognized"] = 100 * time.Millisecond
	first.record(db)
	second := newPersistencePublicationTiming()
	second.phases["clone"] = 4 * time.Millisecond
	second.phases["sqlite_save"] = 7 * time.Millisecond
	second.record(db)
	profile := LocalPersistenceProfile(db)
	if profile == nil || profile.Samples != 2 || profile.Clone != (PersistencePhaseTiming{Count: 2, TotalMS: 16, LastMS: 4, MaximumMS: 12}) || profile.Publish != (PersistencePhaseTiming{Count: 1, TotalMS: 9, LastMS: 9, MaximumMS: 9}) || profile.SQLiteSave != (PersistencePhaseTiming{Count: 1, TotalMS: 7, LastMS: 7, MaximumMS: 7}) {
		t.Fatalf("bounded persistence aggregate = %#v", profile)
	}
	if profile.Clone.MeanMS() != 8 || profile.Redact.Count != 0 {
		t.Fatalf("profile mean or unmeasured phase = %#v", profile)
	}
}

func TestLocalPersistenceProfileRejectsMalformedLocalRecord(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Set(LocalPersistenceProfileKey, `{"version":999}`); err != nil {
		t.Fatal(err)
	}
	if profile := LocalPersistenceProfile(db); profile != nil {
		t.Fatalf("unsupported local profile accepted: %#v", profile)
	}
}
