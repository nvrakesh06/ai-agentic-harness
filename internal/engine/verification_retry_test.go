package engine_test

import (
	"context"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == "_aih-native-source-failure" {
		fmt.Fprintln(os.Stderr, "src/studio-server/http.ts(291,69): TS2740: cannot use Duplex as Socket")
		fmt.Fprintln(os.Stderr, "token=sk-abcdefghijklmnopqrstuvwxyz012345")
		os.Exit(1)
	}
}

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
		// demo.Worker writes feature-<title>.txt. This remains an unstarted
		// fixture so normal canonical hydration classifies the explicit file.
		Areas: []string{"feature-" + title + ".txt"}, Domains: []string{title}, Risk: "low", State: model.Ready,
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
	if task.Blocker == nil || task.Blocker.Origin != model.BlockerOriginVerificationOnly || task.Blocker.Resume != model.SyncRequired || task.Verification == nil || !task.Verification.NativeOnly {
		t.Fatalf("missing capability was not durably routed to native verification: %+v", task)
	}
	if got := eventCount(t, f, "retry_suppressed"); got != 1 {
		t.Fatalf("suppressed retry events = %d, want 1", got)
	}
	if err = f.P.DB.Close(); err != nil {
		t.Fatal(err)
	}
	recoveredProject, err := f.Open(ctx, filepath.Join(f.Root, "machine-b"))
	if err != nil {
		t.Fatal(err)
	}
	defer recoveredProject.DB.Close()
	if err = recoveredProject.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := recoveredProject.DB.Load()
	if err != nil || recovered.Tasks["missing-tool"].Blocker == nil || recovered.Tasks["missing-tool"].Blocker.Origin != model.BlockerOriginVerificationOnly {
		t.Fatalf("cross-machine recovery lost verification-only blocker provenance: %#v err=%v", recovered.Tasks["missing-tool"], err)
	}
}

func TestWriterCheckpointMakesPreflightExactForMissingCapabilityContinuation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"aih-command-that-does-not-exist"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	seedReadyTask(t, ctx, f, "writer-checkpoint")
	snapshot, head, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Tasks["writer-checkpoint"].UI = true
	snapshot.Tasks["writer-checkpoint"].Areas = []string{"feature-writer-checkpoint.txt"}
	task := snapshot.Tasks["writer-checkpoint"]
	task.BaseSHA = head
	task.AssignedAreas = []string{"feature-writer-checkpoint.txt"}
	task.AssignedAreaKinds = map[string]string{"feature-writer-checkpoint.txt": model.AreaFile}
	if err = f.P.Git.Worktree(ctx, f.P.TaskPath(task), task.Branch, "refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.P.TaskPath(task), "feature-writer-checkpoint.txt"), []byte("prior checkpoint\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prior, err := f.P.Git.Checkpoint(ctx, f.P.TaskPath(task), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: task.Branch, New: prior}}); err != nil {
		t.Fatal(err)
	}
	task.HeadSHA = prior
	next, err := f.P.Git.StateCommit(ctx, head, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: head, New: next}}); err != nil {
		t.Fatal(err)
	}
	blocked := runUntilTaskState(t, ctx, f, "writer-checkpoint", model.Blocked)
	if blocked.Blocker == nil || blocked.Blocker.Origin != model.BlockerOriginVerificationOnly || blocked.Preflight == nil || blocked.Preflight.HeadSHA != blocked.HeadSHA {
		t.Fatalf("normal writer checkpoint did not preserve exact preflight identity for verification recovery: blocker=%#v task=%#v", blocked.Blocker, blocked)
	}
	if err = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "answer", Target: "writer-checkpoint", Payload: "AIH-CONTINUE CHECKPOINT " + blocked.HeadSHA}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(ctx) }()
	var resumed *model.Task
	deadline := time.NewTimer(75 * time.Second)
	defer deadline.Stop()
	for resumed == nil {
		current, _, loadErr := f.P.DB.Load()
		if loadErr == nil {
			task := current.Tasks["writer-checkpoint"]
			if task != nil && f.Provider.ImplementationCount("writer-checkpoint") == 2 && task.State == model.Blocked {
				resumed = task
			}
		}
		select {
		case serveErr := <-done:
			t.Fatalf("supervisor stopped before exact checkpoint writer admission: %v", serveErr)
		case <-deadline.C:
			t.Fatal("exact checkpoint continuation did not reach its retained-guidance writer pass")
		case <-time.After(25 * time.Millisecond):
		}
	}
	if got := f.Provider.ImplementationCount("writer-checkpoint"); got != 2 {
		t.Fatalf("exact checkpoint continuation ran implementer %d times, want the initial writer plus one retained-guidance pass", got)
	}
	if resumed.Preflight == nil || resumed.Preflight.ReuseCount != 1 || !strings.Contains(resumed.Preflight.ReuseReason, "human checkpoint") {
		t.Fatalf("normal writer checkpoint lost retained guidance after exact verification unblock: %#v", resumed.Preflight)
	}
	if err = f.P.DB.Submit(storeCommand("handoff")); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNativeOnlySourceFailureRoutesToFixAndPersistsRedactedEvidence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := demo.New(ctx, t.TempDir(), []string{exe, "_aih-native-source-failure"})
	if err != nil {
		t.Fatal(err)
	}
	f.Provider.EnvironmentBlocks = map[string]int{"native-source": 1}
	seedReadyTask(t, ctx, f, "native-source")
	task := runUntilTaskState(t, ctx, f, "native-source", model.Blocked)
	if got := f.Provider.ImplementationCount("native-source"); got != 2 {
		t.Fatalf("implementer ran %d times, want initial implementation plus one native source-failure fix", got)
	}
	if task.Blocker == nil || task.Blocker.Resume != model.Fix || task.Verification == nil || !task.Verification.NativeOnly || task.Verification.Attempts != 2 {
		t.Fatalf("native source failure did not become a bounded FIX recovery: %+v", task)
	}
	if !strings.Contains(task.Blocker.Reason, "fixture acceptance failed") || !strings.Contains(task.Blocker.Reason, "TS2740") || strings.Contains(task.Blocker.Reason, "sk-abcdefghijklmnopqrstuvwxyz012345") {
		t.Fatalf("native source evidence was not named and redacted: %q", task.Blocker.Reason)
	}
	pulls, _ := f.Hub.Pulls()
	if len(pulls) != 1 || !strings.Contains(pulls[0].Body, "fixture acceptance failed") || strings.Contains(pulls[0].Body, "sk-abcdefghijklmnopqrstuvwxyz012345") {
		t.Fatalf("draft PR did not preserve bounded redacted check evidence: %#v", pulls)
	}
	if err = f.P.DB.Close(); err != nil {
		t.Fatal(err)
	}
	recoveredProject, err := f.Open(ctx, filepath.Join(f.Root, "machine-b"))
	if err != nil {
		t.Fatal(err)
	}
	defer recoveredProject.DB.Close()
	if err = recoveredProject.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := recoveredProject.DB.Load()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Tasks["native-source"].State != model.Blocked || recovered.Tasks["native-source"].Blocker == nil || recovered.Tasks["native-source"].Blocker.Resume != model.Fix || recovered.Tasks["native-source"].Verification == nil {
		t.Fatalf("restart recovery lost bounded native source-failure state: %+v", recovered.Tasks["native-source"])
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
	if task.Blocker == nil || task.Blocker.Resume != model.Fix {
		t.Fatalf("repeated verification failure did not produce a durable implementer recovery blocker: %+v", task.Blocker)
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

func TestProviderAuthenticationFailurePreservesTaskBudgetAndSkipsAdvisor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.Provider.AuthFailures = map[string]int{"auth-blocked": 1}
	seedReadyTask(t, ctx, f, "auth-blocked")
	task := runUntilTaskState(t, ctx, f, "auth-blocked", model.Blocked)
	if task.Attempts != 0 || len(task.FixCycles) != 0 || task.AdvisorUsed {
		t.Fatalf("authentication failure consumed task recovery budget: %+v", task)
	}
	if task.Blocker == nil || task.Blocker.Origin != model.BlockerOriginProviderAuthentication || task.Blocker.Resume != model.Ready {
		t.Fatalf("authentication failure did not preserve implementer stage: %+v", task.Blocker)
	}
	if got := f.Provider.ImplementationCount("auth-blocked"); got != 1 {
		t.Fatalf("implementer calls = %d, want one failed provider invocation", got)
	}
	if got := f.Provider.AdvisorCount("auth-blocked"); got != 0 {
		t.Fatalf("advisor calls = %d, want none after authentication failure", got)
	}
	if strings.Contains(task.Blocker.Reason, "fixture provider invocation failed") {
		t.Fatalf("raw provider diagnostic entered blocker: %q", task.Blocker.Reason)
	}
}
