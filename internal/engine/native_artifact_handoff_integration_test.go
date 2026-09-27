package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

// The helper deliberately exits successfully without writing a PNG. It uses
// the normal opted-in child environment, so the controller must first produce
// ordinary passed-check evidence and then block before review consumption.
func init() {
	if len(os.Args) > 1 && os.Args[1] == "_aih-native-artifact-empty" {
		os.Exit(0)
	}
}

// TestMissingNativeArtifactReceiptBlocksBeforeReadOnlyReview is an ordinary
// real-Git/controller fixture. It remains in the normal test inventory without
// a tag or environment skip; workers do not execute it while the shared permit
// is occupied.
func TestMissingNativeArtifactReceiptBlocksBeforeReadOnlyReview(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{os.Args[0], "_aih-native-artifact-empty"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.Project.VisualCapture = &config.VisualCapture{Server: []string{os.Args[0], "_aih-visual-must-not-run"}, Timeout: 1}
	setFixtureChecks(t, ctx, f, []config.Check{{Name: "missing native artifact", Command: []string{os.Args[0], "_aih-native-artifact-empty"}, Timeout: 30, Artifacts: true}})
	seedReadyTask(t, ctx, f, "missing-native-artifact")

	task := runUntilTaskState(t, ctx, f, "missing-native-artifact", model.Blocked)
	if task.Blocker == nil || task.Blocker.Resume != model.Verifying || !strings.Contains(task.Blocker.Reason, "native artifact producer unavailable") {
		t.Fatalf("missing receipt did not create the verification readiness blocker: %#v", task)
	}
	if task.AdvisorUsed || len(task.FixCycles) != 0 || task.Verification != nil {
		t.Fatalf("missing receipt spent a source recovery budget: %#v", task)
	}
	if len(f.Provider.Reviews) != 0 || f.Provider.AdvisorCount(task.ID) != 0 {
		t.Fatalf("a paid review or advisor was dispatched before native readiness: reviews=%#v advisors=%d", f.Provider.Reviews, f.Provider.AdvisorCount(task.ID))
	}
	if eventCount(t, f, "visual_capture_queued") != 0 || eventCount(t, f, "visual_capture_running") != 0 {
		t.Fatal("missing native receipt reached the visual-capture profile")
	}
	portable, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(portable), f.P.Dir) || strings.Contains(string(portable), "native-check-artifacts") {
		t.Fatalf("blocked portable task retained local native artifact paths: %s", portable)
	}
}
