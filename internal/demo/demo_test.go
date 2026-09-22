package demo

import (
	"bytes"
	"context"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	if os.Getenv("AIH_DEMO_HELPER") == "1" {
		dir, _ := os.Getwd()
		if e := Check(dir); e != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
}
func TestEndToEndRecovery(t *testing.T) {
	t.Setenv("AIH_DEMO_HELPER", "1")
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var output bytes.Buffer
	root, e := Run(ctx, &output, []string{exe})
	t.Cleanup(func() {
		if strings.Contains(filepath.Base(root), "aih-demo-") {
			_ = os.RemoveAll(root)
		}
	})
	if e != nil {
		t.Fatalf("%s\n%s\nartifacts: %s", e, output.String(), root)
	}
	if !strings.Contains(output.String(), "reconstructed every task") {
		t.Fatal(output.String())
	}
	g := gitx.Git{Dir: filepath.Join(root, "origin.git")}
	merges, e := g.Run(ctx, "", "rev-list", "--count", "--merges", "main")
	if e != nil || merges != "3" {
		t.Fatal("merge train did not integrate three candidates", merges, e)
	}
	b, e := g.Show(ctx, "aih-state", "snapshot.json")
	if e != nil {
		t.Fatal(e)
	}
	s, _, e := model.Decode([]byte(b))
	if e != nil {
		t.Fatal(e)
	}
	bases := map[string]bool{}
	for _, task := range s.Tasks {
		if task.State != model.Done {
			continue
		}
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
		bases[parent] = true
	}
	if len(bases) != 3 {
		t.Fatal("merge train did not advance and reverify each candidate")
	}
}
