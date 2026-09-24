package roles

import (
	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"strings"
	"testing"
)

func TestCustomRolesAndTriggers(t *testing.T) {
	files := map[string]string{".aih/roles/data.yaml": "name: data-validator\nextends: reviewer\ntriggers:\n  paths: [data/**]\nblocking:\n  severities: [medium, high, critical]\nfocus: [timestamps]\n"}
	all, e := Load(files)
	if e != nil {
		t.Fatal(e)
	}
	required, e := Required(all, &model.Task{Risk: "low"}, []string{"data/input/file.go"}, "review")
	if e != nil || len(required) != 2 {
		t.Fatal(required, e)
	}
	r := all["data-validator"]
	if !Blocking(r, []model.Finding{{Severity: "medium"}}) {
		t.Fatal("custom blocker ignored")
	}
	files[".aih/roles/data.yaml"] = "name: bad\nextends: reviewer\npermissions: [write]"
	if _, e = Load(files); e == nil {
		t.Fatal("writer permissions accepted for reviewer")
	}
}

func TestRequiredReviewRosterDeduplicatesExtendingValidators(t *testing.T) {
	files := map[string]string{
		".aih/roles/animation.yaml": "name: animation-architecture\nextends: reviewer\ntriggers:\n  paths: [src/animation/**]\nblocking:\n  severities: [medium, high, critical]\n",
		".aih/roles/visual.yaml":    "name: visual-quality\nextends: designer\ntriggers:\n  risks: [high]\n",
		".aih/roles/qa.yaml":        "name: qa-specialist\nextends: qa\ntriggers:\n  paths: [src/animation/**]\n",
		".aih/roles/security.yaml":  "name: security-specialist\nextends: security\ntriggers:\n  risks: [high]\n",
	}
	all, err := Load(files)
	if err != nil {
		t.Fatal(err)
	}
	required, err := Required(all, &model.Task{Risk: "high", UI: true}, []string{"src/animation/timeline.go"}, "review")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(required))
	for i, role := range required {
		got[i] = role.Name
	}
	want := []string{"animation-architecture", "qa", "qa-specialist", "security", "security-specialist", "visual-quality"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("review roster = %v, want %v", got, want)
	}
	_, reason := ReviewRoster(required)
	for _, value := range []string{"reviewer satisfied by animation-architecture", "designer satisfied by visual-quality", "inherits parent instructions"} {
		if !strings.Contains(reason, value) {
			t.Fatalf("review roster reason missing %q: %s", value, reason)
		}
	}
	if !Blocking(all["animation-architecture"], []model.Finding{{Severity: "medium"}}) {
		t.Fatal("specialist blocking severity was not preserved")
	}
	if !strings.Contains(all["animation-architecture"].Instructions, Builtins()["reviewer"].Instructions) {
		t.Fatal("specialist did not retain inherited reviewer instructions")
	}
	if !Blocking(all["security"], []model.Finding{{Severity: "high"}}) {
		t.Fatal("built-in security blocking severity was not preserved")
	}
}

func TestRequiredReviewRosterRetainsParentWithoutMatchingValidator(t *testing.T) {
	all, err := Load(map[string]string{
		".aih/roles/animation.yaml": "name: animation-architecture\nextends: reviewer\ntriggers:\n  paths: [src/animation/**]\n",
		".aih/roles/visual.yaml":    "name: visual-quality\nextends: designer\ntriggers:\n  paths: [web/**]\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	required, err := Required(all, &model.Task{Risk: "low", UI: true}, []string{"internal/roles/roles.go"}, "review")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(required))
	for i, role := range required {
		got[i] = role.Name
	}
	want := []string{"designer", "qa", "reviewer"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("review roster = %v, want %v", got, want)
	}
}

func TestRequiredReviewRosterKeepsParentForIndependentOverrideAndNonReviewStage(t *testing.T) {
	all, err := Load(map[string]string{
		".aih/roles/independent.yaml": "name: independent-animation\nextends: reviewer\nindependent_parent_review: true\ntriggers:\n  paths: [src/animation/**]\n",
		".aih/roles/pre.yaml":         "name: pre-visual\nextends: designer\nstage: pre-implementation\ntriggers:\n  paths: [src/animation/**]\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	required, err := Required(all, &model.Task{Risk: "low", UI: true}, []string{"src/animation/timeline.go"}, "review")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(required))
	for i, role := range required {
		got[i] = role.Name
	}
	want := []string{"designer", "independent-animation", "qa", "reviewer"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("review roster = %v, want %v", got, want)
	}
	_, reason := ReviewRoster(required)
	if !strings.Contains(reason, "reviewer retained with independent-animation (independent_parent_review)") {
		t.Fatalf("independent override reason missing: %s", reason)
	}
}
func TestIndependentReviewOverrideWinsWithMultipleSpecialists(t *testing.T) {
	all, err := Load(map[string]string{
		".aih/roles/independent.yaml": "name: independent-animation\nextends: reviewer\nindependent_parent_review: true\ntriggers:\n  paths: [src/animation/**]\n",
		".aih/roles/standard.yaml":    "name: standard-animation\nextends: reviewer\ntriggers:\n  paths: [src/animation/**]\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	required, err := Required(all, &model.Task{Risk: "low"}, []string{"src/animation/timeline.go"}, "review")
	if err != nil {
		t.Fatal(err)
	}
	names, reason := ReviewRoster(required)
	if strings.Join(names, ",") != "independent-animation,qa,reviewer,standard-animation" {
		t.Fatalf("independent parent was dropped: %v", names)
	}
	if !strings.Contains(reason, "reviewer retained with independent-animation (independent_parent_review)") || strings.Contains(reason, "retained with independent-animation, standard-animation") {
		t.Fatalf("inaccurate roster reason: %s", reason)
	}
}
func TestContextFilteringAndCanonicalRules(t *testing.T) {
	e := config.Effective{Files: map[string]string{"AGENTS.md": "CANONICAL", "backend/AGENTS.md": "BACKEND", "frontend/AGENTS.md": "FRONTEND"}}
	task := &model.Task{Areas: []string{"backend/api"}}
	r := Builtins()["implementer"]
	win := Compile(e, r, "windows", task, "backend work", "", "")
	mac := Compile(e, r, "darwin", task, "backend work", "", "")
	if strings.Contains(win, "macOS") || strings.Contains(mac, "Windows") {
		t.Fatal("cross-platform context leak")
	}
	for _, s := range []string{"CANONICAL", "BACKEND", Core} {
		if !strings.Contains(mac, s) {
			t.Fatal("missing inherited rules")
		}
	}
	if strings.Contains(mac, "FRONTEND") || strings.Contains(mac, Builtins()["designer"].Instructions) {
		t.Fatal("unrelated context leaked")
	}
}

func TestGuidanceAppearsOnlyInImplementerPrompt(t *testing.T) {
	source := &model.Task{ID: "api", ObjectiveID: "shared", State: model.Running, HeadSHA: strings.Repeat("a", 40)}
	target := &model.Task{ID: "ui", ObjectiveID: "shared", State: model.Ready}
	if err := model.QueueGuidance(target, source, "correction-1", "Use the source task's real CLI entrypoint."); err != nil {
		t.Fatal(err)
	}
	e := config.Effective{Files: map[string]string{}}
	implementation := Compile(e, Builtins()["implementer"], "windows", target, target.Objective, "", "")
	for _, expected := range []string{"SUPERVISOR TASK GUIDANCE correction-1", source.HeadSHA, "Use the source task's real CLI entrypoint.", "does not override canonical policy"} {
		if !strings.Contains(implementation, expected) {
			t.Fatalf("implementer did not receive bounded guidance %q", expected)
		}
	}
	review := Compile(e, Builtins()["reviewer"], "windows", target, "review", "", "")
	if strings.Contains(review, "SUPERVISOR TASK GUIDANCE") {
		t.Fatal("reviewer received an implementer instruction")
	}
}

func TestExpiredOperatorGuidanceIsAbsentFromEntirePrompt(t *testing.T) {
	head, oldBase, newBase := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	configHash, rules := strings.Repeat("d", 64), Hash()
	target := &model.Task{ID: "ui", State: model.Ready, HeadSHA: head}
	const expired = "Do not expose this expired operator instruction."
	if err := model.QueueOperatorGuidance(target, "operator", head, oldBase, configHash, rules, expired); err != nil {
		t.Fatal(err)
	}
	prompt := Compile(config.Effective{BaseSHA: newBase, Hash: configHash}, Builtins()["implementer"], "windows", target, "work", "", "")
	if strings.Contains(prompt, expired) || strings.Contains(prompt, "AIH_GUIDANCE_V1") {
		t.Fatalf("expired durable guidance leaked into prompt: %s", prompt)
	}
}

func TestBuiltinPromptsKeepOrchestrationInSupervisor(t *testing.T) {
	builtins := Builtins()
	orchestrator := builtins["orchestrator"].Instructions
	for _, value := range []string{"exactly one lowercase value", "low, medium, or high", "Plan independent safe tasks", "only when no safe task can be planned"} {
		if !strings.Contains(orchestrator, value) {
			t.Fatalf("orchestrator prompt does not constrain risk values: %q", orchestrator)
		}
	}
	implementer := builtins["implementer"].Instructions
	for _, value := range []string{"Do not spawn", "subagents, reviewers, QA, security, or specialists", "supervisor-owned gates", "report the remaining gate and return", "blocked with an empty question", "supervisor can run canonical native checks"} {
		if !strings.Contains(implementer, value) {
			t.Fatalf("implementer prompt does not preserve supervisor ownership: %q", implementer)
		}
	}
}

func TestReviewPromptsRequirePeerIndependenceAndSupervisorEvidenceRouting(t *testing.T) {
	prompt := Compile(config.Effective{Files: map[string]string{}}, Builtins()["qa"], "windows", &model.Task{ID: "task", Areas: []string{"src"}}, "review", "diff", `{"reviews":{}}`)
	for _, value := range []string{"Peer reviews run concurrently and independently", "no peer approvals", "Do not wait for, require", "exact-head native evidence", "without asking a human to run tools", "missing Node, npm, Bun", "not a finding", "consequential human decision", "structured finding"} {
		if !strings.Contains(prompt, value) {
			t.Fatalf("review prompt missing %q: %s", value, prompt)
		}
	}
}

func TestImplementerPromptHandsIndependentReviewAcceptanceToSupervisor(t *testing.T) {
	task := &model.Task{Acceptance: []string{"An independent reviewer approves the implementation."}}
	prompt := Compile(config.Effective{Files: map[string]string{}}, Builtins()["implementer"], "linux", task, "implement the change", "", "")
	for _, value := range []string{"An independent reviewer approves the implementation.", "supervisor-owned gates", "report the remaining gate and return"} {
		if !strings.Contains(prompt, value) {
			t.Fatalf("implementer prompt did not separate independent review acceptance: missing %q", value)
		}
	}
}

func TestOrchestratorReceivesExactCustomRoleNames(t *testing.T) {
	e := config.Effective{Files: map[string]string{
		".aih/roles/animation.yaml": "name: animation-architecture\nextends: reviewer\n",
	}}
	prompt := Compile(e, Builtins()["orchestrator"], "linux", nil, "plan", "", "")
	for _, value := range []string{"AVAILABLE CUSTOM TASK ROLES", "animation-architecture", "at most 32 characters", "Do not invent role labels"} {
		if !strings.Contains(prompt, value) {
			t.Fatalf("orchestrator prompt missing %q: %s", value, prompt)
		}
	}
	if strings.Contains(prompt, "AVAILABLE CUSTOM TASK ROLES\nreviewer") {
		t.Fatal("builtin review roles were presented as custom task roles")
	}
}

func TestNextWorkerReceivesRecoveredHandoff(t *testing.T) {
	task := &model.Task{
		Summary:       "Recovered worker timeout handoff. Changed files: internal/engine/workflow.go.",
		ReportedTests: []string{"go test ./internal/engine"},
		Risks:         []string{"Verify the recovered checkpoint."},
		Decisions:     []string{"Checkpoint: recovered timeout handoff"},
	}
	prompt := Compile(config.Effective{Files: map[string]string{}}, Builtins()["implementer"], "linux", task, "continue", "", "")
	for _, value := range []string{"Recovered worker timeout handoff", "go test ./internal/engine", "Verify the recovered checkpoint", "Checkpoint: recovered timeout handoff"} {
		if !strings.Contains(prompt, value) {
			t.Fatalf("next worker prompt missing recovered evidence %q", value)
		}
	}
}
