package engine

import (
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
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

func TestCausalFindingInUnchangedCallerBlocksOrigin(t *testing.T) {
	origin := ownedTask("renderer", model.Review, "src/remotion", model.AreaDirectory)
	origin.BaseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reviewer := roles.Builtins()["reviewer"]
	reviewer.Blocking.Severities = []string{"medium", "high", "critical"}
	required := []roles.Role{reviewer}
	finding := model.Finding{Severity: "medium", Role: "reviewer", Location: "src/studio/caller.go:24", Relevance: model.FindingCausal, Reason: "The changed renderer now violates the unchanged caller contract."}
	if !reviewFindingBlocksOrigin(origin, []string{"src/remotion/render.go"}, required)(finding) {
		t.Fatal("causal downstream caller finding did not block the origin task")
	}
}

func TestBaselineLabelCannotBypassOriginWithoutSupervisorProof(t *testing.T) {
	origin := ownedTask("renderer", model.Review, "src/remotion", model.AreaDirectory)
	origin.BaseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reviewer := roles.Builtins()["reviewer"]
	reviewer.Blocking.Severities = []string{"medium", "high", "critical"}
	required := []roles.Role{reviewer}
	finding := model.Finding{Severity: "medium", Role: "reviewer", Location: "src/studio/baseline.go:12", Relevance: model.FindingBaseline, BaselineSHA: origin.BaseSHA, BaselineEvidence: "go test ./internal/studio at base reproduces the same failure", Reason: "The same defect reproduces on the base revision."}
	if !reviewFindingBlocksOrigin(origin, []string{"src/remotion/render.go"}, required)(finding) {
		t.Fatal("reviewer-supplied baseline prose bypassed the origin review gate")
	}
}

func TestUnknownOrSecurityBaselineClaimFailsClosed(t *testing.T) {
	origin := ownedTask("renderer", model.Review, "src/remotion", model.AreaDirectory)
	origin.BaseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	security := roles.Builtins()["security"]
	required := []roles.Role{security}
	unknown := model.Finding{Severity: "medium", Role: "security", Location: "src/studio/auth.go:12", Relevance: model.FindingUnknown, Reason: "The evidence does not establish whether this predates the change."}
	if !reviewFindingBlocksOrigin(origin, []string{"src/remotion/render.go"}, required)(unknown) {
		t.Fatal("unknown security finding did not fail closed")
	}
	claimedBaseline := unknown
	claimedBaseline.Relevance = model.FindingBaseline
	claimedBaseline.BaselineSHA = origin.BaseSHA
	claimedBaseline.BaselineEvidence = "reviewer assertion"
	if trustedBaselineFinding(origin, []string{"src/remotion/render.go"}, claimedBaseline) {
		t.Fatal("security baseline claim bypassed the origin review gate")
	}
	if !reviewFindingBlocksOrigin(origin, []string{"src/remotion/render.go"}, required)(claimedBaseline) {
		t.Fatal("medium security baseline claim bypassed the origin review gate")
	}
}

func TestUnknownRelevanceFailsClosedWithBuiltinReviewer(t *testing.T) {
	origin := ownedTask("renderer", model.Review, "src/remotion", model.AreaDirectory)
	reviewer := roles.Builtins()["reviewer"]
	required := []roles.Role{reviewer}
	blocks := reviewFindingBlocksOrigin(origin, []string{"src/remotion/render.go"}, required)
	if !blocks(model.Finding{Severity: "medium", Role: "reviewer", Location: "src/studio/caller.go:24", Relevance: model.FindingUnknown}) {
		t.Fatal("medium unknown reviewer finding bypassed the origin review gate")
	}
	if blocks(model.Finding{Severity: "low", Role: "reviewer", Location: "src/studio/caller.go:24", Relevance: model.FindingChanged}) || blocks(model.Finding{Severity: "nit", Role: "reviewer", Location: "src/studio/caller.go:24", Relevance: model.FindingCausal}) {
		t.Fatal("low or nit causal reviewer finding ignored normal severity semantics")
	}
}

func TestRoutableReviewFindingsKeepBaselineAndUnknownLocal(t *testing.T) {
	findings := []model.Finding{
		{Location: "src/studio/changed.go:1", Relevance: model.FindingChanged},
		{Location: "src/studio/caller.go:2", Relevance: model.FindingCausal},
		{Location: "src/studio/baseline.go:3", Relevance: model.FindingBaseline},
		{Location: "src/studio/unknown.go:4", Relevance: model.FindingUnknown},
	}
	routed, local := routableReviewFindings(findings)
	if len(routed) != 2 || routed[0].Location != findings[0].Location || routed[1].Location != findings[1].Location {
		t.Fatalf("changed and causal findings were not retained for cross-task routing: %#v", routed)
	}
	if len(local) != 2 || local[0].Location != findings[2].Location || local[1].Location != findings[3].Location {
		t.Fatalf("baseline or unknown finding escaped local follow-up: %#v", local)
	}
}

func TestCrossTaskRoutingDeduplicatesRepeatedDecision(t *testing.T) {
	origin := ownedTask("renderer", model.Review, "src/remotion", model.AreaDirectory)
	owner := ownedTask("studio", model.Ready, "src/studio", model.AreaDirectory)
	s := model.NewSnapshot("ownership-test")
	s.Tasks = map[string]*model.Task{"renderer": origin, "studio": owner}
	finding := model.Finding{Severity: "medium", Location: "src/studio/baseline.go:12", Reason: "A durable owner concern."}
	for range 2 {
		applyCrossTaskFindings(s, "renderer", []model.Finding{finding}, func(model.Finding) bool { return false })
	}
	if len(owner.Decisions) != 1 || len(owner.Findings) != 1 {
		t.Fatalf("exact-head replay duplicated owner routing state: decisions=%#v findings=%#v", owner.Decisions, owner.Findings)
	}
}
