package model

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
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

func TestSnapshotClonePreservesEmptyReplanReceiptLedger(t *testing.T) {
	s := NewSnapshot("project123")
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]json.RawMessage
	if err = json.Unmarshal(b, &encoded); err != nil {
		t.Fatal(err)
	}
	if got, ok := encoded["replan_receipts"]; !ok || string(got) != "{}" {
		t.Fatalf("empty replan receipt ledger was not durably encoded: %s", b)
	}
	cloned := Clone(s)
	if cloned.Replans == nil {
		t.Fatal("clone dropped the empty replan receipt ledger")
	}
	delete(encoded, "replan_receipts") // Legacy snapshots remain recoverable.
	legacy, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	recovered, _, err := Decode(legacy)
	if err != nil || recovered.Replans == nil {
		t.Fatalf("legacy receipt omission was not recovered: %#v %v", recovered, err)
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

func TestSupersededDependencyRequiresReplacementCompletion(t *testing.T) {
	s := NewSnapshot("project123")
	s.Tasks["original"] = &Task{ID: "original", ObjectiveID: "objective", State: Superseded, SupersededBy: "replacement"}
	s.Tasks["replacement"] = &Task{ID: "replacement", ObjectiveID: "objective", State: Ready}
	s.Tasks["dependent"] = &Task{ID: "dependent", ObjectiveID: "other", State: Ready, Dependencies: []string{"original"}}
	if DependencyDone(s, "original") || completedDependencies(s, s.Tasks["dependent"]) {
		t.Fatal("superseded original satisfied a dependency before successor completed")
	}
	s.Tasks["replacement"].State = Done
	if !DependencyDone(s, "original") || !completedDependencies(s, s.Tasks["dependent"]) || !ObjectiveComplete(s, "objective") {
		t.Fatal("completed replacement did not satisfy successor-aware dependency closure")
	}
	s.Tasks["replacement"].State = Superseded
	s.Tasks["replacement"].SupersededBy = "original"
	if DependencyDone(s, "original") {
		t.Fatal("supersession cycle satisfied a dependency")
	}
}

func TestSchemaNineMigratesWithoutInventingSupersession(t *testing.T) {
	s := NewSnapshot("project123")
	s.Schema = 9
	s.Tasks["task"] = &Task{ID: "task", State: Ready}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	recovered, changed, err := Decode(b)
	if err != nil || !changed || recovered.Schema != StateSchema || recovered.Tasks["task"].SupersededBy != "" || recovered.Tasks["task"].Replan != nil {
		t.Fatalf("schema nine migration invented replacement state: %#v changed=%v err=%v", recovered, changed, err)
	}
}

func TestRunnablePrioritizesDraftPRContinuationDeterministically(t *testing.T) {
	s := NewSnapshot("project123")
	s.Tasks["a-new"] = &Task{ID: "a-new", State: Ready, Domains: []string{"new"}}
	s.Tasks["z-fix"] = &Task{ID: "z-fix", State: Fix, PR: 42, Domains: []string{"fix"}}
	s.Tasks["dependent"] = &Task{ID: "dependent", State: Ready, Dependencies: []string{"z-fix"}, Domains: []string{"dependent"}}

	ready := Runnable(s, map[string]bool{}, 1)
	if len(ready) != 1 || ready[0].ID != "z-fix" {
		t.Fatalf("draft PR continuation did not win a safe writer slot: %+v", ready)
	}

	delete(s.Tasks, "dependent")
	s.Tasks["y-fix"] = &Task{ID: "y-fix", State: Fix, PR: 43, Domains: []string{"other-fix"}}
	ready = Runnable(s, map[string]bool{}, 2)
	if len(ready) != 2 || ready[0].ID != "y-fix" || ready[1].ID != "z-fix" {
		t.Fatalf("equal draft PR continuations were not deterministically ordered: %+v", ready)
	}
}

func TestRunnableDoesNotLetDraftPRContinuationBypassSafety(t *testing.T) {
	s := NewSnapshot("project123")
	s.Tasks["active"] = &Task{ID: "active", State: Running, Domains: []string{"shared"}}
	s.Tasks["a-new"] = &Task{ID: "a-new", State: Ready, Domains: []string{"new"}}
	s.Tasks["z-fix"] = &Task{ID: "z-fix", State: Fix, PR: 42, Domains: []string{"shared"}}

	ready := Runnable(s, map[string]bool{"active": true}, 2)
	if len(ready) != 1 || ready[0].ID != "a-new" {
		t.Fatalf("conflicting draft PR continuation bypassed writer safety: %+v", ready)
	}
}

func TestRunnableDraftPRPriorityDoesNotReduceWriterOccupancy(t *testing.T) {
	s := NewSnapshot("project123")
	s.Tasks["a-new"] = &Task{ID: "a-new", State: Ready, Domains: []string{"a"}}
	s.Tasks["b-new"] = &Task{ID: "b-new", State: Ready, Domains: []string{"b"}}
	s.Tasks["z-fix"] = &Task{ID: "z-fix", State: Fix, PR: 42, Domains: []string{"a", "b"}}

	ready := Runnable(s, map[string]bool{}, 2)
	if len(ready) != 2 || ready[0].ID != "a-new" || ready[1].ID != "b-new" {
		t.Fatalf("draft PR priority reduced baseline writer occupancy: %+v", ready)
	}
}

func TestRunnableDraftPRPriorityIgnoresBlockedDependants(t *testing.T) {
	s := NewSnapshot("project123")
	s.Tasks["a-fix"] = &Task{ID: "a-fix", State: Fix, PR: 41, Domains: []string{"a"}}
	s.Tasks["a-blocked-dependent"] = &Task{ID: "a-blocked-dependent", State: Blocked, Dependencies: []string{"a-fix"}, Domains: []string{"blocked"}}
	s.Tasks["z-fix"] = &Task{ID: "z-fix", State: Fix, PR: 42, Domains: []string{"z"}}
	s.Tasks["z-waiting-dependent"] = &Task{ID: "z-waiting-dependent", State: Ready, Dependencies: []string{"z-fix"}, Domains: []string{"waiting"}}

	ready := Runnable(s, map[string]bool{}, 1)
	if len(ready) != 1 || ready[0].ID != "z-fix" {
		t.Fatalf("blocked dependant boosted a draft PR ahead of actionable work: %+v", ready)
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

func TestSchemaSixHydratesOnlyUntouchedTaskAreaAssignments(t *testing.T) {
	s := NewSnapshot("project123")
	s.Schema = 6
	head := strings.Repeat("a", 40)
	s.Tasks["ready"] = &Task{ID: "ready", State: Ready, Areas: []string{"src/engine", "README.md"}}
	s.Tasks["started"] = &Task{ID: "started", State: Running, Areas: []string{"legacy-output.txt"}, HeadSHA: head}
	s.Tasks["partial"] = &Task{ID: "partial", State: Ready, AssignedAreas: []string{"cmd/aih"}, AssignedAreaKinds: map[string]string{"cmd/aih": "invalid"}}
	b, _ := json.Marshal(s)
	migrated, changed, err := Decode(b)
	if err != nil || !changed || migrated.Schema != StateSchema {
		t.Fatalf("schema-6 migration failed: %#v changed=%t err=%v", migrated, changed, err)
	}
	ready := migrated.Tasks["ready"]
	if got := fmt.Sprint(ready.AssignedAreas); got != "[src/engine README.md]" || ready.AssignedAreaKinds["src/engine"] != AreaUnknown || ready.AssignedAreaKinds["README.md"] != AreaUnknown {
		t.Fatalf("untouched task ownership was not hydrated fail-closed: %#v", ready)
	}
	if got := migrated.Tasks["started"]; len(got.AssignedAreas) != 0 || len(got.AssignedAreaKinds) != 0 {
		t.Fatalf("started legacy task must not adopt mutable Areas: %#v", got)
	}
	if got := migrated.Tasks["partial"]; got.AssignedAreaKinds["cmd/aih"] != AreaUnknown {
		t.Fatalf("invalid historical kind was not hydrated to unknown: %#v", got)
	}
	copyAreas, ok := ImmutableAreas(ready)
	if !ok || len(copyAreas) != 2 {
		t.Fatalf("immutable areas unavailable: %v %#v", ok, copyAreas)
	}
	copyAreas[0] = "mutated"
	if ready.AssignedAreas[0] == "mutated" {
		t.Fatal("immutable areas returned mutable task storage")
	}
	kinds := ImmutableAreaKinds(ready)
	kinds["src/engine"] = AreaFile
	if ready.AssignedAreaKinds["src/engine"] != AreaUnknown {
		t.Fatal("immutable area kinds returned mutable task storage")
	}
}

func TestAssignedAreaKindsRejectIncompleteCurrentState(t *testing.T) {
	s := NewSnapshot("project123")
	s.Tasks["task"] = &Task{ID: "task", State: Ready, AssignedAreas: []string{"src"}, AssignedAreaKinds: map[string]string{"other": AreaFile}}
	b, _ := json.Marshal(s)
	if _, _, err := Decode(b); err == nil {
		t.Fatal("current state accepted incomplete assigned area kinds")
	}
}

func TestSchemaSevenBlockerOriginMigratesFailClosed(t *testing.T) {
	s := NewSnapshot("project123")
	s.Schema = 7
	s.Tasks["task"] = &Task{ID: "task", State: Blocked, Blocker: &Blocker{Question: "Repair native verification.", Reason: "missing tool", Impact: "task waits", Resume: SyncRequired, Origin: BlockerOriginVerificationOnly}}
	b, _ := json.Marshal(s)
	migrated, changed, err := Decode(b)
	if err != nil || !changed || migrated.Schema != StateSchema || migrated.Tasks["task"].Blocker.Origin != "" {
		t.Fatalf("schema 7 blocker origin did not migrate fail closed: %#v changed=%t err=%v", migrated.Tasks["task"].Blocker, changed, err)
	}
	current := NewSnapshot("project123")
	current.Tasks["task"] = &Task{ID: "task", State: Blocked, Blocker: &Blocker{Question: "Repair native verification.", Reason: "missing tool", Impact: "task waits", Resume: SyncRequired, Origin: BlockerOriginVerificationOnly}}
	b, _ = json.Marshal(current)
	roundTrip, changed, err := Decode(b)
	if err != nil || changed || roundTrip.Tasks["task"].Blocker.Origin != BlockerOriginVerificationOnly {
		t.Fatalf("schema 8 blocker origin did not round-trip: %#v changed=%t err=%v", roundTrip.Tasks["task"].Blocker, changed, err)
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
	migrated.Tasks["task"].Preflight = &Preflight{Phase: "waiting", BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Config: fmt.Sprintf("%064x", 1), Rules: fmt.Sprintf("%064x", 2), Scope: fmt.Sprintf("%064x", 3), ReuseCount: 1, ReuseReason: "reused unchanged bounded FIX guidance", Completed: []string{"architecture"}}
	b, _ = json.Marshal(migrated)
	recovered, changed, err := Decode(b)
	if err != nil || changed || recovered.Tasks["task"].Preflight.Phase != "waiting" || recovered.Tasks["task"].Preflight.ReuseCount != 1 || recovered.Tasks["task"].Preflight.ReuseReason == "" || fmt.Sprint(recovered.Tasks["task"].Preflight.Completed) != "[architecture]" {
		t.Fatal(recovered, changed, err)
	}
}

func TestSchemaThreeSnapshotMigratesVisualEvidenceAndRequiresMatchingConfig(t *testing.T) {
	// This is a current schema-3 shaped review snapshot: capacity, preflight,
	// roster, and ordinary exact-head evidence were already durable before v4.
	s := NewSnapshot("project123")
	s.Schema = 3
	head := strings.Repeat("a", 40)
	configHash := strings.Repeat("b", 64)
	s.Tasks["task-a"] = &Task{ID: "task-a", State: Review, Preflight: &Preflight{Phase: "ready", BaseSHA: head, HeadSHA: head, Config: configHash, Rules: strings.Repeat("c", 64), Completed: []string{"designer"}}, Evidence: &Evidence{Base: head, Head: head, Config: configHash, Rules: strings.Repeat("c", 64), Checks: []string{"check=tests exit=0"}, Reviews: map[string]string{}, ReviewRoster: []string{"reviewer"}}}
	b, _ := json.Marshal(s)
	migrated, changed, err := Decode(b)
	if err != nil || !changed || migrated.Schema != StateSchema || migrated.Tasks["task-a"].Evidence.Visual != nil {
		t.Fatalf("schema-3 migration changed current review state: %#v %v", migrated, err)
	}
	visual := &VisualEvidence{Head: head, Config: configHash, Manifest: "visual-evidence/task-a/" + head + "-" + configHash[:16] + "/manifest.json", ManifestSHA256: strings.Repeat("d", 64), Artifacts: []VisualArtifact{{Path: "desktop.png", SHA256: strings.Repeat("e", 64)}}, Summary: "blank page"}
	migrated.Tasks["task-a"].Evidence.Visual = visual
	b, _ = json.Marshal(migrated)
	if _, changed, err := Decode(b); err != nil || changed {
		t.Fatalf("valid visual evidence rejected: %v", err)
	}
	visual.Config = strings.Repeat("f", 64)
	b, _ = json.Marshal(migrated)
	if _, _, err := Decode(b); err == nil {
		t.Fatal("visual config different from evidence config accepted")
	}
}

func TestSchemaFiveMigratesReviewProvenanceWithoutInventingReuse(t *testing.T) {
	s := NewSnapshot("project123")
	s.Schema = 5
	head := strings.Repeat("a", 40)
	s.Tasks["task"] = &Task{ID: "task", State: Review, HeadSHA: head}
	b, _ := json.Marshal(s)
	migrated, changed, err := Decode(b)
	if err != nil || !changed || migrated.Schema != StateSchema || len(migrated.Tasks["task"].ReviewProvenance) != 0 {
		t.Fatalf("schema-5 migration invented review reuse: %#v changed=%t err=%v", migrated.Tasks["task"], changed, err)
	}
	configHash, rulesHash, scope := strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	migrated.Tasks["task"].ReviewProvenance = map[string]ReviewProvenance{"security": {Role: "security", Base: head, Head: head, Config: configHash, Rules: rulesHash, Roster: []string{"qa", "reviewer", "security"}, Scope: scope, Provider: "codex", Runtime: "codex/gpt-6-sol/1.0.0", Summary: "passed", CompletedAt: time.Now().UTC()}}
	b, _ = json.Marshal(migrated)
	if _, changed, err = Decode(b); err != nil || changed {
		t.Fatalf("valid durable provenance rejected: changed=%t err=%v", changed, err)
	}
	migrated.Tasks["task"].ReviewProvenance["security"] = ReviewProvenance{Role: "security", Base: head, Head: head, Config: "bad", Rules: rulesHash, Roster: []string{"security"}, Scope: scope, Provider: "codex", Runtime: "runtime", CompletedAt: time.Now().UTC()}
	b, _ = json.Marshal(migrated)
	if _, _, err = Decode(b); err == nil {
		t.Fatal("invalid provenance identity accepted")
	}
}

func TestSchemaSevenMigratesFindingRelevanceFailClosed(t *testing.T) {
	s := NewSnapshot("project123")
	s.Schema = 7
	s.Tasks["task"] = &Task{ID: "task", State: Review, Findings: []Finding{{Severity: "medium", Location: "internal/studio/base.go:12", Reason: "Historical observation."}}}
	b, _ := json.Marshal(s)
	migrated, changed, err := Decode(b)
	if err != nil || !changed || migrated.Schema != StateSchema {
		t.Fatalf("schema-7 migration failed: %#v changed=%t err=%v", migrated, changed, err)
	}
	if got := migrated.Tasks["task"].Findings[0].Relevance; got != FindingUnknown {
		t.Fatalf("historical finding relevance = %q, want %q", got, FindingUnknown)
	}
}

func TestDirectFixWaiverRoundTripsAndRejectsIncompleteIdentity(t *testing.T) {
	s := NewSnapshot("project123")
	base, head := fmt.Sprintf("%040x", 1), fmt.Sprintf("%040x", 2)
	configHash, rulesHash, scope, findings := fmt.Sprintf("%064x", 3), fmt.Sprintf("%064x", 4), fmt.Sprintf("%064x", 5), fmt.Sprintf("%064x", 6)
	s.Tasks["task"] = &Task{ID: "task", State: Fix, HeadSHA: head, Preflight: &Preflight{Phase: "ready", BaseSHA: base, HeadSHA: head, Config: configHash, Rules: rulesHash, Scope: scope, DirectFix: &DirectFixWaiver{Role: "designer", Disposition: "waived", Reason: "exact-head visual repair", BaseSHA: base, HeadSHA: head, Config: configHash, Rules: rulesHash, Scope: scope, Findings: findings}}}
	b, _ := json.Marshal(s)
	decoded, changed, err := Decode(b)
	if err != nil || changed || decoded.Tasks["task"].Preflight.DirectFix == nil || decoded.Tasks["task"].Preflight.DirectFix.Findings != findings {
		t.Fatalf("direct FIX waiver did not round-trip: %#v changed=%t err=%v", decoded, changed, err)
	}
	s.Tasks["task"].Preflight.DirectFix.Findings = "not-a-fingerprint"
	b, _ = json.Marshal(s)
	if _, _, err = Decode(b); err == nil {
		t.Fatal("incomplete direct FIX waiver identity accepted")
	}
}

func TestEightVisualTargetsAndDiagnosticsRoundTrip(t *testing.T) {
	s := NewSnapshot("project123")
	head := strings.Repeat("a", 40)
	configHash := strings.Repeat("b", 64)
	artifacts := make([]VisualArtifact, 0, MaxVisualEvidenceArtifacts)
	for i := 0; i < 8; i++ {
		artifacts = append(artifacts, VisualArtifact{Path: fmt.Sprintf("target-%d.png", i), SHA256: strings.Repeat("c", 64)})
	}
	artifacts = append(artifacts, VisualArtifact{Path: "network.txt", SHA256: strings.Repeat("d", 64)})
	visual := &VisualEvidence{
		Head: head, Config: configHash,
		Manifest:       "visual-evidence/task-a/" + head + "-" + configHash[:16] + "/manifest.json",
		ManifestSHA256: strings.Repeat("e", 64), Artifacts: artifacts, Summary: "eight exact-head states",
	}
	s.Tasks["task-a"] = &Task{ID: "task-a", State: Review, Evidence: &Evidence{
		Base: head, Head: head, Config: configHash, Rules: strings.Repeat("f", 64),
		Checks: []string{"check=tests exit=0"}, Reviews: map[string]string{}, Visual: visual,
	}}
	b, _ := json.Marshal(s)
	decoded, changed, err := Decode(b)
	if err != nil || changed || len(decoded.Tasks["task-a"].Evidence.Visual.Artifacts) != MaxVisualEvidenceArtifacts {
		t.Fatalf("nine bounded visual artifacts did not round trip: changed=%t err=%v", changed, err)
	}
	visual.Artifacts = append(visual.Artifacts, VisualArtifact{Path: "extra.png", SHA256: strings.Repeat("c", 64)})
	b, _ = json.Marshal(s)
	if _, _, err := Decode(b); err == nil {
		t.Fatal("unbounded visual artifact list accepted")
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

func TestOperatorGuidanceScopesFixAndDeduplicates(t *testing.T) {
	head, configHash, rules := strings.Repeat("a", 40), strings.Repeat("b", 64), strings.Repeat("c", 64)
	for _, state := range []State{Ready, Running, Fix, SyncRequired} {
		task := &Task{ID: "task", State: state, HeadSHA: head}
		if err := QueueOperatorGuidance(task, "operator-1", head, head, configHash, rules, "Keep the listener owned by the task."); err != nil {
			t.Fatalf("%s operator guidance rejected: %v", state, err)
		}
		if err := QueueOperatorGuidance(task, "operator-2", head, head, configHash, rules, "Keep the listener owned by the task."); err == nil {
			t.Fatal("duplicate operator guidance accepted")
		}
	}
}

func TestOperatorGuidanceUsesBaseBeforeFirstCheckoutAndExpiresByPolicy(t *testing.T) {
	base, configHash, rules := strings.Repeat("a", 40), strings.Repeat("b", 64), strings.Repeat("c", 64)
	task := &Task{ID: "fresh", State: Ready, BaseSHA: base}
	if err := QueueOperatorGuidance(task, "operator", base, base, configHash, rules, "Start with the supplied contract."); err != nil {
		t.Fatal(err)
	}
	if got := EligibleGuidance(task, base, configHash, rules); len(got) != 1 {
		t.Fatalf("fresh READY guidance missing: %#v", got)
	}
	if got := EligibleGuidance(task, base, strings.Repeat("d", 64), rules); len(got) != 0 {
		t.Fatalf("stale policy guidance delivered: %#v", got)
	}
	if got := EligibleGuidance(task, base, configHash, strings.Repeat("e", 64)); len(got) != 0 {
		t.Fatalf("stale rule guidance delivered: %#v", got)
	}
	if got := EligibleGuidance(task, strings.Repeat("f", 40), configHash, rules); len(got) != 0 {
		t.Fatalf("main-only base change retained guidance: %#v", got)
	}
}
