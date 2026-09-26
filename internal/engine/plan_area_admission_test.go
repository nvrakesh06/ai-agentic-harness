package engine_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

type malformedPlanProvider struct {
	started chan struct{}
	once    sync.Once
}

func (*malformedPlanProvider) Name() string                   { return "codex" }
func (*malformedPlanProvider) Validate(context.Context) error { return nil }
func (p *malformedPlanProvider) Run(_ context.Context, request provider.Request) (provider.Result, error) {
	if request.Role != "orchestrator" {
		return provider.Result{Schema: 1, Status: "completed"}, nil
	}
	p.once.Do(func() { close(p.started) })
	return provider.Result{Schema: 1, Status: "completed", Plan: []model.PlanTask{{
		Key: "studio", Title: "Studio", Objective: "Add the planner", Acceptance: []string{"planner works"},
		Areas: []string{"src/studio/Planner.tsx (new)"}, Domains: []string{"studio"}, Risk: "low",
	}}}, nil
}

func TestMalformedNewPlanAreasPublishNoTasksOwnershipOrIssues(t *testing.T) {
	f, err := demo.New(context.Background(), t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	planner := &malformedPlanProvider{started: make(chan struct{})}
	f.P.Provider = planner

	controller := engine.New(f.P)
	done := make(chan error, 1)
	go func() { done <- controller.Serve(ctx) }()
	waitStarted(t, f.P)
	if err = f.P.DB.Submit(store.Command{ID: "bad-plan", Kind: "run", Payload: "Create the studio planner."}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-planner.started:
	case err = <-done:
		t.Fatalf("controller stopped before plan admission: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, _, loadErr := f.P.DB.Load()
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if objective := snapshot.Objectives["bad-plan"]; objective != nil && objective.Attempts > 0 {
			if len(snapshot.Tasks) != 0 || objective.Issue != 0 {
				t.Fatalf("malformed plan published task state or objective issue: %+v", snapshot)
			}
			issues, issuesErr := f.Hub.Issues(ctx)
			if issuesErr != nil || len(issues) != 0 {
				t.Fatalf("malformed plan created issues: %#v err=%v", issues, issuesErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("malformed plan did not reach admission failure")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err = f.P.DB.Submit(store.Command{ID: "stop-bad-plan", Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
