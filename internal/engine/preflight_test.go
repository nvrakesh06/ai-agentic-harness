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
