package model

import (
	"encoding/json"
	"testing"
)

func TestTransitionsAndBlockers(t *testing.T) {
	task := &Task{State: Planned}
	for _, to := range []State{Ready, Running, Implemented, SyncRequired, Verifying, Review, MergeReady, MergeTrain, PostVerify, Done} {
		if e := Transition(task, to); e != nil {
			t.Fatal(e)
		}
	}
	if e := Transition(task, Ready); e == nil {
		t.Fatal("terminal state reopened")
	}
	task.State = Running
	Block(task, "Choose", "decision", Ready)
	if e := Answer(task, "proceed"); e != nil || task.State != Ready || len(task.Decisions) != 1 {
		t.Fatal(task, e)
	}
}
func TestSchedulerDependenciesDomainsAndBlocked(t *testing.T) {
	s := NewSnapshot("project123")
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		s.Tasks[id] = &Task{ID: id, State: Ready, Domains: []string{id}}
	}
	s.Tasks["a"].State = Blocked
	s.Tasks["b"].Dependencies = []string{"a"}
	s.Tasks["c"].Domains = []string{"shared"}
	s.Tasks["d"].Domains = []string{"shared"}
	ready := Runnable(s, map[string]bool{}, 3)
	if len(ready) != 3 || ready[0].ID != "c" || ready[1].ID != "e" || ready[2].ID != "f" {
		t.Fatalf("unexpected ready tasks: %+v", ready)
	}
	s.Tasks["a"].State = Done
	if got := Runnable(s, map[string]bool{"c": true, "e": true, "f": true}, 3); len(got) != 0 {
		t.Fatal("writer limit exceeded")
	}
	if got := Runnable(s, map[string]bool{"c": true}, 3); len(got) != 2 || got[0].ID != "b" {
		t.Fatal("dependency not released")
	}
}
func TestPlanRejectsCyclesMissingAndUnsafeKeys(t *testing.T) {
	a := PlanTask{Key: "a", Title: "a", Objective: "a", Acceptance: []string{"works"}, Areas: []string{"a"}, Domains: []string{"a"}, Risk: "low"}
	b := a
	b.Key = "b"
	a.Dependencies = []string{"b"}
	b.Dependencies = []string{"a"}
	if ValidatePlan([]PlanTask{a, b}) == nil {
		t.Fatal("cycle accepted")
	}
	if ValidatePlan([]PlanTask{a}) == nil {
		t.Fatal("missing dependency accepted")
	}
	a.Dependencies = nil
	a.Key = "../escape"
	if ValidatePlan([]PlanTask{a}) == nil {
		t.Fatal("unsafe key accepted")
	}
}

func TestPlanRiskUsesPortableEnum(t *testing.T) {
	task := PlanTask{Key: "task", Title: "task", Objective: "task", Acceptance: []string{"works"}, Areas: []string{"src"}, Domains: []string{"source"}}
	for _, risk := range []string{"low", "medium", "high"} {
		task.Risk = risk
		if err := ValidatePlan([]PlanTask{task}); err != nil {
			t.Fatalf("valid risk %q rejected: %v", risk, err)
		}
	}
	task.Risk = "High: filesystem boundary"
	if err := ValidatePlan([]PlanTask{task}); err == nil {
		t.Fatal("descriptive risk accepted")
	}
}
func TestSchemaMigrationAndFutureRejection(t *testing.T) {
	s := NewSnapshot("project123")
	s.Schema = 0
	b, _ := json.Marshal(s)
	m, migrated, e := Decode(b)
	if e != nil || !migrated || m.Schema != 1 {
		t.Fatal(e)
	}
	s.Schema = 2
	b, _ = json.Marshal(s)
	if _, _, e = Decode(b); e == nil {
		t.Fatal("newer schema accepted")
	}
}
