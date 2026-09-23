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
	cmd.Target = "ui"
	secret, _ := json.Marshal(guidanceCommand{Source: "api", Text: "Use token=abcdefghijklmnopqrstuvwxyz123456 in the launcher"})
	cmd.Payload = string(secret)
	if _, err := validateGuidanceCommand(s, cmd); err == nil {
		t.Fatal("secret-like guidance accepted for durable state")
	}
}

func TestOperatorGuidanceRejectsStaleScopeAndReplaysLateWorker(t *testing.T) {
	head, configHash, rules := strings.Repeat("a", 40), strings.Repeat("b", 64), strings.Repeat("c", 64)
	s := model.NewSnapshot("project123")
	target := &model.Task{ID: "ui", State: model.Fix, HeadSHA: head}
	s.Tasks[target.ID] = target
	payload, _ := json.Marshal(guidanceCommand{Operator: true, Head: head, Config: configHash, Rules: rules, Text: "Use the owner-provided endpoint."})
	cmd := store.Command{ID: "operator", Kind: "guide", Target: target.ID, Payload: string(payload)}
	if _, err := validateGuidanceCommand(s, cmd); err != nil {
		t.Fatal(err)
	}
	target.HeadSHA = strings.Repeat("d", 40)
	if _, err := validateGuidanceCommand(s, cmd); err == nil {
		t.Fatal("stale operator head accepted")
	}
	target.HeadSHA = head
	if err := model.QueueOperatorGuidance(target, cmd.ID, head, configHash, rules, "Use the owner-provided endpoint."); err != nil {
		t.Fatal(err)
	}
	started := 0
	target.State = model.Running
	replay, err := completeImplementation(target, started)
	if err != nil || !replay || target.State != model.Ready {
		t.Fatalf("late operator guidance did not replay safely: %v %t %s", err, replay, target.State)
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

func TestNativeOnlyWorkerReplaysLateGuidanceBeforeVerificationRoute(t *testing.T) {
	source := &model.Task{ID: "api", ObjectiveID: "objective", State: model.Running, HeadSHA: strings.Repeat("a", 40)}
	target := &model.Task{ID: "ui", ObjectiveID: "objective", State: model.Running}
	startedWith := len(model.TaskGuidance(target))
	if err := model.QueueGuidance(target, source, "command", "Use the durable API CLI."); err != nil {
		t.Fatal(err)
	}
	guard := &model.Verification{Environment: "windows/native", NativeOnly: true}
	replay, err := completeNativeOnlyImplementation(target, startedWith, guard)
	if err != nil || !replay || target.State != model.Ready || target.Verification != nil {
		t.Fatalf("late guidance was bypassed by native-only verification: state=%s replay=%t guard=%#v err=%v", target.State, replay, target.Verification, err)
	}
	target.State = model.Running
	replay, err = completeNativeOnlyImplementation(target, len(model.TaskGuidance(target)), guard)
	if err != nil || replay || target.State != model.Implemented || target.Verification != guard {
		t.Fatalf("native-only route did not resume after guidance: state=%s replay=%t guard=%#v err=%v", target.State, replay, target.Verification, err)
	}
}
