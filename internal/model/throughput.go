package model

import (
	"errors"
	"regexp"
	"sort"
	"time"
)

// TaskTiming records residence under a controller lease, not CPU use or writer
// utilization. A missing clock is unavailable history, never a zero duration.
type TaskTiming struct {
	ObservedFrom   time.Time       `json:"observed_from"`
	PartialHistory bool            `json:"partial_history"`
	State          State           `json:"state"`
	Since          time.Time       `json:"since"`
	LeaseOwned     bool            `json:"lease_owned"`
	StateMS        map[State]int64 `json:"state_ms"`
	OperatorWaitMS int64           `json:"operator_wait_ms"`
	StoppedMS      int64           `json:"stopped_ms"`
}

// RunContext pins the invocation inputs. An empty head is meaningful for a
// planner or a task without a first checkpoint; legacy nil context is unknown.
type RunContext struct {
	Base      string `json:"base"`
	Head      string `json:"head"`
	Policy    string `json:"policy"`
	Rules     string `json:"rules"`
	Stage     string `json:"stage"`
	TaskState State  `json:"task_state,omitempty"`
}

func terminalTiming(state State) bool { return state == Done || state == Superseded }

func accrueTiming(clock *TaskTiming, lease Lease, at time.Time) {
	if clock == nil || terminalTiming(clock.State) || !at.After(clock.Since) {
		return
	}
	elapsed := at.Sub(clock.Since).Milliseconds()
	owned := elapsed
	if !clock.LeaseOwned {
		owned = 0
	} else if lease.Expires.Before(at) {
		owned = lease.Expires.Sub(clock.Since).Milliseconds()
		if owned < 0 {
			owned = 0
		}
	}
	if clock.State == Blocked {
		clock.OperatorWaitMS += owned
	} else {
		clock.StateMS[clock.State] += owned
	}
	clock.StoppedMS += elapsed - owned
}

// AccountTaskTransitions is called only after a meaningful mutation has been
// established. A tick, lease renewal, or unrelated mutation does not move clocks.
func AccountTaskTransitions(before, after *Snapshot, at time.Time) {
	ownerChanged := before.Controller.Owner != after.Controller.Owner
	for id, task := range after.Tasks {
		prior := before.Tasks[id]
		if prior != nil && prior.State == task.State && !ownerChanged {
			continue
		}
		if task.Timing == nil {
			task.Timing = &TaskTiming{ObservedFrom: at.UTC(), PartialHistory: prior != nil, StateMS: map[State]int64{}}
		} else {
			accrueTiming(task.Timing, before.Controller, at)
		}
		task.Timing.State = task.State
		task.Timing.Since = at.UTC()
		task.Timing.LeaseOwned = after.Controller.Owner != ""
		if prior == nil || prior.State != task.State {
			task.Updated = at.UTC()
		}
	}
}

// FinalizeInterruptedRuns records cancellation cost at the observed stop
// boundary. Recovery uses an estimated lease boundary rather than claiming a
// dead process continued working until the next machine attached.
func FinalizeInterruptedRuns(s *Snapshot, at time.Time, estimated bool) {
	for i := range s.Runs {
		run := &s.Runs[i]
		if run.Outcome != "running" {
			continue
		}
		run.Outcome = "interrupted"
		if run.Started.IsZero() {
			run.DurationMS = 0
			continue
		}
		run.DurationMS = at.Sub(run.Started).Milliseconds()
		if run.DurationMS < 0 {
			run.DurationMS = 0
		}
		run.DurationRecorded = !run.Started.IsZero()
		run.DurationEstimated = estimated
	}
}

func validateTaskTiming(task *Task) error {
	c := task.Timing
	if c == nil {
		return nil
	}
	if c.ObservedFrom.IsZero() || c.Since.Before(c.ObservedFrom) || c.State != task.State || c.StateMS == nil || c.OperatorWaitMS < 0 || c.StoppedMS < 0 {
		return errors.New("invalid task timing")
	}
	for state, duration := range c.StateMS {
		if _, ok := edges[state]; !ok && state != Done && state != Superseded {
			return errors.New("invalid timed task state")
		}
		if duration < 0 {
			return errors.New("negative task state duration")
		}
	}
	return nil
}

func validateRunMetrics(runs []Run) error {
	sha := regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
	hash := regexp.MustCompile(`^[a-f0-9]{64}$`)
	for _, run := range runs {
		if run.Context == nil {
			continue
		}
		c := run.Context
		if !sha.MatchString(c.Base) || (c.Head != "" && !sha.MatchString(c.Head)) || !hash.MatchString(c.Policy) || !hash.MatchString(c.Rules) || len(c.Stage) == 0 || len(c.Stage) > 80 || run.Started.IsZero() || run.DurationMS < 0 || (run.DurationEstimated && !run.DurationRecorded) {
			return errors.New("invalid run throughput provenance")
		}
	}
	return nil
}

type TaskThroughput struct {
	Task                 string           `json:"task"`
	State                State            `json:"state"`
	Timing               *TaskTiming      `json:"timing,omitempty"`
	TimingAvailable      bool             `json:"timing_available"`
	Runs                 int              `json:"runs"`
	ProviderMS           int64            `json:"provider_ms"`
	ProviderStageMS      map[string]int64 `json:"provider_stage_ms"`
	ActiveRuns           int              `json:"active_runs"`
	UnavailableDurations int              `json:"unavailable_durations"`
	EstimatedDurations   int              `json:"estimated_durations"`
}

type RoleRepetition struct {
	Task       string     `json:"task"`
	Role       string     `json:"role"`
	Context    RunContext `json:"context"`
	Runs       int        `json:"runs"`
	ProviderMS int64      `json:"provider_ms"`
}

type ThroughputReport struct {
	At                     time.Time        `json:"at"`
	Project                string           `json:"project"`
	Tasks                  []TaskThroughput `json:"tasks"`
	RepeatedRoles          []RoleRepetition `json:"repeated_roles"`
	UnavailableRunContexts int              `json:"unavailable_run_contexts"`
	UtilizationAvailable   bool             `json:"historical_writer_utilization_available"`
}

// Throughput observes a copy and never updates durable clocks on report reads.
func Throughput(s *Snapshot, at time.Time) ThroughputReport {
	report := ThroughputReport{At: at.UTC(), Project: s.Project, Tasks: []TaskThroughput{}, RepeatedRoles: []RoleRepetition{}}
	totals := map[string]*TaskThroughput{}
	for _, task := range Ordered(s) {
		summary := TaskThroughput{Task: task.ID, State: task.State, TimingAvailable: task.Timing != nil, ProviderStageMS: map[string]int64{}}
		if task.Timing != nil {
			c := *task.Timing
			c.StateMS = map[State]int64{}
			for state, duration := range task.Timing.StateMS {
				c.StateMS[state] = duration
			}
			accrueTiming(&c, s.Controller, at)
			summary.Timing = &c
		}
		totals[task.ID] = &summary
	}
	type repeatKey struct {
		task, role string
		context    RunContext
	}
	repeats := map[repeatKey]*RoleRepetition{}
	for _, run := range s.Runs {
		knownDuration := run.DurationRecorded || run.DurationMS > 0
		duration := run.DurationMS
		if run.Outcome == "running" {
			knownDuration = false
			duration = 0
		}
		if total := totals[run.Task]; total != nil {
			total.Runs++
			if knownDuration {
				total.ProviderMS += duration
				stage := "unknown"
				if run.Context != nil {
					stage = run.Context.Stage
				}
				total.ProviderStageMS[stage] += duration
			} else if run.Outcome == "running" {
				total.ActiveRuns++
			} else {
				total.UnavailableDurations++
			}
			if run.DurationEstimated {
				total.EstimatedDurations++
			}
		}
		if run.Context == nil {
			report.UnavailableRunContexts++
			continue
		}
		// No exact source exists before the first checkpoint, so those invocations
		// are not described as repeated reviews of unchanged source.
		if run.Context.Head == "" {
			continue
		}
		key := repeatKey{run.Task, run.Role, *run.Context}
		group := repeats[key]
		if group == nil {
			group = &RoleRepetition{Task: run.Task, Role: run.Role, Context: *run.Context}
			repeats[key] = group
		}
		group.Runs++
		if knownDuration {
			group.ProviderMS += duration
		}
	}
	for _, task := range Ordered(s) {
		report.Tasks = append(report.Tasks, *totals[task.ID])
	}
	for _, group := range repeats {
		if group.Runs > 1 {
			report.RepeatedRoles = append(report.RepeatedRoles, *group)
		}
	}
	sort.Slice(report.RepeatedRoles, func(i, j int) bool {
		a, b := report.RepeatedRoles[i], report.RepeatedRoles[j]
		if a.Task != b.Task {
			return a.Task < b.Task
		}
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		if a.Context.Stage != b.Context.Stage {
			return a.Context.Stage < b.Context.Stage
		}
		if a.Context.TaskState != b.Context.TaskState {
			return a.Context.TaskState < b.Context.TaskState
		}
		if a.Context.Base != b.Context.Base {
			return a.Context.Base < b.Context.Base
		}
		if a.Context.Head != b.Context.Head {
			return a.Context.Head < b.Context.Head
		}
		if a.Context.Policy != b.Context.Policy {
			return a.Context.Policy < b.Context.Policy
		}
		return a.Context.Rules < b.Context.Rules
	})
	return report
}
