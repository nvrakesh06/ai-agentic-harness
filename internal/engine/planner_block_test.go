package engine

import (
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func TestPlannerQuestionStopsBackfillWithoutConsumingRetries(t *testing.T) {
	s, project, now := capacityFixture()
	s.Objectives["capture"] = &model.Objective{ID: "capture", Attempts: 2}
	s.Backlog = []string{"capture"}
	markObjectivePlanningBlocked(s.Objectives["capture"], " Is temporary trace storage allowed? ")

	if got := s.Objectives["capture"]; got.Attempts != 2 || got.Blocker != "Planning needs input: Is temporary trace storage allowed?" {
		t.Fatalf("question must be visible without another retry: %+v", got)
	}
	if id, _ := nextBacklogObjective(s); id != "" {
		t.Fatalf("blocked objective was selected again: %q", id)
	}
	decision := decideCapacity(s, nil, project, false, 0, now)
	if decision.status.ReasonCode != "blocked_work" || !strings.Contains(decision.status.Reason, "1 objective(s)") {
		t.Fatalf("capacity did not explain the empty writer slot: %+v", decision.status)
	}
}

func TestObjectiveAnswerRequiresSubstance(t *testing.T) {
	o := &model.Objective{ID: "capture", Text: "Capture safely", Attempts: 2, Blocker: "Planning needs input: trace policy?"}
	for _, answer := range []string{"", " \t\n"} {
		if err := applyObjectiveAnswer(o, answer); err == nil {
			t.Fatalf("blank objective answer %q was accepted", answer)
		}
		if o.Blocker == "" || o.Attempts != 2 || o.Text != "Capture safely" {
			t.Fatalf("blank answer changed objective: %+v", o)
		}
	}
	if err := applyObjectiveAnswer(o, "Keep traces off for sensitive flows"); err != nil {
		t.Fatalf("concrete objective answer was rejected: %v", err)
	}
	if o.Blocker != "" || o.Attempts != 0 || !strings.Contains(o.Text, "Human answer: Keep traces off for sensitive flows") {
		t.Fatalf("answered objective did not resume planning: %+v", o)
	}
	s, _, _ := capacityFixture()
	s.Objectives[o.ID] = o
	s.Backlog = []string{o.ID}
	if id, _ := nextBacklogObjective(s); id != o.ID {
		t.Fatalf("answered objective was not eligible for planning: %q", id)
	}
}
