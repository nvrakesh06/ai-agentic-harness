package engine

import (
	"encoding/json"
	"reflect"
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
	snapshot.Tasks["independent"] = &model.Task{ID: "independent", State: model.Ready, Domains: []string{"independent"}}
	decision := decideCapacity(snapshot, map[string]bool{"reviewing": true}, project, false, 1, now)
	if len(decision.writers) != 1 || decision.writers[0].ID != "independent" {
		t.Fatalf("idle writer slot was not backfilled: %#v", decision)
	}
	if decision.status.ActiveWriters != 1 || decision.status.ActiveReaders != 1 {
		t.Fatalf("useful writer/reader utilization was conflated: %#v", decision.status)
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
