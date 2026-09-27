package engine_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
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
		p.implementations.Add(1)
		if content, readErr := os.ReadFile(p.nativeLog); readErr == nil && len(content) == 0 {
			p.implementedBeforeNative.Store(true)
		}
		if err = os.WriteFile(filepath.Join(request.Directory, "feature-"+task.Title+".txt"), []byte("repaired\n"), 0o600); err != nil {
			return provider.Result{}, err
		}
		return provider.Result{Schema: 1, Status: "completed", Summary: "repaired retained review defect"}, nil
	}
	switch request.Role {
	case "reviewer":
		p.reviewers.Add(1)
	case "security":
		p.security.Add(1)
	case "qa":
		p.qa.Add(1)
	}
	return provider.Result{Schema: 1, Status: "completed", Summary: request.Role + " accepted repaired head"}, nil
}

func repairFirstFindingFingerprint(finding model.Finding) string {
	payload, _ := json.Marshal(struct {
		Severity, Category, Location, Reason, Resolution, Role, Relevance, BaselineSHA, BaselineEvidence string
	}{finding.Severity, finding.Category, finding.Location, finding.Reason, finding.Resolution, finding.Role, finding.Relevance, finding.BaselineSHA, finding.BaselineEvidence})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
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
	task := snapshot.Tasks["planner"]
	base, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	worktree := f.P.TaskPath(task)
	if err = f.P.Git.Worktree(ctx, worktree, task.Branch, base); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(worktree, "feature-planner.txt"), []byte("rejected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	head, err := f.P.Git.Checkpoint(ctx, worktree, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	finding := model.Finding{Severity: "high", Category: "correctness", Location: "feature-planner.txt:1", Reason: "the review retained a concrete draft-loss defect", Resolution: "repair the saved draft", Role: "reviewer", Relevance: model.FindingChanged}
	summary := model.Finding{Severity: "high", Category: "reviewer", Reason: "reviewer retained a blocking defect", Role: "reviewer", Relevance: model.FindingChanged}
	task.BaseSHA, task.HeadSHA, task.Risk = base, head, "high"
	task.AssignedAreas = []string{"feature-planner.txt"}
	task.AssignedAreaKinds = map[string]string{"feature-planner.txt": model.AreaFile}
	task.Findings = []model.Finding{finding, summary}
	task.Evidence = &model.Evidence{Base: base, Head: head, Config: f.P.Config.Hash, Rules: roles.Hash(), Checks: []string{"stage=native check=\"prior\" command=\"fixture\" command_id=aaaaaaaaaaaa exit=0 pass_counts=\"none\" stdout=empty stdout_bytes=0 stdout_lines=0"}, Reviews: map[string]string{"reviewer": summary.Reason}, ReviewRoster: []string{"reviewer"}}
	task.ReviewFindingProvenance = []model.ReviewFindingProvenance{
		{Finding: repairFirstFindingFingerprint(finding), SourceTask: task.ID, Base: base, Head: head, Config: f.P.Config.Hash, Rules: roles.Hash(), Role: "reviewer"},
		{Finding: repairFirstFindingFingerprint(summary), SourceTask: task.ID, Base: base, Head: head, Config: f.P.Config.Hash, Rules: roles.Hash(), Role: "reviewer", ControllerSummary: true},
	}
	model.BlockWithOrigin(task, "Confirm the immutable-owner repair can resume.", "owner routing completed while native verification was unavailable", model.SyncRequired, model.BlockerOriginVerificationOnly)
	next, err := f.P.Git.StateCommit(ctx, stateRef, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: task.Branch, New: head}, {Branch: "aih-state", Old: stateRef, New: next}}); err != nil {
		t.Fatal(err)
	}
	if err = f.P.DB.Save(next, snapshot); err != nil {
		t.Fatal(err)
	}
	workers := &repairFirstProvider{nativeLog: nativeLog}
	f.P.Provider = workers
	if err = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "answer", Target: task.ID, Payload: "resume after immutable ownership routing"}); err != nil {
		t.Fatal(err)
	}
	completed := runUntilTaskState(t, ctx, f, task.ID, model.Done)
	count, err := os.ReadFile(nativeLog)
	if err != nil {
		t.Fatal(err)
	}
	if workers.implementations.Load() != 1 || !workers.implementedBeforeNative.Load() || strings.Count(string(count), "native\n") != 1 {
		t.Fatalf("repair-first ordering or native count wrong: implementer=%d before_native=%t native=%q", workers.implementations.Load(), workers.implementedBeforeNative.Load(), count)
	}
	if completed.HeadSHA == head || completed.Evidence == nil || completed.Evidence.Head != completed.HeadSHA || completed.RepairFirst != nil || workers.reviewers.Load() == 0 || workers.security.Load() == 0 || workers.qa.Load() == 0 {
		t.Fatalf("changed-head repair did not pass the normal final gates: task=%#v reviewer=%d security=%d qa=%d", completed, workers.reviewers.Load(), workers.security.Load(), workers.qa.Load())
	}
}
