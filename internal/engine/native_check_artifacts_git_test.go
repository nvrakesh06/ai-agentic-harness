//go:build nativeartifactgit

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
)

// This fixture intentionally has an opt-in build tag because it creates a
// real checkout and invokes a local helper. The ordinary focused suite covers
// the pure identity predicate; this proves the two Git-backed call paths when
// the shared heavy-check permit is available.
func TestNativeArtifactGitFixturesRejectHeadMismatchAndDirtyCheckout(t *testing.T) {
	if os.Getenv("AIH_RUN_NATIVE_ARTIFACT_GIT_FIXTURES") != "1" {
		t.Skip("requires the shared real-Git/helper permit")
	}
	ctx, repo := context.Background(), filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	g := gitx.Git{Dir: repo}
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "fixture"}, {"config", "user.email", "fixture@example.invalid"}} {
		if _, err := g.Run(ctx, "", args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("clean\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "", "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "", "commit", "-m", "fixture"); err != nil {
		t.Fatal(err)
	}
	head, err := g.SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	check := config.Check{Name: "artifact", Command: []string{"powershell", "-NoProfile", "-Command", "Set-Content -NoNewline README.md dirty"}, Timeout: 30, Artifacts: true}
	bind := &nativeArtifactContext{ExpectedHead: strings.Repeat("0", 40), Config: strings.Repeat("a", 64), Rules: strings.Repeat("b", 64), PlanInput: strings.Repeat("c", 64), Toolchain: "powershell=fixture", Project: "fixture", Task: "task", StateRoot: filepath.Join(filepath.Dir(repo), "state"), SourceRoot: repo, SealRoot: filepath.Join(filepath.Dir(repo), "state", "seal")}
	if _, err = beginNativeArtifacts(ctx, check, 0, repo, bind); err == nil {
		t.Fatal("checkout HEAD mismatch accepted")
	}
	bind.ExpectedHead = head
	if _, err = verifyChecksWithPermit(ctx, []config.Check{check}, repo, nil, bind); err == nil || !strings.Contains(err.Error(), "verification modified source") {
		t.Fatalf("dirty checkout was not rejected: %v", err)
	}
}
