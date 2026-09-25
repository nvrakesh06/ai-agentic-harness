package engine

import (
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func ownedTask(id string, state model.State, area string, kind string) *model.Task {
	return &model.Task{ID: id, State: state, AssignedAreas: []string{area}, AssignedAreaKinds: map[string]string{area: kind}, FixCycles: map[string]int{}}
}

func TestImmutableScopeHonorsDirectoryIntent(t *testing.T) {
	task := ownedTask("studio", model.Ready, "src/studio", model.AreaDirectory)
	if !findingInTaskScope(task, model.Finding{Location: "src/studio/live.ts:12"}) {
		t.Fatal("directory intent did not own nested file")
	}
	if findingInTaskScope(task, model.Finding{Location: "src/remotion/root.ts:1"}) {
		t.Fatal("directory intent widened outside its tree")
	}
}

func TestCrossTaskRoutingDoesNotHumanBlockForNonblockingOrphan(t *testing.T) {
	origin := ownedTask("renderer", model.Review, "src/remotion", model.AreaDirectory)
	owner := ownedTask("studio", model.Ready, "src/studio", model.AreaDirectory)
	s := model.NewSnapshot("ownership-test")
	s.Tasks = map[string]*model.Task{"renderer": origin, "studio": owner}
	blocking := model.Finding{Severity: "high", Role: "qa", Location: "src/studio/live.ts:9", Reason: "Must repair."}
	orphan := model.Finding{Severity: "medium", Role: "qa", Location: "elsewhere/missing.ts:2", Reason: "Optional note."}
	route, owners := applyCrossTaskFindings(s, "renderer", []model.Finding{blocking, orphan}, func(f model.Finding) bool { return f.Severity == "high" })
	if !route.gated || origin.State != model.Blocked {
		t.Fatalf("blocking owned finding did not gate origin: %#v state=%s", route, origin.State)
	}
	if len(owners["studio"]) != 1 || len(route.local) != 1 || route.local[0].Location != orphan.Location {
		t.Fatalf("routing mixed owned blocker with nonblocking orphan: owners=%#v local=%#v", owners, route.local)
	}
	if origin.Blocker == nil || origin.Blocker.Resume != model.SyncRequired {
		t.Fatalf("nonblocking orphan escalated to human block: %#v", origin.Blocker)
	}
}

func TestStartedLegacyTaskFailsClosed(t *testing.T) {
	task := &model.Task{ID: "legacy", State: model.Running, Areas: []string{"src/engine"}, HeadSHA: "head"}
	if _, ok := immutableScope(task); ok {
		t.Fatal("started legacy task acquired a guessed immutable scope")
	}
}
