package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
)

// The complete release inventory must include this real-Git causal fixture.
// Focused pure-test commands exclude it while another heavy check is active.
func TestNativeArtifactGitFixturesRejectHeadMismatchAndDirtyCheckout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repo := filepath.Join(t.TempDir(), "repo")
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
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	check := config.Check{Name: "artifact", Command: []string{helper, "-test.run=^TestNativeArtifactDirtyHelper$", "--", "native-artifact-dirty"}, Timeout: 30, Artifacts: true}
	bind := &nativeArtifactContext{ExpectedHead: strings.Repeat("0", 40), Config: strings.Repeat("a", 64), Rules: strings.Repeat("b", 64), PlanInput: strings.Repeat("c", 64), Toolchain: "powershell=fixture", Project: "fixture", Task: "task", StateRoot: filepath.Join(filepath.Dir(repo), "state"), SourceRoot: repo, SealRoot: filepath.Join(filepath.Dir(repo), "state", "seal")}
	if _, err = beginNativeArtifacts(ctx, check, 0, repo, bind); err == nil {
		t.Fatal("checkout HEAD mismatch accepted")
	}
	bind.ExpectedHead = head
	if _, err = verifyChecksWithPermit(ctx, []config.Check{check}, repo, nil, bind); err == nil || !strings.Contains(err.Error(), "verification modified source") {
		t.Fatalf("dirty checkout was not rejected: %v", err)
	}
}

func TestNativeArtifactDirtyHelper(t *testing.T) {
	for _, argument := range os.Args {
		if argument == "native-artifact-dirty" {
			if err := os.WriteFile("README.md", []byte("dirty"), 0600); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
}
