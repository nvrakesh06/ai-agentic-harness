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
	if e != nil || len(required) != 3 {
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

func TestBuiltinPromptsKeepOrchestrationInSupervisor(t *testing.T) {
	builtins := Builtins()
	orchestrator := builtins["orchestrator"].Instructions
	for _, value := range []string{"exactly one lowercase value", "low, medium, or high"} {
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
