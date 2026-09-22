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
	for _, value := range []string{"Do not spawn", "subagents or reviewers", "supervisor schedules independent roles", "blocked with an empty question", "supervisor can run canonical native checks"} {
		if !strings.Contains(implementer, value) {
			t.Fatalf("implementer prompt does not preserve supervisor ownership: %q", implementer)
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
