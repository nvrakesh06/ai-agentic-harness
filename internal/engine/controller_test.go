package engine_test

import (
	"context"
	"errors"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitStarted(t *testing.T, p *engine.Project) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p.DB.Get("pid") != "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("controller failed to start")
}
func TestLiveLeaseRejectedAndExpiredLeaseRecovered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, e := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if e != nil {
		t.Fatal(e)
	}
	defer f.P.DB.Close()
	a := engine.New(f.P)
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	waitStarted(t, f.P)
	p2, e := f.Open(ctx, filepath.Join(f.Root, "machine-b"))
	if e != nil {
		t.Fatal(e)
	}
	defer p2.DB.Close()
	e = engine.New(p2).Serve(ctx)
	if !errors.Is(e, engine.ErrLease) {
		t.Fatal("live controller not rejected", e)
	}
	if e = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"}); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	s, h, e := p2.Git.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	oldEpoch := s.Controller.Epoch
	s.Controller.Owner = "dead-controller"
	s.Controller.Expires = time.Now().Add(-time.Minute)
	next, e := p2.Git.StateCommit(ctx, h, s)
	if e != nil {
		t.Fatal(e)
	}
	if e = p2.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: h, New: next}}); e != nil {
		t.Fatal(e)
	}
	go func() { done <- engine.New(p2).Serve(ctx) }()
	waitStarted(t, p2)
	fresh, _, e := p2.Git.Load(ctx)
	if e != nil || fresh.Controller.Epoch <= oldEpoch {
		t.Fatal("lease epoch not advanced", e)
	}
	_ = p2.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"})
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}

func TestHandoffPreservesUnresolvedPendingMergeAndReleasesLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	base, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	task := &model.Task{ID: "pending", ObjectiveID: "objective", State: model.Fix, Branch: "aih/pending", BaseSHA: base, AssignedAreas: []string{"README.md"}, AssignedAreaKinds: map[string]string{"README.md": model.AreaFile}, FixCycles: map[string]int{}}
	dir := f.P.TaskPath(task)
	if err = f.P.Git.Worktree(ctx, dir, task.Branch, base); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "README.md"), []byte("task change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task.HeadSHA, err = f.P.Git.Checkpoint(ctx, dir, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: task.Branch, New: task.HeadSHA}}); err != nil {
		t.Fatal(err)
	}
	source := gitx.Git{Dir: f.Source}
	if err = os.WriteFile(filepath.Join(f.Source, "README.md"), []byte("main change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Run(ctx, "", "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Run(ctx, "", "commit", "-m", "main conflict"); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Run(ctx, "", "push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	target, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	s, stateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task.SyncBase = target
	s.Objectives[task.ObjectiveID] = &model.Objective{ID: task.ObjectiveID, Planned: true}
	s.Tasks[task.ID] = task
	next, err := f.P.Git.StateCommit(ctx, stateHead, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	conflict, err := f.P.Git.PrepareTaskMerge(ctx, dir, task.HeadSHA, target)
	if err != nil || !conflict {
		t.Fatalf("prepare pending conflict = conflict %t, error %v", conflict, err)
	}
	beforeMerge, err := (gitx.Git{Dir: dir}).SHA(ctx, "MERGE_HEAD")
	if err != nil || beforeMerge != target {
		t.Fatalf("pending merge = %q, want %q, error %v", beforeMerge, target, err)
	}
	beforeUnmerged, err := (gitx.Git{Dir: dir}).Run(ctx, "", "diff", "--name-only", "--diff-filter=U")
	if err != nil || beforeUnmerged == "" {
		t.Fatalf("unmerged paths = %q, error %v", beforeUnmerged, err)
	}
	beforeContent, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	if err = engine.New(f.P).Serve(ctx); err != nil {
		t.Fatalf("handoff rejected preserved local merge: %v", err)
	}
	after, _, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Controller.Owner != "" || after.Capacity.State != "stopped" {
		t.Fatalf("handoff did not release controller: %#v %#v", after.Controller, after.Capacity)
	}
	preserved := after.Tasks[task.ID]
	if preserved == nil || preserved.State != model.Fix || preserved.BaseSHA != base || preserved.HeadSHA != task.HeadSHA || preserved.SyncBase != target {
		t.Fatalf("handoff changed pending merge state: %#v", preserved)
	}
	afterMerge, err := (gitx.Git{Dir: dir}).SHA(ctx, "MERGE_HEAD")
	afterUnmerged, unmergedErr := (gitx.Git{Dir: dir}).Run(ctx, "", "diff", "--name-only", "--diff-filter=U")
	afterContent, contentErr := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil || afterMerge != beforeMerge || unmergedErr != nil || afterUnmerged != beforeUnmerged || contentErr != nil || string(afterContent) != string(beforeContent) {
		t.Fatalf("handoff changed local conflict: merge=%q/%q err=%v unmerged=%q/%q err=%v content=%q/%q err=%v", afterMerge, beforeMerge, err, afterUnmerged, beforeUnmerged, unmergedErr, afterContent, beforeContent, contentErr)
	}
	remoteHead, err := f.P.Git.RemoteHead(ctx, task.Branch)
	if err != nil || remoteHead != task.HeadSHA {
		t.Fatalf("handoff published a conflict marker checkpoint: head=%q want=%q err=%v", remoteHead, task.HeadSHA, err)
	}
}
