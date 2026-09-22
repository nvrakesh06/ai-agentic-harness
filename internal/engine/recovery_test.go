package engine_test

import (
	"context"
	"crypto/sha256"
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

func TestPartialGraphRecoversWithoutLocalProject(t *testing.T) {
	ctx := context.Background()
	f, e := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if e != nil {
		t.Fatal(e)
	}
	s, h, e := f.P.Git.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s.Objectives["objective"] = &model.Objective{ID: "objective", Text: "recover", Planned: true}
	base, e := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if e != nil {
		t.Fatal(e)
	}
	updates := []gitx.Update{}
	states := []model.State{model.Done, model.Running, model.Review, model.Blocked, model.Ready}
	for i, state := range states {
		id := string(rune('a' + i))
		task := &model.Task{ID: id, ObjectiveID: "objective", Title: id, State: state, HeadSHA: base, Branch: "aih/" + id, FixCycles: map[string]int{}}
		if state == model.Blocked {
			model.Block(task, "Choose", "decision", model.Ready)
		}
		s.Tasks[id] = task
		updates = append(updates, gitx.Update{Branch: task.Branch, New: base})
	}
	s.Tasks["c"].Verification = &model.Verification{
		Environment: "windows/native/check", HeadSHA: base,
		Fingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte("check failed"))), Attempts: 1,
	}
	next, e := f.P.Git.StateCommit(ctx, h, s)
	if e != nil {
		t.Fatal(e)
	}
	updates = append(updates, gitx.Update{Branch: "aih-state", Old: h, New: next})
	if e = f.P.Git.Publish(ctx, updates); e != nil {
		t.Fatal(e)
	}
	dir := f.P.Dir
	if e = f.P.DB.Close(); e != nil {
		t.Fatal(e)
	}
	rel, e := filepath.Rel(f.Root, dir)
	if e != nil || rel == "." || strings.HasPrefix(rel, "..") {
		t.Fatal("unsafe test deletion")
	}
	if e = os.RemoveAll(dir); e != nil {
		t.Fatal(e)
	}
	p, e := f.Open(ctx, filepath.Join(f.Root, "machine-b"))
	if e != nil {
		t.Fatal(e)
	}
	defer p.DB.Close()
	if e = p.Attach(ctx); e != nil {
		t.Fatal(e)
	}
	recovered, _, e := p.DB.Load()
	if e != nil {
		t.Fatal(e)
	}
	for id, task := range s.Tasks {
		if recovered.Tasks[id].State != task.State {
			t.Fatal("lost state", id)
		}
		if task.Verification != nil && (recovered.Tasks[id].Verification == nil || recovered.Tasks[id].Verification.Fingerprint != task.Verification.Fingerprint) {
			t.Fatal("lost verification retry guard", id)
		}
		if task.State != model.Done {
			if _, e = os.Stat(filepath.Join(p.TaskPath(task), "README.md")); e != nil {
				t.Fatal("missing recovered source", e)
			}
		}
	}
	// Stop is queued before scheduling: recovery transitions are still exercised.
	if e = p.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"}); e != nil {
		t.Fatal(e)
	}
	if e = engine.New(p).Serve(ctx); e != nil {
		t.Fatal(e)
	}
	recovered, _, e = p.DB.Load()
	if e != nil {
		t.Fatal(e)
	}
	if recovered.Tasks["b"].State != model.Ready || recovered.Tasks["c"].State != model.SyncRequired || recovered.Tasks["d"].Blocker == nil {
		t.Fatal("interruption recovery incorrect")
	}
	if recovered.Tasks["c"].Verification == nil {
		t.Fatal("interruption recovery lost verification retry guard")
	}
}

func init() {
	if len(os.Args) > 1 && os.Args[1] == "_aih-post-check" {
		if os.Getenv("AIH_FAIL_POST_CHECK") == "1" {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestPostVerifyHoldAndHumanRetry(t *testing.T) {
	t.Setenv("AIH_FAIL_POST_CHECK", "1")
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, e := demo.New(ctx, t.TempDir(), []string{exe, "_aih-post-check"})
	if e != nil {
		t.Fatal(e)
	}
	defer f.P.DB.Close()
	s, h, e := f.P.Git.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	base, e := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if e != nil {
		t.Fatal(e)
	}
	s.Objectives["objective"] = &model.Objective{ID: "objective", Planned: true}
	s.Tasks["task"] = &model.Task{ID: "task", ObjectiveID: "objective", State: model.PostVerify, MergeSHA: base, HeadSHA: base, Branch: "aih/task", FixCycles: map[string]int{}}
	next, e := f.P.Git.StateCommit(ctx, h, s)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: h, New: next}}); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}()
	for {
		s, _, e = f.P.DB.Load()
		if e == nil && s.IntegrationBlocked == "task" {
			break
		}
		select {
		case e = <-done:
			t.Fatal("supervisor exited", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if s.Tasks["task"].State != model.Blocked || s.Tasks["task"].Blocker.Resume != model.PostVerify {
		t.Fatal("post-verify did not fail closed")
	}
	t.Setenv("AIH_FAIL_POST_CHECK", "0")
	if e = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "answer", Target: "task", Payload: "The verification environment is repaired; recheck."}); e != nil {
		t.Fatal(e)
	}
	for {
		s, _, e = f.P.DB.Load()
		if e == nil && s.Tasks["task"].State == model.Done {
			break
		}
		select {
		case e = <-done:
			t.Fatal("supervisor exited", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if s.IntegrationBlocked != "" {
		t.Fatal("hold not cleared")
	}
	_ = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"})
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
