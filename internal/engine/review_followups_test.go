package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/github"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func TestReviewFollowupsGroupsDuplicateRolesAndRefreshesSameHead(t *testing.T) {
	hub := &followupHub{issues: map[int]github.Issue{}}
	c := &Controller{ctx: context.Background(), P: &Project{Hub: hub}}
	task := &model.Task{ID: "review-followups-48", Issue: 48, PR: 112, HeadSHA: "abc123"}
	findings := []model.Finding{
		// These match the actual API-review pattern: one CONNECT defect received
		// different category labels and nearby locations from independent reviewers.
		{Severity: "medium", Category: "protocol-boundary", Location: "http.ts:288", Role: "qa", Reason: "CONNECT requests return a plain-text response instead of the API error envelope.", Resolution: "Route CONNECT through the JSON error writer."},
		{Severity: "medium", Category: "HTTP error contract", Location: "http.ts:290", Role: "reviewer", Reason: "The Node CONNECT path bypasses the documented error response.", Resolution: "Use the shared protocol response helper."},
		{Severity: "medium", Category: "Unsupported method mapping", Location: "http.ts:290", Role: "security", Reason: "CONNECT is mapped outside the HTTP error contract.", Resolution: "Return the contract error for CONNECT."},
		{Severity: "medium", Category: "animation semantics", Location: "KafkaConsumerFailure.tsx:220", Role: "animation", Reason: "The heartbeat label fades with the stopped arrow and becomes unreadable.", Resolution: "Keep the heartbeat label at readable opacity."},
		{Severity: "medium", Category: "teaching cue", Location: "KafkaConsumerFailure.tsx:223", Role: "reviewer", Reason: "Heartbeat-state text disappears with the arrow, hiding the teaching cue.", Resolution: "Render the label independently from the arrow."},
		{Severity: "medium", Category: "layout", Location: "ServerNode.tsx:61", Role: "qa", Reason: "ServerNode bounds omit the label collision region.", Resolution: "Include the label bounds in layout validation."},
		{Severity: "medium", Category: "collision analysis", Location: "ServerNode.tsx:64", Role: "reviewer", Reason: "The ServerNode collision region falls outside its declared bounds.", Resolution: "Validate the bounds used by collision analysis."},
	}
	if err := c.reviewFollowups(task, findings); err != nil {
		t.Fatal(err)
	}
	issues, err := hub.Issues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected one implementation follow-up work item; got %d: %#v", len(issues), issues)
	}
	followup := issues[0]
	for _, want := range []string{
		"**qa**", "**reviewer**", "**security**", "**animation**",
		"http.ts:288", "KafkaConsumerFailure.tsx:220", "ServerNode.tsx:61",
		"Route CONNECT through the JSON error writer.", "Keep the heartbeat label at readable opacity.",
		"Include the label bounds in layout validation.",
		"## Owner scope: `file:http.ts`", "## Owner scope: `file:kafkaconsumerfailure.tsx`", "## Owner scope: `file:servernode.tsx`",
	} {
		if !strings.Contains(followup.Body, want) {
			t.Fatalf("task follow-up omitted %q:\n%s", want, followup.Body)
		}
	}
	hub.issuesCalls = 0

	// A recovery/re-review may change wording and ordering, but it must update
	// the same stable follow-up issues rather than create a fresh notification.
	findings[0].Category = "socket handling"
	findings[0].Reason = "CONNECT still bypasses the JSON error envelope."
	findings[1].Category = "method behavior"
	findings[1].Reason = "The CONNECT branch has no protocol-safe error body."
	task.HeadSHA = "def456"
	added := model.Finding{Severity: "medium", Category: "request dispatch", Location: "http.ts:291", Role: "design", Reason: "CONNECT must share the documented API error contract.", Resolution: "Use the same JSON envelope for CONNECT."}
	if err := c.reviewFollowups(task, []model.Finding{findings[4], findings[3], findings[1], added, findings[2], findings[0]}); err != nil {
		t.Fatal(err)
	}
	if hub.issuesCalls != 1 {
		t.Fatalf("existing task follow-up should use its single history lookup, got %d issue listings", hub.issuesCalls)
	}
	issues, err = hub.Issues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("same-head re-review created duplicate follow-up issues: %#v", issues)
	}
	followup = issueContaining(t, issues, "CONNECT still bypasses")
	if !strings.Contains(followup.Body, "CONNECT still bypasses") || !strings.Contains(followup.Body, "Review head: `def456`") || !strings.Contains(followup.Body, "ServerNode bounds omit the label collision region.") {
		t.Fatalf("re-review did not refresh the task follow-up or retain earlier findings:\n%s", followup.Body)
	}

	// Retrying the same head replaces that head's snapshot instead of adding
	// another rendered copy for changed reviewer prose. Findings from earlier
	// heads remain until an explicit reconciliation removes them.
	retry := model.Finding{Severity: "medium", Category: "request dispatch", Location: "http.ts:291", Role: "design", Reason: "CONNECT needs a bounded protocol error path.", Resolution: "Use the same JSON envelope for CONNECT."}
	if err := c.reviewFollowups(task, []model.Finding{retry}); err != nil {
		t.Fatal(err)
	}
	issues, err = hub.Issues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("same-head retry created duplicate follow-up issues: %#v", issues)
	}
	followup = issues[0]
	if !strings.Contains(followup.Body, retry.Reason) || strings.Contains(followup.Body, "CONNECT must share the documented API error contract.") || !strings.Contains(followup.Body, "ServerNode bounds omit the label collision region.") {
		t.Fatalf("same-head retry did not replace its snapshot while preserving prior-head findings:\n%s", followup.Body)
	}
}

func TestReviewFollowupsRejectMalformedPersistedHistory(t *testing.T) {
	hub := &followupHub{issues: map[int]github.Issue{1: {Number: 1, Body: github.Marker("review-history-48-review-followup") + "\n\n" + reviewFollowupHistoryPrefix + "not-base64 -->"}}}
	c := &Controller{ctx: context.Background(), P: &Project{Hub: hub}}
	err := c.reviewFollowups(&model.Task{ID: "review-history-48", Issue: 48, HeadSHA: "abc123"}, []model.Finding{{Severity: "medium", Reason: "A finding."}})
	if err == nil || !strings.Contains(err.Error(), "parse review follow-up history") {
		t.Fatalf("malformed persisted history must fail without replacing it, got %v", err)
	}
}

func TestReviewFollowupsRedactBaselineFieldsFromPersistedHistory(t *testing.T) {
	const secret = "sk-abcdefghijklmnopqrstuvwxyz123456"
	task := &model.Task{ID: "review-baseline-redaction", Issue: 48, HeadSHA: "abc123"}
	finding := model.Finding{
		Severity: "medium", Role: "reviewer", Location: "src/http.ts:10",
		Reason: "This occurs on the baseline.", Relevance: model.FindingBaseline,
		BaselineSHA: secret, BaselineEvidence: "baseline output contained " + secret,
	}
	body := reviewFollowupBody(t, groupReviewFollowups(task, []model.Finding{finding})[0], task)
	history, err := parseReviewFollowupHistory(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Reviews) != 1 || len(history.Reviews[0].Findings) != 1 {
		t.Fatalf("missing persisted finding: %#v", history)
	}
	got := history.Reviews[0].Findings[0]
	if strings.Contains(got.BaselineSHA, secret) || strings.Contains(got.BaselineEvidence, secret) {
		t.Fatalf("persisted baseline finding retained secret: %#v", got)
	}
	if got.BaselineSHA != "[REDACTED]" || got.BaselineEvidence != "baseline output contained [REDACTED]" {
		t.Fatalf("baseline fields were not redacted: %#v", got)
	}
}

func TestReviewFollowupsSameHeadRefreshPreservesUnrefreshedRoles(t *testing.T) {
	hub := &followupHub{issues: map[int]github.Issue{}}
	c := &Controller{ctx: context.Background(), P: &Project{Hub: hub}}
	task := &model.Task{ID: "review-roles-48", Issue: 48, HeadSHA: "abc123"}
	qa := model.Finding{Severity: "medium", Role: "qa", Location: "src/http.ts:10", Reason: "QA finding remains until QA is refreshed.", Resolution: "Keep QA coverage."}
	security := model.Finding{Severity: "medium", Role: "security", Location: "src/http.ts:11", Reason: "Original security finding.", Resolution: "Use the first security remedy."}
	if err := c.reviewFollowups(task, []model.Finding{qa, security}); err != nil {
		t.Fatal(err)
	}
	refreshedSecurity := security
	refreshedSecurity.Reason = "Refreshed security finding."
	refreshedSecurity.Resolution = "Use the updated security remedy."
	if err := c.reviewFollowups(task, []model.Finding{refreshedSecurity}); err != nil {
		t.Fatal(err)
	}
	issues, err := hub.Issues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("same-head role refresh created duplicate issues: %#v", issues)
	}
	body := issues[0].Body
	if !strings.Contains(body, qa.Reason) || !strings.Contains(body, refreshedSecurity.Reason) || strings.Contains(body, security.Reason) {
		t.Fatalf("same-head role refresh did not preserve QA and replace only security:\n%s", body)
	}
}

func TestReviewFollowupsPreservesDistinctConcernsInOneOwnerIssue(t *testing.T) {
	task := &model.Task{ID: "review-followups-distinct", Issue: 48, HeadSHA: "abc123"}
	findings := []model.Finding{
		{Severity: "medium", Category: "protocol", Location: "http.ts:288", Role: "qa", Reason: "CONNECT bypasses the JSON error envelope.", Resolution: "Use the protocol error writer."},
		{Severity: "medium", Category: "privacy", Location: "http.ts:288", Role: "security", Reason: "Authentication tokens appear in response logs.", Resolution: "Redact tokens before logging."},
	}
	groups := groupReviewFollowups(task, findings)
	if len(groups) != 1 || len(groups[0].findings) != 2 {
		t.Fatalf("same-file concerns should share one owner issue with both observations: %#v", groups)
	}
	for _, want := range []string{"CONNECT bypasses", "Authentication tokens", "Redact tokens"} {
		if !strings.Contains(reviewFollowupBody(t, groups[0], task), want) {
			t.Fatalf("owner issue dropped distinct concern %q: %s", want, reviewFollowupBody(t, groups[0], task))
		}
	}
}

func TestReviewFollowupsKeepsGenericSameFileRemediesVisible(t *testing.T) {
	task := &model.Task{ID: "review-followups-generic", Issue: 48, HeadSHA: "abc123"}
	findings := []model.Finding{
		{Severity: "medium", Category: "reliability", Location: "http.ts:288", Reason: "Connection timeout needs a bounded retry.", Resolution: "Retry the connection after timeout."},
		{Severity: "medium", Category: "privacy", Location: "http.ts:290", Reason: "Connection credentials leak into logs.", Resolution: "Redact connection credentials."},
	}
	groups := groupReviewFollowups(task, findings)
	if len(groups) != 1 || len(groups[0].findings) != 2 {
		t.Fatalf("one source owner must retain both independent remedies: %#v", groups)
	}
	for _, want := range []string{"bounded retry", "credentials leak", "Redact connection credentials"} {
		if !strings.Contains(reviewFollowupBody(t, groups[0], task), want) {
			t.Fatalf("owner issue dropped independent remedy %q", want)
		}
	}
}

func TestReviewFollowupIdentitySurvivesAdditionalAcronym(t *testing.T) {
	task := &model.Task{ID: "review-followups-stable", Issue: 48, HeadSHA: "abc123"}
	first := model.Finding{Severity: "medium", Category: "protocol", Location: "http.ts:288", Reason: "CONNECT bypasses error envelope."}
	second := first
	second.Reason = "CONNECT bypasses WEBSOCKET and API error envelopes."
	second.Category = "HTTP contract"
	second.Location = "http.ts:291"
	before := groupReviewFollowups(task, []model.Finding{first})
	after := groupReviewFollowups(task, []model.Finding{second})
	if len(before) != 1 || len(after) != 1 || before[0].key != after[0].key {
		t.Fatalf("same-head wording change created a new work item: before=%#v after=%#v", before, after)
	}
}

func TestReviewFollowupsGroupAdjacentFilesByOwnerArea(t *testing.T) {
	task := &model.Task{ID: "review-followups-area", Issue: 48, HeadSHA: "abc123"}
	findings := []model.Finding{
		{Severity: "medium", Category: "accessibility", Location: "src/studio/App.tsx:40", Role: "designer", Reason: "Focus is missing."},
		{Severity: "medium", Category: "reliability", Location: "src/studio/api-client.ts:80", Role: "qa", Reason: "Refresh can overlap."},
		{Severity: "medium", Category: "layout", Location: "src/engine/layout.ts:12", Role: "reviewer", Reason: "Safe bounds are incorrect."},
		{Severity: "medium", Category: "test", Location: "tests/studio-live.spec.ts:90", Role: "qa", Reason: "Capture lacks a narrow viewport."},
	}
	groups := groupReviewFollowups(task, findings)
	if len(groups) != 1 {
		t.Fatalf("four file observations should create one task follow-up, got %#v", groups)
	}
	body := reviewFollowupBody(t, groups[0], task)
	for _, want := range []string{"## Owner scope: `file:src/studio`", "## Owner scope: `file:src/engine`", "## Owner scope: `file:tests`", "App.tsx:40", "api-client.ts:80"} {
		if !strings.Contains(body, want) {
			t.Fatalf("task follow-up lost owner section or finding %q: %s", want, body)
		}
	}
}

func TestReviewFollowupProseLocationFallsBackToCategory(t *testing.T) {
	task := &model.Task{ID: "review-followups-prose", Issue: 48, HeadSHA: "abc123"}
	first := model.Finding{Severity: "medium", Category: "visual evidence", Location: "current-head studio browser captures", Reason: "Capture is missing."}
	second := first
	second.Location = "exact-head visual screenshots"
	before := groupReviewFollowups(task, []model.Finding{first})
	after := groupReviewFollowups(task, []model.Finding{second})
	if len(before) != 1 || len(after) != 1 || before[0].key != after[0].key || !strings.Contains(reviewFollowupBody(t, before[0], task), "## Owner scope: `category:visual evidence`") {
		t.Fatalf("prose location created fake source issue or unstable key: before=%#v after=%#v", before, after)
	}
}

func reviewFollowupBody(t *testing.T, group reviewFollowupGroup, task *model.Task) string {
	t.Helper()
	body, err := group.body(task, reviewFollowupHistory{})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestReviewFollowupSourceScopeIgnoresLocationNotation(t *testing.T) {
	const source = "src/http.ts"
	for _, location := range []string{
		"src/http.ts:288",
		"src/http.ts:288:4",
		"src/http.ts:288-290",
		"src/http.ts:288-290:4",
		"./src/http.ts:288",
		"`src/http.ts:288`",
		"src/http.ts (line 288)",
		"src/http.ts#L288",
		"other.ts:3; src/http.ts:288",
		"src/http.ts:288; other.ts:3",
		"src/http.ts:288, 290",
		"290-305, src/http.ts:288",
		"src/http.ts:288, 290:4",
	} {
		want := source
		if strings.Contains(location, "other.ts") {
			want = "other.ts"
		}
		if got := followupSourceFile(location); got != want {
			t.Errorf("source scope for %q: got %q want %q", location, got, want)
		}
	}
	if got := followupSourceFile(`C:\repo\src\http.ts:288-290:4`); got != "c/repo/src/http.ts" {
		t.Fatalf("Windows source path range normalized to %q", got)
	}
}

type followupHub struct {
	issues      map[int]github.Issue
	next        int
	issuesCalls int
}

func (h *followupHub) EnsureIssue(_ context.Context, key, _ string, body string) (int, error) {
	for number, issue := range h.issues {
		if strings.Contains(issue.Body, github.Marker(key)) {
			return number, nil
		}
	}
	h.next++
	h.issues[h.next] = github.Issue{Number: h.next, Body: github.Marker(key) + "\n\n" + body, State: "open"}
	return h.next, nil
}

func (h *followupHub) UpdateIssue(_ context.Context, number int, body string, closed bool) error {
	issue, ok := h.issues[number]
	if !ok {
		return fmt.Errorf("missing issue %d", number)
	}
	issue.Body = body
	if closed {
		issue.State = "closed"
	} else {
		issue.State = "open"
	}
	h.issues[number] = issue
	return nil
}

func (h *followupHub) Issues(_ context.Context) ([]github.Issue, error) {
	h.issuesCalls++
	issues := make([]github.Issue, 0, len(h.issues))
	for _, issue := range h.issues {
		issues = append(issues, issue)
	}
	return issues, nil
}

func (h *followupHub) EnsurePR(context.Context, string, string, string, string) (int, error) {
	return 0, fmt.Errorf("unexpected EnsurePR")
}
func (h *followupHub) UpdatePR(context.Context, int, string) error {
	return fmt.Errorf("unexpected UpdatePR")
}
func (h *followupHub) SetPRDraft(context.Context, int, bool) error {
	return fmt.Errorf("unexpected SetPRDraft")
}
func (h *followupHub) Pull(context.Context, int) (github.Pull, error) {
	return github.Pull{}, fmt.Errorf("unexpected Pull")
}

func issueContaining(t *testing.T, issues []github.Issue, text string) github.Issue {
	t.Helper()
	for _, issue := range issues {
		if strings.Contains(issue.Body, text) {
			return issue
		}
	}
	t.Fatalf("could not find issue containing %q: %#v", text, issues)
	return github.Issue{}
}
