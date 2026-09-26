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

// AdmissionCounts describes the scheduler's current reservation view. The
// classes are independent observations, not a partition or a total: an
// objective-level decision blocker can coexist with a task-level condition.
// Only PreparedAdmittable is constrained by the currently available writer
// slots.
type AdmissionCounts struct {
	ActiveReservations int `json:"active_reservations"`
	ActiveWriters      int `json:"active_writers"`
	ActivePreflights   int `json:"active_preflights"`
	PreparedAdmittable int `json:"prepared_admittable"`
	DependencyBlocked  int `json:"dependency_blocked"`
	DomainBlocked      int `json:"domain_blocked"`
	PreflightWaiting   int `json:"preflight_waiting"`
	DecisionBlocked    int `json:"decision_blocked"`
}

// AdmissionReport is local scheduler observability. It never grants a lease,
// authorizes a task, or replaces the durable capacity record. Counts are
// present only when they were observed from the controller's in-memory
// reservation map.
type AdmissionReport struct {
	Availability string                  `json:"availability"`
	ObservedAt   *time.Time              `json:"observed_at,omitempty"`
	Reason       string                  `json:"reason,omitempty"`
	Counts       *AdmissionCounts        `json:"counts,omitempty"`
	Durable      *DurableAdmissionCounts `json:"durable_counts,omitempty"`
	Unavailable  []string                `json:"unavailable,omitempty"`
}

// DurableAdmissionCounts contains facts that remain true without attempting to
// reconstruct a controller reservation map. These counts are eligibility
// checkpoints, never a live utilization claim.
type DurableAdmissionCounts struct {
	DependencyBlocked int `json:"dependency_blocked"`
	PreflightPending  int `json:"preflight_pending"`
	DecisionBlocked   int `json:"decision_blocked"`
}

// AdmissionReportForReservations reports exact scheduler eligibility only when
// the caller supplies the controller's current reservation map. It is exposed
// for read-only consumers that have that map; status intentionally does not
// synthesize it from durable task state.
func AdmissionReportForReservations(s *model.Snapshot, active map[string]bool, project config.Project, now time.Time) AdmissionReport {
	return admissionInventoryFor(s, active, project, now).AdmissionReport
}

// DurableAdmissionReport reports only independent facts from a portable
// checkpoint. Active reservations, domain conflicts, and current writer-slot
// availability are deliberately unavailable until a caller supplies the live
// controller map through AdmissionReportForReservations.
func DurableAdmissionReport(s *model.Snapshot, reason string) AdmissionReport {
	counts := &DurableAdmissionCounts{}
	for _, task := range model.Ordered(s) {
		if task.State == model.Blocked {
			counts.DecisionBlocked++
			continue
		}
		if task.State != model.Ready && task.State != model.Fix {
			continue
		}
		waiting := false
		for _, dependency := range task.Dependencies {
			if !model.DependencyDone(s, dependency) {
				waiting = true
				break
			}
		}
		if waiting {
			counts.DependencyBlocked++
			continue
		}
		if task.Preflight == nil || task.Preflight.Phase != "ready" {
			counts.PreflightPending++
		}
	}
	for _, objective := range s.Objectives {
		if objective.Blocker != "" {
			counts.DecisionBlocked++
		}
	}
	return AdmissionReport{
		Availability: "durable_eligibility",
		Reason:       strings.TrimSpace(reason),
		Durable:      counts,
		Unavailable:  []string{"active_reservations", "prepared_admittable", "domain_blocked"},
	}
}

type admissionInventory struct {
	AdmissionReport
	writers        map[string]bool
	runnable       []*model.Task
	nextDependency string
	nextDomain     string
	decisionTasks  int
	decisionGoals  int
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
	inventory := admissionInventoryFor(s, active, project, now)
	status.ActivePreflights = inventory.Counts.ActivePreflights
	status.ActiveWriters = inventory.Counts.ActiveWriters
	decision := capacityDecision{status: status, writers: inventory.runnable}
	if status.ActiveWriters >= status.TargetWriters {
		decision.status.State = "satisfied"
		decision.status.ReasonCode = ""
		decision.status.Reason = ""
		decision.status.NextSafeWork = ""
		decision.status.UnderutilizedSince = time.Time{}
		return decision
	}
	if status.ActiveWriters+len(inventory.runnable) >= status.TargetWriters {
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
	decision.status.ReasonCode, decision.status.Reason, decision.status.NextSafeWork = capacitySuppression(inventory)
	return decision
}

// admissionInventoryFor is shared by capacity selection and reservation-aware
// read-only callers. It deliberately calls RunnableWhere with the same eligible
// predicate that decideCapacity uses, so PreparedAdmittable means "can be
// admitted on this scheduler tick", not merely "looks READY in a durable
// snapshot".
func admissionInventoryFor(s *model.Snapshot, active map[string]bool, project config.Project, now time.Time) admissionInventory {
	counts := &AdmissionCounts{ActiveReservations: len(active)}
	writers := map[string]bool{}
	domains := map[string]bool{}
	for id, writing := range active {
		if !writing {
			counts.ActivePreflights++
			continue
		}
		task := s.Tasks[id]
		if task == nil || task.State != model.Running {
			continue
		}
		counts.ActiveWriters++
		writers[id] = true
		for _, domain := range task.Domains {
			domains[domain] = true
		}
	}
	runnable := model.RunnableWhere(s, writers, project.MaxWriters, func(t *model.Task) bool {
		return !hasActive(active, t.ID) && t.Preflight != nil && t.Preflight.Phase == "ready"
	})
	counts.PreparedAdmittable = len(runnable)
	observedAt := now.UTC()
	inventory := admissionInventory{AdmissionReport: AdmissionReport{Availability: "reservation_aware", ObservedAt: &observedAt, Counts: counts}, writers: writers, runnable: runnable}
	for _, task := range model.Ordered(s) {
		if task.State == model.Blocked {
			counts.DecisionBlocked++
			inventory.decisionTasks++
			continue
		}
		if hasActive(active, task.ID) || (task.State != model.Ready && task.State != model.Fix) {
			continue
		}
		var waiting []string
		for _, dependency := range task.Dependencies {
			if !model.DependencyDone(s, dependency) {
				waiting = append(waiting, dependency)
			}
		}
		if len(waiting) != 0 {
			counts.DependencyBlocked++
			if inventory.nextDependency == "" {
				inventory.nextDependency = task.ID + " after " + strings.Join(waiting, ",")
			}
			continue
		}
		for _, domain := range task.Domains {
			if domains[domain] {
				counts.DomainBlocked++
				if inventory.nextDomain == "" {
					inventory.nextDomain = task.ID + " after conflict domain " + domain + " is released"
				}
				break
			}
		}
		if task.Preflight == nil || task.Preflight.Phase != "ready" {
			counts.PreflightWaiting++
		}
	}
	for _, objective := range s.Objectives {
		if objective.Blocker != "" {
			counts.DecisionBlocked++
			inventory.decisionGoals++
		}
	}
	return inventory
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

func capacitySuppression(inventory admissionInventory) (string, string, string) {
	counts := inventory.Counts
	if counts.DependencyBlocked > 0 {
		return "dependencies", fmt.Sprintf("%d task(s) wait on unfinished dependencies", counts.DependencyBlocked), inventory.nextDependency
	}
	if counts.DomainBlocked > 0 {
		return "conflict_domains", fmt.Sprintf("%d task(s) conflict with active writer domains", counts.DomainBlocked), inventory.nextDomain
	}
	if counts.DecisionBlocked > 0 {
		return "blocked_work", fmt.Sprintf("%d task(s) and %d objective(s) require recovery or human input", inventory.decisionTasks, inventory.decisionGoals), ""
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
