package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"gopkg.in/yaml.v3"
)

type preflightDeadlineProvider struct {
	mu       sync.Mutex
	timeouts []time.Duration
}

func (*preflightDeadlineProvider) Name() string                   { return "codex" }
func (*preflightDeadlineProvider) Validate(context.Context) error { return nil }
func (p *preflightDeadlineProvider) Run(_ context.Context, request provider.Request) (provider.Result, error) {
	if request.Role == "designer" {
		p.mu.Lock()
		p.timeouts = append(p.timeouts, request.Timeout)
		p.mu.Unlock()
		return provider.Result{}, context.DeadlineExceeded
	}
	return provider.Result{Schema: 1, Status: "completed"}, nil
}

func (p *preflightDeadlineProvider) Timeouts() []time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Duration(nil), p.timeouts...)
}

type restartFenceProvider struct {
	calls        atomic.Int32
	retryStarted chan struct{}
}

func (*restartFenceProvider) Name() string                   { return "codex" }
func (*restartFenceProvider) Validate(context.Context) error { return nil }
func (p *restartFenceProvider) Run(ctx context.Context, request provider.Request) (provider.Result, error) {
	if request.Role != "designer" {
		return provider.Result{Schema: 1, Status: "completed"}, nil
	}
	if p.calls.Add(1) == 1 {
		return provider.Result{}, context.DeadlineExceeded
	}
	if p.retryStarted != nil {
		close(p.retryStarted)
		<-ctx.Done()
	}
	return provider.Result{}, context.DeadlineExceeded
}

func configureFixtureRoleTimeouts(t *testing.T, ctx context.Context, f *demo.Fixture, timeouts config.RoleTimeouts) {
	t.Helper()
	f.Project.RoleTimeouts = &timeouts
	encoded, err := yaml.Marshal(f.Project)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.Source, ".aih", "project.yaml")
	if err = os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	g := gitx.Git{Dir: f.Source}
	if _, err = g.Run(ctx, "", "add", ".aih/project.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err = g.Run(ctx, "", "commit", "-m", "Bound fixture preflight reader budget"); err != nil {
		t.Fatal(err)
	}
	if _, err = g.Run(ctx, "", "push", "origin", "HEAD:main"); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightDeadlineUsesRemainingBudgetWithoutCodeFix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	configureFixtureRoleTimeouts(t, ctx, f, config.RoleTimeouts{Preflight: 10})
	seedReadyTask(t, ctx, f, "deadline-reader")
	snapshot, stateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Tasks["deadline-reader"].UI = true
	next, err := f.P.Git.StateCommit(ctx, stateHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	workers := &preflightDeadlineProvider{}
	f.P.Provider = workers
	task := runUntilTaskState(t, ctx, f, "deadline-reader", model.Blocked)
	if got := workers.Timeouts(); len(got) != 2 || got[0] != 8*time.Second || got[1] != 2*time.Second {
		t.Fatalf("reader timeout slices = %v, want 8s then 2s", got)
	}
	if task.Attempts != 0 || len(task.FixCycles) != 0 || task.AdvisorUsed || task.Blocker == nil || task.Blocker.Origin != model.BlockerOriginVerificationOnly || task.Blocker.Resume != model.Ready {
		t.Fatalf("reader deadline consumed implementation recovery state: %#v", task)
	}
	guard := task.ReadOnlyRetries["pre-implementation/designer"]
	if guard.Attempts != 2 || guard.RemainingSeconds != 0 || task.Preflight == nil || task.Preflight.BaseSHA == "" {
		t.Fatalf("reader deadline guard did not retain exact preflight state: %#v task=%#v", guard, task)
	}
}

func TestReaderRetryAdmissionIsExhaustedAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	configureFixtureRoleTimeouts(t, ctx, f, config.RoleTimeouts{Preflight: 10})
	seedReadyTask(t, ctx, f, "restart-fence")
	snapshot, stateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Tasks["restart-fence"].UI = true
	next, err := f.P.Git.StateCommit(ctx, stateHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	first := &restartFenceProvider{retryStarted: make(chan struct{})}
	f.P.Provider = first
	firstCtx, stopFirst := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(firstCtx) }()
	select {
	case <-first.retryStarted:
	case serveErr := <-done:
		t.Fatalf("supervisor stopped before narrower retry admission: %v", serveErr)
	case <-time.After(20 * time.Second):
		t.Fatal("initial reader deadline did not admit its narrower retry")
	}
	stopFirst()
	if err = <-done; err != nil {
		t.Fatal(err)
	}

	// The retry reservation has been published but its provider result was
	// interrupted. A new controller must block it rather than invoke a fresh
	// full-budget reader pass.
	snapshot, _, err = f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	guard := snapshot.Tasks["restart-fence"].ReadOnlyRetries["pre-implementation/designer"]
	if guard.Attempts != 2 || guard.RemainingSeconds != 0 {
		t.Fatalf("narrow retry was not durably consumed before provider dispatch: %#v", guard)
	}
	restarted := &restartFenceProvider{}
	f.P.Provider = restarted
	task := runUntilTaskState(t, ctx, f, "restart-fence", model.Blocked)
	if restarted.calls.Load() != 0 || task.Blocker == nil || task.Blocker.Origin != model.BlockerOriginVerificationOnly || task.ReadOnlyRetries["pre-implementation/designer"].Attempts != 2 {
		t.Fatalf("restart replayed exhausted reader retry: calls=%d task=%#v", restarted.calls.Load(), task)
	}
}
