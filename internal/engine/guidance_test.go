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
	if _, err := validateGuidanceCommand(s, cmd, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(target.Decisions) != 0 {
		t.Fatal("prevalidation mutated portable state")
	}
	source.HeadSHA = ""
	if _, err := validateGuidanceCommand(s, cmd, "", "", ""); err == nil {
		t.Fatal("uncheckpointed source accepted")
	}
	source.HeadSHA = strings.Repeat("a", 40)
	cmd.Target = "missing"
	if _, err := validateGuidanceCommand(s, cmd, "", "", ""); err == nil {
		t.Fatal("unknown target accepted")
	}
	cmd.Target = "ui"
	secret, _ := json.Marshal(guidanceCommand{Source: "api", Text: "Use token=abcdefghijklmnopqrstuvwxyz123456 in the launcher"})
	cmd.Payload = string(secret)
	if _, err := validateGuidanceCommand(s, cmd, "", "", ""); err == nil {
		t.Fatal("secret-like guidance accepted for durable state")
	}
}

func TestOperatorGuidanceRejectsStaleScopeAndReplaysLateWorker(t *testing.T) {
	head, configHash, rules := strings.Repeat("a", 40), strings.Repeat("b", 64), strings.Repeat("c", 64)
	s := model.NewSnapshot("project123")
	target := &model.Task{ID: "ui", State: model.Fix, HeadSHA: head}
	s.Tasks[target.ID] = target
	payload, _ := json.Marshal(guidanceCommand{Operator: true, Head: head, Base: head, Config: configHash, Rules: rules, Text: "Use the owner-provided endpoint."})
	cmd := store.Command{ID: "operator", Kind: "guide", Target: target.ID, Payload: string(payload)}
	if _, err := validateGuidanceCommand(s, cmd, head, configHash, rules); err != nil {
		t.Fatal(err)
	}
	if _, err := validateGuidanceCommand(s, cmd, head, strings.Repeat("e", 64), rules); err == nil {
		t.Fatal("stale operator config accepted")
	}
	if _, err := validateGuidanceCommand(s, cmd, head, configHash, strings.Repeat("f", 64)); err == nil {
		t.Fatal("stale operator rules accepted")
	}
	target.HeadSHA = strings.Repeat("d", 40)
	if _, err := validateGuidanceCommand(s, cmd, head, configHash, rules); err == nil {
		t.Fatal("stale operator head accepted")
	}
	target.HeadSHA = head
	if err := model.QueueOperatorGuidance(target, cmd.ID, head, head, configHash, rules, "Use the owner-provided endpoint."); err != nil {
		t.Fatal(err)
	}
	started := 0
	target.State = model.Running
	replay, err := completeImplementation(target, started)
	if err != nil || !replay || target.State != model.Ready {
		t.Fatalf("late operator guidance did not replay safely: %v %t %s", err, replay, target.State)
	}
}

func TestRunningOperatorGuidanceCarriesAcrossCheckpointAndDeliversOnceAfterRestart(t *testing.T) {
	head1, head2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	base, configHash, rules := strings.Repeat("c", 40), strings.Repeat("d", 64), strings.Repeat("e", 64)
	target := &model.Task{ID: "ui", State: model.Running, HeadSHA: head1}
	if err := model.QueueOperatorGuidance(target, "operator", head1, base, configHash, rules, "Reconcile the bounded correction."); err != nil {
		t.Fatal(err)
	}
	// The supervisor checkpoint advances the task after the provider started at H1.
	target.HeadSHA = head2
	replay, err := completeImplementation(target, 0)
	if err != nil || !replay || target.State != model.Ready {
		t.Fatalf("changed-head replay = %t, %s, %v", replay, target.State, err)
	}
	encoded, err := json.Marshal(model.NewSnapshot("project123"))
	if err != nil {
		t.Fatal(err)
	}
	var restored model.Snapshot
	if err = json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	// Use the normal portable encoding path to model attach/restart exactly.
	restored.Tasks["ui"] = target
	encoded, err = json.Marshal(&restored)
	if err != nil {
		t.Fatal(err)
	}
	recovered, _, err := model.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	delivery := model.EligibleGuidance(recovered.Tasks["ui"], base, configHash, rules)
	if len(delivery) != 1 || !delivery[0].Pending || delivery[0].Head != head1 {
		t.Fatalf("restart lost authorized pending delivery: %#v", delivery)
	}
	if got := model.EligibleGuidance(recovered.Tasks["ui"], strings.Repeat("f", 40), configHash, rules); len(got) != 0 {
		t.Fatalf("main-only base advance retained pending delivery: %#v", got)
	}
	model.MarkOperatorGuidanceDelivered(recovered.Tasks["ui"], delivery)
	if got := model.EligibleGuidance(recovered.Tasks["ui"], base, configHash, rules); len(got) != 0 {
		t.Fatalf("operator guidance delivered more than once: %#v", got)
	}
}

func TestQueuedOperatorGuidanceDuplicateIsVisibleAtAcceptance(t *testing.T) {
	head, configHash, rules := strings.Repeat("a", 40), strings.Repeat("b", 64), strings.Repeat("c", 64)
	s := model.NewSnapshot("project123")
	target := &model.Task{ID: "ui", State: model.Ready, HeadSHA: head}
	s.Tasks[target.ID] = target
	payload, _ := json.Marshal(guidanceCommand{Operator: true, Head: head, Base: head, Config: configHash, Rules: rules, Text: "Keep the recovery bounded."})
	cmd := store.Command{ID: "one", Kind: "guide", Target: target.ID, Payload: string(payload)}
	if _, err := validateGuidanceCommand(s, cmd, head, configHash, rules); err != nil {
		t.Fatal(err)
	}
	if err := model.QueueOperatorGuidance(target, cmd.ID, head, head, configHash, rules, "Keep the recovery bounded."); err != nil {
		t.Fatal(err)
	}
	cmd.ID = "two"
	if _, err := validateGuidanceCommand(s, cmd, head, configHash, rules); err == nil {
		t.Fatal("duplicate queued operator guidance was accepted")
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
