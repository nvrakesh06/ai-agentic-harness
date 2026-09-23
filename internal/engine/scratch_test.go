package engine_test

import (
	"context"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkerScratchToolingNeverBecomesCheckpointedSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.Provider.ScratchTooling = map[string]bool{"dashboard": true}
	seedReadyTask(t, ctx, f, "dashboard")
	task := runUntilTaskState(t, ctx, f, "dashboard", model.Done)
	if _, err = os.Stat(filepath.Join(f.P.TaskPath(task), ".tmp-npm")); !os.IsNotExist(err) {
		t.Fatalf("worker tooling appeared in source worktree: %v", err)
	}
	if _, err = os.Stat(f.P.TaskScratchPath(task)); !os.IsNotExist(err) {
		t.Fatalf("completed task scratch was not cleaned: %v", err)
	}
}
