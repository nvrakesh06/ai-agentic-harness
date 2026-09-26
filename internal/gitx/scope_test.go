package gitx_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
)

func TestClassifyAreasAtRefMakesBaseTreeIntentImmutable(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	source := gitx.Git{Dir: f.Source}
	if err := os.MkdirAll(filepath.Join(f.Source, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Source, "docs", "guide.md"), []byte("guide\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Run(ctx, "", "add", "docs/guide.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Run(ctx, "", "commit", "-m", "add docs directory"); err != nil {
		t.Fatal(err)
	}
	areas, err := source.ClassifyAreasAtRef(ctx, "HEAD", []string{"README.md", "planned.md", "docs", "assets/**", `windows\\path/`})
	if err != nil {
		t.Fatal(err)
	}
	want := []gitx.Area{
		{Pattern: "README.md", Kind: gitx.AreaTrackedFile},
		{Pattern: "planned.md", Kind: gitx.AreaExplicitFile},
		{Pattern: "docs", Kind: gitx.AreaDirectory},
		{Pattern: "assets", Kind: gitx.AreaDirectory},
		{Pattern: "windows/path", Kind: gitx.AreaDirectory},
	}
	if !reflect.DeepEqual(areas, want) {
		t.Fatalf("classification = %#v, want %#v", areas, want)
	}

	// A task owns the tracked README file, not a directory a later worker might
	// create at that name. A classified directory does include newly-created files.
	if err := gitx.ValidateScopePaths(areas, []string{"README.md", "planned.md", "docs/new.md", "assets/new.svg", "windows/path/new.txt"}); err != nil {
		t.Fatalf("allowed paths rejected: %v", err)
	}
	if err := gitx.ValidateScopePaths(areas, []string{`docs\new.md`}); err == nil {
		t.Fatal("literal backslash filename bypassed directory scope")
	}
	err = gitx.ValidateScopePaths(areas, []string{"README.md/child", "other.txt", "docs/new.md", "other.txt"})
	var scopeErr *gitx.ScopeError
	if !errors.As(err, &scopeErr) || !reflect.DeepEqual(scopeErr.Paths, []string{"README.md/child", "other.txt"}) {
		t.Fatalf("unexpected rejection: %#v", err)
	}
}

func TestClassifyAreasAtRefRejectsAmbiguousPatterns(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	g := gitx.Git{Dir: f.Source}
	for _, areas := range [][]string{
		{},
		{"src/*.go"},
		{"src/**/file.go"},
		{"../outside"},
		{"C:\\work"},
		{"README.md/"},
		{"docs/", "docs/**"},
	} {
		_, err := g.ClassifyAreasAtRef(ctx, "HEAD", areas)
		var areaErr *gitx.AreaError
		if !errors.As(err, &areaErr) {
			t.Fatalf("areas %q accepted or wrong error: %v", areas, err)
		}
	}
	if _, err := g.ClassifyAreasAtRef(ctx, "does-not-exist", []string{"README.md"}); err == nil {
		t.Fatal("unknown base ref was accepted")
	}
	if err := gitx.ValidateScopePaths([]gitx.Area{{Pattern: "src", Kind: "unknown"}}, nil); err == nil {
		t.Fatal("unknown durable kind was accepted without changed paths")
	}
	if err := gitx.ValidateScopePaths([]gitx.Area{{Pattern: "src/", Kind: gitx.AreaDirectory}}, nil); err == nil {
		t.Fatal("non-canonical durable pattern was accepted")
	}
}

func TestValidateCheckpointScopeIncludesUntrackedAndRenameSides(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	dir := filepath.Join(f.P.Dir, "worktrees", "scope")
	if err := f.P.Git.Worktree(ctx, dir, "aih/scope", "refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	areas, err := f.P.Git.ClassifyAreasAtRef(ctx, "refs/remotes/origin/main", []string{"generated/**"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "generated", "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outside.txt"), []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = f.P.Git.ValidateCheckpointScope(ctx, dir, areas)
	var scopeErr *gitx.ScopeError
	if !errors.As(err, &scopeErr) || !reflect.DeepEqual(scopeErr.Paths, []string{"outside.txt"}) {
		t.Fatalf("untracked out-of-scope file evaded check: %#v", err)
	}
	if err := os.Remove(filepath.Join(dir, "outside.txt")); err != nil {
		t.Fatal(err)
	}
	if err := f.P.Git.ValidateCheckpointScope(ctx, dir, areas); err != nil {
		t.Fatalf("new in-directory file rejected: %v", err)
	}
	if _, err := (gitx.Git{Dir: dir}).Run(ctx, "", "mv", "README.md", "generated/README.md"); err != nil {
		t.Fatal(err)
	}
	err = f.P.Git.ValidateCheckpointScope(ctx, dir, areas)
	if !errors.As(err, &scopeErr) || !strings.Contains(strings.Join(scopeErr.Paths, ","), "README.md") {
		t.Fatalf("rename source escaped scope check: %#v", err)
	}
}

func TestValidateCommitScopeRejectsOutOfAreaCheckpoint(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	dir := filepath.Join(f.P.Dir, "worktrees", "imported")
	if err := f.P.Git.Worktree(ctx, dir, "aih/imported", "refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	base, err := (gitx.Git{Dir: dir}).SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	areas, err := f.P.Git.ClassifyAreasAtRef(ctx, base, []string{"allowed/**"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "allowed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "allowed", "good.txt"), []byte("good\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outside.txt"), []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	head, err := f.P.Git.Checkpoint(ctx, dir, "imported")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.P.Git.ValidateCommitScope(ctx, base, head, areas); err == nil {
		t.Fatal("out-of-area imported checkpoint was accepted")
	}
}

func TestValidateFullCheckpointScopeIncludesEarlierCommits(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	dir := filepath.Join(f.P.Dir, "worktrees", "full-scope")
	if err := f.P.Git.Worktree(ctx, dir, "aih/full-scope", "refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	base, err := (gitx.Git{Dir: dir}).SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	areas, err := f.P.Git.ClassifyAreasAtRef(ctx, base, []string{"allowed/**"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outside.txt"), []byte("first checkpoint\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.P.Git.Checkpoint(ctx, dir, "full-scope"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "allowed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "allowed", "later.txt"), []byte("later\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = f.P.Git.ValidateFullCheckpointScope(ctx, dir, base, areas)
	var scopeErr *gitx.ScopeError
	if !errors.As(err, &scopeErr) || !reflect.DeepEqual(scopeErr.Paths, []string{"outside.txt"}) {
		t.Fatalf("earlier out-of-area checkpoint escaped admission: %#v", err)
	}
}

func TestValidateCommitScopeRejectsFileToDirectoryReplacement(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	dir := filepath.Join(f.P.Dir, "worktrees", "file-replacement")
	if err := f.P.Git.Worktree(ctx, dir, "aih/file-replacement", "refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	base, err := (gitx.Git{Dir: dir}).SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	areas, err := f.P.Git.ClassifyAreasAtRef(ctx, base, []string{"README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "README.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md", "nested.txt"), []byte("nested\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	head, err := f.P.Git.Checkpoint(ctx, dir, "file-replacement")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.P.Git.ValidateCommitScope(ctx, base, head, areas); err == nil {
		t.Fatal("file assignment accepted a file-to-directory replacement")
	}
}

func TestValidateCommitScopeRejectsBehindAndDivergedHeads(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	source := gitx.Git{Dir: f.Source}
	root, err := source.SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Source, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Run(ctx, "", "add", "base.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Run(ctx, "", "commit", "-m", "base child"); err != nil {
		t.Fatal(err)
	}
	base, err := source.SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := source.Run(ctx, "", "rev-parse", root+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	side, err := source.Run(ctx, "sibling\n", "commit-tree", tree, "-p", root)
	if err != nil {
		t.Fatal(err)
	}
	areas, err := source.ClassifyAreasAtRef(ctx, base, []string{"README.md"})
	if err != nil {
		t.Fatal(err)
	}
	for _, head := range []string{root, side} {
		if err := source.ValidateCommitScope(ctx, base, head, areas); err == nil {
			t.Fatalf("non-descendant head %s was accepted", head)
		}
	}
}

func TestValidateCommitScopeSeparatesRevisionsFromPathsInDeepWorktree(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	for len(repo) < 208 {
		repo = filepath.Join(repo, "d")
	}
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if len(filepath.Join(repo, "allowed", "good.txt")) >= 260 {
		t.Fatalf("fixture file path is too long: %d", len(filepath.Join(repo, "allowed", "good.txt")))
	}

	gitDir := t.TempDir()
	g := gitx.Git{Dir: repo}
	if _, err := g.Run(ctx, "", "init", "--separate-git-dir", gitDir, "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "", "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "", "commit", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	base, err := g.SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	areas, err := g.ClassifyAreasAtRef(ctx, base, []string{"allowed/**"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "allowed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "allowed", "good.txt"), []byte("good\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "", "add", "allowed/good.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "", "commit", "-m", "allowed change"); err != nil {
		t.Fatal(err)
	}
	head, err := g.SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.ValidateCommitScope(ctx, base, head, areas); err != nil {
		t.Fatalf("in-scope deep-worktree change rejected: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "outside.txt"), []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "", "add", "outside.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "", "commit", "-m", "outside change"); err != nil {
		t.Fatal(err)
	}
	head, err = g.SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	err = g.ValidateCommitScope(ctx, base, head, areas)
	var scopeErr *gitx.ScopeError
	if !errors.As(err, &scopeErr) || !reflect.DeepEqual(scopeErr.Paths, []string{"outside.txt"}) {
		t.Fatalf("out-of-scope deep-worktree change escaped check: %#v", err)
	}
}
