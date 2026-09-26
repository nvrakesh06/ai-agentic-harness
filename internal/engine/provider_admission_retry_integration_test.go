package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"gopkg.in/yaml.v3"
)

type providerRetryProbeProvider struct {
	mode string

	calls atomic.Int32
	mu    sync.Mutex
	dirs  []string
}

func (*providerRetryProbeProvider) Name() string                   { return "codex" }
func (*providerRetryProbeProvider) Validate(context.Context) error { return nil }
func (p *providerRetryProbeProvider) Run(_ context.Context, request provider.Request) (provider.Result, error) {
	if request.Role != "reviewer" || request.Write {
		return provider.Result{}, errors.New("provider retry invoked a task-writing or non-probe role")
	}
	p.calls.Add(1)
	p.mu.Lock()
	p.dirs = append(p.dirs, request.Directory)
	p.mu.Unlock()
	switch p.mode {
	case "failed":
		return provider.Result{}, errors.New("fixture probe transport failure")
	case "malformed":
		return provider.Result{Schema: 1, Status: "completed"}, nil
	default:
		return provider.Result{Schema: 1, Status: "completed", Summary: "provider protocol accepted"}, nil
	}
}

func (p *providerRetryProbeProvider) directories() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.dirs...)
}

// deferredProvider creates a real overlapping provider invocation. The first
// writer publishes the hold while the second writer still owns a running
// provider record, so a subsequently queued public retry must remain pending
// until that active provider exits.
type deferredProvider struct {
	secondStarted chan struct{}
	releaseSecond chan struct{}
	secondOnce    sync.Once
	probeCalls    atomic.Int32
}

func (*deferredProvider) Name() string                   { return "codex" }
func (*deferredProvider) Validate(context.Context) error { return nil }
func (p *deferredProvider) Run(ctx context.Context, request provider.Request) (provider.Result, error) {
	if request.Role == "reviewer" {
		if request.Write {
			return provider.Result{}, errors.New("provider retry probe was writable")
		}
		p.probeCalls.Add(1)
		return provider.Result{Schema: 1, Status: "completed", Summary: "deferred probe accepted"}, nil
	}
	if request.Role != "implementer" {
		return provider.Result{}, errors.New("unexpected non-probe role")
	}
	task, err := demo.Task(request.Prompt)
	if err != nil {
		return provider.Result{}, err
	}
	switch task.Title {
	case "first":
		select {
		case <-p.secondStarted:
		case <-ctx.Done():
			return provider.Result{}, ctx.Err()
		}
		return provider.Result{}, &provider.InvocationError{Cause: errors.New("fixture schema rejection"), Failure: provider.FailureRequestRejected, Rejection: provider.RejectionInvalidJSONSchema}
	case "second":
		p.secondOnce.Do(func() { close(p.secondStarted) })
		select {
		case <-p.releaseSecond:
		case <-ctx.Done():
			return provider.Result{}, ctx.Err()
		}
		return provider.Result{Schema: 1, Status: "completed", Summary: "second writer exited without source edits"}, nil
	default:
		return provider.Result{}, errors.New("unexpected fixture task")
	}
}

func seedProviderRetryHold(t *testing.T, ctx context.Context, f *demo.Fixture) (string, model.ProviderAdmissionHold) {
	t.Helper()
	effective, err := engine.Canonical(ctx, f.P.Git)
	if err != nil {
		t.Fatal(err)
	}
	reviewer := roles.Builtins()["reviewer"]
	resolved := effective.Project.ResolveModel(reviewer.Name, reviewer.Capability)
	hold := model.ProviderAdmissionHold{
		Provider: effective.Project.Provider, Class: model.ProviderAdmissionRequestRejected,
		Rejection: model.ProviderRejectionInvalidJSONSchema, SchemaSHA256: provider.SchemaSHA256(),
		OriginPolicy: effective.Hash, OriginRules: roles.Hash(), OriginModel: resolved.EffectiveModel,
	}
	key, err := hold.ScopeKey()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, stateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ProviderAdmissionHolds[key] = hold
	next, err := f.P.Git.StateCommit(ctx, stateHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	if err = f.P.DB.Save(next, snapshot); err != nil {
		t.Fatal(err)
	}
	return key, hold
}

func waitProviderRetryCommand(t *testing.T, ctx context.Context, f *demo.Fixture, id, wanted string) string {
	t.Helper()
	for {
		var status, message string
		err := f.P.DB.DB.QueryRow("SELECT status,error FROM commands WHERE id=?", id).Scan(&status, &message)
		if err == nil && status == wanted {
			return message
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%v waiting for provider retry command %s to become %s", ctx.Err(), id, wanted)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func startProviderRetryController(t *testing.T, ctx context.Context, f *demo.Fixture) func() {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(ctx) }()
	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		if err := f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"}); err != nil {
			t.Error(err)
			return
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			t.Error("provider retry supervisor did not stop after handoff")
		}
	}
}

func assertProviderRetryCleanedUp(t *testing.T, ctx context.Context, f *demo.Fixture, dirs []string) {
	t.Helper()
	if len(dirs) != 1 {
		t.Fatalf("provider retry probe directories = %v", dirs)
	}
	if _, err := os.Stat(dirs[0]); !os.IsNotExist(err) {
		t.Fatalf("disposable provider retry checkout remained at %s: %v", dirs[0], err)
	}
	status, err := (gitx.Git{Dir: f.Source}).Run(ctx, "", "status", "--porcelain")
	if err != nil || status != "" {
		t.Fatalf("provider retry dirtied canonical source: status=%q err=%v", status, err)
	}
}

func TestProviderRetryReleasesExactHoldOnceAndCleansDetachedCheckout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	key, _ := seedProviderRetryHold(t, ctx, f)
	worker := &providerRetryProbeProvider{}
	f.P.Provider = worker
	const commandID = "provider-retry-success"
	if err = f.P.DB.Submit(store.Command{ID: commandID, Kind: "provider-retry", Target: key}); err != nil {
		t.Fatal(err)
	}
	stop := startProviderRetryController(t, ctx, f)
	defer stop()
	waitProviderRetryCommand(t, ctx, f, commandID, "accepted")
	snapshot, _, err := f.P.DB.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, held := snapshot.ProviderAdmissionHolds[key]; held || !snapshot.Applied[commandID] || worker.calls.Load() != 1 {
		t.Fatalf("successful retry did not atomically remove only its hold: holds=%#v applied=%t calls=%d", snapshot.ProviderAdmissionHolds, snapshot.Applied[commandID], worker.calls.Load())
	}
	assertProviderRetryCleanedUp(t, ctx, f, worker.directories())
	// Simulate a local command-queue replay after its portable applied receipt
	// already published. The controller must acknowledge it without a second
	// provider invocation.
	if _, err = f.P.DB.DB.Exec("UPDATE commands SET status='queued',error='' WHERE id=?", commandID); err != nil {
		t.Fatal(err)
	}
	waitProviderRetryCommand(t, ctx, f, commandID, "accepted")
	if worker.calls.Load() != 1 {
		t.Fatalf("applied provider retry replayed probe %d times", worker.calls.Load())
	}
}

func TestProviderRetryFailureAndMalformedResultRetainHoldWithoutTaskBudget(t *testing.T) {
	for _, mode := range []string{"failed", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
			if err != nil {
				t.Fatal(err)
			}
			defer f.P.DB.Close()
			seedReadyTask(t, ctx, f, "guarded")
			key, _ := seedProviderRetryHold(t, ctx, f)
			worker := &providerRetryProbeProvider{mode: mode}
			f.P.Provider = worker
			commandID := "provider-retry-" + mode
			if err = f.P.DB.Submit(store.Command{ID: commandID, Kind: "provider-retry", Target: key}); err != nil {
				t.Fatal(err)
			}
			stop := startProviderRetryController(t, ctx, f)
			defer stop()
			if message := waitProviderRetryCommand(t, ctx, f, commandID, "failed"); message == "" {
				t.Fatal("rejected provider retry recorded no local command outcome")
			}
			snapshot, _, err := f.P.DB.Load()
			if err != nil {
				t.Fatal(err)
			}
			task := snapshot.Tasks["guarded"]
			if _, held := snapshot.ProviderAdmissionHolds[key]; !held || snapshot.Applied[commandID] || task == nil || task.State != model.Ready || task.Attempts != 0 || len(task.FixCycles) != 0 || task.AdvisorUsed {
				t.Fatalf("rejected retry changed durable admission or task recovery state: holds=%#v applied=%t task=%#v", snapshot.ProviderAdmissionHolds, snapshot.Applied[commandID], task)
			}
			if worker.calls.Load() != 1 {
				t.Fatalf("%s probe calls = %d, want one", mode, worker.calls.Load())
			}
			assertProviderRetryCleanedUp(t, ctx, f, worker.directories())
			var events int
			if err = f.P.DB.DB.QueryRow("SELECT COUNT(*) FROM events WHERE kind='provider_admission_probe_rejected' AND message LIKE ?", commandID+" %").Scan(&events); err != nil || events != 1 {
				t.Fatalf("rejected provider retry outcome event = %d, %v", events, err)
			}
		})
	}
}

func configureTwoWriterProviderRetryFixture(t *testing.T, ctx context.Context, f *demo.Fixture) {
	t.Helper()
	f.Project.MaxWriters = 2
	f.Project.Scheduling.TargetWriters = 2
	encoded, err := yaml.Marshal(f.Project)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.Source, ".aih", "project.yaml")
	if err = os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	source := gitx.Git{Dir: f.Source}
	for _, args := range [][]string{{"add", ".aih/project.yaml"}, {"commit", "-m", "Configure two provider retry writers"}, {"push", "origin", "HEAD:main"}} {
		if _, err = source.Run(ctx, "", args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.P.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	f.P.Config.Project.MaxWriters = 2
	f.P.Config.Project.Scheduling.TargetWriters = 2
}

func TestProviderRetryDefersWhileAnotherProviderRunIsActive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	configureTwoWriterProviderRetryFixture(t, ctx, f)
	seedReadyTask(t, ctx, f, "first")
	seedReadyTask(t, ctx, f, "second")
	key, _ := seedProviderRetryHold(t, ctx, f)
	// Start with no hold so both writers can begin. The first writer then
	// publishes the real typed hold while the second remains active.
	snapshot, stateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	delete(snapshot.ProviderAdmissionHolds, key)
	next, err := f.P.Git.StateCommit(ctx, stateHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	if err = f.P.DB.Save(next, snapshot); err != nil {
		t.Fatal(err)
	}
	const commandID = "provider-retry-deferred"
	worker := &deferredProvider{secondStarted: make(chan struct{}), releaseSecond: make(chan struct{})}
	f.P.Provider = worker
	stop := startProviderRetryController(t, ctx, f)
	defer stop()
	waitProviderAdmissionHold(t, ctx, f)
	if err = f.P.DB.Submit(store.Command{ID: commandID, Kind: "provider-retry", Target: key}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-time.After(900 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if worker.probeCalls.Load() != 0 {
		t.Fatalf("provider retry ran while another provider run was active: %d", worker.probeCalls.Load())
	}
	var status string
	if err = f.P.DB.DB.QueryRow("SELECT status FROM commands WHERE id=?", commandID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("provider retry was not deferred: status=%q err=%v", status, err)
	}
	close(worker.releaseSecond)
	waitProviderRetryCommand(t, ctx, f, commandID, "accepted")
	if worker.probeCalls.Load() != 1 {
		t.Fatalf("provider retry calls after active run exit = %d, want one", worker.probeCalls.Load())
	}
}
