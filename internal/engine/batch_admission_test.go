package engine

import (
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

func TestBatchAdmissionMatchesOnlyCurrentCanonicalIdentity(t *testing.T) {
	base := strings.Repeat("a", 40)
	effective := config.Effective{BaseSHA: base, Hash: strings.Repeat("b", 64)}
	task := &model.Task{State: model.MergeReady, BaseSHA: base, HeadSHA: strings.Repeat("c", 40), Evidence: &model.Evidence{Base: base, Head: strings.Repeat("c", 40), Config: effective.Hash, Rules: roles.Hash()}}
	if !batchAdmissionMatchesRuntime(task, effective) {
		t.Fatal("current merge-ready task was rejected")
	}
	for _, change := range []func(){
		func() { task.BaseSHA = strings.Repeat("d", 40) },
		func() { task.Evidence.Config = strings.Repeat("e", 64) },
		func() { task.Evidence.Rules = strings.Repeat("f", 64) },
		func() { task.Evidence.Head = strings.Repeat("d", 40) },
	} {
		copy := *task
		copy.Evidence = &model.Evidence{Base: task.Evidence.Base, Head: task.Evidence.Head, Config: task.Evidence.Config, Rules: task.Evidence.Rules}
		changeTask := &copy
		saved := task
		task = changeTask
		change()
		if batchAdmissionMatchesRuntime(task, effective) {
			t.Fatal("stale batch identity was accepted")
		}
		task = saved
	}
}
