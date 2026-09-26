package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func validReplanRequest() ReplanRequest {
	sha := strings.Repeat("a", 40)
	hash := strings.Repeat("b", 64)
	return ReplanRequest{Schema: replanSchema, CommandID: "repair-1", Expected: ReplanExpected{BaseSHA: sha, Config: hash, Rules: hash, StateRef: sha},
		Originals:   []ReplanOriginal{{TaskID: "old", State: model.Blocked, HeadSHA: sha}},
		Sources:     []ReplanSource{{TaskID: "old", BaseSHA: strings.Repeat("c", 40), HeadSHA: sha, Order: 1}},
		Replacement: ReplanReplacement{ID: "replacement", Title: "Repair", Objective: "repair", Acceptance: []string{"works"}, Areas: []string{"internal/engine"}, Domains: []string{"engine"}, Risk: "medium"}, Reason: "explicit bounded repair"}
}

func TestDecodeReplanRequestFailsClosed(t *testing.T) {
	request := validReplanRequest()
	b, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeReplanRequest(b); err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeReplanRequest(append(b[:len(b)-1], []byte(`,"unexpected":true}`)...)); err == nil {
		t.Fatal("unknown replan input field accepted")
	}
	request.Sources[0].Order = 0
	if err = validateReplanRequest(request); err == nil {
		t.Fatal("unordered source checkpoint accepted")
	}
	request = validReplanRequest()
	request.Expected.StateRef = ""
	if err = validateReplanRequest(request); err == nil {
		t.Fatal("state-unbound replan request accepted")
	}
	request = validReplanRequest()
	request.Originals = append(request.Originals, ReplanOriginal{TaskID: "queued", State: model.Ready})
	if err = validateReplanRequest(request); err != nil {
		t.Fatalf("provably unstarted original was rejected at manifest shape: %v", err)
	}
	request.Sources[0].HeadSHA = strings.Repeat("d", 40)
	if err = validateReplanRequest(request); err == nil {
		t.Fatal("source checkpoint detached from original head accepted")
	}
}

func TestReplanUnstartedRequiresEmptyLifecycle(t *testing.T) {
	s := model.NewSnapshot("project123")
	task := &model.Task{ID: "queued", State: model.Ready, Branch: "aih/queued", FixCycles: map[string]int{}}
	s.Tasks[task.ID] = task
	if !replanUnstarted(s, task) {
		t.Fatal("empty queued task was not accepted")
	}
	task.Attempts = 1
	if replanUnstarted(s, task) {
		t.Fatal("attempted task was accepted as unstarted")
	}
	task.Attempts = 0
	s.Runs = []model.Run{{Task: task.ID, Outcome: "interrupted"}}
	if replanUnstarted(s, task) {
		t.Fatal("task with a durable run was accepted as unstarted")
	}
}

func TestReplanContractPreservesRiskRolesAndDependencies(t *testing.T) {
	request := validReplanRequest()
	request.Replacement.Risk = "low"
	request.Replacement.Roles = []string{"reviewer"}
	request.Replacement.Dependencies = []string{"external"}
	old := &model.Task{ID: "old", Objective: "repair", Acceptance: []string{"legacy acceptance"}, Roles: []string{"security"}, Dependencies: []string{"external", "gate"}, Risk: "high", Security: true}
	next, err := mergeReplanContract(request, []*model.Task{old})
	if err != nil || next.Risk != "high" || !next.Security || strings.Join(next.Roles, ",") != "reviewer,security" || strings.Join(next.Dependencies, ",") != "external,gate" || strings.Join(next.Acceptance, ",") != "legacy acceptance,works" {
		t.Fatalf("contract preservation failed: %#v err=%v", next, err)
	}
	old.Objective = "different"
	if _, err = mergeReplanContract(request, []*model.Task{old}); err == nil {
		t.Fatal("contract replacement was inferred")
	}
}

func TestReplanDependencyCycleFailsClosed(t *testing.T) {
	s := model.NewSnapshot("project123")
	s.Tasks["a"] = &model.Task{ID: "a", Dependencies: []string{"replacement"}}
	if !createsDependencyCycle(s, "replacement", []string{"a"}) {
		t.Fatal("replacement dependency cycle accepted")
	}
}

func TestReplanRejectsCycleIntroducedBySuccessorLink(t *testing.T) {
	s := model.NewSnapshot("project123")
	s.Tasks["original"] = &model.Task{ID: "original", State: model.Blocked}
	s.Tasks["downstream"] = &model.Task{ID: "downstream", State: model.Ready, Dependencies: []string{"original"}}
	next := &model.Task{ID: "replacement", State: model.Ready, Dependencies: []string{"downstream"}}
	if !prospectiveReplanCycle(s, next, []ReplanOriginal{{TaskID: "original"}}) {
		t.Fatal("replacement edge cycle was accepted")
	}
}

func TestReplanAllowsInterruptedReviewButRejectsLiveRun(t *testing.T) {
	s := model.NewSnapshot("project123")
	task := &model.Task{ID: "review", State: model.Review, Preflight: &model.Preflight{Phase: "writing"}}
	s.Tasks[task.ID] = task
	s.Runs = []model.Run{{Task: task.ID, Outcome: "interrupted"}}
	if replanActive(s, task) {
		t.Fatal("interrupted review checkpoint was treated as live")
	}
	s.Runs[0].Outcome = "running"
	if !replanActive(s, task) {
		t.Fatal("live run was accepted")
	}
}

func TestReplanReceiptBindsExactManifest(t *testing.T) {
	s := model.NewSnapshot("project123")
	s.Tasks["replacement"] = &model.Task{ID: "replacement"}
	request := validReplanRequest()
	s.Applied[request.CommandID] = true
	s.Replans[request.CommandID] = model.ReplanReceipt{Digest: replanDigest(request), ReplacementID: request.Replacement.ID}
	if applied, err := replanReceipt(s, request); err != nil || !applied {
		t.Fatalf("exact retry rejected: %v", err)
	}
	request.Reason = "different"
	if _, err := replanReceipt(s, request); err == nil {
		t.Fatal("altered request reused receipt")
	}
}

func TestAcceptedReplanRetryDoesNotRequireCurrentPolicy(t *testing.T) {
	s := model.NewSnapshot("project123")
	s.Tasks["replacement"] = &model.Task{ID: "replacement"}
	request := validReplanRequest()
	s.Applied[request.CommandID] = true
	s.Replans[request.CommandID] = model.ReplanReceipt{Digest: replanDigest(request), ReplacementID: "replacement"}
	if required, err := replanPolicyRequired(s, request); err != nil || required {
		t.Fatalf("accepted retry requested stale policy validation: required=%t err=%v", required, err)
	}
}

func TestReplanOverlapUsesUnstartedImmutableAreaFallback(t *testing.T) {
	queued := &model.Task{ID: "queued", State: model.Ready, Areas: []string{"feature-queued.txt"}}
	areas, known := model.ImmutableAreas(queued)
	if !known || !areasOverlap([]string{"feature-queued.txt"}, areas) {
		t.Fatal("queued immutable Areas fallback was treated as disjoint")
	}
}
