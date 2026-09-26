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
	return ReplanRequest{Schema: replanSchema, CommandID: "repair-1", Expected: ReplanExpected{BaseSHA: sha, Config: hash, Rules: hash},
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
