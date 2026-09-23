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

func TestVerificationCapacityRoundTripAndValidation(t *testing.T) {
	s := NewSnapshot("project123")
	s.Tasks["task-a"] = &Task{ID: "task-a", State: Verifying}
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	s.Capacity.Verification = []VerificationCheck{{Task: "task-a", Check: "release", Class: "heavy", Phase: "queued", QueuedAt: at}}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := Decode(b)
	if err != nil || len(decoded.Capacity.Verification) != 1 {
		t.Fatalf("round trip: %+v, %v", decoded, err)
	}
	s.Capacity.Verification[0].Phase = "running"
	b, _ = json.Marshal(s)
	if _, _, err := Decode(b); err == nil {
		t.Fatal("running owner without start accepted")
	}
	s.Capacity.Verification[0].StartedAt = at
	s.Capacity.Verification[0].Task = "missing"
	b, _ = json.Marshal(s)
	if _, _, err := Decode(b); err == nil {
		t.Fatal("unknown owner accepted")
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

func TestSchemaTwoMigrationDropsUnownedCheckRecords(t *testing.T) {
	s := NewSnapshot("project123")
	s.Schema = 2
	s.Tasks["task-a"] = &Task{ID: "task-a", State: Verifying}
	s.Capacity = Capacity{TargetWriters: 2, MaxWriters: 3, MaxReaders: 2, GraceSeconds: 30, BacklogSource: "queued_objectives", MaxHeavyChecks: 8, Verification: []VerificationCheck{{Task: "task-a", Check: "release", Class: "heavy", Phase: "running", QueuedAt: time.Now(), StartedAt: time.Now()}}}
	b, _ := json.Marshal(s)
	migrated, changed, err := Decode(b)
	if err != nil || !changed || migrated.Schema != StateSchema {
		t.Fatal(migrated, changed, err)
	}
	if migrated.Capacity.MaxHeavyChecks != 0 || len(migrated.Capacity.Verification) != 0 {
		t.Fatal("schema 2 invented verification ownership")
	}
	if migrated.Tasks["task-a"].State != Verifying {
		t.Fatal("migration changed lifecycle before controller recovery")
	}
}

func TestSchemaTwoMigratesAndPreflightProgressRoundTrips(t *testing.T) {
	s := NewSnapshot("project123")
	s.Schema = 2
	s.Tasks["task"] = &Task{ID: "task", State: Ready}
	b, _ := json.Marshal(s)
	migrated, changed, err := Decode(b)
	if err != nil || !changed || migrated.Schema != StateSchema || migrated.Tasks["task"].Preflight != nil {
		t.Fatal(migrated, changed, err)
	}
	migrated.Tasks["task"].Preflight = &Preflight{Phase: "waiting", BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Config: fmt.Sprintf("%064x", 1), Rules: fmt.Sprintf("%064x", 2), Completed: []string{"architecture"}}
	b, _ = json.Marshal(migrated)
	recovered, changed, err := Decode(b)
	if err != nil || changed || recovered.Tasks["task"].Preflight.Phase != "waiting" || fmt.Sprint(recovered.Tasks["task"].Preflight.Completed) != "[architecture]" {
		t.Fatal(recovered, changed, err)
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

func TestGuidanceIsBoundedPortableAndScopedToParallelTasks(t *testing.T) {
	s := NewSnapshot("project123")
	source := &Task{ID: "api", ObjectiveID: "objective", State: Running, HeadSHA: fmt.Sprintf("%040x", 42)}
	target := &Task{ID: "ui", ObjectiveID: "objective", State: Running}
	s.Tasks[source.ID], s.Tasks[target.ID] = source, target
	if err := QueueGuidance(target, source, "command-1", "Use src/server/cli.ts --port 4318."); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	recovered, _, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	items := TaskGuidance(recovered.Tasks["ui"])
	if len(items) != 1 || items[0].SourceID != "api" || items[0].SourceSHA != source.HeadSHA || items[0].CommandID != "command-1" || items[0].Text != "Use src/server/cli.ts --port 4318." {
		t.Fatalf("guidance did not survive portable snapshot: %#v", items)
	}
	other := &Task{ID: "other", ObjectiveID: "different", State: Ready}
	if err := QueueGuidance(other, source, "command-2", "unrelated"); err == nil {
		t.Fatal("cross-objective guidance accepted")
	}
	target.State = Review
	if err := QueueGuidance(target, source, "command-3", "too late"); err == nil {
		t.Fatal("review task accepted implementation guidance")
	}
	target.State = Ready
	if err := QueueGuidance(target, source, "command-4", string(make([]byte, MaxGuidanceBytes+1))); err == nil {
		t.Fatal("oversized guidance accepted")
	}
}
