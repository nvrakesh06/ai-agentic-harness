package model

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"
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
	if e != nil || !migrated || m.Schema != StateSchema {
		t.Fatal(e)
	}
	s.Schema = StateSchema + 1
	b, _ = json.Marshal(s)
	if _, _, e = Decode(b); e == nil {
		t.Fatal("newer schema accepted")
	}
}

func TestSchemaOneMigratesAuthorizedBacklogAndCapacityRoundTrips(t *testing.T) {
	s := NewSnapshot("project123")
	s.Schema = 1
	s.Backlog = nil
	s.Objectives["objective-b"] = &Objective{ID: "objective-b", Text: "b"}
	s.Objectives["objective-a"] = &Objective{ID: "objective-a", Text: "a"}
	b, _ := json.Marshal(s)
	migrated, changed, err := Decode(b)
	if err != nil || !changed || migrated.Schema != StateSchema || fmt.Sprint(migrated.Backlog) != "[objective-a objective-b]" {
		t.Fatal(migrated, changed, err)
	}
	migrated.Capacity = Capacity{TargetWriters: 2, MaxWriters: 3, MaxReaders: 4, GraceSeconds: 30, BacklogSource: "queued_objectives", BacklogCursor: 1, State: "underutilized", ReasonCode: "dependencies", Transitions: []CapacityTransition{{At: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC), Kind: "capacity_backfill_suppressed", ActiveWriters: 1, TargetWriters: 2, ReasonCode: "dependencies"}}}
	b, _ = json.Marshal(migrated)
	roundTrip, changed, err := Decode(b)
	if err != nil || changed || roundTrip.Schema != StateSchema || roundTrip.Capacity.BacklogCursor != 1 || roundTrip.Capacity.ReasonCode != "dependencies" || len(roundTrip.Capacity.Transitions) != 1 {
		t.Fatal(roundTrip, changed, err)
	}
}

func TestVerificationRetryGuardRoundTripsAndValidates(t *testing.T) {
	s := NewSnapshot("project123")
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte("native verification")))
	s.Tasks["task"] = &Task{ID: "task", State: SyncRequired, Verification: &Verification{Environment: "windows/native/powershell", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Fingerprint: fingerprint, Attempts: 1, NativeOnly: true}}
	b, _ := json.Marshal(s)
	decoded, _, err := Decode(b)
	if err != nil || decoded.Tasks["task"].Verification == nil || !decoded.Tasks["task"].Verification.NativeOnly {
		t.Fatal("verification retry guard did not round-trip", err)
	}
	s.Tasks["task"].Verification.Fingerprint = "not-a-fingerprint"
	b, _ = json.Marshal(s)
	if _, _, err = Decode(b); err == nil {
		t.Fatal("invalid verification retry guard accepted")
	}
}
