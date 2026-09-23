package engine

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/nvrakesh06/ai-agentic-harness/internal/github"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
)

// reviewFollowups creates one durable work item per source file. Distinct
// observations in that file remain separate bullets so one owner can address
// them together without duplicate issues from independent review roles.
func (c *Controller) reviewFollowups(task *model.Task, findings []model.Finding) error {
	for _, group := range groupReviewFollowups(task, findings) {
		issue, err := c.P.Hub.EnsureIssue(c.ctx, group.key, group.title, group.body(task))
		if err != nil {
			return err
		}
		// The key is independent of review prose and role, so a re-review
		// refreshes the same issue even if the wording or acronyms change.
		if err = c.P.Hub.UpdateIssue(c.ctx, issue, github.Marker(group.key)+"\n\n"+group.body(task), false); err != nil {
			return err
		}
	}
	return nil
}

type reviewFollowupGroup struct {
	key      string
	title    string
	scope    string
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

	byScope := map[string]*reviewFollowupGroup{}
	for _, finding := range medium {
		source := followupSourceFile(finding.Location)
		scope := "file:" + source
		if source == "" {
			scope = "category:" + normalizedCategory(finding.Category)
		}
		group := byScope[scope]
		if group == nil {
			group = &reviewFollowupGroup{scope: scope}
			byScope[scope] = group
		}
		group.findings = append(group.findings, finding)
	}

	groups := make([]reviewFollowupGroup, 0, len(byScope))
	for _, group := range byScope {
		group.key = fmt.Sprintf("%s-review-followup-%x", task.ID, sha256.Sum256([]byte(group.scope)))
		group.title = short("Review follow-ups: "+strings.TrimPrefix(strings.TrimPrefix(group.scope, "file:"), "category:"), 90)
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].key < groups[j].key })
	return groups
}

func (g reviewFollowupGroup) body(task *model.Task) string {
	var body strings.Builder
	fmt.Fprintf(&body, "Nonblocking review follow-ups for implementation issue #%d.\n\n", task.Issue)
	if task.PR != 0 {
		fmt.Fprintf(&body, "Implementation PR: #%d\n", task.PR)
	}
	if task.HeadSHA != "" {
		fmt.Fprintf(&body, "Review head: `%s`\n", task.HeadSHA)
	}
	fmt.Fprintf(&body, "Owner scope: `%s`\n\n", g.scope)
	body.WriteString("Address each observation below and verify the affected paths. Similar wording may describe one defect; different remedies remain visible separately.\n")
	for _, finding := range g.findings {
		role := finding.Role
		if role == "" {
			role = "reviewer"
		}
		location := finding.Location
		if location == "" {
			location = "unspecified location"
		}
		fmt.Fprintf(&body, "\n- **%s** — `%s`\n  - Category: `%s`\n  - Finding: %s\n", role, location, normalizedCategory(finding.Category), safety.Redact(finding.Reason))
		if finding.Resolution != "" {
			fmt.Fprintf(&body, "  - Suggested resolution: %s\n", safety.Redact(finding.Resolution))
		}
	}
	return body.String()
}

func normalizedCategory(category string) string {
	category = strings.TrimSpace(strings.ToLower(category))
	if category == "" {
		return "general"
	}
	return category
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
		candidate = strings.ReplaceAll(candidate, "\\", "/")
		candidate = followupParenLine.ReplaceAllString(candidate, "")
		candidate = followupHashLine.ReplaceAllString(candidate, "")
		candidate = followupColonLine.ReplaceAllString(candidate, "")
		candidate = strings.Trim(strings.TrimSpace(candidate), "`\"' ")
		for strings.HasPrefix(candidate, "./") {
			candidate = strings.TrimPrefix(candidate, "./")
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

func followupFindingKey(finding model.Finding) string {
	return strings.Join([]string{normalizedCategory(finding.Category), strings.ToLower(finding.Location), strings.ToLower(finding.Reason), strings.ToLower(finding.Resolution), strings.ToLower(finding.Role)}, "\x00")
}
