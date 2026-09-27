package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

// The helper deliberately exits successfully without writing a PNG. It uses
// the normal opted-in child environment, so the controller must first produce
// ordinary passed-check evidence and then block before review consumption.
func init() {
	if len(os.Args) > 1 && os.Args[1] == "_aih-native-artifact-empty" {
		os.Exit(0)
	}
}

// seedVisualImplementedTask gives the normal controller an exact-head final
// visual requirement after a real implementation checkpoint. This keeps visual
// capture eligible when native artifact readiness is evaluated.
func seedVisualImplementedTask(t *testing.T, ctx context.Context, f *demo.Fixture, id string) *model.Task {
	t.Helper()
	snapshot, stateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := snapshot.Tasks[id]
	effective, err := engine.Canonical(ctx, f.P.Git)
	if err != nil {
		t.Fatal(err)
	}
	task.BaseSHA = effective.BaseSHA
	task.AssignedAreas = []string{"feature-" + id + ".txt"}
	task.AssignedAreaKinds = map[string]string{"feature-" + id + ".txt": model.AreaFile}
	if err = f.P.Git.Worktree(ctx, f.P.TaskPath(task), task.Branch, "refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.P.TaskPath(task), "feature-"+id+".txt"), []byte("implemented\n"), 0600); err != nil {
		t.Fatal(err)
	}
	head, err := f.P.Git.Checkpoint(ctx, f.P.TaskPath(task), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: task.Branch, New: head}}); err != nil {
		t.Fatal(err)
	}
	task.HeadSHA = head
	task.State = model.Implemented
	task.UI = true
	task.VisualRequired = &model.VisualRequirement{Role: "designer", Base: task.BaseSHA, Head: task.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Reason: "final rendered evidence required"}
	next, err := f.P.Git.StateCommit(ctx, stateHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	return task
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
	eligible := seedVisualImplementedTask(t, ctx, f, "missing-native-artifact")
	if eligible.VisualRequired == nil || eligible.VisualRequired.Role != "designer" || eligible.VisualRequired.Base != eligible.BaseSHA || eligible.VisualRequired.Head != eligible.HeadSHA || eligible.VisualRequired.Config == "" || eligible.VisualRequired.Rules != roles.Hash() {
		t.Fatalf("fixture did not configure an exact-head eligible visual requirement: %#v", eligible)
	}

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
