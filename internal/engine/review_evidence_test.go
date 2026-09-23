package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

func TestPassedCheckEvidenceDoesNotPublishOutputOrArguments(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 30)
	privateFixture := "customer-email@example.invalid"
	output := secret + "\n" + privateFixture + "\n"
	evidence := passedCheckEvidence(config.Check{Name: "verified vertical slice", Command: []string{"powershell", "-File", "private-fixture.ps1"}}, output)
	for _, want := range []string{`check="verified vertical slice"`, `command="powershell"`, "exit=0", "stdout=captured", "stdout_bytes=", "stdout_lines=2"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("check evidence omitted %q: %s", want, evidence)
		}
	}
	for _, forbidden := range []string{secret, privateFixture, "private-fixture.ps1", "-File"} {
		if strings.Contains(evidence, forbidden) {
			t.Fatalf("check evidence published private output or arguments %q: %s", forbidden, evidence)
		}
	}
}

func TestReviewEvidenceRequestsDoNotBecomeHumanDecisions(t *testing.T) {
	for _, result := range []provider.Result{
		{Status: "blocked", Question: "Can the supervisor provide exact npm validation outputs and independent peer approvals for this head?", Summary: "No confirmed defect; native and peer evidence is missing."},
		{Status: "blocked", Question: "Can a native verification worker provide current-head outputs?", Summary: "This restricted worker lacks Node/npm and cannot read dependencies through Bun."},
		{Status: "in_progress", Summary: "Need the missing exact-head native check output before I can complete QA."},
	} {
		if !supervisorEvidenceRequest(result) {
			t.Fatalf("supervisor-owned evidence was treated as human input: %#v", result)
		}
	}
	for _, result := range []provider.Result{
		{Status: "blocked", Question: "Choose whether the API may break backward compatibility.", Summary: "A product decision is required."},
		{Status: "blocked", Question: "Authorize an exception to accept risk in production.", Summary: "The supervisor cannot make this decision."},
		{Status: "blocked", Question: "Should the V1 threat model defend against a same-user junction-swap race condition?", Summary: "Native verification passed, but the trusted-workspace security boundary requires a scope decision."},
	} {
		if supervisorEvidenceRequest(result) {
			t.Fatalf("consequential decision was misclassified as evidence: %#v", result)
		}
	}
}

func TestConcurrentReviewAssessmentPreservesFindingsAndPeerIndependence(t *testing.T) {
	builtins := roles.Builtins()
	required := []roles.Role{builtins["qa"], builtins["reviewer"], builtins["security"]}
	outcomes := []reviewOutcome{
		{result: provider.Result{Status: "blocked", Question: "Can the supervisor provide native check output and peer approvals?", Summary: "No confirmed defect; evidence is missing."}},
		{result: provider.Result{Status: "completed", Summary: "correctness review passed"}},
		{result: provider.Result{Status: "completed", Summary: "security review passed"}},
	}
	assessment := assessReviews(required, outcomes)
	if assessment.human != -1 || assessment.failure != nil || assessment.blocking != -1 || len(assessment.evidence) != 1 || assessment.evidence[0] != 0 {
		t.Fatalf("concurrent evidence request was not isolated from peer results: %#v", assessment)
	}

	outcomes[0].result.Findings = []model.Finding{{Severity: "high", Category: "visual", Location: "scene.tsx:42", Reason: "failure text has insufficient contrast", Resolution: "use the semantic failure token"}}
	assessment = assessReviews(required, outcomes)
	if assessment.blocking != 0 || len(assessment.findings) != 1 || assessment.findings[0].Role != "qa" {
		t.Fatalf("concrete finding was buried by evidence blocker: %#v", assessment)
	}
	if got := appendUniqueFindings(assessment.findings, assessment.findings); len(got) != 1 {
		t.Fatalf("review retry duplicated concrete findings: %#v", got)
	}
}

func TestCompletedReviewDoesNotPublishSupervisorEvidenceOnlyFinding(t *testing.T) {
	builtins := roles.Builtins()
	required := []roles.Role{builtins["qa"]}
	outcomes := []reviewOutcome{{result: provider.Result{
		Status:  "completed",
		Summary: "No code defect found; this worker could not run the configured native check.",
		Findings: []model.Finding{{
			Severity:   "medium",
			Category:   "verification",
			Reason:     "Node and npm are unavailable and Bun returned EPERM, so current-head native logs were not independently reproduced.",
			Resolution: "Provide the supervisor-owned native check output.",
		}},
	}}}
	assessment := assessReviews(required, outcomes)
	if assessment.blocking != -1 || len(assessment.findings) != 0 || assessment.failure != nil {
		t.Fatalf("supervisor evidence-only finding became durable review work: %#v", assessment)
	}

	outcomes[0].result.Findings[0] = model.Finding{Severity: "medium", Category: "verification", Reason: "The configured check passes even when the generated manifest is missing.", Resolution: "Add a manifest assertion."}
	assessment = assessReviews(required, outcomes)
	if len(assessment.findings) != 1 {
		t.Fatalf("concrete verification defect was suppressed: %#v", assessment)
	}
}

func TestReviewEvidencePayloadIsExactHeadAndPeerFree(t *testing.T) {
	evidence := &model.Evidence{
		Base: "base", Head: strings.Repeat("a", 40), Config: "config", Rules: "rules",
		Checks:  []string{`check="native" command="go" exit=0 stdout="ok"`},
		Reviews: map[string]string{"security": "must not leak to a concurrent peer"}, At: time.Now().UTC(),
	}
	var payload struct {
		Head    string            `json:"head"`
		Checks  []string          `json:"checks"`
		Reviews map[string]string `json:"reviews"`
		Attempt int               `json:"review_attempt"`
		Policy  string            `json:"review_policy"`
	}
	if err := json.Unmarshal([]byte(reviewEvidencePayload(evidence, 2)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Head != evidence.Head || payload.Attempt != 2 || len(payload.Checks) != 1 || len(payload.Reviews) != 0 || !strings.Contains(payload.Policy, "concurrent and independent") {
		t.Fatalf("review evidence payload is not exact-head and peer-independent: %#v", payload)
	}
}
