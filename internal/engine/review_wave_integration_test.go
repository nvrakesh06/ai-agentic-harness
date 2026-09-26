package engine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	auditedReviewer atomic.Int32
	auditedSecurity atomic.Int32
	auditedQA       atomic.Int32
	independent     atomic.Int32
	qaPeerFailure   atomic.Value
}

type mixedReviewDeadlineProvider struct {
	securityCalls atomic.Int32
	qaCalls       atomic.Int32
	firstTimeout  chan struct{}
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

func (*reviewWaveProvider) Name() string                   { return "codex" }
func (*reviewWaveProvider) Validate(context.Context) error { return nil }

func (p *reviewWaveProvider) Run(_ context.Context, request provider.Request) (provider.Result, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.P.Config.Project.MaxWriters = 2
	f.P.Config.Project.MaxReaders = 2
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
	workers := &reviewWaveProvider{}
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
			t.Errorf("supervisor did not stop after QA fixture failure")
		}
	}()
	deadline := time.NewTimer(45 * time.Second)
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
			t.Fatalf("review waves did not durably complete: reviewer=%d security=%d QA=%d independent=%d state=%#v qa_failure=%v", workers.auditedReviewer.Load(), workers.auditedSecurity.Load(), workers.auditedQA.Load(), workers.independent.Load(), last, workers.qaPeerFailure.Load())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if failure := workers.qaPeerFailure.Load(); failure != nil {
		t.Fatal(failure)
	}
	if workers.independent.Load() != 1 {
		t.Fatalf("independent writer did not remain runnable while audited review queued: %d", workers.independent.Load())
	}
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
	select {
	case <-workers.firstTimeout:
	case serveErr := <-done:
		t.Fatalf("supervisor stopped before review timeout: %v", serveErr)
	case <-time.After(30 * time.Second):
		t.Fatal("security review did not reach its first bounded timeout")
	}
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		current, _, loadErr := f.P.DB.Load()
		if loadErr == nil {
			task := current.Tasks["mixed"]
			if task != nil && len(task.Findings) == 1 && task.Findings[0].Severity == "high" && task.ReadOnlyRetries["review/security"].Attempts == 1 {
				if task.FixCycles["reviewer"] != 1 || workers.qaCalls.Load() != 0 || task.Evidence.ReviewDispositions["reviewer"].Disposition != "" {
					t.Fatalf("review timeout bypassed finding/auth recovery semantics: task=%#v qa=%d", task, workers.qaCalls.Load())
				}
				break
			}
		}
		select {
		case serveErr := <-done:
			t.Fatalf("supervisor stopped before partial review progress: %v", serveErr)
		case <-deadline.C:
			t.Fatal("partial reviewer finding was not persisted before retry")
		case <-time.After(25 * time.Millisecond):
		}
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
