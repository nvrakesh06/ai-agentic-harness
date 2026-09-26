package engine

import (
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func TestScopeRecoveryUnstartedRequiresEmptyLifecycle(t *testing.T) {
	snapshot := model.NewSnapshot("project123")
	task := &model.Task{ID: "queued", State: model.Ready, Branch: "aih/queued", FixCycles: map[string]int{}}
	snapshot.Tasks[task.ID] = task
	if !scopeRecoveryUnstarted(snapshot, task) {
		t.Fatal("empty ready task was not accepted")
	}
	task.Attempts = 1
	if scopeRecoveryUnstarted(snapshot, task) {
		t.Fatal("attempted task was accepted as unstarted")
	}
	task.Attempts = 0
	snapshot.Runs = []model.Run{{Task: task.ID, Outcome: "interrupted"}}
	if scopeRecoveryUnstarted(snapshot, task) {
		t.Fatal("task with historical run was accepted as unstarted")
	}
}
