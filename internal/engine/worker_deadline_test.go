package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
)

type checkpointProvider struct {
	finishCheckpoint bool
	checkpointErr    error
	requests         []provider.Request
}

func (p *checkpointProvider) Name() string                   { return "fixture" }
func (p *checkpointProvider) Validate(context.Context) error { return nil }
func (p *checkpointProvider) Run(ctx context.Context, request provider.Request) (provider.Result, error) {
	p.requests = append(p.requests, request)
	if len(p.requests) == 1 {
		<-ctx.Done()
		return provider.Result{}, &provider.InvocationError{Cause: ctx.Err(), LastActivity: time.Now().UTC(), OutputBytes: 8}
	}
	if p.finishCheckpoint {
		return provider.Result{Schema: 1, Status: "in_progress", Summary: "safe edits checkpointed"}, nil
	}
	if p.checkpointErr != nil {
		return provider.Result{}, p.checkpointErr
	}
	<-ctx.Done()
	return provider.Result{}, ctx.Err()
}

func TestCheckpointNonDeadlineFailureUsesOrdinaryFailurePath(t *testing.T) {
	providerErr := errors.New("malformed checkpoint result")
	p := &checkpointProvider{checkpointErr: providerErr}
	var events []string
	recovered := false
	result, err := runWithCheckpoint(context.Background(), p, provider.Request{Runtime: t.TempDir()}, "checkpoint now", 2*time.Second, deadlineHooks{
		active: func(error) bool { return true },
		event: func(kind, _ string) {
			events = append(events, kind)
		},
		recover: func(error) provider.Result {
			recovered = true
			return provider.Result{Schema: 1, Status: "in_progress", Summary: "must not recover"}
		},
	})
	if !errors.Is(err, providerErr) || result.Status != "" || recovered {
		t.Fatal(result, err, recovered)
	}
	if strings.Join(events, ",") != "worker_checkpoint_requested,worker_checkpoint_failed" {
		t.Fatal(events)
	}
}

func TestActiveWorkerGetsOneBoundedCheckpointPass(t *testing.T) {
	p := &checkpointProvider{finishCheckpoint: true}
	var events []string
	recovered := false
	started := time.Now()
	result, err := runWithCheckpoint(context.Background(), p, provider.Request{Runtime: t.TempDir()}, "checkpoint now", 2*time.Second, deadlineHooks{
		active: func(error) bool { return true },
		event: func(kind, _ string) {
			events = append(events, kind)
		},
		recover: func(error) provider.Result {
			recovered = true
			return provider.Result{Schema: 1, Status: "in_progress", Summary: "recovered"}
		},
	})
	if err != nil || result.Summary != "safe edits checkpointed" || recovered {
		t.Fatal(result, err, recovered)
	}
	if len(p.requests) != 2 || p.requests[0].Timeout != 1600*time.Millisecond {
		t.Fatalf("requests = %#v", p.requests)
	}
	if !strings.HasSuffix(p.requests[1].Runtime, "checkpoint") || p.requests[1].Prompt != "checkpoint now" || p.requests[1].Timeout <= 0 || p.requests[1].Timeout > 400*time.Millisecond {
		t.Fatalf("checkpoint request = %#v", p.requests[1])
	}
	if strings.Join(events, ",") != "worker_checkpoint_requested,worker_checkpoint_completed" {
		t.Fatal(events)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatal("checkpoint flow exceeded its hard bound")
	}
}

func TestHardDeadlineReturnsSyntheticHandoffWithoutExtendingAgain(t *testing.T) {
	p := &checkpointProvider{}
	var events []string
	recoveries := 0
	started := time.Now()
	result, err := runWithCheckpoint(context.Background(), p, provider.Request{Runtime: t.TempDir()}, "checkpoint now", 2*time.Second, deadlineHooks{
		active: func(error) bool { return true },
		event: func(kind, _ string) {
			events = append(events, kind)
		},
		recover: func(runErr error) provider.Result {
			if !errors.Is(runErr, context.DeadlineExceeded) {
				t.Fatalf("recovery reason = %v", runErr)
			}
			recoveries++
			return provider.Result{Schema: 1, Status: "in_progress", Summary: "synthetic handoff"}
		},
	})
	if err != nil || result.Summary != "synthetic handoff" {
		t.Fatal(result, err)
	}
	if len(p.requests) != 2 || recoveries != 1 {
		t.Fatalf("calls = %d recoveries = %d", len(p.requests), recoveries)
	}
	if strings.Join(events, ",") != "worker_checkpoint_requested,worker_hard_timeout" {
		t.Fatal(events)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatal("hard deadline was extended")
	}
}

func TestSyntheticHandoffCapturesRecoverableWorktreeAndCommandEvidence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "worktree")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	g := gitx.Git{Dir: dir}
	if _, err := g.Run(ctx, "", "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "", "add", "--all"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "", "commit", "-m", "baseline"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracked, []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(root, "session")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	logLine := `{"type":"item.completed","item":{"type":"command_execution","command":"go test ./internal/engine --token=do-not-persist","exit_code":0}}` + "\n" +
		`{"type":"item.completed","item":{"type":"command_execution","command":"curl https://user:password@example.test -H 'Authorization: Bearer do-not-persist'","exit_code":0}}` + "\n"
	if err := os.WriteFile(filepath.Join(runtimeDir, "output.log"), []byte(logLine), 0600); err != nil {
		t.Fatal(err)
	}
	c := &Controller{P: &Project{Dir: filepath.Join(root, "control"), Root: dir, Home: root}}
	result := c.syntheticHandoff(dir, runtimeDir, context.DeadlineExceeded)
	if result.Status != "in_progress" || !strings.Contains(result.Summary, "go test") || strings.Contains(result.Summary, "do-not-persist") {
		t.Fatalf("result = %#v", result)
	}
	if strings.Join(result.ChangedAreas, ",") != "new.txt,tracked.txt" {
		t.Fatal(result.ChangedAreas)
	}
	b, err := os.ReadFile(filepath.Join(runtimeDir, "handoff.json"))
	if err != nil {
		t.Fatal(err)
	}
	var handoff timeoutHandoff
	if err = json.Unmarshal(b, &handoff); err != nil {
		t.Fatal(err)
	}
	if handoff.Reason == "" || strings.Join(handoff.ChangedFiles, ",") != "new.txt,tracked.txt" || len(handoff.WorktreeStatus) != 2 || len(handoff.CompletedCommands) != 1 {
		t.Fatalf("handoff = %#v", handoff)
	}
	if handoff.CompletedCommands[0] != "go test" || strings.Contains(string(b), "do-not-persist") || strings.Contains(string(b), "password") || strings.Contains(string(b), "Authorization") {
		t.Fatalf("handoff persisted raw command arguments: %s", b)
	}
}
