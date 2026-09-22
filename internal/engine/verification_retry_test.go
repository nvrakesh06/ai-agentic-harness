package engine_test

import (
	"context"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"testing"
	"time"
)

func seedReadyTask(t *testing.T, ctx context.Context, f *demo.Fixture, title string) {
	t.Helper()
	snapshot, head, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Objectives["objective"] = &model.Objective{ID: "objective", Text: "verification routing", Planned: true}
	snapshot.Tasks[title] = &model.Task{
		ID: title, ObjectiveID: "objective", Issue: 1, Title: title,
		Objective: "Create the fixture and verify it", Acceptance: []string{"fixture exists"},
		Areas: []string{title}, Domains: []string{title}, Risk: "low", State: model.Ready,
		Branch: "aih/" + title, FixCycles: map[string]int{},
	}
	next, err := f.P.Git.StateCommit(ctx, head, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: head, New: next}}); err != nil {
		t.Fatal(err)
	}
}

func runUntilTaskState(t *testing.T, ctx context.Context, f *demo.Fixture, id string, wanted ...model.State) *model.Task {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(ctx) }()
	wantedStates := map[model.State]bool{}
	for _, state := range wanted {
		wantedStates[state] = true
	}
	for {
		snapshot, _, err := f.P.DB.Load()
		if err == nil && snapshot.Tasks[id] != nil && wantedStates[snapshot.Tasks[id].State] {
			if err = f.P.DB.Submit(storeCommand("handoff")); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			return snapshot.Tasks[id]
		}
		select {
		case err = <-done:
			t.Fatal("supervisor stopped before target state", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func storeCommand(kind string) store.Command {
	return store.Command{ID: model.ID(), Kind: kind}
}

func eventCount(t *testing.T, f *demo.Fixture, kind string) int {
	t.Helper()
	var count int
	if err := f.P.DB.DB.QueryRow("SELECT COUNT(*) FROM events WHERE kind=?", kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestEnvironmentOnlyImplementerBlockRoutesToNativeVerification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.Provider.EnvironmentBlocks = map[string]int{"environment": 1}
	seedReadyTask(t, ctx, f, "environment")
	task := runUntilTaskState(t, ctx, f, "environment", model.Review, model.MergeReady, model.Done)
	if got := f.Provider.ImplementationCount("environment"); got != 1 {
		t.Fatalf("implementer ran %d times, want one native-verification reroute", got)
	}
	if task.Verification != nil {
		t.Fatal("successful native verification left a retry guard")
	}
	if got := eventCount(t, f, "verification_rerouted"); got != 1 {
		t.Fatalf("verification reroute events = %d, want 1", got)
	}
}

func TestMissingNativeCapabilityBlocksWithoutImplementerRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"aih-command-that-does-not-exist"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	seedReadyTask(t, ctx, f, "missing-tool")
	task := runUntilTaskState(t, ctx, f, "missing-tool", model.Blocked)
	if got := f.Provider.ImplementationCount("missing-tool"); got != 1 {
		t.Fatalf("implementer ran %d times after a missing native tool", got)
	}
	if task.Blocker == nil || task.Blocker.Resume != model.SyncRequired || task.Verification == nil || !task.Verification.NativeOnly {
		t.Fatalf("missing capability was not durably routed to native verification: %+v", task)
	}
	if got := eventCount(t, f, "retry_suppressed"); got != 1 {
		t.Fatalf("suppressed retry events = %d, want 1", got)
	}
}

func TestRepeatedNativeFailureAtSameHeadStopsEquivalentWriterLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "rev-parse", "--verify", "refs/heads/missing-native-check"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	seedReadyTask(t, ctx, f, "persistent-check")
	task := runUntilTaskState(t, ctx, f, "persistent-check", model.Blocked)
	if got := f.Provider.ImplementationCount("persistent-check"); got != 2 {
		t.Fatalf("implementer ran %d times, want initial implementation plus one bounded fix", got)
	}
	if task.Verification == nil || task.Verification.Attempts != 2 || task.Verification.NativeOnly {
		t.Fatalf("repeated verification failure was not fingerprinted: %+v", task.Verification)
	}
	if task.Blocker == nil || task.Blocker.Resume != model.SyncRequired {
		t.Fatalf("repeated verification failure did not produce a durable verification blocker: %+v", task.Blocker)
	}
}

func TestImplementationFailureKeepsNormalFixLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.Provider.Failures = map[string]int{"code-defect": 1}
	seedReadyTask(t, ctx, f, "code-defect")
	runUntilTaskState(t, ctx, f, "code-defect", model.Implemented, model.Verifying, model.Review, model.MergeReady, model.Done)
	if got := f.Provider.ImplementationCount("code-defect"); got != 2 {
		t.Fatalf("code failure ran implementer %d times, want normal bounded retry", got)
	}
}
