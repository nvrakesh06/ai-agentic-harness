package engine_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

func TestTaskGuidanceCommandPersistsAndSurvivesAttach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.P.DB.Close()
	s, head, err := fixture.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mainSHA, err := fixture.P.Git.Run(ctx, "", "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	source := &model.Task{ID: "api", ObjectiveID: "objective", State: model.Done, HeadSHA: mainSHA}
	gate := &model.Task{ID: "gate", ObjectiveID: "objective", State: model.Blocked, Blocker: &model.Blocker{Question: "Hold target", Resume: model.Ready}}
	target := &model.Task{ID: "ui", ObjectiveID: "objective", State: model.Ready, Dependencies: []string{"gate"}}
	s.Tasks[source.ID], s.Tasks[target.ID], s.Tasks[gate.ID] = source, target, gate
	next, err := fixture.P.Git.StateCommit(ctx, head, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: head, New: next}}); err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.DB.Save(next, s); err != nil {
		t.Fatal(err)
	}
	controller := engine.New(fixture.P)
	done := make(chan error, 1)
	go func() { done <- controller.Serve(ctx) }()
	waitStarted(t, fixture.P)
	payload, _ := json.Marshal(map[string]string{"source_task": "api", "text": "Use src/studio-server/cli.ts with --projects-root and --port."})
	if err = fixture.P.DB.Submit(store.Command{ID: "guidance-command", Kind: "guide", Target: "ui", Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		persisted, _, loadErr := fixture.P.Git.Load(ctx)
		if loadErr == nil && len(model.TaskGuidance(persisted.Tasks["ui"])) == 1 {
			if guidance := model.TaskGuidance(persisted.Tasks["ui"])[0]; guidance.CommandID != "guidance-command" || guidance.SourceSHA != source.HeadSHA {
				t.Fatalf("wrong durable guidance: %#v", guidance)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err = fixture.P.DB.Submit(store.Command{ID: "stop-command", Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := fixture.P.DB.Load()
	if err != nil || len(model.TaskGuidance(recovered.Tasks["ui"])) != 1 {
		t.Fatalf("attach lost guidance: %#v %v", recovered.Tasks["ui"], err)
	}
}
