package engine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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
)

type reviewWaveProvider struct {
	auditedReviewer    atomic.Int32
	auditedSecurity    atomic.Int32
	auditedQA          atomic.Int32
	independent        atomic.Int32
	independentStarted chan struct{}
	releaseIndependent chan struct{}
	independentOnce    sync.Once
	qaPeerFailure      atomic.Value
}

type mixedReviewDeadlineProvider struct {
	securityCalls atomic.Int32
	qaCalls       atomic.Int32
	firstTimeout  chan struct{}
}

// providerHoldPeerWaveProvider controls the two independent review peers around
// the reader reservation boundary. The second peer pauses only after its first
// admission check; the first peer then publishes the durable hold before that
// second peer can reserve MaxReaders=1.
type providerHoldPeerWaveProvider struct {
	readerCalls             atomic.Int32
	qaCalls                 atomic.Int32
	firstReader             chan struct{}
	secondPassedInitialGate chan struct{}
	releaseFirstReader      chan struct{}
	releaseSecondReader     chan struct{}
	callbackMu              sync.Mutex
	firstReaderRole         string
	secondOnce              sync.Once
}

func (*mixedReviewDeadlineProvider) Name() string                   { return "codex" }
func (*mixedReviewDeadlineProvider) Validate(context.Context) error { return nil }
func (p *mixedReviewDeadlineProvider) Run(ctx context.Context, request provider.Request) (provider.Result, error) {
	task, err := demo.Task(request.Prompt)
	if err != nil {
		return provider.Result{}, err
	}
	if request.Role == "implementer" {
		if err := os.WriteFile(filepath.Join(request.Directory, "feature-"+task.Title+".txt"), []byte("implemented\n"), 0o600); err != nil {
			return provider.Result{}, err
		}
		return provider.Result{Schema: 1, Status: "completed", Summary: "implemented " + task.Title}, nil
	}
	if task.Title != "mixed" {
		return provider.Result{Schema: 1, Status: "completed"}, nil
	}
	switch request.Role {
	case "reviewer":
		return provider.Result{Schema: 1, Status: "completed", Summary: "P1 regression", Findings: []model.Finding{{Severity: "high", Category: "regression", Location: "feature-mixed.txt:1", Reason: "observable regression", Resolution: "repair it", Relevance: model.FindingChanged}}}, nil
	case "security":
		if p.securityCalls.Add(1) == 1 {
			close(p.firstTimeout)
			return provider.Result{}, context.DeadlineExceeded
		}
		<-ctx.Done()
		return provider.Result{}, ctx.Err()
	case "qa":
		p.qaCalls.Add(1)
		return provider.Result{}, errors.New("QA ran before the timed-out peer recovered")
	default:
		return provider.Result{Schema: 1, Status: "completed"}, nil
	}
}

func (*providerHoldPeerWaveProvider) Name() string                   { return "codex" }
func (*providerHoldPeerWaveProvider) Validate(context.Context) error { return nil }

func (p *providerHoldPeerWaveProvider) beforeReaderReservation(ctx context.Context, role, taskID string) {
	if taskID != "held-peer-wave" || (role != "reviewer" && role != "security") {
		return
	}
	p.callbackMu.Lock()
	if p.firstReaderRole == "" {
		p.firstReaderRole = role
		p.callbackMu.Unlock()
		return
	}
	p.callbackMu.Unlock()
	p.secondOnce.Do(func() { close(p.secondPassedInitialGate) })
	select {
	case <-p.releaseSecondReader:
	case <-ctx.Done():
	}
}

func (p *providerHoldPeerWaveProvider) Run(ctx context.Context, request provider.Request) (provider.Result, error) {
	task, err := demo.Task(request.Prompt)
	if err != nil {
		return provider.Result{}, err
	}
	if request.Role == "implementer" {
		if err := os.WriteFile(filepath.Join(request.Directory, "feature-"+task.Title+".txt"), []byte("implemented\n"), 0o600); err != nil {
			return provider.Result{}, err
		}
		return provider.Result{Schema: 1, Status: "completed", Summary: "implemented " + task.Title}, nil
	}
	if task.Title != "held-peer-wave" {
		return provider.Result{Schema: 1, Status: "completed"}, nil
	}
	switch request.Role {
	case "reviewer", "security":
		if p.readerCalls.Add(1) != 1 {
			return provider.Result{}, errors.New("queued reader reached provider after a durable admission hold")
		}
		close(p.firstReader)
		select {
		case <-p.releaseFirstReader:
		case <-ctx.Done():
			return provider.Result{}, ctx.Err()
		}
		return provider.Result{}, &provider.InvocationError{Cause: errors.New("fixture schema rejected"), Failure: provider.FailureRequestRejected, Rejection: provider.RejectionInvalidJSONSchema}
	case "qa":
		p.qaCalls.Add(1)
		return provider.Result{}, errors.New("QA ran after an independent provider admission rejection")
	default:
		return provider.Result{Schema: 1, Status: "completed"}, nil
	}
}

func (*reviewWaveProvider) Name() string                   { return "codex" }
func (*reviewWaveProvider) Validate(context.Context) error { return nil }

func (p *reviewWaveProvider) Run(ctx context.Context, request provider.Request) (provider.Result, error) {
	task, err := demo.Task(request.Prompt)
	if err != nil {
		return provider.Result{}, err
	}
	if request.Role == "implementer" {
		if err := os.WriteFile(filepath.Join(request.Directory, "feature-"+task.Title+".txt"), []byte("implemented\n"), 0o600); err != nil {
			return provider.Result{}, err
		}
		if task.Title == "independent" {
			p.independent.Add(1)
			if p.independentStarted != nil {
				p.independentOnce.Do(func() { close(p.independentStarted) })
			}
			if p.releaseIndependent != nil {
				select {
				case <-p.releaseIndependent:
				case <-ctx.Done():
					return provider.Result{}, ctx.Err()
				}
			}
		}
		return provider.Result{Schema: 1, Status: "completed", Summary: "implemented " + task.Title}, nil
	}
	if task.Title != "audited" {
		return provider.Result{Schema: 1, Status: "completed", Summary: "unrelated review"}, nil
	}
	switch request.Role {
	case "reviewer":
		p.auditedReviewer.Add(1)
		return provider.Result{Schema: 1, Status: "completed", Summary: "reviewer exact-head summary"}, nil
	case "security":
		p.auditedSecurity.Add(1)
		return provider.Result{Schema: 1, Status: "completed", Summary: "security exact-head summary"}, nil
	case "qa":
		if p.auditedReviewer.Load() != 1 || p.auditedSecurity.Load() != 1 || !strings.Contains(request.Prompt, `"reviewer":"reviewer exact-head summary"`) || !strings.Contains(request.Prompt, `"security":"security exact-head summary"`) {
			p.qaPeerFailure.Store(fmt.Errorf("QA prompt lacks completed peer artifacts: reviewer=%d security=%d prompt=%q", p.auditedReviewer.Load(), p.auditedSecurity.Load(), request.Prompt))
			return provider.Result{}, fmt.Errorf("QA did not receive completed peer artifacts")
		}
		p.auditedQA.Add(1)
		return provider.Result{Schema: 1, Status: "completed", Summary: "QA exact-head acceptance"}, nil
	default:
		return provider.Result{Schema: 1, Status: "completed"}, nil
	}
}

func TestReviewQAWaveSeesCompletedPeersWhileIndependentWriterRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.P.Config.Project.MaxWriters = 2
	f.P.Config.Project.MaxReaders = 2
	f.Project.MaxWriters = 2
	f.Project.MaxReaders = 2
	f.Project.WorkerSeconds = 120
	configureFixtureRoleTimeouts(t, ctx, f, config.RoleTimeouts{})
	seedReadyTask(t, ctx, f, "audited")
	seedReadyTask(t, ctx, f, "independent")
	snapshot, stateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Tasks["audited"].Risk = "high"
	next, err := f.P.Git.StateCommit(ctx, stateHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	workers := &reviewWaveProvider{independentStarted: make(chan struct{}), releaseIndependent: make(chan struct{})}
	f.P.Provider = workers
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(ctx) }()
	stopped := false
	releaseIndependent := func() {
		select {
		case <-workers.releaseIndependent:
		default:
			close(workers.releaseIndependent)
		}
	}
	defer func() {
		if stopped {
			return
		}
		releaseIndependent()
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("supervisor did not stop after QA fixture failure")
		}
	}()
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	var last *model.Task
	for {
		current, _, loadErr := f.P.DB.Load()
		if loadErr == nil && current.Tasks["audited"] != nil {
			task := current.Tasks["audited"]
			last = task
			evidence := task.Evidence
			qa := evidence != nil && evidence.Reviews["qa"] == "QA exact-head acceptance" && evidence.Head == task.HeadSHA && evidence.ReviewDispositions["qa"].Disposition == "completed" && evidence.ReviewDispositions["qa"].SourceHead == task.HeadSHA
			if qa && (task.State == model.MergeReady || task.State == model.Done) {
				break
			}
		}
		select {
		case serveErr := <-done:
			stopped = true
			t.Fatalf("supervisor stopped before durable QA wave: %v state=%#v qa_failure=%v", serveErr, last, workers.qaPeerFailure.Load())
		case <-deadline.C:
			stacks := make([]byte, 1<<20)
			n := runtime.Stack(stacks, true)
			var evidence *model.Evidence
			if last != nil {
				evidence = last.Evidence
			}
			t.Fatalf("review waves did not durably complete: reviewer=%d security=%d QA=%d independent=%d task_state=%#v evidence=%#v qa_failure=%v goroutines=\n%s", workers.auditedReviewer.Load(), workers.auditedSecurity.Load(), workers.auditedQA.Load(), workers.independent.Load(), last, evidence, workers.qaPeerFailure.Load(), stacks[:n])
		case <-time.After(25 * time.Millisecond):
		}
	}
	if failure := workers.qaPeerFailure.Load(); failure != nil {
		t.Fatal(failure)
	}
	if workers.independent.Load() != 1 {
		t.Fatalf("independent writer did not remain runnable while audited review queued: %d", workers.independent.Load())
	}
	select {
	case <-workers.independentStarted:
	default:
		t.Fatal("independent writer was not occupied while audited QA completed")
	}
	releaseIndependent()
	if err = f.P.DB.Submit(storeCommand("handoff")); err != nil {
		t.Fatal(err)
	}
	serveErr := <-done
	stopped = true
	if serveErr != nil {
		t.Fatal(serveErr)
	}
	current, _, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	evidence := current.Tasks["audited"].Evidence
	if evidence == nil || evidence.Reviews["reviewer"] != "reviewer exact-head summary" || evidence.Reviews["security"] != "security exact-head summary" || evidence.Reviews["qa"] != "QA exact-head acceptance" {
		t.Fatalf("durable wave evidence = %#v", evidence)
	}
}

func TestProviderAdmissionHoldRechecksQueuedReviewPeerAfterReaderReservation(t *testing.T) {
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelSetup()
	f, err := demo.New(setupCtx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	// The canonical file, not only the local project copy, must constrain this
	// wave to one reader so security queues behind the initial reviewer.
	f.P.Config.Project.MaxReaders = 1
	f.Project.MaxReaders = 1
	configureFixtureRoleTimeouts(t, setupCtx, f, config.RoleTimeouts{})
	seedReadyTask(t, setupCtx, f, "held-peer-wave")
	snapshot, stateHead, err := f.P.Git.Load(setupCtx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Tasks["held-peer-wave"].Risk = "high"
	next, err := f.P.Git.StateCommit(setupCtx, stateHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(setupCtx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	cancelSetup()
	workflowCtx, cancelWorkflow := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelWorkflow()
	workers := &providerHoldPeerWaveProvider{
		firstReader:             make(chan struct{}),
		secondPassedInitialGate: make(chan struct{}),
		releaseFirstReader:      make(chan struct{}),
		releaseSecondReader:     make(chan struct{}),
	}
	f.P.Provider = workers
	controller := engine.New(f.P)
	engine.SetBeforeReaderReservationForTest(controller, workers.beforeReaderReservation)
	supervisor := newFixtureSupervisorForController(workflowCtx, controller)
	drained := false
	defer func() {
		if drained {
			return
		}
		if drainErr := supervisor.drain("queued provider admission review fixture cleanup"); drainErr != nil {
			t.Errorf("queued provider admission review fixture supervisor drain: %v", drainErr)
		}
	}()
	select {
	case <-workers.firstReader:
	case <-supervisor.completion():
		drained = true
		t.Fatalf("supervisor stopped before first independent reader rejection: %v", supervisor.completedResult())
	case <-workflowCtx.Done():
		t.Fatalf("reviewer/security wave did not start within bounded workflow budget: %v", workflowCtx.Err())
	}
	select {
	case <-workers.secondPassedInitialGate:
	case <-supervisor.completion():
		drained = true
		t.Fatalf("supervisor stopped before the queued peer passed its initial admission gate: %v", supervisor.completedResult())
	case <-workflowCtx.Done():
		t.Fatalf("queued peer did not pass its initial admission gate: %v", workflowCtx.Err())
	}
	// The first reader remains the only provider invocation while its peer is
	// paused after the first gate. Releasing it creates the typed failure and
	// publishes the hold before the paused peer can reserve the reader slot.
	close(workers.releaseFirstReader)
	_ = waitProviderAdmissionHold(t, workflowCtx, f)
	// The paused peer now proceeds to the reservation. Its required second gate
	// sees the already-durable hold, so it must return without Provider.Run.
	close(workers.releaseSecondReader)
	if got := workers.readerCalls.Load(); got != 1 {
		t.Fatalf("queued independent reader calls = %d, want exactly the initial rejected provider call", got)
	}
	if got := workers.qaCalls.Load(); got != 0 {
		t.Fatalf("QA calls = %d, want no QA after independent provider rejection", got)
	}
	if err = f.P.DB.Submit(storeCommand("handoff")); err != nil {
		t.Fatal(err)
	}
	err = supervisor.waitHandoff("queued provider admission review fixture handoff")
	drained = true
	if err != nil {
		t.Fatal(err)
	}
}

func TestReviewTimeoutPersistsPeerFindingBeforeRetryAndDefersQA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	configureFixtureRoleTimeouts(t, ctx, f, config.RoleTimeouts{Review: 10})
	seedReadyTask(t, ctx, f, "mixed")
	snapshot, stateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Tasks["mixed"].Risk = "high"
	next, err := f.P.Git.StateCommit(ctx, stateHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	workers := &mixedReviewDeadlineProvider{firstTimeout: make(chan struct{})}
	f.P.Provider = workers
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(ctx) }()
	stopped := false
	defer func() {
		if stopped {
			return
		}
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("supervisor did not stop after mixed review fixture failure")
		}
	}()
	select {
	case <-workers.firstTimeout:
	case serveErr := <-done:
		stopped = true
		t.Fatalf("supervisor stopped before review timeout: %v", serveErr)
	case <-time.After(30 * time.Second):
		t.Fatal("security review did not reach its first bounded timeout")
	}
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	var last *model.Task
	for {
		current, _, loadErr := f.P.DB.Load()
		if loadErr == nil {
			task := current.Tasks["mixed"]
			last = task
			if task != nil && task.Evidence != nil && slices.ContainsFunc(task.Findings, func(finding model.Finding) bool {
				return finding.Role == "reviewer" && finding.Severity == "high" && finding.Category == "regression" && finding.Location == "feature-mixed.txt:1" && finding.Relevance == model.FindingChanged
			}) && task.ReadOnlyRetries["review/security"].Attempts == 1 && task.FixCycles["reviewer"] == 1 && workers.qaCalls.Load() == 0 && task.Evidence.ReviewDispositions["reviewer"].Disposition == "" {
				break
			}
		}
		select {
		case serveErr := <-done:
			stopped = true
			t.Fatalf("supervisor stopped before routed review recovery: %v", serveErr)
		case <-deadline.C:
			stacks := make([]byte, 1<<20)
			n := runtime.Stack(stacks, true)
			var evidence *model.Evidence
			if last != nil {
				evidence = last.Evidence
			}
			t.Fatalf("reviewer finding was not durably routed without running QA: security_calls=%d qa_calls=%d task=%#v evidence=%#v goroutines=\n%s", workers.securityCalls.Load(), workers.qaCalls.Load(), last, evidence, stacks[:n])
		case <-time.After(25 * time.Millisecond):
		}
	}
	cancel()
	serveErr := <-done
	stopped = true
	if serveErr != nil {
		t.Fatal(serveErr)
	}
}
