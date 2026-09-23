package engine

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/nvrakesh06/ai-agentic-harness/internal/github"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
)

// reviewFollowups turns nonblocking review observations into a small set of
// actionable work items. Reviewers often describe the same defect differently;
// grouping must therefore not include the reviewer role or full prose in its key.
func (c *Controller) reviewFollowups(task *model.Task, findings []model.Finding) error {
	for _, group := range groupReviewFollowups(task, findings) {
		issue, err := c.P.Hub.EnsureIssue(c.ctx, group.key, group.title, group.body(task))
		if err != nil {
			return err
		}
		// EnsureIssue intentionally only creates. Refresh the durable issue body so
		// a repeated exact-head review preserves every current source role, location,
		// and suggested resolution without creating another notification.
		if err = c.P.Hub.UpdateIssue(c.ctx, issue, github.Marker(group.key)+"\n\n"+group.body(task), false); err != nil {
			return err
		}
	}
	return nil
}

type reviewFollowupGroup struct {
	key      string
	title    string
	category string
	source   string
	topic    string
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

	groups := []reviewFollowupGroup{}
	for _, finding := range medium {
		placed := false
		for i := range groups {
			if !sameFollowupConcern(groups[i], finding) {
				continue
			}
			groups[i].findings = append(groups[i].findings, finding)
			placed = true
			break
		}
		if !placed {
			groups = append(groups, reviewFollowupGroup{category: normalizedCategory(finding.Category), source: followupSourceFile(finding.Location), findings: []model.Finding{finding}})
		}
	}

	for i := range groups {
		group := &groups[i]
		sort.Slice(group.findings, func(i, j int) bool {
			return followupFindingKey(group.findings[i]) < followupFindingKey(group.findings[j])
		})
		group.topic = followupTopic(group.findings)
		if group.source == "" {
			group.source = followupSourceFile(group.findings[0].Location)
		}
		// The source file and primary semantic anchor are durable across reviewers
		// with different category labels and prose. Category remains visible in the
		// body but is deliberately not part of an anchored issue identity.
		scope := group.category
		if group.source != "" {
			scope = group.source
		}
		group.key = fmt.Sprintf("%s-review-followup-%x", task.ID, sha256.Sum256([]byte(scope+"\n"+group.topic)))
		group.title = "Review follow-up: " + followupTitle(group.topic)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].key < groups[j].key })
	return groups
}

func (g reviewFollowupGroup) body(task *model.Task) string {
	var body strings.Builder
	fmt.Fprintf(&body, "Nonblocking review follow-up for implementation issue #%d.\n\n", task.Issue)
	if task.PR != 0 {
		fmt.Fprintf(&body, "Implementation PR: #%d\n", task.PR)
	}
	if task.HeadSHA != "" {
		fmt.Fprintf(&body, "Review head: `%s`\n", task.HeadSHA)
	}
	fmt.Fprintf(&body, "Category: `%s`\n\n", g.category)
	body.WriteString("Address this one semantic concern, then verify the affected paths. Source observations:\n")
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

func sameFollowupConcern(group reviewFollowupGroup, candidate model.Finding) bool {
	for _, finding := range group.findings {
		shared, distinctive := sharedFollowupTerms(finding, candidate)
		if group.source != "" && group.source == followupSourceFile(candidate.Location) && (distinctive || (group.category == normalizedCategory(candidate.Category) && shared >= 2)) {
			return true
		}
	}
	return false
}

func sharedFollowupTerms(a, b model.Finding) (int, bool) {
	left, right := followupTerms(a), followupTerms(b)
	shared := 0
	distinctive := false
	for term := range left {
		if _, ok := right[term]; !ok {
			continue
		}
		shared++
		if len([]rune(term)) >= 5 && !genericFollowupTerm(term) {
			distinctive = true
		}
	}
	return shared, distinctive
}

func followupTopic(findings []model.Finding) string {
	counts := map[string]int{}
	for _, finding := range findings {
		for term := range followupNarrativeTerms(finding) {
			counts[term]++
		}
	}
	type candidate struct {
		term         string
		count, score int
	}
	terms := []candidate{}
	for term, count := range counts {
		if !genericFollowupTerm(term) {
			score := count * 100
			if isExplicitTechnicalAnchor(findings, term) {
				score += 10000
			}
			terms = append(terms, candidate{term, count, score})
		}
	}
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].score != terms[j].score {
			return terms[i].score > terms[j].score
		}
		if terms[i].count != terms[j].count {
			return terms[i].count > terms[j].count
		}
		return terms[i].term < terms[j].term
	})
	if len(terms) > 0 {
		// One primary anchor is intentionally more stable than a collection whose
		// secondary wording can vary as reviewers are added on recovery.
		return terms[0].term
	}
	for _, finding := range findings {
		if location := normalizedLocation(finding.Location); location != "" {
			return location
		}
	}
	return "general"
}

func isExplicitTechnicalAnchor(findings []model.Finding, wanted string) bool {
	for _, finding := range findings {
		for _, text := range []string{finding.Reason, finding.Resolution} {
			for _, token := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
				if strings.EqualFold(token, wanted) && token == strings.ToUpper(token) && len([]rune(token)) >= 3 {
					return true
				}
			}
		}
	}
	return false
}

func followupTitle(topic string) string {
	words := strings.FieldsFunc(topic, func(r rune) bool { return r == '-' || r == '_' || r == '/' || r == ':' })
	for i, word := range words {
		if len(word) <= 4 {
			words[i] = strings.ToUpper(word)
		} else {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return strings.Join(words, " ")
}

func normalizedLocation(location string) string {
	location = strings.TrimSpace(strings.ToLower(location))
	location = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '/' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return -1
	}, location)
	return location
}

func followupSourceFile(location string) string {
	location = strings.TrimSpace(strings.ToLower(location))
	parts := strings.Split(location, ":")
	for i := len(parts) - 1; i > 0; i-- {
		if numericLocationPart(parts[i]) {
			parts = parts[:i]
			continue
		}
		break
	}
	return normalizedLocation(strings.Join(parts, ":"))
}

func numericLocationPart(part string) bool {
	part = strings.TrimSpace(part)
	if part == "" {
		return false
	}
	for _, r := range part {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func followupTerms(finding model.Finding) map[string]struct{} {
	terms := map[string]struct{}{}
	for _, text := range []string{finding.Reason, finding.Resolution} {
		for _, token := range followupTextTerms(text) {
			if len([]rune(token)) >= 3 && !genericFollowupTerm(token) {
				terms[token] = struct{}{}
			}
		}
	}
	return terms
}

func followupNarrativeTerms(finding model.Finding) map[string]struct{} {
	terms := map[string]struct{}{}
	for _, text := range []string{finding.Reason, finding.Resolution} {
		for _, token := range followupTextTerms(text) {
			if len([]rune(token)) >= 3 && !genericFollowupTerm(token) {
				terms[token] = struct{}{}
			}
		}
	}
	return terms
}

func followupTextTerms(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func genericFollowupTerm(term string) bool {
	_, generic := map[string]bool{
		"add": true, "affected": true, "after": true, "and": true, "assertion": true, "check": true, "code": true,
		"configured": true, "current": true, "defect": true, "finding": true, "for": true, "from": true,
		"implementation": true, "issue": true, "missing": true, "native": true, "path": true, "review": true,
		"reviewer": true, "should": true, "suggested": true, "test": true, "the": true, "this": true,
		"use": true, "verify": true, "with": true, "arrow": true, "body": true, "branch": true,
		"error": true, "helper": true, "label": true, "opacity": true, "response": true, "state": true,
		"text": true, "through": true,
	}[term]
	return generic
}

func followupFindingKey(finding model.Finding) string {
	return strings.Join([]string{normalizedCategory(finding.Category), normalizedLocation(finding.Location), strings.ToLower(finding.Reason), strings.ToLower(finding.Resolution), strings.ToLower(finding.Role)}, "\x00")
}
