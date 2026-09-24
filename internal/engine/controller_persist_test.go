package engine

import (
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func TestSnapshotRemainsAvailableWhilePublicationIsSerialized(t *testing.T) {
	c := &Controller{s: model.NewSnapshot("project")}

	// persistMu represents a publication waiting on Git or SQLite. Snapshot is
	// used by the scheduler and worker completion paths, so it must not wait for
	// that external work.
	c.persistMu.Lock()
	defer c.persistMu.Unlock()

	done := make(chan *model.Snapshot, 1)
	go func() { done <- c.Snapshot() }()

	select {
	case snapshot := <-done:
		if snapshot.Project != "project" {
			t.Fatalf("snapshot project = %q, want project", snapshot.Project)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot waited for serialized publication")
	}
}
