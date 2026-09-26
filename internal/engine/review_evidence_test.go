package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
)

func mustPassedCheckEvidence(t *testing.T, check config.Check, output string) string {
	t.Helper()
	return passedCheckEvidence(check, output)
}

func TestBoundedFailureDiagnosticPreservesTailAfterLongSuccessfulOutput(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz012345"
	output := strings.Repeat("successful check output token="+secret+"\n", 300) + "src/studio-server/http.ts(291,69): TS2740: cannot use Duplex as Socket\n"
	failure := &checkFailure{name: "fixture acceptance", err: errors.New("exit status 1"), output: boundedFailureDiagnostic(safety.Redact(output))}
	reason := failure.Error()
	for _, want := range []string{"fixture acceptance failed", "exit status 1", "TS2740", "bytes omitted"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("bounded native failure reason omitted %q: %q", want, reason)
		}
	}
	if strings.Contains(reason, secret) {
		t.Fatalf("bounded native failure reason leaked a secret: %q", reason)
	}
	if len(reason) > maxFailureDiagnosticBytes {
		t.Fatalf("bounded native failure reason is %d bytes, want at most %d", len(reason), maxFailureDiagnosticBytes)
	}
}

func TestReviewAuthenticationFailureTakesPrecedenceOverConcurrentFinding(t *testing.T) {
	builtins := roles.Builtins()
	required := []roles.Role{builtins["reviewer"], builtins["qa"]}
	outcomes := []reviewOutcome{
		{result: provider.Result{Status: "completed", Findings: []model.Finding{{Severity: "high", Category: "correctness", Reason: "fixture finding", Resolution: "repair fixture"}}}},
		{err: &provider.InvocationError{Cause: errors.New("fixture provider failure"), Failure: provider.FailureAuthentication}},
	}
	assessment := assessReviews(required, outcomes)
	if assessment.blocking != 0 || len(assessment.findings) != 1 {
		t.Fatalf("mixed review assessment lost the concrete finding: %+v", assessment)
	}
	if !reviewAuthenticationFailure(outcomes) {
		t.Fatal("typed review authentication failure was not detected")
	}
}

func TestPassedCheckEvidenceDoesNotPublishOutputOrArguments(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 30)
	privateFixture := "customer-email@example.invalid"
	output := secret + "\n" + privateFixture + "\n"
	evidence := mustPassedCheckEvidence(t, config.Check{Name: "verified vertical slice", Command: []string{"powershell", "-File", "private-fixture.ps1"}}, output)
	for _, want := range []string{"stage=native", `check="verified vertical slice"`, `command="powershell"`, "command_id=", "exit=0", `pass_counts="none"`, "stdout=captured", "stdout_bytes=", "stdout_lines=2"} {
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

func TestPassedCheckEvidencePublishesOnlyBoundedPassCounts(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 30)
	output := "Test Files  9 passed (9)\nTests  173 passed\n6 passed (32.6s)\n" + secret + " customer-email@example.invalid\n"
	evidence := mustPassedCheckEvidence(t, config.Check{Name: "native validation", Command: []string{"powershell", "-File", "scripts/verify.ps1"}}, output)
	for _, want := range []string{"stage=native", `command="powershell"`, "command_id=", `pass_counts="test_files=9_passed,tests=173_passed,passed=6"`, "exit=0"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("check evidence omitted %q: %s", want, evidence)
		}
	}
	for _, forbidden := range []string{secret, "customer-email@example.invalid", "scripts/verify.ps1", "6 passed"} {
		if strings.Contains(evidence, forbidden) {
			t.Fatalf("check evidence published unsafe output %q: %s", forbidden, evidence)
		}
	}
}

func TestPassedCheckEvidencePublishesBareRunnerPassCount(t *testing.T) {
	evidence := mustPassedCheckEvidence(t, config.Check{Name: "browser", Command: []string{"npx", "playwright", "test"}}, "6 passed (32.6s)\n")
	if !strings.Contains(evidence, `pass_counts="passed=6"`) {
		t.Fatalf("check evidence omitted bare runner pass count: %s", evidence)
	}
}

func TestPassedCheckEvidenceBoundsOutputAndPassCountItems(t *testing.T) {
	output := strings.Repeat("x", maxVerificationEvidenceScan+1) + "\nTest Files 9 passed\nTests 173 passed\n6 passed\n"
	evidence := mustPassedCheckEvidence(t, config.Check{Name: "bounded", Command: []string{"go", "test"}}, output)
	if !strings.Contains(evidence, "test_files=9_passed") || !strings.Contains(evidence, "tests=173_passed") || !strings.Contains(evidence, "passed=6") {
		t.Fatalf("pass-count scan was not bounded to the final output window: %s", evidence)
	}
	if got := strings.Count(strings.Split(strings.Split(evidence, `pass_counts="`)[1], `"`)[0], ",") + 1; got > maxVerificationPassCountItems {
		t.Fatalf("pass-count item cap exceeded: %d in %s", got, evidence)
	}
	tooManyDigits := mustPassedCheckEvidence(t, config.Check{Name: "digits", Command: []string{"go", "test"}}, "Tests 1234567890 passed\n")
	if !strings.Contains(tooManyDigits, `pass_counts="none"`) {
		t.Fatalf("pass count accepted more than nine digits: %s", tooManyDigits)
	}
	items := make([]string, 0, maxVerificationPassCountItems+4)
	for count := 1; count <= cap(items); count++ {
		items = append(items, fmt.Sprintf("%d passed", count))
	}
	itemCapped := mustPassedCheckEvidence(t, config.Check{Name: "items", Command: []string{"go", "test"}}, strings.Join(items, "\n"))
	values := strings.Split(strings.Split(itemCapped, `pass_counts="`)[1], `"`)[0]
	if got := strings.Count(values, ",") + 1; got != maxVerificationPassCountItems || strings.Contains(values, "passed=9") {
		t.Fatalf("pass-count item cap failed: %s", itemCapped)
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
		Checks:  []string{mustPassedCheckEvidence(t, config.Check{Name: "native validation", Command: []string{"powershell", "-File", "scripts/verify.ps1"}}, "Test Files 9 passed\nTests 173 passed\n6 passed\n")},
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
	for _, want := range []string{"stage=native", `command="powershell"`, "command_id=", `pass_counts="test_files=9_passed,tests=173_passed,passed=6"`, "exit=0"} {
		if !strings.Contains(payload.Checks[0], want) {
			t.Fatalf("reviewer payload omitted bounded native detail %q: %s", want, payload.Checks[0])
		}
	}
	prompt := roles.Compile(config.Effective{Files: map[string]string{}}, roles.Builtins()["reviewer"], "windows", &model.Task{ID: "fixture", Areas: []string{"src"}}, "review", "diff", reviewEvidencePayload(evidence, 2))
	for _, want := range []string{"VERIFICATION EVIDENCE", "stage=native", `command=\"powershell\"`, "test_files=9_passed", "tests=173_passed", "passed=6"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("reviewer prompt omitted native verification detail %q", want)
		}
	}
}
