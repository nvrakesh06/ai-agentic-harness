package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func capacityFixture() (*model.Snapshot, config.Project, time.Time) {
	project := config.Defaults()
	project.Scheduling.UnderutilizationGraceSeconds = 0
	snapshot := model.NewSnapshot("project123")
	snapshot.Capacity = configuredCapacity(project, snapshot.Capacity)
	return snapshot, project, time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
}

func TestCapacityBackfillsWriterWhenPeerEntersReview(t *testing.T) {
	snapshot, project, now := capacityFixture()
	snapshot.Tasks["reviewing"] = &model.Task{ID: "reviewing", State: model.Review, Domains: []string{"reviewed"}}
	snapshot.Tasks["independent"] = &model.Task{ID: "independent", State: model.Ready, Domains: []string{"independent"}, Preflight: &model.Preflight{Phase: "ready"}}
	decision := decideCapacity(snapshot, map[string]bool{"reviewing": true}, project, false, 1, now)
	if len(decision.writers) != 1 || decision.writers[0].ID != "independent" {
		t.Fatalf("idle writer slot was not backfilled: %#v", decision)
	}
	if decision.status.ActiveWriters != 0 || decision.status.ActiveReaders != 1 {
		t.Fatalf("useful writer/reader utilization was conflated: %#v", decision.status)
	}
}

func TestAdmissionReportUsesReservationAwareAdmissionPredicates(t *testing.T) {
	snapshot, project, now := capacityFixture()
	project.MaxWriters = 2
	project.Scheduling.TargetWriters = 2
	ready := func(id string, domains ...string) *model.Task {
		return &model.Task{ID: id, State: model.Ready, Domains: domains, Preflight: &model.Preflight{Phase: "ready"}}
	}
	snapshot.Tasks["writer"] = &model.Task{ID: "writer", State: model.Running, Domains: []string{"shared"}}
	snapshot.Tasks["prepared"] = ready("prepared", "independent")
	snapshot.Tasks["dependency"] = ready("dependency", "dependency")
	snapshot.Tasks["dependency"].Dependencies = []string{"producer"}
	snapshot.Tasks["producer"] = &model.Task{ID: "producer", State: model.Review}
	snapshot.Tasks["domain"] = ready("domain", "shared")
	snapshot.Tasks["preflight"] = &model.Task{ID: "preflight", State: model.Ready, Domains: []string{"waiting"}, Preflight: &model.Preflight{Phase: "waiting"}}
	snapshot.Tasks["blocked"] = &model.Task{ID: "blocked", State: model.Blocked}
	snapshot.Objectives["decision"] = &model.Objective{ID: "decision", Blocker: "human answer required"}

	active := map[string]bool{"writer": true}
	report := AdmissionReportForReservations(snapshot, active, project, now)
	if report.Availability != "reservation_aware" || report.Counts == nil {
		t.Fatalf("reservation-aware report unavailable: %#v", report)
	}
	counts := report.Counts
	if counts.ActiveReservations != 1 || counts.ActiveWriters != 1 || counts.ActivePreflights != 0 || counts.PreparedAdmittable != 1 || counts.DependencyBlocked != 1 || counts.DomainBlocked != 1 || counts.PreflightWaiting != 1 || counts.DecisionBlocked != 2 {
		t.Fatalf("unexpected independent admission counts: %#v", counts)
	}
	decision := decideCapacity(snapshot, active, project, false, 0, now)
	if len(decision.writers) != counts.PreparedAdmittable || len(decision.writers) != 1 || decision.writers[0].ID != "prepared" {
		t.Fatalf("report diverged from scheduler admission: report=%#v decision=%#v", report, decision)
	}
	durable := DurableAdmissionReport(snapshot, "controller reservations unavailable")
	if durable.Availability != "durable_eligibility" || durable.Counts != nil || durable.Durable == nil || durable.Durable.DependencyBlocked != 1 || durable.Durable.PreflightPending != 1 || durable.Durable.DecisionBlocked != 2 || strings.Join(durable.Unavailable, ",") != "active_reservations,prepared_admittable,domain_blocked" {
		t.Fatalf("durable report fabricated reservation-aware counts: %#v", durable)
	}
}

func TestCapacityPrefersWriterReadyDraftPRContinuation(t *testing.T) {
	snapshot, project, now := capacityFixture()
	project.MaxWriters = 1
	project.Scheduling.TargetWriters = 1
	snapshot.Tasks["a-new"] = &model.Task{ID: "a-new", State: model.Ready, Domains: []string{"new"}, Preflight: &model.Preflight{Phase: "ready"}}
	snapshot.Tasks["z-fix"] = &model.Task{ID: "z-fix", State: model.Fix, PR: 42, Domains: []string{"fix"}, Preflight: &model.Preflight{Phase: "ready"}}

	decision := decideCapacity(snapshot, nil, project, false, 0, now)
	if len(decision.writers) != 1 || decision.writers[0].ID != "z-fix" {
		t.Fatalf("writer-ready draft PR continuation did not win the available slot: %#v", decision)
	}

	snapshot.Tasks["z-fix"].Preflight.Phase = "waiting"
	decision = decideCapacity(snapshot, nil, project, false, 0, now)
	if len(decision.writers) != 1 || decision.writers[0].ID != "a-new" {
		t.Fatalf("draft PR continuation without ready preflight displaced fresh writer work: %#v", decision)
	}
}

func TestQueuedDesignerDoesNotConsumeWriterCapacity(t *testing.T) {
	snapshot, project, now := capacityFixture()
	project.MaxWriters = 2
	project.Scheduling.TargetWriters = 2
	snapshot.Tasks["existing"] = &model.Task{ID: "existing", State: model.Running, Domains: []string{"existing"}}
	snapshot.Tasks["dashboard"] = &model.Task{ID: "dashboard", State: model.Ready, UI: true, Domains: []string{"ui"}, Preflight: &model.Preflight{Phase: "waiting"}}
	snapshot.Tasks["http"] = &model.Task{ID: "http", State: model.Ready, Domains: []string{"http"}, Preflight: &model.Preflight{Phase: "ready"}}
	active := map[string]bool{"existing": true, "dashboard": false}
	decision := decideCapacity(snapshot, active, project, false, 2, now)
	if len(decision.writers) != 1 || decision.writers[0].ID != "http" {
		t.Fatalf("reader-queued UI preflight suppressed independent writer: %#v", decision)
	}
	if decision.status.ActiveWriters != 1 || decision.status.ActivePreflights != 1 || decision.status.ActiveReaders != 2 {
		t.Fatalf("capacity does not distinguish actual writers and waiting guidance: %#v", decision.status)
	}
	if decision.status.State != "dispatching" || decision.status.ReasonCode != "writer_admission" {
		t.Fatalf("imminent writer was reported as already active: %#v", decision.status)
	}
	snapshot.Capacity = decision.status
	again := decideCapacity(snapshot, active, project, false, 2, now.Add(time.Second))
	if again.status.ActivePreflights != 1 {
		t.Fatalf("preflight count accumulated across ticks: %#v", again.status)
	}
}

func TestCapacityConsumesQueuedObjectivesWithoutReplanningCompletedWork(t *testing.T) {
	snapshot, project, now := capacityFixture()
	snapshot.Objectives["first"] = &model.Objective{ID: "first", Planned: true}
	snapshot.Objectives["second"] = &model.Objective{ID: "second"}
	snapshot.Backlog = []string{"first", "second"}
	snapshot.Capacity.BacklogCursor = 1
	decision := decideCapacity(snapshot, nil, project, false, 0, now)
	if decision.planObjective != "second" || decision.status.BacklogCursor != 2 || decision.status.LastDispatch != "second" {
		t.Fatalf("wrong authorized backlog selection: %#v", decision)
	}
	snapshot.Capacity = decision.status
	snapshot.Objectives["second"].Planned = true
	next := decideCapacity(snapshot, nil, project, false, 0, now.Add(time.Second))
	if next.planObjective != "" || next.status.ReasonCode != "authorized_backlog_empty" {
		t.Fatalf("completed objective was planned again: %#v", next)
	}
}

func TestCapacityCursorAndPolicySurviveSnapshotRecovery(t *testing.T) {
	snapshot, project, now := capacityFixture()
	snapshot.Objectives["one"] = &model.Objective{ID: "one", Planned: true}
	snapshot.Objectives["two"] = &model.Objective{ID: "two"}
	snapshot.Backlog = []string{"one", "two"}
	snapshot.Capacity.BacklogCursor = 1
	snapshot.Capacity.LastDispatch = "one"
	b, _ := json.Marshal(snapshot)
	recovered, migrated, err := model.Decode(b)
	if err != nil || migrated {
		t.Fatal(err, migrated)
	}
	decision := decideCapacity(recovered, nil, project, false, 0, now)
	if decision.planObjective != "two" || decision.status.TargetWriters != 2 || decision.status.LastDispatch != "two" {
		t.Fatalf("restart lost throughput policy/backlog position: %#v", decision)
	}
}

func TestCapacityExplainsDependencyConflictAndEmptyBacklog(t *testing.T) {
	snapshot, project, now := capacityFixture()
	snapshot.Tasks["active"] = &model.Task{ID: "active", State: model.Running, Domains: []string{"renderer"}}
	snapshot.Tasks["dependent"] = &model.Task{ID: "dependent", State: model.Ready, Dependencies: []string{"missing"}, Domains: []string{"other"}}
	snapshot.Tasks["missing"] = &model.Task{ID: "missing", State: model.Review, Domains: []string{"contract"}}
	snapshot.Tasks["conflict"] = &model.Task{ID: "conflict", State: model.Ready, Domains: []string{"renderer"}}
	decision := decideCapacity(snapshot, map[string]bool{"active": true}, project, false, 0, now)
	if decision.status.ReasonCode != "dependencies" || decision.status.NextSafeWork == "" {
		t.Fatalf("dependency suppression was not explained: %#v", decision.status)
	}
	delete(snapshot.Tasks, "dependent")
	decision = decideCapacity(snapshot, map[string]bool{"active": true}, project, false, 0, now)
	if decision.status.ReasonCode != "conflict_domains" {
		t.Fatalf("conflict suppression was not explained: %#v", decision.status)
	}
	delete(snapshot.Tasks, "conflict")
	decision = decideCapacity(snapshot, map[string]bool{"active": true}, project, false, 0, now)
	if decision.status.ReasonCode != "authorized_backlog_empty" {
		t.Fatalf("empty authorized backlog was not explained: %#v", decision.status)
	}
}

func TestCapacityNoWorkDecisionIsStableAcrossSchedulerTicks(t *testing.T) {
	snapshot, project, now := capacityFixture()
	first := decideCapacity(snapshot, nil, project, false, 0, now)
	var firstKind string
	first.status, firstKind = withCapacityTransition(snapshot.Capacity, first.status, first.planObjective, now)
	if firstKind != "capacity_backfill_suppressed" || len(first.status.Transitions) != 1 {
		t.Fatalf("first idle transition was not recorded once: %#v", first.status)
	}
	snapshot.Capacity = first.status
	second := decideCapacity(snapshot, nil, project, false, 0, now.Add(400*time.Millisecond))
	var secondKind string
	second.status, secondKind = withCapacityTransition(snapshot.Capacity, second.status, second.planObjective, now.Add(400*time.Millisecond))
	if !reflect.DeepEqual(first.status, second.status) || secondKind != "" || second.planObjective != "" || len(second.writers) != 0 {
		t.Fatalf("idle scheduler would create state churn: first=%#v second=%#v", first, second)
	}
}

func TestCapacitySnapshotMatchesFullCloneAndIsDetached(t *testing.T) {
	snapshot, _, _ := capacityFixture()
	zone := time.FixedZone("fixture", 5*60*60+30*60)
	snapshot.Capacity = model.Capacity{
		Verification: []model.VerificationCheck{{Task: "task", Check: "check", Class: "heavy", Phase: "running", QueuedAt: time.Date(2026, 9, 26, 9, 0, 0, 0, zone)}},
		Transitions:  []model.CapacityTransition{{At: time.Date(2026, 9, 26, 9, 1, 0, 0, zone), Kind: "capacity_backfill_selected", Objective: "objective"}},
	}
	controller := &Controller{s: snapshot}
	want := model.Clone(snapshot).Capacity
	got := controller.capacitySnapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capacity-only clone differs from full snapshot clone:\n got %#v\nwant %#v", got, want)
	}
	got.Verification[0].Check = "changed"
	got.Transitions[0].Kind = "changed"
	if snapshot.Capacity.Verification[0].Check != "check" || snapshot.Capacity.Transitions[0].Kind != "capacity_backfill_selected" {
		t.Fatalf("capacity snapshot mutation reached controller state: %#v", snapshot.Capacity)
	}
	snapshot.Capacity.Verification[0].Phase = "complete"
	snapshot.Capacity.Transitions[0].Objective = "other"
	if got.Verification[0].Phase != "running" || got.Transitions[0].Objective != "objective" {
		t.Fatalf("controller state mutation reached capacity snapshot: %#v", got)
	}
}

func TestCapacitySnapshotMatchesFullCloneEmptyCollections(t *testing.T) {
	snapshot, _, _ := capacityFixture()
	snapshot.Capacity.Verification = []model.VerificationCheck{}
	snapshot.Capacity.Transitions = []model.CapacityTransition{}
	controller := &Controller{s: snapshot}
	want := model.Clone(snapshot).Capacity
	got := controller.capacitySnapshot()
	if !reflect.DeepEqual(got, want) || got.Verification != nil || got.Transitions != nil {
		t.Fatalf("capacity empty collection normalization = %#v, full clone = %#v", got, want)
	}
}

func TestCapacitySnapshotSerializesConcurrentCapacityMutation(t *testing.T) {
	snapshot, _, _ := capacityFixture()
	snapshot.Capacity.Verification = []model.VerificationCheck{{Check: "0"}}
	snapshot.Capacity.Transitions = []model.CapacityTransition{{Objective: "0"}}
	controller := &Controller{s: snapshot}
	const mutations = 500
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for index := 1; index <= mutations; index++ {
			value := strconv.Itoa(index)
			controller.mu.Lock()
			controller.s.Capacity.Verification = []model.VerificationCheck{{Check: value}}
			controller.s.Capacity.Transitions = []model.CapacityTransition{{Objective: value}}
			controller.mu.Unlock()
		}
	}()
	for index := 0; index < mutations; index++ {
		capacity := controller.capacitySnapshot()
		if len(capacity.Verification) != 1 || len(capacity.Transitions) != 1 || capacity.Verification[0].Check != capacity.Transitions[0].Objective {
			t.Fatalf("capacity snapshot observed a torn mutation: %#v", capacity)
		}
	}
	writer.Wait()
}

func BenchmarkCapacitySnapshotCopy(b *testing.B) {
	for _, size := range []int{512 * 1024, 2 * 1024 * 1024} {
		snapshot := benchmarkCapacitySnapshot(size)
		encoded, _ := json.Marshal(snapshot)
		b.Run(fmt.Sprintf("full_snapshot/%dKiB", len(encoded)/1024), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(encoded)))
			for range b.N {
				_ = model.Clone(snapshot)
			}
		})
		controller := &Controller{s: snapshot}
		b.Run(fmt.Sprintf("capacity_only/%dKiB", len(encoded)/1024), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(encoded)))
			for range b.N {
				_ = controller.capacitySnapshot()
			}
		})
	}
}

func benchmarkCapacitySnapshot(targetBytes int) *model.Snapshot {
	snapshot := model.NewSnapshot("capacity-benchmark")
	payload := strings.Repeat("e", 2048)
	for index := 0; len(snapshot.Tasks)*len(payload) < targetBytes; index++ {
		id := fmt.Sprintf("task-%04d", index)
		snapshot.Tasks[id] = &model.Task{ID: id, Title: id, Objective: "benchmark", State: model.Ready, Areas: []string{"fixture.txt"}, AssignedAreas: []string{"fixture.txt"}, AssignedAreaKinds: map[string]string{"fixture.txt": model.AreaFile}, Decisions: []string{payload}}
	}
	snapshot.Capacity = model.Capacity{Verification: []model.VerificationCheck{{Task: "task-0000", Check: "fixture", Class: "heavy", Phase: "queued", QueuedAt: time.Now().UTC()}}, Transitions: []model.CapacityTransition{{At: time.Now().UTC(), Kind: "capacity_backfill_selected", Objective: "benchmark"}}}
	return snapshot
}

func TestCapacityFirstObjectiveStartsImmediatelyAndLaterBackfillHonorsGrace(t *testing.T) {
	snapshot, project, now := capacityFixture()
	project.Scheduling.UnderutilizationGraceSeconds = 30
	snapshot.Capacity = configuredCapacity(project, snapshot.Capacity)
	snapshot.Objectives["first"] = &model.Objective{ID: "first"}
	snapshot.Objectives["second"] = &model.Objective{ID: "second"}
	snapshot.Backlog = []string{"first", "second"}

	first := decideCapacity(snapshot, nil, project, false, 0, now)
	if first.planObjective != "first" || first.status.ReasonCode != "backfill_selected" {
		t.Fatalf("initial authorized work paid the utilization grace: %#v", first)
	}
	snapshot.Capacity = first.status
	snapshot.Objectives["first"].Planned = true
	snapshot.Tasks["first-done"] = &model.Task{ID: "first-done", ObjectiveID: "first", State: model.Done}
	next := decideCapacity(snapshot, nil, project, false, 0, now.Add(time.Second))
	if next.planObjective != "" || next.status.ReasonCode != "grace_period" || next.status.NextSafeWork != "second" {
		t.Fatalf("later backfill did not honor the utilization grace: %#v", next)
	}
	snapshot.Capacity = next.status
	afterGrace := decideCapacity(snapshot, nil, project, false, 0, now.Add(31*time.Second))
	if afterGrace.planObjective != "second" {
		t.Fatalf("authorized backfill did not start after the grace: %#v", afterGrace)
	}
}

func TestCapacityTransitionHistoryIsBounded(t *testing.T) {
	previous := model.Capacity{TargetWriters: 2}
	for i := 0; i < model.CapacityTransitionLimit+5; i++ {
		next := previous
		next.State = "underutilized"
		next.ReasonCode = "grace_period"
		next.UnderutilizedSince = time.Date(2026, 9, 23, 12, 0, i, 0, time.UTC)
		previous.State = "satisfied"
		previous.ReasonCode = ""
		previous, _ = withCapacityTransition(previous, next, "", next.UnderutilizedSince)
	}
	if len(previous.Transitions) != model.CapacityTransitionLimit || previous.Transitions[0].At.Second() != 5 {
		t.Fatalf("capacity transitions were not retained as a bounded tail: %#v", previous.Transitions)
	}
}
