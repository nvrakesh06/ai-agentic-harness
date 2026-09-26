package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func timingFixture() (*Snapshot, time.Time) {
	at := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	s := NewSnapshot("project123")
	s.Controller = Lease{Owner: "owner", Expires: at.Add(time.Hour)}
	return s, at
}

func TestTaskTimingDirectTransitionsNoopsAndStoppedOperatorWait(t *testing.T) {
	before, at := timingFixture()
	s := Clone(before)
	s.Tasks["task"] = &Task{ID: "task", State: Ready}
	AccountTaskTransitions(before, s, at)
	before = Clone(s)
	AccountTaskTransitions(before, s, at.Add(time.Second))
	if !reflect.DeepEqual(before, s) {
		t.Fatal("idle observation moved a durable clock")
	}
	s.Tasks["task"].State = Running // direct assignments use the same accounting
	AccountTaskTransitions(before, s, at.Add(2*time.Second))
	before = Clone(s)
	s.Tasks["task"].State = Blocked
	AccountTaskTransitions(before, s, at.Add(5*time.Second))
	before = Clone(s)
	s.Controller.Owner = ""
	AccountTaskTransitions(before, s, at.Add(9*time.Second))
	before = Clone(s)
	s.Controller.Owner = "resumed"
	AccountTaskTransitions(before, s, at.Add(15*time.Second))
	c := s.Tasks["task"].Timing
	if c.StateMS[Ready] != 2000 || c.StateMS[Running] != 3000 || c.OperatorWaitMS != 4000 || c.StoppedMS != 6000 || c.PartialHistory {
		t.Fatalf("wrong independent timing buckets: %#v", c)
	}
	copy := Clone(s)
	report := Throughput(s, at.Add(16*time.Second))
	if report.Tasks[0].Timing.OperatorWaitMS != 5000 || !reflect.DeepEqual(copy, s) {
		t.Fatal("report mutated persisted clocks or omitted current interval")
	}
}

func TestTaskTimingLeaseExpiryNeverCountsDeadOwnerAsLiveWork(t *testing.T) {
	before, at := timingFixture()
	s := Clone(before)
	s.Controller.Expires = at.Add(3 * time.Second)
	s.Tasks["task"] = &Task{ID: "task", State: Review}
	AccountTaskTransitions(before, s, at)
	before = Clone(s)
	s.Controller.Owner = "replacement"
	s.Controller.Expires = at.Add(time.Hour)
	AccountTaskTransitions(before, s, at.Add(8*time.Second))
	c := s.Tasks["task"].Timing
	if c.StateMS[Review] != 3000 || c.StoppedMS != 5000 {
		t.Fatalf("expired lease counted as work: %#v", c)
	}
}

func TestInterruptedRunDurationAndPinnedRoleRepetition(t *testing.T) {
	s, at := timingFixture()
	s.Tasks["task"] = &Task{ID: "task", State: Review}
	context := &RunContext{Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), Policy: strings.Repeat("c", 64), Rules: strings.Repeat("d", 64), Stage: "review", TaskState: Review}
	s.Runs = []Run{{ID: "first", Task: "task", Role: "qa", Started: at, Context: context, Outcome: "running"}, {ID: "second", Task: "task", Role: "qa", Started: at, Context: context, Outcome: "completed", DurationRecorded: true, DurationMS: 1000}, {ID: "legacy", Task: "task", Role: "qa", Outcome: "interrupted"}}
	FinalizeInterruptedRuns(s, at.Add(4*time.Second), false)
	if s.Runs[0].DurationMS != 4000 || !s.Runs[0].DurationRecorded || s.Runs[0].DurationEstimated || s.Runs[2].DurationRecorded {
		t.Fatalf("incorrect cancellation duration: %#v", s.Runs)
	}
	report := Throughput(s, at.Add(time.Minute))
	if len(report.RepeatedRoles) != 1 || report.RepeatedRoles[0].Runs != 2 || report.RepeatedRoles[0].ProviderMS != 5000 || report.Tasks[0].UnavailableDurations != 1 || report.UnavailableRunContexts != 1 || report.UtilizationAvailable {
		t.Fatalf("incorrect throughput precision: %#v", report)
	}
	different := *context
	different.Rules = strings.Repeat("e", 64)
	s.Runs[1].Context = &different
	if len(Throughput(s, at).RepeatedRoles) != 0 {
		t.Fatal("different rules were treated as repeated identical inputs")
	}
}

func TestThroughputMigrationLeavesHistoricalTimingUnavailable(t *testing.T) {
	s, _ := timingFixture()
	s.Schema = 10
	s.Tasks["task"] = &Task{ID: "task", State: Ready}
	s.Runs = []Run{{Task: "task", Role: "qa", Outcome: "completed", DurationMS: 42}}
	b, _ := json.Marshal(s)
	migrated, changed, err := Decode(b)
	if err != nil || !changed || migrated.Schema != 11 || migrated.Tasks["task"].Timing != nil || migrated.Runs[0].Context != nil {
		t.Fatalf("historical metrics inferred: %#v %v", migrated, err)
	}
	report := Throughput(migrated, time.Now())
	if report.Tasks[0].TimingAvailable || report.Tasks[0].ProviderMS != 42 {
		t.Fatalf("legacy measured provider duration lost: %#v", report)
	}
	s.Schema = StateSchema + 1
	b, _ = json.Marshal(s)
	if _, _, err = Decode(b); err == nil {
		t.Fatal("newer state schema accepted")
	}
}
