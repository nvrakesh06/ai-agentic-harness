package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

func TestGuidanceCommandValidatesSourceAndTarget(t *testing.T) {
	s := model.NewSnapshot("project123")
	source := &model.Task{ID: "api", ObjectiveID: "objective", State: model.Running, HeadSHA: strings.Repeat("a", 40)}
	target := &model.Task{ID: "ui", ObjectiveID: "objective", State: model.Running}
	s.Tasks[source.ID], s.Tasks[target.ID] = source, target
	payload, _ := json.Marshal(guidanceCommand{Source: "api", Text: "Use cli.ts with --port."})
	cmd := store.Command{ID: "command", Kind: "guide", Target: "ui", Payload: string(payload)}
	if _, err := validateGuidanceCommand(s, cmd); err != nil {
		t.Fatal(err)
	}
	if len(target.Decisions) != 0 {
		t.Fatal("prevalidation mutated portable state")
	}
	source.HeadSHA = ""
	if _, err := validateGuidanceCommand(s, cmd); err == nil {
		t.Fatal("uncheckpointed source accepted")
	}
	source.HeadSHA = strings.Repeat("a", 40)
	cmd.Target = "missing"
	if _, err := validateGuidanceCommand(s, cmd); err == nil {
		t.Fatal("unknown target accepted")
	}
}

func TestCompletedWorkerReplaysLateGuidanceAfterCheckpoint(t *testing.T) {
	source := &model.Task{ID: "api", ObjectiveID: "objective", State: model.Running, HeadSHA: strings.Repeat("a", 40)}
	target := &model.Task{ID: "ui", ObjectiveID: "objective", State: model.Running}
	startedWith := len(model.TaskGuidance(target))
	if err := model.QueueGuidance(target, source, "command", "Use the durable API CLI."); err != nil {
		t.Fatal(err)
	}
	replay, err := completeImplementation(target, startedWith)
	if err != nil || !replay || target.State != model.Ready {
		t.Fatalf("late guidance did not request a safe replay: state=%s replay=%t err=%v", target.State, replay, err)
	}
	target.State = model.Running
	replay, err = completeImplementation(target, len(model.TaskGuidance(target)))
	if err != nil || replay || target.State != model.Implemented {
		t.Fatalf("worker that received guidance did not finish: state=%s replay=%t err=%v", target.State, replay, err)
	}
}
