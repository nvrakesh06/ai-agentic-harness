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
	if !route.gated || route.unresolved || origin.State != model.SyncRequired {
		t.Fatalf("blocking owned finding did not gate origin: %#v state=%s", route, origin.State)
	}
	if len(owners["studio"]) != 1 || len(route.local) != 1 || route.local[0].Location != orphan.Location {
		t.Fatalf("routing mixed owned blocker with nonblocking orphan: owners=%#v local=%#v", owners, route.local)
	}
	if origin.Blocker != nil || dependenciesComplete(s, origin) {
		t.Fatalf("routed blocker must wait automatically for its owner: %#v", origin.Blocker)
	}
	owner.State = model.Done
	if !dependenciesComplete(s, origin) {
		t.Fatal("origin did not become schedulable after routed owner completed")
	}
}

func TestStartedLegacyTaskFailsClosed(t *testing.T) {
	task := &model.Task{ID: "legacy", State: model.Running, Areas: []string{"src/engine"}, HeadSHA: "head"}
	if _, ok := immutableScope(task); ok {
		t.Fatal("started legacy task acquired a guessed immutable scope")
	}
}

func TestRebasedTaskScopeUsesAdvancedMainBase(t *testing.T) {
	// A task that owns only src/remotion must not inherit unrelated src/studio
	// changes merely because main advanced before its rebase. checkpointAtBase
	// receives the advanced main SHA, so the Git gate compares only the rebased
	// task delta; immutableScope itself remains unchanged.
	task := ownedTask("renderer", model.Ready, "src/remotion", model.AreaDirectory)
	if _, ok := immutableScope(task); !ok {
		t.Fatal("advanced-base validation lost the original immutable assignment")
	}
}

func TestCrossTaskRoutingRejectsReverseDependencyCycle(t *testing.T) {
	origin := ownedTask("renderer", model.Review, "src/remotion", model.AreaDirectory)
	owner := ownedTask("studio", model.Ready, "src/studio", model.AreaDirectory)
	owner.Dependencies = []string{"renderer"}
	s := model.NewSnapshot("ownership-test")
	s.Tasks = map[string]*model.Task{"renderer": origin, "studio": owner}
	route, owners := applyCrossTaskFindings(s, "renderer", []model.Finding{{Severity: "high", Location: "src/studio/live.ts:1"}}, func(model.Finding) bool { return true })
	if !route.gated || !route.unresolved || len(owners) != 0 || origin.State != model.Blocked || origin.Blocker == nil {
		t.Fatalf("reverse dependency cycle was routed unsafely: route=%#v owners=%#v task=%#v", route, owners, origin)
	}
}

func TestCrossTaskRoutingDoesNotLoseFindingToRunningOwner(t *testing.T) {
	origin := ownedTask("renderer", model.Review, "src/remotion", model.AreaDirectory)
	owner := ownedTask("studio", model.Running, "src/studio", model.AreaDirectory)
	origin.ObjectiveID, owner.ObjectiveID = "different-origin", "different-owner"
	origin.HeadSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s := model.NewSnapshot("ownership-test")
	s.Tasks = map[string]*model.Task{"renderer": origin, "studio": owner}
	route, owners := applyCrossTaskFindings(s, "renderer", []model.Finding{{Severity: "high", Location: "src/studio/live.ts:1"}}, func(model.Finding) bool { return true })
	if !route.gated || route.unresolved || len(owners["studio"]) != 1 || origin.State != model.SyncRequired || origin.Blocker != nil {
		t.Fatalf("running owner did not receive a durable replay handoff: route=%#v owners=%#v origin=%#v", route, owners, origin)
	}
	if len(model.TaskGuidance(owner)) != 1 {
		t.Fatalf("running owner has no durable routed-finding guidance: %#v", owner.Decisions)
	}
	changed := model.Finding{Severity: "high", Location: "src/studio/live.ts:1", Reason: "The revised failure still needs repair.", Resolution: "Apply the updated safe fix."}
	route, owners = applyCrossTaskFindings(s, "renderer", []model.Finding{changed}, func(model.Finding) bool { return true })
	if !route.gated || route.unresolved || len(owners["studio"]) != 1 || len(model.TaskGuidance(owner)) != 2 {
		t.Fatalf("changed same-location finding did not create a fresh replay epoch: route=%#v owners=%#v guidance=%#v", route, owners, model.TaskGuidance(owner))
	}
	replay, err := completeImplementation(owner, 0)
	if err != nil || !replay || owner.State != model.Ready {
		t.Fatalf("running owner did not enter bounded replay after handoff: replay=%t state=%s err=%v", replay, owner.State, err)
	}
}
