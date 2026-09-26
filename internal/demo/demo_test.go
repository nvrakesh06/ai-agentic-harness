package demo

import (
	"bytes"
	"context"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	if os.Getenv("AIH_DEMO_HELPER") == "1" {
		dir, _ := os.Getwd()
		if countFile := os.Getenv("AIH_DEMO_CHECK_COUNT_FILE"); countFile != "" {
			f, e := os.OpenFile(countFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				os.Exit(1)
			}
			_, e = f.WriteString(filepath.ToSlash(dir) + "\n")
			if closeErr := f.Close(); e == nil {
				e = closeErr
			}
			if e != nil {
				os.Exit(1)
			}
		}
		if e := Check(dir); e != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestPlannerAreasMatchGeneratedFixtureFiles(t *testing.T) {
	result, err := (&Worker{}).Run(context.Background(), provider.Request{Role: "orchestrator"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan) == 0 {
		t.Fatal("planner returned no fixture tasks")
	}
	for _, task := range result.Plan {
		want := "feature-" + task.Key + ".txt"
		if len(task.Areas) != 1 || task.Areas[0] != want {
			t.Fatalf("planner areas for %q = %v, want [%q]", task.Key, task.Areas, want)
		}
		if len(task.Domains) != 1 || task.Domains[0] != task.Key {
			t.Fatalf("planner domains for %q = %v, want [%q]", task.Key, task.Domains, task.Key)
		}
		wantRisk := "low"
		if task.Key == "beta" {
			wantRisk = "medium"
		}
		if task.Risk != wantRisk {
			t.Fatalf("planner risk for %q = %q, want %q", task.Key, task.Risk, wantRisk)
		}
	}
}

func TestEndToEndRecovery(t *testing.T) {
	t.Setenv("AIH_DEMO_HELPER", "1")
	countFile := filepath.Join(t.TempDir(), "check-count.txt")
	t.Setenv("AIH_DEMO_CHECK_COUNT_FILE", countFile)
	exe, _ := os.Executable()
	// This end-to-end fixture executes a serial merge train, including a fresh-main
	// re-verification after every merge. Keep a finite watchdog for genuine hangs, but
	// allow its complete workflow to run on a loaded local Windows host.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	var output bytes.Buffer
	root, e := Run(ctx, &output, []string{exe})
	cleanup := true
	t.Cleanup(func() {
		if cleanup && strings.Contains(filepath.Base(root), "aih-demo-") {
			_ = os.RemoveAll(root)
		}
	})
	if e != nil {
		cleanup = false
		t.Fatalf("%s\n%s\nfailed demo artifacts retained at: %s", e, output.String(), root)
	}
	if !strings.Contains(output.String(), "reconstructed every task") {
		t.Fatal(output.String())
	}
	g := gitx.Git{Dir: filepath.Join(root, "origin.git")}
	b, e := g.Show(ctx, "aih-state", "snapshot.json")
	if e != nil {
		t.Fatal(e)
	}
	s, _, e := model.Decode([]byte(b))
	if e != nil {
		t.Fatal(e)
	}
	bases := map[string]bool{}
	done := map[string]bool{}
	for _, task := range s.Tasks {
		if task.State != model.Done {
			continue
		}
		done[task.Title] = true
		parent, e := g.SHA(ctx, task.MergeSHA+"^1")
		if e != nil {
			t.Fatal(e)
		}
		if task.Evidence == nil || task.Evidence.Base != parent || len(task.Evidence.Checks) == 0 || len(task.Evidence.Reviews) < 2 {
			t.Fatal("merged without fresh-main review/check evidence", task.ID)
		}
		if !g.Ancestor(ctx, parent, task.Evidence.Head) {
			t.Fatal("reviewed branch was not synchronized")
		}
		if !g.Ancestor(ctx, task.Evidence.Head, task.MergeSHA) || !g.Ancestor(ctx, task.MergeSHA, "main") {
			t.Fatal("verified candidate was not integrated into main", task.ID)
		}
		content, e := g.Show(ctx, "main", "feature-"+task.Title+".txt")
		if e != nil || strings.TrimSpace(content) != "implemented" {
			t.Fatalf("integrated candidate content for %q = %q, err %v", task.Title, content, e)
		}
		bases[parent] = true
	}
	if len(done) != 3 || !done["alpha"] || !done["beta"] || !done["dependent"] {
		t.Fatalf("integrated candidates = %v, want alpha, beta, and dependent", done)
	}
	if len(bases) != 3 {
		t.Fatal("merge train did not advance and reverify each candidate")
	}
	checkRuns, e := os.ReadFile(countFile)
	if e != nil {
		t.Fatal(e)
	}
	runs := string(checkRuns)
	if got := strings.Count(runs, "/integration/"); got != 3 {
		t.Fatalf("exact-merge verification ran %d times, want 3", got)
	}
	if got := strings.Count(runs, "/post-verify/"); got != 0 {
		t.Fatalf("normal integration repeated post-merge verification %d times", got)
	}
}
