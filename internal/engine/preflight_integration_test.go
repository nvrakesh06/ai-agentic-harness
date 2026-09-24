package engine_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

type heldPreflightProvider struct {
	designers atomic.Int32
	writers   atomic.Int32
}

func TestRecoveredCompletedDesignerIsNotRunAgain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	s, old, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := engine.Canonical(ctx, f.P.Git)
	if err != nil {
		t.Fatal(err)
	}
	base := effective.BaseSHA
	s.Tasks["ui"] = &model.Task{ID: "ui", Title: "ui", Objective: "Fixture UI", Acceptance: []string{"works"},
		Areas: []string{"ui"}, Domains: []string{"ui"}, Risk: "low", UI: true,
		State: model.Ready, Branch: "aih/ui", BaseSHA: base, HeadSHA: base,
		Preflight: &model.Preflight{Phase: "waiting", BaseSHA: base, HeadSHA: base,
			Config: effective.Hash, Rules: roles.Hash(), Completed: []string{"designer"}}}
	if err = f.P.Git.Worktree(ctx, f.P.TaskPath(s.Tasks["ui"]), s.Tasks["ui"].Branch, base); err != nil {
		t.Fatal(err)
	}
	next, err := f.P.Git.StateCommit(ctx, old, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: old, New: next}}); err != nil {
		t.Fatal(err)
	}
	workers := &heldPreflightProvider{}
	f.P.Provider = workers
	c := engine.New(f.P)
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx) }()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for workers.writers.Load() == 0 {
		select {
		case err := <-done:
			t.Fatalf("supervisor exited before resumed writer: %v", err)
		case <-deadline.C:
			t.Fatal("prepared UI task did not resume after restart")
		case <-time.After(25 * time.Millisecond):
		}
	}
	if workers.designers.Load() != 0 {
		t.Fatal("completed designer guidance was duplicated")
	}
	cancel()
	<-done
}

func (*heldPreflightProvider) Name() string                   { return "codex" }
func (*heldPreflightProvider) Validate(context.Context) error { return nil }
func (p *heldPreflightProvider) Run(ctx context.Context, request provider.Request) (provider.Result, error) {
	switch request.Role {
	case "designer":
		p.designers.Add(1)
	case "implementer":
		p.writers.Add(1)
	default:
		return provider.Result{Schema: 1, Status: "completed"}, nil
	}
	<-ctx.Done()
	return provider.Result{}, ctx.Err()
}

func TestPreflightReaderQueueLeavesIndependentWriterSlotsAvailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.P.Config.Project.MaxWriters = 2
	f.P.Config.Project.Scheduling.TargetWriters = 2
	s, old, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id string
		ui bool
	}{
		{"ui_a", true}, {"ui_b", true}, {"ui_c", true}, {"code_a", false}, {"code_b", false},
	} {
		s.Tasks[item.id] = &model.Task{ID: item.id, Title: item.id, Objective: "Fixture task " + item.id,
			Acceptance: []string{"fixture succeeds"}, Areas: []string{item.id}, Domains: []string{item.id},
			Risk: "low", UI: item.ui, State: model.Ready, Branch: "aih/" + item.id,
			BaseSHA: base, HeadSHA: base}
		if err = f.P.Git.Worktree(ctx, f.P.TaskPath(s.Tasks[item.id]), s.Tasks[item.id].Branch, base); err != nil {
			t.Fatal(err)
		}
	}
	next, err := f.P.Git.StateCommit(ctx, old, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: old, New: next}}); err != nil {
		t.Fatal(err)
	}
	workers := &heldPreflightProvider{}
	f.P.Provider = workers
	c := engine.New(f.P)
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- c.Serve(ctx) }()
	readerDeadline := time.NewTimer(90 * time.Second)
	defer readerDeadline.Stop()
	for workers.designers.Load() < 2 {
		select {
		case err := <-done:
			t.Fatalf("supervisor exited before reader pressure was established: %v", err)
		case <-readerDeadline.C:
			t.Fatalf("reader pressure was not established: designers=%d writers=%d", workers.designers.Load(), workers.writers.Load())
		case <-time.After(25 * time.Millisecond):
		}
	}
	readerPressureAt := time.Now()
	t.Logf("preflight admission fixture: first two designers active after %s", readerPressureAt.Sub(started).Round(time.Millisecond))
	// Start the starvation observation once reader pressure exists. Real Git and
	// state setup may be slow under load, but it must not consume the interval
	// that proves independent writer admission. A reader-only scheduler cannot
	// satisfy this condition because the held designers never release capacity.
	writerDeadline := time.NewTimer(45 * time.Second)
	defer writerDeadline.Stop()
	for {
		current := c.Snapshot()
		if current != nil && workers.designers.Load() == 2 && workers.writers.Load() == 2 {
			waiting := 0
			for _, task := range current.Tasks {
				if task.UI && task.Preflight != nil && task.Preflight.Phase == "waiting" {
					waiting++
				}
			}
			if waiting > 0 && current.Capacity.ActiveWriters == 2 && current.Capacity.ActiveReaders == 2 {
				t.Logf("preflight admission fixture: two independent writers active %s after reader pressure", time.Since(readerPressureAt).Round(time.Millisecond))
				cancel()
				<-done
				return
			}
		}
		select {
		case err := <-done:
			t.Fatalf("supervisor exited before independent writers started: %v", err)
		case <-writerDeadline.C:
			t.Fatal(fmt.Sprintf("reader queue suppressed writers after reader pressure: designers=%d writers=%d snapshot=%#v", workers.designers.Load(), workers.writers.Load(), current))
		case <-time.After(25 * time.Millisecond):
		}
	}
}
