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
