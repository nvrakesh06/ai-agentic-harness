package engine

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
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

func TestReviewPromptTaskIncludesNestedInstructionsForActualChangedPath(t *testing.T) {
	task := &model.Task{ID: "review", Areas: []string{"docs"}}
	promptTask := reviewPromptTask(task, []string{"src/service/handler.go"})
	if len(task.Areas) != 1 || task.Areas[0] != "docs" {
		t.Fatalf("review prompt copy mutated canonical task areas: %#v", task.Areas)
	}
	prompt := roles.Compile(config.Effective{Files: map[string]string{
		"AGENTS.md":             "root instruction",
		"src/service/AGENTS.md": "service instruction",
	}}, roles.Builtins()["reviewer"], "windows", promptTask, "review", "diff", "evidence")
	if !strings.Contains(prompt, "CANONICAL src/service/AGENTS.md\nservice instruction") {
		t.Fatalf("actual changed-path nested instructions missing from review prompt: %s", prompt)
	}
}

func TestReviewReuseScopeInvalidatesChangedTaskContract(t *testing.T) {
	task := &model.Task{Title: "Explain ownership", Objective: "Show the reassignment", Acceptance: []string{"Show the timeout"}, Areas: []string{"fixtures/review-data/**"}, Security: true, Risk: "low"}
	paths := []string{"fixtures/review-data/dashboard.txt"}
	roster := []string{"qa", "reviewer", "security"}
	original := reviewScope(task, paths, roster)
	for name, change := range map[string]func(*model.Task){
		"objective":         func(next *model.Task) { next.Objective = "Show authentication too" },
		"acceptance":        func(next *model.Task) { next.Acceptance = []string{"Show authorization"} },
		"human answer":      func(next *model.Task) { next.Decisions = []string{"Human answer: include credential handling"} },
		"operator guidance": func(next *model.Task) { next.Decisions = []string{"GUIDANCE: verify credential redaction"} },
	} {
		t.Run(name, func(t *testing.T) {
			next := *task
			change(&next)
			if reviewScope(&next, paths, roster) == original {
				t.Fatal("changed task contract retained prior security review scope")
			}
		})
	}
	if reviewScope(task, paths, roster) != original {
		t.Fatal("unchanged task contract did not retain deterministic scope")
	}
}

func TestReviewScopeRejectsLegacyEphemeralChangedPathDuplicate(t *testing.T) {
	task := &model.Task{Title: "Windows fixture", Objective: "verify immediate shutdown", Areas: []string{"tests\\studio-core.test.ts"}, Risk: "low"}
	paths := []string{"tests/studio-core.test.ts"}
	roster := []string{"qa", "reviewer", "security"}
	expected := reviewScope(task, paths, roster)
	if !acceptedReviewScope(task, paths, roster, expected) {
		t.Fatal("current exact-head review scope was rejected")
	}

	// verifyReview used to append the Git path to its local Task copy. That
	// copy was not persisted, so final integration rebuilt a different scope
	// and discarded valid MERGE_READY approvals.
	ephemeral := *task
	ephemeral.Areas = append(ephemeral.Areas, paths[0])
	legacyScope := reviewScope(&ephemeral, paths, roster)
	if legacyScope == expected || acceptedReviewScope(task, paths, roster, legacyScope) {
		t.Fatal("legacy scope without a historical durable task contract was accepted")
	}

	changedContract := *task
	changedContract.Areas = append(changedContract.Areas, "scripts/release.ps1")
	if acceptedReviewScope(task, paths, roster, reviewScope(&changedContract, paths, roster)) {
		t.Fatal("an unrelated task-area contract change retained prior review scope")
	}

	// The previous task contract had an additional path matching the diff, but
	// the current task was narrowed. The old fingerprint remains rejected.
	oldContract := &model.Task{Title: task.Title, Objective: task.Objective, Areas: []string{"scripts/release.ps1", paths[0]}, Risk: task.Risk}
	oldScope := reviewScope(oldContract, paths, roster)
	narrowed := &model.Task{Title: task.Title, Objective: task.Objective, Areas: []string{"scripts/release.ps1"}, Risk: task.Risk}
	if acceptedReviewScope(narrowed, paths, roster, oldScope) {
		t.Fatal("narrowed task areas retained an old review scope through legacy compatibility")
	}
}

func TestReviewReuseRejectsGitSymlinkAndExecutableMetadata(t *testing.T) {
	allowed := []string{"fixtures/review-data/*.txt"}
	path := "fixtures/review-data/link.txt"
	for _, file := range []struct{ name, mode, content string }{
		{"symlink", "120000", ".."},
		{"executable", "100755", "safe"},
	} {
		t.Run(file.name, func(t *testing.T) {
			dir := t.TempDir()
			runGit := func(input string, args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
				cmd.Stdin = strings.NewReader(input)
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			runGit("", "init", "-q")
			blob := runGit(file.content, "hash-object", "-w", "--stdin")
			runGit("", "update-index", "--add", "--cacheinfo", file.mode, blob, path)
			diff := runGit("", "diff", "--cached", "--no-ext-diff")
			if !strings.Contains(diff, "new file mode "+file.mode) {
				t.Fatalf("fixture lacks expected Git mode: %s", diff)
			}
			if reviewReuseDiffSafe(diff, []string{path}, allowed) {
				t.Fatal("non-regular Git object reused security review")
			}
		})
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
