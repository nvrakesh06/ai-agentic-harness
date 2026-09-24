package engine

import (
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

func TestReviewReuseThreeHeadPolicyFailsClosed(t *testing.T) {
	base := strings.Repeat("a", 40)
	first := strings.Repeat("b", 40)
	second := strings.Repeat("c", 40)
	config := strings.Repeat("d", 64)
	rulesHash := strings.Repeat("e", 64)
	task := &model.Task{BaseSHA: base, HeadSHA: second, Security: true, Risk: "low"}
	roster := []string{"qa", "reviewer", "security"}
	allowed := []string{"fixtures/review-data/*.txt"}
	scope := reviewScope(task, []string{"fixtures/review-data/dashboard.txt"}, roster)
	provenance := model.ReviewProvenance{Role: "security", Base: base, Head: first, Config: config, Rules: rulesHash, Roster: roster, Scope: scope, Provider: "codex", Runtime: "codex/gpt-6-terra/1.0.0"}
	if !reviewReuseDiffSafe("diff --git a/fixtures/review-data/dashboard.txt b/fixtures/review-data/dashboard.txt\n+ Improve dashboard hierarchy explanation.", []string{"fixtures/review-data/dashboard.txt"}, allowed) {
		t.Fatal("second configured text-data head was not eligible")
	}
	if reviewReuseDiffSafe("diff --git a/docs/dashboard.md b/docs/dashboard.md\n+ <script>alert(1)</script>", []string{"docs/dashboard.md"}, allowed) {
		t.Fatal("raw HTML Markdown delta was incorrectly eligible")
	}
	if reviewReuseDiffSafe("diff --git a/docs/dashboard.mdx b/docs/dashboard.mdx\n+ {fetch('/api/credentials')}", []string{"docs/dashboard.mdx"}, allowed) {
		t.Fatal("executable MDX delta was incorrectly eligible")
	}
	if reviewReuseDiffSafe("diff --git a/ui/dashboard.css b/ui/dashboard.css\n+ @namespace svg url(http://example.invalid/svg);", []string{"ui/dashboard.css"}, allowed) {
		t.Fatal("CSS resource-loading delta was incorrectly eligible")
	}
	for _, policyPath := range []string{"AGENTS.md", "docs/CLAUDE.md", ".github/copilot-instructions.md", ".codex/policy.md", "docs/build-instructions.md"} {
		if reviewReuseDiffSafe("diff --git a/"+policyPath+" b/"+policyPath+"\n+ change execution guidance", []string{policyPath}, allowed) {
			t.Fatalf("agent/build policy markdown %q was incorrectly eligible", policyPath)
		}
	}
	if reviewReuseDiffSafe("diff --git a/internal/auth/session.go b/internal/auth/session.go", []string{"fixtures/review-data/dashboard.txt", "internal/auth/session.go"}, allowed) {
		t.Fatal("third security-sensitive head was incorrectly eligible")
	}
	changedScope := reviewScope(task, []string{"fixtures/review-data/dashboard.txt", "internal/auth/session.go"}, roster)
	if changedScope == scope {
		t.Fatal("security-sensitive third head did not change review scope")
	}
	if provenance.Config != config || provenance.Rules != rulesHash || !sameRoster(provenance.Roster, roster) {
		t.Fatal("fixture lost provenance identity")
	}
}

func TestReviewReuseDispositionRequiresWholeRoster(t *testing.T) {
	required := []roles.Role{roles.Builtins()["qa"], roles.Builtins()["reviewer"], roles.Builtins()["security"]}
	head := strings.Repeat("a", 40)
	evidence := &model.Evidence{Head: head, ReviewScope: strings.Repeat("b", 64), ReviewRoster: []string{"qa", "reviewer", "security"}, ReviewDispositions: map[string]model.ReviewDisposition{
		"qa":       {Disposition: "completed", SourceHead: head, Runtime: "codex/gpt-6-terra/1.0.0"},
		"reviewer": {Disposition: "completed", SourceHead: head, Runtime: "codex/gpt-6-sol/1.0.0"},
		"security": {Disposition: "reused", SourceHead: strings.Repeat("c", 40), Runtime: "codex/gpt-6-sol/1.0.0", Reason: "presentation-only"},
	}}
	if !validReviewDispositions(required, evidence) {
		t.Fatal("complete roster with narrow security reuse rejected")
	}
	delete(evidence.ReviewDispositions, "qa")
	if validReviewDispositions(required, evidence) {
		t.Fatal("missing QA disposition accepted")
	}
}
