package engine_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

// These exercises use the real local Git fixture and are intentionally deferred
// while the shared native gate owns heavy capacity.
func TestCanonicalBatchMatchesLegacyFilesHashAndCommit(t *testing.T) {
	ctx := context.Background()
	f := canonicalFixture(t, ctx, map[string]string{
		"docs/shared.md":         "shared context\n",
		"docs/other.md":          "other context\n",
		".aih/roles/first.yaml":  canonicalRole("first-role", []string{"docs/shared.md", "docs/shared.md"}),
		".aih/roles/second.yaml": canonicalRole("second-role", []string{"docs/shared.md", "docs/other.md"}),
	})
	defer f.P.DB.Close()

	got, err := engine.Canonical(ctx, f.P.Git)
	if err != nil {
		t.Fatal(err)
	}
	want := legacyCanonical(t, ctx, f.P.Git)
	if got.BaseSHA != want.BaseSHA || got.Hash != want.Hash {
		t.Fatalf("canonical identity = (%s, %s), want legacy (%s, %s)", got.BaseSHA, got.Hash, want.BaseSHA, want.Hash)
	}
	if got.Files["docs/shared.md"] != "shared context" || got.Files["docs/other.md"] != "other context" {
		t.Fatalf("canonical contexts = %#v", got.Files)
	}
	if _, ok := got.Files["docs/shared.md"]; !ok {
		t.Fatal("duplicated role context was omitted")
	}
}

func TestCanonicalBatchPreservesLegacyTerminalCRLFBoundary(t *testing.T) {
	ctx := context.Background()
	boundary := strings.Repeat("x", 128*1024) + "\r\n"
	f := canonicalFixture(t, ctx, map[string]string{
		"docs/boundary.md":         boundary,
		".aih/roles/boundary.yaml": canonicalRole("boundary-role", []string{"docs/boundary.md"}),
	})
	defer f.P.DB.Close()

	got, err := engine.Canonical(ctx, f.P.Git)
	if err != nil {
		t.Fatalf("terminal CRLF at legacy boundary rejected: %v", err)
	}
	if got.Files["docs/boundary.md"] != strings.TrimRight(boundary, "\r\n") {
		t.Fatal("terminal CRLF boundary did not retain legacy Show text")
	}
}

func TestCanonicalBatchNamesMissingAndOversizedContexts(t *testing.T) {
	ctx := context.Background()
	for name, scenario := range map[string]struct{ content, want string }{
		"missing":   {"", "docs/missing.md"},
		"oversized": {strings.Repeat("x", 128*1024+1), "docs/oversized.md"},
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{
				".aih/roles/context.yaml": canonicalRole("context-role", []string{scenario.want}),
			}
			if name == "oversized" {
				files[scenario.want] = scenario.content
			}
			f := canonicalFixture(t, ctx, files)
			defer f.P.DB.Close()
			_, err := engine.Canonical(ctx, f.P.Git)
			if err == nil || !strings.Contains(err.Error(), scenario.want) {
				t.Fatalf("Canonical error = %v, want path %q", err, scenario.want)
			}
		})
	}
}

func canonicalFixture(t *testing.T, ctx context.Context, files map[string]string) *demo.Fixture {
	t.Helper()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(f.Source, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	source := gitx.Git{Dir: f.Source}
	for _, args := range [][]string{{"add", "--all"}, {"commit", "-m", "canonical contexts"}, {"push", "origin", "HEAD:main"}} {
		if _, err := source.Run(ctx, "", args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.P.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

func canonicalRole(name string, contexts []string) string {
	return fmt.Sprintf("name: %s\ndescription: canonical test\nextends: reviewer\ncontext:\n%s", name, func() string {
		var b strings.Builder
		for _, context := range contexts {
			fmt.Fprintf(&b, "  - %s\n", context)
		}
		return b.String()
	}())
}

func legacyCanonical(t *testing.T, ctx context.Context, g gitx.Git) config.Effective {
	t.Helper()
	ref, err := g.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	names, err := g.Files(ctx, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, name := range names {
		if strings.HasPrefix(name, ".aih/") || filepath.Base(name) == "AGENTS.md" {
			value, err := g.Show(ctx, ref, name)
			if err != nil {
				t.Fatal(err)
			}
			if len(value) > 128*1024 {
				t.Fatalf("legacy initial file too large: %s", name)
			}
			files[name] = value
		}
	}
	all, err := roles.Load(files)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range all {
		for _, name := range role.Context {
			if _, ok := files[name]; ok {
				continue
			}
			value, err := g.Show(ctx, ref, name)
			if err != nil {
				t.Fatal(err)
			}
			if len(value) > 128*1024 {
				t.Fatalf("legacy context too large: %s", name)
			}
			files[name] = value
		}
	}
	effective, err := config.Parse(files)
	if err != nil {
		t.Fatal(err)
	}
	effective.BaseSHA = ref
	return effective
}
