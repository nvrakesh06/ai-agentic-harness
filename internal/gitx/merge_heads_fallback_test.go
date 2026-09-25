package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeHeadsFallbackUsesTemporaryIndexWithoutRefs(t *testing.T) {
	ctx := context.Background()
	t.Run("two heads", func(t *testing.T) {
		g, base := fallbackRepository(t, ctx)
		first := fallbackHead(t, ctx, g, base, "first", "first.txt", "first\n")
		second := fallbackHead(t, ctx, g, base, "second", "second.txt", "second\n")
		merged, err := g.mergeHeads(ctx, base, []string{first, second}, "Batch merge", false)
		if err != nil {
			t.Fatal(err)
		}
		assertFallbackMerged(t, ctx, g, merged, first, second)
		assertFallbackFile(t, ctx, g, merged, "first.txt", "first")
		assertFallbackFile(t, ctx, g, merged, "second.txt", "second")
		assertFallbackIndexCleaned(t, ctx, g)
	})
	t.Run("three heads", func(t *testing.T) {
		g, base := fallbackRepository(t, ctx)
		first := fallbackHead(t, ctx, g, base, "first", "first.txt", "first\n")
		second := fallbackHead(t, ctx, g, base, "second", "second.txt", "second\n")
		third := fallbackHead(t, ctx, g, base, "third", "third.txt", "third\n")
		merged, err := g.mergeHeads(ctx, base, []string{first, second, third}, "Batch merge", false)
		if err != nil {
			t.Fatal(err)
		}
		assertFallbackMerged(t, ctx, g, merged, first, second, third)
		assertFallbackFile(t, ctx, g, merged, "third.txt", "third")
		assertFallbackIndexCleaned(t, ctx, g)
	})
	t.Run("conflict", func(t *testing.T) {
		g, base := fallbackRepository(t, ctx)
		first := fallbackHead(t, ctx, g, base, "first", "README.md", "first\n")
		second := fallbackHead(t, ctx, g, base, "second", "README.md", "second\n")
		before, err := g.Run(ctx, "", "show-ref", "--head")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = g.mergeHeads(ctx, base, []string{first, second}, "Batch merge", false); err == nil || !strings.Contains(err.Error(), "conflicts") {
			t.Fatalf("fallback conflict error = %v", err)
		}
		after, err := g.Run(ctx, "", "show-ref", "--head")
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("fallback conflict advanced refs:\nbefore %s\nafter  %s", before, after)
		}
		assertFallbackIndexCleaned(t, ctx, g)
	})
}

func fallbackRepository(t *testing.T, ctx context.Context) (Git, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	g := Git{Dir: dir}
	if _, err := g.Run(ctx, "", "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "base"}} {
		if _, err := g.Run(ctx, "", args...); err != nil {
			t.Fatal(err)
		}
	}
	base, err := g.SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return g, base
}

func fallbackHead(t *testing.T, ctx context.Context, g Git, base, branch, name, content string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(g.Dir), branch)
	if _, err := g.Run(ctx, "", "worktree", "add", "-b", branch, dir, base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = g.Run(context.Background(), "", "worktree", "remove", "--force", dir) })
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	worktree := Git{Dir: dir}
	for _, args := range [][]string{{"add", name}, {"commit", "-m", branch}} {
		if _, err := worktree.Run(ctx, "", args...); err != nil {
			t.Fatal(err)
		}
	}
	head, err := worktree.SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func assertFallbackMerged(t *testing.T, ctx context.Context, g Git, merged string, heads ...string) {
	t.Helper()
	for _, head := range heads {
		if !g.Ancestor(ctx, head, merged) {
			t.Fatalf("head %s is not an ancestor of %s", head, merged)
		}
	}
}

func assertFallbackFile(t *testing.T, ctx context.Context, g Git, head, name, want string) {
	t.Helper()
	got, err := g.Show(ctx, head, name)
	if err != nil || got != want {
		t.Fatalf("%s = %q, want %q (error %v)", name, got, want, err)
	}
}

func assertFallbackIndexCleaned(t *testing.T, ctx context.Context, g Git) {
	t.Helper()
	gitDir, err := g.Run(ctx, "", "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(gitDir, "aih-merge-index-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("fallback temporary index directories remain: %v", paths)
	}
}
