package engine

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/nvrakesh06/ai-agentic-harness/internal/github"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
)

// reviewFollowups creates at most one durable, nonblocking work item for an
// implementation task. Its body separates observations by owner scope while
// keeping all review work for the task in one place.
func (c *Controller) reviewFollowups(task *model.Task, findings []model.Finding) error {
	for _, group := range groupReviewFollowups(task, findings) {
		history, issue, err := c.reviewFollowupHistory(group.key)
		if err != nil {
			return err
		}
		body, err := group.body(task, history)
		if err != nil {
			return err
		}
		if issue == 0 {
			issue, err = c.P.Hub.EnsureIssue(c.ctx, group.key, group.title, body)
			if err != nil {
				return err
			}
		}
		// The key is independent of review prose and role, so a re-review
		// refreshes the same issue even if the wording or acronyms change.
		if err = c.P.Hub.UpdateIssue(c.ctx, issue, github.Marker(group.key)+"\n\n"+body, false); err != nil {
			return err
		}
	}
	return nil
}

type reviewFollowupHistory struct {
	Reviews []reviewFollowupReview `json:"reviews"`
	// Findings supports history emitted by the initial task-keyed rollout. It is
	// converted into an immutable legacy snapshot on the next update.
	Findings []model.Finding `json:"findings,omitempty"`
}

type reviewFollowupReview struct {
	Head     string          `json:"head"`
	Findings []model.Finding `json:"findings"`
}

const reviewFollowupHistoryPrefix = "<!-- aih-review-followup-history:"

func (c *Controller) reviewFollowupHistory(key string) (reviewFollowupHistory, int, error) {
	issues, err := c.P.Hub.Issues(c.ctx)
	if err != nil {
		return reviewFollowupHistory{}, 0, err
	}
	for _, issue := range issues {
		if strings.Contains(issue.Body, github.Marker(key)) {
			history, err := parseReviewFollowupHistory(issue.Body)
			if err != nil {
				return reviewFollowupHistory{}, 0, fmt.Errorf("parse review follow-up history for issue #%d: %w", issue.Number, err)
			}
			return history, issue.Number, nil
		}
	}
	return reviewFollowupHistory{}, 0, nil
}

func parseReviewFollowupHistory(body string) (reviewFollowupHistory, error) {
	start := strings.Index(body, reviewFollowupHistoryPrefix)
	if start < 0 {
		return reviewFollowupHistory{}, fmt.Errorf("history marker is missing")
	}
	encoded := body[start+len(reviewFollowupHistoryPrefix):]
	end := strings.Index(encoded, "-->")
	if end < 0 {
		return reviewFollowupHistory{}, fmt.Errorf("history marker is unterminated")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded[:end]))
	if err != nil {
		return reviewFollowupHistory{}, fmt.Errorf("decode history: %w", err)
	}
	var history reviewFollowupHistory
	if err := json.Unmarshal(decoded, &history); err != nil {
		return reviewFollowupHistory{}, fmt.Errorf("decode history JSON: %w", err)
	}
	if len(history.Findings) > 0 {
		history.Reviews = append(history.Reviews, reviewFollowupReview{Head: "legacy", Findings: history.Findings})
		history.Findings = nil
	}
	return history, nil
}

type reviewFollowupGroup struct {
	key      string
	title    string
	findings []model.Finding
}

func groupReviewFollowups(task *model.Task, findings []model.Finding) []reviewFollowupGroup {
	medium := make([]model.Finding, 0, len(findings))
	for _, finding := range findings {
		if finding.Severity == "medium" {
			medium = append(medium, finding)
		}
	}
	sort.Slice(medium, func(i, j int) bool { return followupFindingKey(medium[i]) < followupFindingKey(medium[j]) })

	if len(medium) == 0 {
		return nil
	}

	// Task identity, not a review finding or source path, owns the durable
	// follow-up. A re-review can change wording, locations, and commit head
	// without opening a new issue.
	return []reviewFollowupGroup{{
		key:      task.ID + "-review-followup",
		title:    short(fmt.Sprintf("Review follow-ups for implementation #%d", task.Issue), 90),
		findings: distinctFollowupFindings(medium),
	}}
}

func distinctFollowupFindings(findings []model.Finding) []model.Finding {
	distinct := make([]model.Finding, 0, len(findings))
	seen := make(map[string]struct{}, len(findings))
	for _, finding := range findings {
		key := followupFindingKey(finding)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		distinct = append(distinct, finding)
	}
	return distinct
}

func redactedFollowupFindings(findings []model.Finding) []model.Finding {
	redacted := make([]model.Finding, len(findings))
	copy(redacted, findings)
	for i := range redacted {
		redacted[i].Reason = safety.Redact(redacted[i].Reason)
		redacted[i].Resolution = safety.Redact(redacted[i].Resolution)
		redacted[i].BaselineSHA = safety.Redact(redacted[i].BaselineSHA)
		redacted[i].BaselineEvidence = safety.Redact(redacted[i].BaselineEvidence)
	}
	return redacted
}

func (h reviewFollowupHistory) withReview(head string, findings []model.Finding) reviewFollowupHistory {
	if head == "" {
		head = "unversioned"
	}
	findings = distinctFollowupFindings(redactedFollowupFindings(findings))
	for i := range h.Reviews {
		if h.Reviews[i].Head == head {
			h.Reviews[i].Findings = mergeReviewRoleFindings(h.Reviews[i].Findings, findings)
			return h
		}
	}
	h.Reviews = append(h.Reviews, reviewFollowupReview{Head: head, Findings: findings})
	return h
}

// mergeReviewRoleFindings replaces only roles represented by the refresh.
// workflow refreshes can contain one role at a time, so replacing a whole
// head snapshot would silently discard findings from roles not rerun yet.
func mergeReviewRoleFindings(previous, refreshed []model.Finding) []model.Finding {
	refreshedRoles := make(map[string]struct{}, len(refreshed))
	for _, finding := range refreshed {
		refreshedRoles[followupRole(finding)] = struct{}{}
	}
	merged := make([]model.Finding, 0, len(previous)+len(refreshed))
	for _, finding := range previous {
		if _, replaced := refreshedRoles[followupRole(finding)]; !replaced {
			merged = append(merged, finding)
		}
	}
	return distinctFollowupFindings(append(merged, refreshed...))
}

func followupRole(finding model.Finding) string {
	if finding.Role == "" {
		return "reviewer"
	}
	return finding.Role
}

func (h reviewFollowupHistory) findings() []model.Finding {
	var all []model.Finding
	for _, review := range h.Reviews {
		all = append(all, review.Findings...)
	}
	return distinctFollowupFindings(all)
}

type reviewFollowupSection struct {
	scope    string
	findings []model.Finding
}

func reviewFollowupSections(findings []model.Finding) []reviewFollowupSection {
	byScope := map[string]*reviewFollowupSection{}
	for _, finding := range findings {
		source := followupOwnerScope(followupSourceFile(finding.Location))
		scope := "file:" + source
		if source == "" {
			scope = "category:" + normalizedCategory(finding.Category)
		}
		section := byScope[scope]
		if section == nil {
			section = &reviewFollowupSection{scope: scope}
			byScope[scope] = section
		}
		section.findings = append(section.findings, finding)
	}
	sections := make([]reviewFollowupSection, 0, len(byScope))
	for _, section := range byScope {
		sections = append(sections, *section)
	}
	sort.Slice(sections, func(i, j int) bool { return sections[i].scope < sections[j].scope })
	return sections
}

func (g reviewFollowupGroup) body(task *model.Task, history reviewFollowupHistory) (string, error) {
	history = history.withReview(task.HeadSHA, g.findings)
	allFindings := history.findings()
	historyJSON, err := json.Marshal(history)
	if err != nil {
		return "", err
	}
	var body strings.Builder
	fmt.Fprintf(&body, "Nonblocking review follow-ups for implementation issue #%d.\n\n", task.Issue)
	if task.PR != 0 {
		fmt.Fprintf(&body, "Implementation PR: #%d\n", task.PR)
	}
	if task.HeadSHA != "" {
		fmt.Fprintf(&body, "Review head: `%s`\n", task.HeadSHA)
	}
	body.WriteString("Address each observation below and verify the affected paths. Sections are grouped by owner scope; distinct reviewer roles, locations, reasons, and resolutions remain visible.\n")
	for _, section := range reviewFollowupSections(allFindings) {
		fmt.Fprintf(&body, "\n## Owner scope: `%s`\n", section.scope)
		for _, finding := range section.findings {
			role := followupRole(finding)
			location := finding.Location
			if location == "" {
				location = "unspecified location"
			}
			fmt.Fprintf(&body, "\n- **%s** — `%s`\n  - Category: `%s`\n  - Finding: %s\n", role, location, normalizedCategory(finding.Category), safety.Redact(finding.Reason))
			if finding.Resolution != "" {
				fmt.Fprintf(&body, "  - Suggested resolution: %s\n", safety.Redact(finding.Resolution))
			}
		}
	}
	fmt.Fprintf(&body, "\n%s%s -->\n", reviewFollowupHistoryPrefix, base64.StdEncoding.EncodeToString(historyJSON))
	return body.String(), nil
}

func normalizedCategory(category string) string {
	category = strings.TrimSpace(strings.ToLower(category))
	if category == "" {
		return "general"
	}
	return category
}

func followupOwnerScope(source string) string {
	if source == "" {
		return ""
	}
	parts := strings.Split(source, "/")
	if len(parts) < 2 {
		return source
	}
	switch parts[0] {
	case "src", "internal", "pkg", "lib", "app":
		if len(parts) >= 3 {
			return parts[0] + "/" + parts[1]
		}
		return parts[0]
	case "tests", "test", "scripts", "schemas", "docs", "assets":
		return parts[0]
	case "projects":
		if len(parts) >= 3 {
			return parts[0] + "/" + parts[1]
		}
	}
	return source
}

func followupSourceFile(location string) string {
	location = strings.TrimSpace(strings.ToLower(location))
	if location == "" {
		return ""
	}
	// The source scope must not change when reviewers switch between line,
	// range, column, or prose notation. Sort multi-location candidates so their
	// authored order also cannot create another issue for the same paths.
	var sources []string
	for _, candidate := range strings.FieldsFunc(location, func(r rune) bool { return r == ';' || r == ',' || r == '\n' }) {
		candidate = strings.Trim(strings.TrimSpace(candidate), "`\"' ")
		if followupBareLine.MatchString(candidate) {
			continue
		}
		candidate = strings.ReplaceAll(candidate, "\\", "/")
		candidate = followupParenLine.ReplaceAllString(candidate, "")
		candidate = followupHashLine.ReplaceAllString(candidate, "")
		candidate = followupColonLine.ReplaceAllString(candidate, "")
		candidate = strings.Trim(strings.TrimSpace(candidate), "`\"' ")
		for strings.HasPrefix(candidate, "./") {
			candidate = strings.TrimPrefix(candidate, "./")
		}
		// Reviewers sometimes put prose such as "current-head browser captures"
		// in Location. Do not turn that phrase into a fake source-file issue.
		if strings.ContainsAny(candidate, " \t") || (!strings.Contains(candidate, ".") && !strings.HasPrefix(candidate, "src/") && !strings.HasPrefix(candidate, "internal/") && !strings.HasPrefix(candidate, "pkg/") && !strings.HasPrefix(candidate, "lib/") && !strings.HasPrefix(candidate, "app/") && !strings.HasPrefix(candidate, "tests/") && !strings.HasPrefix(candidate, "docs/") && !strings.HasPrefix(candidate, "projects/")) {
			continue
		}
		candidate = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '/' || r == '.' || r == '_' || r == '-' {
				return r
			}
			return -1
		}, candidate)
		if candidate != "" {
			sources = append(sources, candidate)
		}
	}
	if len(sources) == 0 {
		return ""
	}
	sort.Strings(sources)
	return sources[0]
}

var followupParenLine = regexp.MustCompile(`(?i)\s*\(lines?\s+\d+(?:\s*[-–]\s*\d+)?\)$`)
var followupHashLine = regexp.MustCompile(`(?i)#l\d+(?:-l?\d+)?$`)
var followupColonLine = regexp.MustCompile(`:\d+(?:[-–]\d+)?(?::\d+)?$`)
var followupBareLine = regexp.MustCompile(`(?i)^l?\d+(?:[-–]l?\d+)?(?::\d+)?$`)

func followupFindingKey(finding model.Finding) string {
	return strings.Join([]string{normalizedCategory(finding.Category), strings.ToLower(finding.Location), strings.ToLower(finding.Reason), strings.ToLower(finding.Resolution), strings.ToLower(finding.Role)}, "\x00")
}
