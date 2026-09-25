package engine

import (
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

func TestBatchAdmissionMatchesOnlyCurrentCanonicalIdentity(t *testing.T) {
	base := strings.Repeat("a", 40)
	effective := config.Effective{BaseSHA: base, Hash: strings.Repeat("b", 64)}
	task := &model.Task{State: model.MergeReady, BaseSHA: base, HeadSHA: strings.Repeat("c", 40), Evidence: &model.Evidence{Base: base, Head: strings.Repeat("c", 40), Config: effective.Hash, Rules: roles.Hash()}}
	if !batchAdmissionMatchesRuntime(task, effective) {
		t.Fatal("current merge-ready task was rejected")
	}
	for _, change := range []func(){
		func() { task.BaseSHA = strings.Repeat("d", 40) },
		func() { task.Evidence.Config = strings.Repeat("e", 64) },
		func() { task.Evidence.Rules = strings.Repeat("f", 64) },
		func() { task.Evidence.Head = strings.Repeat("d", 40) },
	} {
		copy := *task
		copy.Evidence = &model.Evidence{Base: task.Evidence.Base, Head: task.Evidence.Head, Config: task.Evidence.Config, Rules: task.Evidence.Rules}
		changeTask := &copy
		saved := task
		task = changeTask
		change()
		if batchAdmissionMatchesRuntime(task, effective) {
			t.Fatal("stale batch identity was accepted")
		}
		task = saved
	}
}

func TestBatchReservationRejectsStaleCanonicalIdentity(t *testing.T) {
	base := strings.Repeat("a", 40)
	effective := config.Effective{BaseSHA: base, Hash: strings.Repeat("b", 64)}
	batch := &model.IntegrationBatch{BaseSHA: base, Config: effective.Hash, Rules: roles.Hash()}
	if !batchReservationMatchesRuntime(batch, effective) {
		t.Fatal("current reservation was rejected")
	}
	for _, mutate := range []func(){
		func() { batch.BaseSHA = strings.Repeat("c", 40) },
		func() { batch.Config = strings.Repeat("d", 64) },
		func() { batch.Rules = strings.Repeat("e", 64) },
	} {
		copy := *batch
		mutateBatch := &copy
		saved := batch
		batch = mutateBatch
		mutate()
		if batchReservationMatchesRuntime(batch, effective) {
			t.Fatal("stale reservation identity was accepted")
		}
		batch = saved
	}
}

func TestSameBatchPathsRequiresExactCurrentDiff(t *testing.T) {
	if !sameBatchPaths([]string{"a.go", "b.go"}, []string{"b.go", "a.go"}) {
		t.Fatal("path ordering changed an otherwise identical diff")
	}
	if sameBatchPaths([]string{"a.go"}, []string{"a.go", "b.go"}) || sameBatchPaths([]string{"a.go"}, []string{"other.go"}) {
		t.Fatal("changed path set was accepted")
	}
}

func TestFreshBatchAdmissionRejectsStaleValidationPlan(t *testing.T) {
	plan := validationPlan{Gate: "focused", Input: strings.Repeat("a", 64), Toolchain: "go=tool", TestInputs: strings.Repeat("b", 40)}
	evidence := &model.Evidence{ValidationGate: plan.Gate, ValidationInput: plan.Input, Toolchain: plan.Toolchain, TestInputs: plan.TestInputs}
	if !validationPlanMatchesEvidence(plan, evidence) {
		t.Fatal("current validation plan was rejected")
	}
	for _, mutate := range []func(){
		func() { evidence.ValidationGate = "full" },
		func() { evidence.ValidationInput = strings.Repeat("c", 64) },
		func() { evidence.Toolchain = "go=other" },
		func() { evidence.TestInputs = strings.Repeat("d", 40) },
	} {
		copy := *evidence
		evidence = &copy
		mutate()
		if validationPlanMatchesEvidence(plan, evidence) {
			t.Fatal("stale validation plan evidence was accepted")
		}
		evidence = &model.Evidence{ValidationGate: plan.Gate, ValidationInput: plan.Input, Toolchain: plan.Toolchain, TestInputs: plan.TestInputs}
	}
}
