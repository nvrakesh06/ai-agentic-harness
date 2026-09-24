package engine

import (
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func TestReadOnlyCheckoutRefUsesPlanningBaseForHeadlessPreflight(t *testing.T) {
	task := &model.Task{BaseSHA: "task-base"}
	if got, err := readOnlyCheckoutRef(task, config.Effective{BaseSHA: "canonical-base"}, ""); err != nil || got != "task-base" {
		t.Fatalf("headless preflight ref = %q, %v; want immutable task base", got, err)
	}
	if got, err := readOnlyCheckoutRef(&model.Task{}, config.Effective{BaseSHA: "canonical-base"}, ""); err != nil || got != "canonical-base" {
		t.Fatalf("headless preflight fallback = %q, %v; want canonical base", got, err)
	}
	if got, err := readOnlyCheckoutRef(&model.Task{HeadSHA: "task-head", BaseSHA: "task-base"}, config.Effective{BaseSHA: "canonical-base"}, ""); err != nil || got != "task-head" {
		t.Fatalf("review ref = %q, %v; want task head", got, err)
	}
	if _, err := readOnlyCheckoutRef(&model.Task{}, config.Effective{}, ""); err == nil {
		t.Fatal("missing immutable revision was allowed to fall back to writer worktree")
	}
}

func TestReadOnlyCheckoutRefUsesExplicitRecoveryTargetOverTaskHead(t *testing.T) {
	task := &model.Task{HeadSHA: "stale-task-head", BaseSHA: "task-base"}
	if got, err := readOnlyCheckoutRef(task, config.Effective{BaseSHA: "repaired-main"}, "repaired-main"); err != nil || got != "repaired-main" {
		t.Fatalf("recovery QA ref = %q, %v; want repaired main instead of stale task head", got, err)
	}
}
