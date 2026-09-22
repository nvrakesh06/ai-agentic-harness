package gitx_test

import (
	"context"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicPublicationRejectsStaleMainAndState(t *testing.T) {
	ctx := context.Background()
	f, e := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if e != nil {
		t.Fatal(e)
	}
	defer f.P.DB.Close()
	g := f.P.Git
	base, e := g.SHA(ctx, "refs/remotes/origin/main")
	if e != nil {
		t.Fatal(e)
	}
	s, h, e := g.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	makeCommit := func(text string) string {
		t.Helper()
		tree, e := g.Run(ctx, "", "rev-parse", base+"^{tree}")
		if e != nil {
			t.Fatal(e)
		}
		sha, e := g.Run(ctx, text, "commit-tree", tree, "-p", base)
		if e != nil {
			t.Fatal(e)
		}
		return sha
	}
	other := makeCommit("external main update")
	candidate := makeCommit("verified candidate")
	if e = g.Publish(ctx, []gitx.Update{{Branch: "main", Old: base, New: other}}); e != nil {
		t.Fatal(e)
	}
	s.Revision++
	newState, e := g.StateCommit(ctx, h, s)
	if e != nil {
		t.Fatal(e)
	}
	if e = g.Publish(ctx, []gitx.Update{{Branch: "main", Old: base, New: candidate}, {Branch: "aih-state", Old: h, New: newState}}); e == nil {
		t.Fatal("stale main accepted")
	}
	actual, _ := g.RemoteHead(ctx, "aih-state")
	if actual != h {
		t.Fatal("state partially published")
	}
	if e = g.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: h, New: newState}}); e != nil {
		t.Fatal(e)
	}
	s.Controller = model.Lease{Owner: "stale", Epoch: 1}
	staleState, _ := g.StateCommit(ctx, h, s)
	tree, _ := g.Run(ctx, "", "rev-parse", other+"^{tree}")
	advancing, _ := g.Run(ctx, "valid descendant candidate", "commit-tree", tree, "-p", other)
	if e = g.Publish(ctx, []gitx.Update{{Branch: "main", Old: other, New: advancing}, {Branch: "aih-state", Old: h, New: staleState}}); e == nil {
		t.Fatal("stale controller accepted")
	}
	actual, _ = g.RemoteHead(ctx, "main")
	if actual != other {
		t.Fatal("main partially changed on lease loss")
	}
	if e = g.Publish(ctx, []gitx.Update{{Branch: "main", Old: other, New: other}}); e == nil {
		t.Fatal("no-op fence accepted")
	}
}

func TestConflictRecoveryAndCheckpointMarkers(t *testing.T) {
	ctx := context.Background()
	f, e := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if e != nil {
		t.Fatal(e)
	}
	defer f.P.DB.Close()
	g := f.P.Git
	dir := filepath.Join(f.P.Dir, "worktrees", "conflict")
	if e = g.Worktree(ctx, dir, "aih/conflict", "refs/remotes/origin/main"); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "README.md"), []byte("task\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = g.Checkpoint(ctx, dir, "conflict"); e != nil {
		t.Fatal(e)
	}
	source := gitx.Git{Dir: f.Source}
	if e = os.WriteFile(filepath.Join(f.Source, "README.md"), []byte("main\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = source.Run(ctx, "", "add", "README.md"); e != nil {
		t.Fatal(e)
	}
	if _, e = source.Run(ctx, "", "commit", "-m", "external overlapping change"); e != nil {
		t.Fatal(e)
	}
	if _, e = source.Run(ctx, "", "push", "origin", "main"); e != nil {
		t.Fatal(e)
	}
	if e = g.Fetch(ctx); e != nil {
		t.Fatal(e)
	}
	base, e := g.SHA(ctx, "refs/remotes/origin/main")
	if e != nil {
		t.Fatal(e)
	}
	if e = g.Rebase(ctx, dir, base); e == nil {
		t.Fatal("expected conflict")
	}
	if e = g.PrepareMerge(ctx, dir, base); e != nil {
		t.Fatal(e)
	}
	if _, e = g.Checkpoint(ctx, dir, "conflict"); e == nil {
		t.Fatal("unresolved markers checkpointed")
	}
	if e = os.WriteFile(filepath.Join(dir, "README.md"), []byte("resolved task and main\n"), 0600); e != nil {
		t.Fatal(e)
	}
	head, e := g.Checkpoint(ctx, dir, "conflict")
	if e != nil {
		t.Fatal(e)
	}
	if !g.Ancestor(ctx, base, head) {
		t.Fatal("resolved checkpoint lost main ancestry")
	}
}
func TestWorktreeCheckpointAndRebase(t *testing.T) {
	ctx := context.Background()
	f, e := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if e != nil {
		t.Fatal(e)
	}
	defer f.P.DB.Close()
	g := f.P.Git
	dir := filepath.Join(f.P.Dir, "worktrees", "test")
	if e = g.Worktree(ctx, dir, "aih/1-test", "refs/remotes/origin/main"); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("done\n"), 0600); e != nil {
		t.Fatal(e)
	}
	sha, e := g.Checkpoint(ctx, dir, "test")
	if e != nil {
		t.Fatal(e)
	}
	if e = g.Publish(ctx, []gitx.Update{{Branch: "aih/1-test", New: sha}}); e != nil {
		t.Fatal(e)
	}
	if e = g.Worktree(ctx, dir, "aih/1-test", "refs/remotes/origin/main"); e != nil {
		t.Fatal("not idempotent", e)
	}
	if e = os.WriteFile(filepath.Join(dir, ".env"), []byte("secret"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = g.Checkpoint(ctx, dir, "test"); e == nil {
		t.Fatal("secret path checkpointed")
	}
}

func TestPushAndLoadDoNotWriteFetchTrackingRefs(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	g := f.P.Git
	if err = g.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	s, before, err := g.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Revision++
	next, err := g.StateCommit(ctx, before, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: before, New: next}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = g.Load(ctx); err != nil {
		t.Fatal(err)
	}
	observed, err := g.SHA(ctx, "refs/remotes/origin/aih-state")
	if err != nil || observed != before {
		t.Fatal("push/Load unexpectedly wrote Fetch's tracking refs", observed, err)
	}
	if err = g.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	observed, err = g.SHA(ctx, "refs/remotes/origin/aih-state")
	if err != nil || observed != next {
		t.Fatal("explicit Fetch did not update tracking ref", observed, err)
	}
}
