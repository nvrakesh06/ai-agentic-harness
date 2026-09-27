package engine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
)

type workerExitCauseProvider struct {
	cause error
}

func (*workerExitCauseProvider) Name() string                   { return "codex" }
func (*workerExitCauseProvider) Validate(context.Context) error { return nil }
func (p *workerExitCauseProvider) Run(_ context.Context, request provider.Request) (provider.Result, error) {
	if request.Role != "implementer" {
		return provider.Result{Schema: 1, Status: "completed", Summary: "fixture reader completed"}, nil
	}
	return provider.Result{}, fmt.Errorf("outer fixture wrapper: %w", &provider.InvocationError{Cause: p.cause})
}

// This exercises the real controller role boundary, SQLite event write, and
// ordinary implementation retry routing. It uses local Git worktrees and is
// intentionally left pending shared-machine full-release admission rather than worker feedback.
func TestWorkerExitCauseEventUsesWrappedInvocationWithoutChangingRouting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	privateCause := "fixture process failure at C:/private/worker-output token=not-for-event"
	f.P.Provider = &workerExitCauseProvider{cause: errors.New(privateCause)}
	seedReadyTask(t, ctx, f, "worker-exit-cause")

	task := runUntilTaskState(t, ctx, f, "worker-exit-cause", model.Fix)
	if task.Attempts != 1 || task.AdvisorUsed || len(task.FixCycles) != 0 {
		t.Fatalf("process failure changed normal retry routing: %#v", task)
	}
	var eventTask, run, role, providerName, message string
	if err = f.P.DB.DB.QueryRow("SELECT task,run,role,provider,message FROM events WHERE kind='worker_exit' ORDER BY id DESC LIMIT 1").Scan(&eventTask, &run, &role, &providerName, &message); err != nil {
		t.Fatal(err)
	}
	if eventTask != task.ID || run == "" || role != "implementer" || providerName != "codex" {
		t.Fatalf("worker exit lost role identity: task=%q run=%q role=%q provider=%q", eventTask, run, role, providerName)
	}
	if message != "outcome=failed capability=normal effective_model=provider-default cause=process_failure" {
		t.Fatalf("worker exit cause event = %q", message)
	}
	for _, forbidden := range []string{privateCause, "C:/private", "token=not-for-event", "outer fixture wrapper"} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("worker exit event retained provider diagnostic %q: %q", forbidden, message)
		}
	}
	if eventCount(t, f, "worker_exit") != 1 {
		t.Fatal("expected exactly one failed implementer worker exit before handoff")
	}
	if _, err = (gitx.Git{Dir: f.P.TaskPath(task)}).Run(ctx, "", "status", "--porcelain"); err != nil {
		t.Fatalf("normal retry path did not preserve the task worktree: %v", err)
	}
}
