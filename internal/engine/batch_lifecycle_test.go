package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func init() {
	if (len(os.Args) == 3 || len(os.Args) == 4) && os.Args[1] == "_aih-batch-check" {
		count, _ := strconv.Atoi(strings.TrimSpace(string(mustRead(os.Args[2]))))
		_ = os.WriteFile(os.Args[2], []byte(strconv.Itoa(count+1)), 0o600)
		if len(os.Args) == 4 && os.Args[3] == "fail" {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestBatchNativeGateFailureDemotesOneMemberAndClearsReservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(t.TempDir(), "checks")
	if err = os.WriteFile(counter, []byte("2"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := demo.New(ctx, t.TempDir(), []string{exe, "_aih-batch-check", counter, "fail"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	_, tasks := seedBatchMembers(t, ctx, f, []string{"alpha", "bravo"})
	c := engine.New(f.P)
	if err = engine.ExportAcquire(c, ctx); err != nil {
		t.Fatal(err)
	}
	if err = engine.ExportMutate(c, func(s *model.Snapshot) error {
		for _, task := range tasks {
			evidence, e := engine.ExportAcceptedEvidence(c, s.Tasks[task.ID], []string{"feature-" + task.ID + ".txt"})
			if e != nil {
				return e
			}
			s.Tasks[task.ID].Evidence = evidence
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	batch, err := engine.ExportReserveBatch(c)
	if err != nil || batch == nil {
		t.Fatalf("reserve batch = %#v, %v", batch, err)
	}
	engine.ExportIntegrateBatch(c, batch.ID)
	s := c.Snapshot()
	if s.IntegrationBatch != nil || s.Tasks["alpha"].State != model.SyncRequired || s.Tasks["alpha"].Evidence != nil || s.Tasks["bravo"].State != model.MergeReady {
		t.Fatalf("failed batch did not leave deterministic serial fallback: alpha=%#v bravo=%#v batch=%#v", s.Tasks["alpha"], s.Tasks["bravo"], s.IntegrationBatch)
	}
	var failure string
	if err = f.P.DB.DB.QueryRow("SELECT message FROM events WHERE kind='batch_integration_failed' ORDER BY id DESC LIMIT 1").Scan(&failure); err != nil || !strings.Contains(failure, "integrated validation") {
		t.Fatalf("missing durable batch failure diagnostic: %q %v", failure, err)
	}
}

func mustRead(path string) []byte { b, _ := os.ReadFile(path); return b }

func TestBatchIntegrationPublishesOneExactFullGateForBothMembers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(t.TempDir(), "checks")
	if err = os.WriteFile(counter, []byte("2"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := demo.New(ctx, t.TempDir(), []string{exe, "_aih-batch-check", counter})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	base, tasks := seedBatchMembers(t, ctx, f, []string{"alpha", "bravo"})
	_ = base
	c := engine.New(f.P)
	if err = engine.ExportAcquire(c, ctx); err != nil {
		t.Fatal(err)
	}
	if err = engine.ExportMutate(c, func(s *model.Snapshot) error {
		for _, task := range tasks {
			evidence, e := engine.ExportAcceptedEvidence(c, s.Tasks[task.ID], []string{"feature-" + task.ID + ".txt"})
			if e != nil {
				return e
			}
			s.Tasks[task.ID].Evidence = evidence
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	batch, err := engine.ExportReserveBatch(c)
	if err != nil || batch == nil {
		t.Fatalf("reserve batch = %#v, %v", batch, err)
	}
	engine.ExportIntegrateBatch(c, batch.ID)
	s := c.Snapshot()
	a, b := s.Tasks["alpha"], s.Tasks["bravo"]
	if a.State != model.Done || b.State != model.Done || a.MergeSHA == "" || a.MergeSHA != b.MergeSHA || s.IntegrationBatch != nil {
		var failure string
		_ = f.P.DB.DB.QueryRow("SELECT message FROM events WHERE kind='batch_integration_failed' ORDER BY id DESC LIMIT 1").Scan(&failure)
		t.Logf("batch failure: %s", failure)
		t.Fatalf("batch did not complete atomically: alpha=%#v bravo=%#v batch=%#v", a, b, s.IntegrationBatch)
	}
	if a.Evidence.ValidationGate != "full" || a.Evidence.ValidationInput != b.Evidence.ValidationInput || a.Evidence.IntegrationSHA != a.MergeSHA || b.Evidence.IntegrationSHA != b.MergeSHA {
		t.Fatalf("members did not reuse exact full evidence: alpha=%#v bravo=%#v", a.Evidence, b.Evidence)
	}
	got, err := os.ReadFile(counter)
	if err != nil || strings.TrimSpace(string(got)) != "3" {
		t.Fatalf("full batch gate runs = %q, err %v", got, err)
	}
}

func seedBatchMembers(t *testing.T, ctx context.Context, f *demo.Fixture, ids []string) (string, []*model.Task) {
	t.Helper()
	base, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	s, h, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Objectives["batch"] = &model.Objective{ID: "batch", Planned: true}
	tasks := make([]*model.Task, 0, len(ids))
	for _, id := range ids {
		task := &model.Task{ID: id, ObjectiveID: "batch", Issue: 1, Title: id, Objective: "fixture", Acceptance: []string{"done"}, Areas: []string{"feature-" + id + ".txt"}, AssignedAreas: []string{"feature-" + id + ".txt"}, AssignedAreaKinds: map[string]string{"feature-" + id + ".txt": model.AreaFile}, Domains: []string{id}, Risk: "low", State: model.MergeReady, Branch: "aih/" + id, BaseSHA: base, FixCycles: map[string]int{}}
		dir := f.P.TaskPath(task)
		if err = f.P.Git.Worktree(ctx, dir, task.Branch, base); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "feature-"+id+".txt"), []byte(id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		task.HeadSHA, err = f.P.Git.Checkpoint(ctx, dir, id)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: task.Branch, New: task.HeadSHA}}); err != nil {
			t.Fatal(err)
		}
		task.PR, err = f.Hub.EnsurePR(ctx, task.Branch, "main", id, "")
		if err != nil {
			t.Fatal(err)
		}
		if err = f.Hub.SetPRDraft(ctx, task.PR, false); err != nil {
			t.Fatal(err)
		}
		s.Tasks[id] = task
		tasks = append(tasks, task)
	}
	next, err := f.P.Git.StateCommit(ctx, h, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: h, New: next}}); err != nil {
		t.Fatal(err)
	}
	return base, tasks
}
