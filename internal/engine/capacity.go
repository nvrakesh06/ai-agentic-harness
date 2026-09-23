package engine

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

type capacityDecision struct {
	status        model.Capacity
	writers       []*model.Task
	planObjective string
}

func configuredCapacity(project config.Project, previous model.Capacity) model.Capacity {
	next := previous
	policyChanged := next.TargetWriters != project.Scheduling.TargetWriters ||
		next.MaxWriters != project.MaxWriters || next.MaxReaders != project.MaxReaders ||
		next.GraceSeconds != project.Scheduling.UnderutilizationGraceSeconds ||
		next.BacklogSource != project.Scheduling.BacklogSource
	next.TargetWriters = project.Scheduling.TargetWriters
	next.MaxWriters = project.MaxWriters
	next.MaxReaders = project.MaxReaders
	next.MaxHeavyChecks = project.Resources.MaxHeavyChecks
	next.MaxLightChecks = project.Resources.MaxLightChecks
	next.GraceSeconds = project.Scheduling.UnderutilizationGraceSeconds
	next.BacklogSource = project.Scheduling.BacklogSource
	if policyChanged {
		next.State = ""
		next.ReasonCode = ""
		next.Reason = ""
		next.NextSafeWork = ""
		next.UnderutilizedSince = time.Time{}
	}
	return next
}

func decideCapacity(s *model.Snapshot, active map[string]bool, project config.Project, planning bool, activeReaders int, now time.Time) capacityDecision {
	status := configuredCapacity(project, s.Capacity)
	status.ActiveReaders = activeReaders
	status.ActivePreflights = 0
	writers := map[string]bool{}
	for id, writing := range active {
		if !writing {
			status.ActivePreflights++
		}
		if task := s.Tasks[id]; writing && task != nil && task.State == model.Running {
			writers[id] = true
		}
	}
	runnable := model.RunnableWhere(s, writers, project.MaxWriters, func(t *model.Task) bool {
		return !hasActive(active, t.ID) && t.Preflight != nil && t.Preflight.Phase == "ready"
	})
	status.ActiveWriters = len(writers)
	decision := capacityDecision{status: status, writers: runnable}
	if status.ActiveWriters >= status.TargetWriters {
		decision.status.State = "satisfied"
		decision.status.ReasonCode = ""
		decision.status.Reason = ""
		decision.status.NextSafeWork = ""
		decision.status.UnderutilizedSince = time.Time{}
		return decision
	}
	if status.ActiveWriters+len(runnable) >= status.TargetWriters {
		decision.status.State = "dispatching"
		decision.status.ReasonCode = "writer_admission"
		decision.status.Reason = "safe prepared tasks are being admitted to writer slots"
		decision.status.NextSafeWork = ""
		decision.status.UnderutilizedSince = time.Time{}
		return decision
	}
	if decision.status.UnderutilizedSince.IsZero() {
		decision.status.UnderutilizedSince = now.UTC()
	}
	decision.status.State = "underutilized"
	if planning {
		decision.status.ReasonCode = "planning_in_progress"
		decision.status.Reason = "an authorized objective is already being planned or provisioned"
		decision.status.NextSafeWork = decision.status.LastDispatch
		return decision
	}
	objective, cursor := nextBacklogObjective(s)
	initialDispatch := status.LastDispatch == "" && len(s.Tasks) == 0 && objective != ""
	if elapsed := now.Sub(decision.status.UnderutilizedSince); !initialDispatch && elapsed < time.Duration(status.GraceSeconds)*time.Second {
		decision.status.ReasonCode = "grace_period"
		decision.status.Reason = "writer utilization is below target during the configured grace period"
		decision.status.NextSafeWork = objective
		return decision
	}
	if objective != "" {
		decision.status.State = "backfilling"
		decision.status.ReasonCode = "backfill_selected"
		decision.status.Reason = "selected the next authorized queued objective"
		decision.status.NextSafeWork = objective
		decision.status.LastDispatch = objective
		decision.status.BacklogCursor = cursor
		decision.planObjective = objective
		return decision
	}
	if status.ActivePreflights > 0 {
		decision.status.ReasonCode = "preflight_in_progress"
		decision.status.Reason = "pre-implementation reader guidance is queued or running"
		return decision
	}
	decision.status.ReasonCode, decision.status.Reason, decision.status.NextSafeWork = capacitySuppression(s, writers)
	return decision
}

func hasActive(active map[string]bool, id string) bool {
	_, ok := active[id]
	return ok
}

func nextBacklogObjective(s *model.Snapshot) (string, int) {
	if len(s.Backlog) == 0 {
		return "", s.Capacity.BacklogCursor
	}
	start := s.Capacity.BacklogCursor
	if start >= len(s.Backlog) {
		start = 0
	}
	for offset := 0; offset < len(s.Backlog); offset++ {
		index := (start + offset) % len(s.Backlog)
		id := s.Backlog[index]
		objective := s.Objectives[id]
		if objective != nil && objective.Blocker == "" && (!objective.Planned || needsProvision(s, id)) {
			return id, index + 1
		}
	}
	return "", s.Capacity.BacklogCursor
}

func capacitySuppression(s *model.Snapshot, writers map[string]bool) (string, string, string) {
	domains := map[string]bool{}
	for id := range writers {
		for _, domain := range s.Tasks[id].Domains {
			domains[domain] = true
		}
	}
	dependencyWaits, conflicts, blocked := 0, 0, 0
	next := ""
	for _, task := range model.Ordered(s) {
		if task.State == model.Blocked {
			blocked++
			continue
		}
		if task.State != model.Ready && task.State != model.Fix {
			continue
		}
		var waiting []string
		for _, dependency := range task.Dependencies {
			if s.Tasks[dependency] == nil || s.Tasks[dependency].State != model.Done {
				waiting = append(waiting, dependency)
			}
		}
		if len(waiting) > 0 {
			dependencyWaits++
			if next == "" {
				next = task.ID + " after " + strings.Join(waiting, ",")
			}
			continue
		}
		for _, domain := range task.Domains {
			if domains[domain] {
				conflicts++
				if next == "" {
					next = task.ID + " after conflict domain " + domain + " is released"
				}
				break
			}
		}
	}
	if dependencyWaits > 0 {
		return "dependencies", fmt.Sprintf("%d task(s) wait on unfinished dependencies", dependencyWaits), next
	}
	if conflicts > 0 {
		return "conflict_domains", fmt.Sprintf("%d task(s) conflict with active writer domains", conflicts), next
	}
	blockedObjectives := 0
	for _, objective := range s.Objectives {
		if objective.Blocker != "" {
			blockedObjectives++
		}
	}
	if blocked > 0 || blockedObjectives > 0 {
		return "blocked_work", fmt.Sprintf("%d task(s) and %d objective(s) require recovery or human input", blocked, blockedObjectives), ""
	}
	return "authorized_backlog_empty", "no other authorized queued objective or safe task is ready", ""
}

func (c *Controller) persistCapacity(next model.Capacity, planObjective string) error {
	previous := c.Snapshot().Capacity
	next.Verification = previous.Verification
	next, eventKind := withCapacityTransition(previous, next, planObjective, time.Now().UTC())
	if reflect.DeepEqual(previous, next) {
		return nil
	}
	if err := c.mutate(func(s *model.Snapshot) error {
		next.Verification = s.Capacity.Verification
		s.Capacity = next
		return nil
	}); err != nil {
		return err
	}
	if eventKind != "" {
		detail := next.ReasonCode + ": " + next.Reason
		if planObjective != "" {
			detail = "objective=" + planObjective
		}
		return c.P.DB.Event("", "", "scheduler", "", eventKind, detail)
	}
	return nil
}

func withCapacityTransition(previous, next model.Capacity, planObjective string, at time.Time) (model.Capacity, string) {
	kind := ""
	if planObjective != "" {
		kind = "capacity_backfill_selected"
	} else if next.State != "satisfied" && next.State != "dispatching" && next.ActiveWriters < next.TargetWriters && (previous.State != next.State || previous.ReasonCode != next.ReasonCode) {
		kind = "capacity_underutilized"
		if next.ReasonCode != "grace_period" && next.ReasonCode != "planning_in_progress" {
			kind = "capacity_backfill_suppressed"
		}
	}
	if kind == "" {
		return next, ""
	}
	next.Transitions = append(next.Transitions, model.CapacityTransition{
		At:            at.UTC(),
		Kind:          kind,
		ActiveWriters: next.ActiveWriters,
		TargetWriters: next.TargetWriters,
		ReasonCode:    next.ReasonCode,
		Objective:     planObjective,
	})
	if extra := len(next.Transitions) - model.CapacityTransitionLimit; extra > 0 {
		next.Transitions = append([]model.CapacityTransition(nil), next.Transitions[extra:]...)
	}
	return next, kind
}
