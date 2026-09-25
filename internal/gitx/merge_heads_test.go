package gitx_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
)

func TestMergeHeadsCombinesTwoHeadsInCallerOrder(t *testing.T) {
	ctx := context.Background()
	f := mergeHeadsFixture(t, ctx)
	base := remoteMain(t, ctx, f)
	first := taskHead(t, ctx, f, "aih/first", "first.txt", "first\n", base)
	second := taskHead(t, ctx, f, "aih/second", "second.txt", "second\n", base)

	merged, err := f.P.Git.MergeHeads(ctx, base, []string{first, second}, "Batch merge")
	if err != nil {
		t.Fatal(err)
	}
	assertMergedFiles(t, ctx, f.P.Git, merged, map[string]string{"first.txt": "first", "second.txt": "second"})
	assertOriginalHeadsAreAncestors(t, ctx, f.P.Git, merged, first, second)
	if got := mergeChainHeads(t, ctx, f.P.Git, base, merged); !reflect.DeepEqual(got, []string{first, second}) {
		t.Fatalf("merge chain heads = %v, want [%s %s]", got, first, second)
	}

	reversed, err := f.P.Git.MergeHeads(ctx, base, []string{second, first}, "Batch merge")
	if err != nil {
		t.Fatal(err)
	}
	if got := mergeChainHeads(t, ctx, f.P.Git, base, reversed); !reflect.DeepEqual(got, []string{second, first}) {
		t.Fatalf("reversed merge chain heads = %v, want [%s %s]", got, second, first)
	}
	tree, err := f.P.Git.Run(ctx, "", "rev-parse", merged+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	reversedTree, err := f.P.Git.Run(ctx, "", "rev-parse", reversed+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	if tree != reversedTree {
		t.Fatalf("independent heads produced different trees: %s != %s", tree, reversedTree)
	}
}

func TestMergeHeadsCombinesThreeHeads(t *testing.T) {
	ctx := context.Background()
	f := mergeHeadsFixture(t, ctx)
	base := remoteMain(t, ctx, f)
	first := taskHead(t, ctx, f, "aih/first", "first.txt", "first\n", base)
	second := taskHead(t, ctx, f, "aih/second", "second.txt", "second\n", base)
	third := taskHead(t, ctx, f, "aih/third", "third.txt", "third\n", base)

	merged, err := f.P.Git.MergeHeads(ctx, base, []string{first, second, third}, "Batch merge")
	if err != nil {
		t.Fatal(err)
	}
	assertMergedFiles(t, ctx, f.P.Git, merged, map[string]string{"first.txt": "first", "second.txt": "second", "third.txt": "third"})
	assertOriginalHeadsAreAncestors(t, ctx, f.P.Git, merged, first, second, third)
	if got := mergeChainHeads(t, ctx, f.P.Git, base, merged); !reflect.DeepEqual(got, []string{first, second, third}) {
		t.Fatalf("merge chain heads = %v, want [%s %s %s]", got, first, second, third)
	}
}

func TestMergeHeadsRejectsConflictsAndInvalidBasesWithoutAdvancingRefs(t *testing.T) {
	ctx := context.Background()
	t.Run("conflict", func(t *testing.T) {
		f := mergeHeadsFixture(t, ctx)
		base := remoteMain(t, ctx, f)
		first := taskHead(t, ctx, f, "aih/first", "README.md", "first\n", base)
		second := taskHead(t, ctx, f, "aih/second", "README.md", "second\n", base)
		before := refs(t, ctx, f.P.Git)
		if _, err := f.P.Git.MergeHeads(ctx, base, []string{first, second}, "Batch merge"); err == nil || !strings.Contains(err.Error(), "conflicts") {
			t.Fatalf("conflicting batch merge error = %v", err)
		}
		if after := refs(t, ctx, f.P.Git); after != before {
			t.Fatalf("conflicting batch merge advanced refs:\nbefore %s\nafter  %s", before, after)
		}
	})
	t.Run("stale shared base", func(t *testing.T) {
		f := mergeHeadsFixture(t, ctx)
		oldBase := remoteMain(t, ctx, f)
		advanceMain(t, ctx, f)
		newBase := remoteMain(t, ctx, f)
		first := taskHead(t, ctx, f, "aih/first", "first.txt", "first\n", newBase)
		second := taskHead(t, ctx, f, "aih/second", "second.txt", "second\n", newBase)
		before := refs(t, ctx, f.P.Git)
		if _, err := f.P.Git.MergeHeads(ctx, oldBase, []string{first, second}, "Batch merge"); err == nil || !strings.Contains(err.Error(), "share the verified base") {
			t.Fatalf("stale base error = %v", err)
		}
		if after := refs(t, ctx, f.P.Git); after != before {
			t.Fatalf("stale batch merge advanced refs:\nbefore %s\nafter  %s", before, after)
		}
	})
	t.Run("unrelated base", func(t *testing.T) {
		f := mergeHeadsFixture(t, ctx)
		base := remoteMain(t, ctx, f)
		first := taskHead(t, ctx, f, "aih/first", "first.txt", "first\n", base)
		second := taskHead(t, ctx, f, "aih/second", "second.txt", "second\n", base)
		unrelated, err := f.P.Git.Run(ctx, "unrelated\n", "commit-tree", base+"^{tree}")
		if err != nil {
			t.Fatal(err)
		}
		before := refs(t, ctx, f.P.Git)
		if _, err = f.P.Git.MergeHeads(ctx, unrelated, []string{first, second}, "Batch merge"); err == nil || !strings.Contains(err.Error(), "not synchronized") {
			t.Fatalf("unrelated base error = %v", err)
		}
		if after := refs(t, ctx, f.P.Git); after != before {
			t.Fatalf("unrelated batch merge advanced refs:\nbefore %s\nafter  %s", before, after)
		}
	})
	t.Run("nonadjacent shared post-base ancestor", func(t *testing.T) {
		f := mergeHeadsFixture(t, ctx)
		base := remoteMain(t, ctx, f)
		shared := taskHead(t, ctx, f, "aih/shared", "shared.txt", "shared\n", base)
		first := taskHead(t, ctx, f, "aih/first", "first.txt", "first\n", shared)
		second := taskHead(t, ctx, f, "aih/second", "second.txt", "second\n", base)
		third := taskHead(t, ctx, f, "aih/third", "third.txt", "third\n", shared)
		before := refs(t, ctx, f.P.Git)
		if _, err := f.P.Git.MergeHeads(ctx, base, []string{first, second, third}, "Batch merge"); err == nil || !strings.Contains(err.Error(), "share the verified base") {
			t.Fatalf("nonadjacent shared ancestor error = %v", err)
		}
		if after := refs(t, ctx, f.P.Git); after != before {
			t.Fatalf("invalid three-head batch merge advanced refs:\nbefore %s\nafter  %s", before, after)
		}
	})
}

func mergeHeadsFixture(t *testing.T, ctx context.Context) *demo.Fixture {
	t.Helper()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.P.DB.Close() })
	return f
}

func remoteMain(t *testing.T, ctx context.Context, f *demo.Fixture) string {
	t.Helper()
	sha, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func taskHead(t *testing.T, ctx context.Context, f *demo.Fixture, branch, name, content, base string) string {
	t.Helper()
	dir := filepath.Join(f.P.Dir, "worktrees", strings.ReplaceAll(branch, "/", "-"))
	if err := f.P.Git.Worktree(ctx, dir, branch, base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.P.Git.RemoveWorktree(context.Background(), dir) })
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	head, err := f.P.Git.Checkpoint(ctx, dir, branch)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func advanceMain(t *testing.T, ctx context.Context, f *demo.Fixture) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.Source, "advanced.txt"), []byte("advanced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := gitx.Git{Dir: f.Source}
	for _, args := range [][]string{{"add", "advanced.txt"}, {"commit", "-m", "advance main"}, {"push", "origin", "HEAD:main"}} {
		if _, err := source.Run(ctx, "", args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.P.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertMergedFiles(t *testing.T, ctx context.Context, g gitx.Git, head string, files map[string]string) {
	t.Helper()
	for name, want := range files {
		got, err := g.Show(ctx, head, name)
		if err != nil || got != want {
			t.Fatalf("merged %s = %q, want %q (error %v)", name, got, want, err)
		}
	}
}

func assertOriginalHeadsAreAncestors(t *testing.T, ctx context.Context, g gitx.Git, merged string, heads ...string) {
	t.Helper()
	for _, head := range heads {
		if !g.Ancestor(ctx, head, merged) {
			t.Fatalf("original head %s is not an ancestor of %s", head, merged)
		}
	}
}

func mergeChainHeads(t *testing.T, ctx context.Context, g gitx.Git, base, head string) []string {
	t.Helper()
	var reversed []string
	for head != base {
		parents, err := g.Run(ctx, "", "show", "-s", "--format=%P", head)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Fields(parents)
		if len(parts) != 2 {
			t.Fatalf("batch merge commit %s parents = %v", head, parts)
		}
		reversed = append(reversed, parts[1])
		head = parts[0]
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed
}

func refs(t *testing.T, ctx context.Context, g gitx.Git) string {
	t.Helper()
	refs, err := g.Run(ctx, "", "show-ref", "--head")
	if err != nil {
		t.Fatal(err)
	}
	return refs
}
