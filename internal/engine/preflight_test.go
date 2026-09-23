package engine

import (
	"fmt"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

func TestManyReadyPreflightsAreBoundedWithoutStarvingCoding(t *testing.T) {
	s := model.NewSnapshot("project123")
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("a_ui_%02d", i)
		s.Tasks[id] = &model.Task{ID: id, State: model.Ready, UI: true, Domains: []string{id}}
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("z_code_%02d", i)
		s.Tasks[id] = &model.Task{ID: id, State: model.Ready, Domains: []string{id}}
	}
	active, guided := map[string]bool{}, map[string]bool{}
	selected := selectPreflights(s, active, guided, 2, 2, roles.Builtins())
	if len(selected) != 5 || selected[0].task.ID != "z_code_00" || selected[1].task.ID != "z_code_01" {
		t.Fatalf("bounded preflight did not prioritize independent coding: %#v", selected)
	}
	for _, candidate := range selected {
		active[candidate.task.ID] = false
		guided[candidate.task.ID] = candidate.guided
	}
	if next := selectPreflights(s, active, guided, 2, 2, roles.Builtins()); len(next) != 0 {
		t.Fatalf("reader and fast queue caps were exceeded: %#v", next)
	}
	delete(active, "z_code_00")
	delete(guided, "z_code_00")
	s.Tasks["z_code_00"].Preflight = &model.Preflight{Phase: "ready"}
	selected = selectPreflights(s, active, guided, 2, 2, roles.Builtins())
	if len(selected) != 1 || selected[0].task.ID != "z_code_02" {
		t.Fatalf("released fast slot was not reused deterministically: %#v", selected)
	}
}

func TestPreflightIdentityInvalidatesOnBaseHeadAndPolicyChange(t *testing.T) {
	task := &model.Task{HeadSHA: "head"}
	effective := config.Effective{BaseSHA: "base", Hash: "config"}
	p := &model.Preflight{Phase: "ready", BaseSHA: "base", HeadSHA: "head", Config: "config", Rules: roles.Hash()}
	if !preflightMatches(p, task, effective) {
		t.Fatal("matching preflight was not reusable")
	}
	for _, changed := range []struct {
		task      *model.Task
		effective config.Effective
	}{
		{&model.Task{HeadSHA: "head"}, config.Effective{BaseSHA: "new-base", Hash: "config"}},
		{&model.Task{HeadSHA: "new-head"}, effective},
		{task, config.Effective{BaseSHA: "base", Hash: "new-config"}},
	} {
		if preflightMatches(p, changed.task, changed.effective) {
			t.Fatal("stale preflight was reusable")
		}
	}
}

func TestCompletedPreflightReusesOnlyBoundedUnchangedFixScope(t *testing.T) {
	effective := config.Effective{BaseSHA: "base", Hash: "config", Policy: config.Policy{ImplementationRetries: 2}}
	newTask := func() *model.Task {
		return &model.Task{State: model.Fix, HeadSHA: "new-head", Objective: "repair the UI", Acceptance: []string{"works"}, Areas: []string{"ui"}, Domains: []string{"ui"}, UI: true}
	}
	required := []roles.Role{{Name: "designer", Stage: "pre-implementation"}}
	newPreflight := func(task *model.Task) *model.Preflight {
		return &model.Preflight{Phase: "writing", BaseSHA: "base", HeadSHA: "old-head", Config: "config", Rules: roles.Hash(), Scope: preflightScope(task), Completed: []string{"designer"}}
	}
	task := newTask()
	p := newPreflight(task)
	if preflightMatches(p, task, effective) {
		t.Fatal("changed FIX head matched exact preflight identity")
	}
	if !reusablePreflightForFix(p, task, effective, required) {
		t.Fatal("completed unchanged FIX preflight was not reusable")
	}
	if !reusePreflightForFix(p, task, effective, required) {
		t.Fatal("eligible FIX preflight was not promoted directly to writer admission")
	}
	if !preflightMatches(p, task, effective) || p.Phase != "ready" || p.ReuseCount != 1 || p.ReuseReason == "" {
		t.Fatalf("reused preflight did not become exact-head writer-ready evidence: %+v", p)
	}

	for _, changed := range []struct {
		name   string
		update func(*model.Preflight, *model.Task)
		eff    config.Effective
	}{
		{"ready state", func(_ *model.Preflight, task *model.Task) { task.State = model.Ready }, effective},
		{"scope", func(_ *model.Preflight, task *model.Task) { task.Objective = "redesign the UI" }, effective},
		{"base", func(_ *model.Preflight, _ *model.Task) {}, config.Effective{BaseSHA: "new-base", Hash: "config", Policy: effective.Policy}},
		{"policy", func(_ *model.Preflight, _ *model.Task) {}, config.Effective{BaseSHA: "base", Hash: "new-config", Policy: effective.Policy}},
		{"retry bound", func(p *model.Preflight, _ *model.Task) { p.ReuseCount = 2 }, effective},
		{"incomplete", func(p *model.Preflight, _ *model.Task) { p.Completed = nil }, effective},
	} {
		t.Run(changed.name, func(t *testing.T) {
			task := newTask()
			p := newPreflight(task)
			changed.update(p, task)
			if reusablePreflightForFix(p, task, changed.eff, required) {
				t.Fatal("stale or incomplete preflight was reusable")
			}
		})
	}
}
