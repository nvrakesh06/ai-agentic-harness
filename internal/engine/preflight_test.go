package engine

import (
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

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
