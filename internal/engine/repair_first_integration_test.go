package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

const repairFirstNativeLog = "AIH_REPAIR_FIRST_NATIVE_LOG"

// TestRepairFirstNativeHelper is a disposable supervisor-native check. The
// parent integration test uses its file receipt to prove ordering without
// treating provider time as a native-check measurement.
func TestRepairFirstNativeHelper(t *testing.T) {
	path := os.Getenv(repairFirstNativeLog)
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err = file.WriteString("native\n"); err != nil {
		t.Fatal(err)
	}
}

type repairFirstProvider struct {
	nativeLog               string
	implementations         atomic.Int32
	reviewers               atomic.Int32
	security                atomic.Int32
	qa                      atomic.Int32
	implementedBeforeNative atomic.Bool
}

func (*repairFirstProvider) Name() string                   { return "codex" }
func (*repairFirstProvider) Validate(context.Context) error { return nil }
func (p *repairFirstProvider) Run(_ context.Context, request provider.Request) (provider.Result, error) {
	task, err := demo.Task(request.Prompt)
	if err != nil {
		return provider.Result{}, err
	}
	if request.Role == "implementer" {
		attempt := p.implementations.Add(1)
		if attempt > 1 {
			if content, readErr := os.ReadFile(p.nativeLog); readErr == nil && len(content) == 0 {
				p.implementedBeforeNative.Store(true)
			}
			if err = os.WriteFile(filepath.Join(request.Directory, "feature-"+task.Title+".txt"), []byte("repaired\n"), 0o600); err != nil {
				return provider.Result{}, err
			}
			return provider.Result{Schema: 1, Status: "completed", Summary: "repaired retained review defect"}, nil
		}
		if err = os.WriteFile(filepath.Join(request.Directory, "feature-"+task.Title+".txt"), []byte("rejected\n"), 0o600); err != nil {
			return provider.Result{}, err
		}
		return provider.Result{Schema: 1, Status: "completed", Summary: "initial rejected implementation"}, nil
	}
	switch request.Role {
	case "reviewer":
		if p.reviewers.Add(1) == 1 {
			return provider.Result{Schema: 1, Status: "completed", Summary: "reviewer retained a blocking defect", Findings: []model.Finding{{Severity: "high", Category: "correctness", Location: "feature-planner.txt:1", Reason: "the review retained a concrete draft-loss defect", Resolution: "repair the saved draft", Relevance: model.FindingChanged}}}, nil
		}
	case "security":
		p.security.Add(1)
	case "qa":
		p.qa.Add(1)
	}
	return provider.Result{Schema: 1, Status: "completed", Summary: request.Role + " accepted repaired head"}, nil
}

func TestRepairFirstAnswerSynchronizesBeforeWriterAndRechecksChangedHead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	nativeLog := filepath.Join(t.TempDir(), "native.log")
	if err := os.WriteFile(nativeLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(repairFirstNativeLog, nativeLog)
	f, err := demo.New(ctx, t.TempDir(), []string{os.Args[0], "-test.run=^TestRepairFirstNativeHelper$"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	seedReadyTask(t, ctx, f, "planner")
	snapshot, stateRef, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Tasks["planner"].Risk = "high"
	next, err := f.P.Git.StateCommit(ctx, stateRef, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateRef, New: next}}); err != nil {
		t.Fatal(err)
	}

	// This first run uses assessReviews, preserveReviewFindings, and retry to
	// create the concrete and controller-summary receipts. The recovery below
	// deliberately consumes only those persisted artifacts.
	workers := &repairFirstProvider{nativeLog: nativeLog}
	f.P.Provider = workers
	initial := runUntilTaskState(t, ctx, f, "planner", model.Fix)
	if workers.implementations.Load() != 1 || initial.Evidence == nil || initial.Evidence.Head != initial.HeadSHA {
		t.Fatalf("initial review lifecycle did not reach retained finding: task=%#v implementer=%d", initial, workers.implementations.Load())
	}
	var concreteReceipt, summaryReceipt bool
	for _, receipt := range initial.ReviewFindingProvenance {
		if receipt.SourceTask != initial.ID || receipt.Base != initial.BaseSHA || receipt.Head != initial.HeadSHA || receipt.Role != "reviewer" {
			continue
		}
		if receipt.ControllerSummary {
			summaryReceipt = true
		} else {
			concreteReceipt = true
		}
	}
	if !concreteReceipt || !summaryReceipt || len(initial.Findings) < 2 {
		t.Fatalf("actual review pipeline did not persist concrete and controller receipts: findings=%#v receipts=%#v", initial.Findings, initial.ReviewFindingProvenance)
	}

	// Model the known routed human block without creating or changing any review
	// evidence. Reset the counter only after the original rejecting native run.
	snapshot, stateRef, err = f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := snapshot.Tasks["planner"]
	model.BlockWithOrigin(task, "Confirm the immutable-owner repair can resume.", "owner routing completed while native verification was unavailable", model.SyncRequired, model.BlockerOriginVerificationOnly)
	next, err = f.P.Git.StateCommit(ctx, stateRef, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateRef, New: next}}); err != nil {
		t.Fatal(err)
	}
	if err = f.P.DB.Save(next, snapshot); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(nativeLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "answer", Target: task.ID, Payload: "resume after immutable ownership routing"}); err != nil {
		t.Fatal(err)
	}

	completed := runUntilTaskState(t, ctx, f, task.ID, model.Done)
	count, err := os.ReadFile(nativeLog)
	if err != nil {
		t.Fatal(err)
	}
	if workers.implementations.Load() != 2 || !workers.implementedBeforeNative.Load() || strings.Count(string(count), "native\n") != 1 {
		t.Fatalf("repair-first ordering or native count wrong: implementer=%d before_native=%t native=%q", workers.implementations.Load(), workers.implementedBeforeNative.Load(), count)
	}
	if completed.HeadSHA == initial.HeadSHA || completed.Evidence == nil || completed.Evidence.Head != completed.HeadSHA || completed.RepairFirst != nil || workers.reviewers.Load() < 2 || workers.security.Load() < 2 || workers.qa.Load() == 0 {
		t.Fatalf("changed-head repair did not pass the normal final gates: task=%#v reviewer=%d security=%d qa=%d", completed, workers.reviewers.Load(), workers.security.Load(), workers.qa.Load())
	}
}
