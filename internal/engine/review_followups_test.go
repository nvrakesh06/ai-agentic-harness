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
	if len(issues) != 3 {
		t.Fatalf("expected CONNECT, heartbeat, and bounds work items; got %d: %#v", len(issues), issues)
	}
	connect := issueContaining(t, issues, "CONNECT requests")
	for _, want := range []string{"**qa**", "**reviewer**", "**security**", "http.ts:288", "http.ts:290", "Route CONNECT through the JSON error writer."} {
		if !strings.Contains(connect.Body, want) {
			t.Fatalf("grouped CONNECT issue omitted %q:\n%s", want, connect.Body)
		}
	}
	heartbeat := issueContaining(t, issues, "heartbeat label")
	if !strings.Contains(heartbeat.Body, "**animation**") || !strings.Contains(heartbeat.Body, "**reviewer**") {
		t.Fatalf("grouped heartbeat issue did not preserve both source roles:\n%s", heartbeat.Body)
	}

	// A recovery/re-review may change wording and ordering, but it must update
	// the same stable follow-up issues rather than create a fresh notification.
	findings[0].Category = "socket handling"
	findings[0].Reason = "CONNECT still bypasses the JSON error envelope."
	findings[1].Category = "method behavior"
	findings[1].Reason = "The CONNECT branch has no protocol-safe error body."
	added := model.Finding{Severity: "medium", Category: "request dispatch", Location: "http.ts:291", Role: "design", Reason: "CONNECT must share the documented API error contract.", Resolution: "Use the same JSON envelope for CONNECT."}
	if err := c.reviewFollowups(task, []model.Finding{findings[6], findings[5], findings[4], findings[3], findings[1], added, findings[2], findings[0]}); err != nil {
		t.Fatal(err)
	}
	issues, err = hub.Issues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 3 {
		t.Fatalf("same-head re-review created duplicate follow-up issues: %#v", issues)
	}
	connect = issueContaining(t, issues, "CONNECT still bypasses")
	if !strings.Contains(connect.Body, "CONNECT still bypasses") || !strings.Contains(connect.Body, "**reviewer**") {
		t.Fatalf("same-head re-review did not refresh the grouped issue:\n%s", connect.Body)
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
		if !strings.Contains(groups[0].body(task), want) {
			t.Fatalf("owner issue dropped distinct concern %q: %s", want, groups[0].body(task))
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
		if !strings.Contains(groups[0].body(task), want) {
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

type followupHub struct {
	issues map[int]github.Issue
	next   int
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
