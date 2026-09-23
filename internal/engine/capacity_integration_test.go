package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func TestCapacitySnapshotSurvivesLostMachineAttach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	firstID := "capacity-first"
	secondID := "capacity-second"
	snapshot, stateHead, err := fixture.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Objectives[firstID] = &model.Objective{ID: firstID, Text: "FIRST_CAPACITY_OBJECTIVE", Planned: true}
	snapshot.Objectives[secondID] = &model.Objective{ID: secondID, Text: "SECOND_CAPACITY_OBJECTIVE"}
	snapshot.Backlog = []string{firstID, secondID}
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	snapshot.Capacity = model.Capacity{
		ActiveWriters: 1, TargetWriters: 2, MaxWriters: 3,
		ActiveReaders: 1, MaxReaders: 2, GraceSeconds: 30,
		BacklogSource: "queued_objectives", BacklogCursor: 1,
		State: "underutilized", ReasonCode: "grace_period",
		Reason:       "writer utilization is below target during the configured grace period",
		NextSafeWork: secondID, LastDispatch: firstID, UnderutilizedSince: at,
		Transitions: []model.CapacityTransition{
			{At: at, Kind: "capacity_underutilized", ActiveWriters: 1, TargetWriters: 2, ReasonCode: "grace_period"},
			{At: at.Add(time.Second), Kind: "capacity_backfill_selected", ActiveWriters: 1, TargetWriters: 2, ReasonCode: "backfill_selected", Objective: firstID},
		},
	}
	snapshot.Revision++
	nextHead, err := fixture.P.Git.StateCommit(ctx, stateHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: nextHead}}); err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.DB.Save(nextHead, snapshot); err != nil {
		t.Fatal(err)
	}

	projectDir := fixture.P.Dir
	portableTransitions := append([]model.CapacityTransition(nil), snapshot.Capacity.Transitions...)
	if err = fixture.P.DB.Close(); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(fixture.Root, projectDir)
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
		t.Fatal("unsafe fixture deletion target", projectDir)
	}
	if err = os.RemoveAll(projectDir); err != nil {
		t.Fatal(err)
	}

	replacement, err := fixture.Open(ctx, filepath.Join(fixture.Root, "capacity-machine-b"))
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.DB.Close()
	if err = replacement.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := replacement.DB.Load()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Capacity.TargetWriters != 2 || recovered.Capacity.BacklogCursor != 1 || recovered.Capacity.LastDispatch != firstID || strings.Join(recovered.Backlog, ",") != firstID+","+secondID {
		t.Fatalf("attach lost throughput policy/backlog position: %#v %#v", recovered.Capacity, recovered.Backlog)
	}
	if !reflect.DeepEqual(recovered.Capacity.Transitions, portableTransitions) {
		t.Fatalf("attach lost portable capacity transition history: got=%#v want=%#v", recovered.Capacity.Transitions, portableTransitions)
	}
}
