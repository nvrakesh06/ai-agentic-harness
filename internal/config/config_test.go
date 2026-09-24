package config

import (
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func canonicalFiles() map[string]string {
	f := map[string]string{}
	for n, v := range map[string]any{".aih/project.yaml": Defaults(), ".aih/policies.yaml": DefaultPolicy(), ".aih/harness.lock": DefaultLock()} {
		b, _ := yaml.Marshal(v)
		f[n] = string(b)
	}
	return f
}

func TestModelResolutionAndMappingWarnings(t *testing.T) {
	p := Defaults()
	if got := p.ResolveModel("implementer", "normal"); got.Capability != "normal" || got.EffectiveModel != ProviderDefaultModel || got.RequestModel != "" {
		t.Fatalf("default resolution = %#v", got)
	}
	warnings := p.ModelMappingWarnings()
	if len(warnings) != 3 || !strings.Contains(strings.Join(warnings, "\n"), "implementer") || !strings.Contains(strings.Join(warnings, "\n"), "advisor") {
		t.Fatalf("default warnings = %v", warnings)
	}
	p.ProviderModels = map[string]string{"normal": "model-a", "strong": "model-b", "strongest": "model-b"}
	if warnings := p.ModelMappingWarnings(); len(warnings) != 0 {
		t.Fatalf("explicit identical mappings warned: %v", warnings)
	}
	if got := p.ResolveModel("advisor", "strongest"); got.EffectiveModel != "model-b" || got.RequestModel != "model-b" {
		t.Fatalf("explicit resolution = %#v", got)
	}
	p.ProviderModels = map[string]string{"normal": "model-a"}
	warnings = p.ModelMappingWarningsForRoles(map[string]string{"custom-review": "strong", "custom-qa": "normal"})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "custom-review") {
		t.Fatalf("custom role warning = %v", warnings)
	}
}
func TestCanonicalValidation(t *testing.T) {
	f := canonicalFiles()
	if _, e := Parse(f); e != nil {
		t.Fatal(e)
	}
	f[".aih/project.yaml"] += "unknown_option: true\n"
	if _, e := Parse(f); e == nil {
		t.Fatal("unknown setting accepted")
	}
	f = canonicalFiles()
	f[".aih/harness.lock"] = "major: 2\nminimum: 2.0.0\n"
	if _, e := Parse(f); e == nil {
		t.Fatal("future runtime accepted")
	}
	if _, e := ProjectDir(t.TempDir(), "../escape"); e == nil {
		t.Fatal("path traversal")
	}
}

func TestCheckResourcesDefaultAndValidation(t *testing.T) {
	p := Defaults()
	if p.Resources.MaxHeavyChecks != 1 || p.Resources.MaxLightChecks != 2 {
		t.Fatalf("unsafe defaults: %+v", p.Resources)
	}
	for _, resources := range []Resources{{}, {MaxHeavyChecks: 9, MaxLightChecks: 1}, {MaxHeavyChecks: 1, MaxLightChecks: 9}} {
		bad := p
		bad.Resources = resources
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted %+v", resources)
		}
	}
	p.Checks = []Check{{Name: "suite", Command: []string{"go", "test"}, Timeout: 10, Class: "unknown"}}
	if err := p.Validate(); err == nil {
		t.Fatal("accepted unknown check class")
	}
	p.Checks[0].Class = "light"
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	legacy := canonicalFiles()
	project := legacy[".aih/project.yaml"]
	start := strings.Index(project, "resources:\n")
	if start < 0 {
		t.Fatal("resources block missing")
	}
	end := strings.Index(project[start:], "\nmodels:")
	if end < 0 {
		t.Fatal("models block missing")
	}
	legacy[".aih/project.yaml"] = project[:start] + project[start+end+1:]
	e, err := Parse(legacy)
	if err != nil || e.Project.Resources.MaxHeavyChecks != 1 {
		t.Fatalf("legacy resources: %+v, %v", e.Project.Resources, err)
	}
}

func TestSchedulingPolicyDefaultsAndValidation(t *testing.T) {
	p := Defaults()
	if p.Scheduling.TargetWriters != 2 || p.Scheduling.UnderutilizationGraceSeconds != 30 || p.Scheduling.BacklogSource != "queued_objectives" {
		t.Fatalf("unexpected scheduling defaults: %#v", p.Scheduling)
	}
	for _, mutate := range []func(*Project){
		func(project *Project) { project.Scheduling.TargetWriters = project.MaxWriters + 1 },
		func(project *Project) { project.Scheduling.UnderutilizationGraceSeconds = -1 },
		func(project *Project) { project.Scheduling.BacklogSource = "invented_scope" },
	} {
		invalid := Defaults()
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid scheduling policy accepted: %#v", invalid.Scheduling)
		}
	}
}

func TestLegacyProjectWithoutSchedulingUsesSafeDefaults(t *testing.T) {
	files := canonicalFiles()
	legacy := files[".aih/project.yaml"]
	index := strings.Index(legacy, "scheduling:\n")
	if index < 0 {
		t.Fatal("fixture has no scheduling block")
	}
	files[".aih/project.yaml"] = legacy[:index]
	effective, err := Parse(files)
	if err != nil || effective.Project.Scheduling.TargetWriters != 2 || effective.Project.Scheduling.BacklogSource != "queued_objectives" {
		t.Fatal(effective.Project.Scheduling, err)
	}
	legacyProject := effective.Project
	legacyProject.MaxWriters = 1
	legacyProject.Scheduling = Scheduling{}
	legacyYAML, _ := yaml.Marshal(legacyProject)
	index = strings.Index(string(legacyYAML), "scheduling:\n")
	legacyYAML = legacyYAML[:index]
	decoded := Defaults()
	if err = DecodeProject(legacyYAML, &decoded); err != nil || decoded.Scheduling.TargetWriters != 1 {
		t.Fatal(decoded.Scheduling, err)
	}
}
func TestMachineAndNativeChecks(t *testing.T) {
	dir := t.TempDir()
	a, e := Install(dir)
	if e != nil {
		t.Fatal(e)
	}
	b, e := Install(dir)
	if e != nil || a.ID != b.ID {
		t.Fatal("machine not stable", e)
	}
	if e = os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"test":"node test.js","lint":"lint"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	if c := DetectChecks(dir); len(c) != 2 || c[0].Command[0] != "npm" {
		t.Fatal(c)
	}
	for _, r := range []string{"https://user:password@github.com/a/b", "https://evil.test/a/b", "https://github.com/a"} {
		if _, e = GitHubRepo(r); e == nil {
			t.Fatal("unsafe remote", r)
		}
	}
}

func TestVisualCaptureRequiresServerAdapterCommand(t *testing.T) {
	p := Defaults()
	p.VisualCapture = &VisualCapture{Server: []string{"node", "scripts/visual-server.mjs"}, Timeout: 30}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid server adapter rejected: %v", err)
	}
	for _, command := range [][]string{nil, {}, {""}} {
		p.VisualCapture.Server = command
		if err := p.Validate(); err == nil {
			t.Fatalf("empty server adapter accepted: %#v", command)
		}
	}
}

func TestVisualCaptureTargetValidationAndLegacyDefault(t *testing.T) {
	p := Defaults()
	p.VisualCapture = &VisualCapture{Server: []string{"node", "scripts/visual-server.mjs"}, Timeout: 30}
	if targets := p.VisualCapture.CaptureTargets(); len(targets) != 1 || targets[0] != (VisualCaptureTarget{ID: "desktop", Path: "/", Width: 1280, Height: 720}) {
		t.Fatalf("legacy target = %#v", targets)
	}
	p.VisualCapture.Targets = []VisualCaptureTarget{{ID: "desktop", Path: "/", Width: 1280, Height: 720}, {ID: "settings", Path: "/settings", Width: 640, Height: 480}}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid visual targets rejected: %v", err)
	}
	for _, targets := range [][]VisualCaptureTarget{
		{{ID: "desktop", Path: "/", Width: 1280, Height: 720}, {ID: "desktop", Path: "/other", Width: 640, Height: 480}},
		{{ID: "healthy", Path: "/", Width: 1280, Height: 720}, {ID: "Healthy", Path: "/other", Width: 640, Height: 480}},
		{{ID: "../../escape", Path: "/", Width: 1280, Height: 720}},
		{{ID: "CON", Path: "/", Width: 1280, Height: 720}},
		{{ID: "lPt9", Path: "/", Width: 1280, Height: 720}},
		{{ID: "external", Path: "https://outside.invalid/", Width: 1280, Height: 720}},
		{{ID: "host", Path: "//outside.invalid/", Width: 1280, Height: 720}},
		{{ID: "query", Path: "/settings?debug=1", Width: 1280, Height: 720}},
		{{ID: "fragment", Path: "/settings#advanced", Width: 1280, Height: 720}},
		{{ID: "large", Path: "/", Width: 4096, Height: 4096}},
	} {
		p.VisualCapture.Targets = targets
		if err := p.Validate(); err == nil {
			t.Fatalf("unsafe targets accepted: %#v", targets)
		}
	}
	p.VisualCapture.Targets = make([]VisualCaptureTarget, maxVisualCaptureTargets+1)
	for i := range p.VisualCapture.Targets {
		p.VisualCapture.Targets[i] = VisualCaptureTarget{ID: "target" + string(rune('a'+i)), Path: "/", Width: 1280, Height: 720}
	}
	if err := p.Validate(); err == nil {
		t.Fatal("too many visual targets accepted")
	}
}

func TestParseLocalIncludesCustomRoles(t *testing.T) {
	root := t.TempDir()
	files := canonicalFiles()
	if err := os.MkdirAll(filepath.Join(root, ".aih", "roles"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".aih", "roles", "custom.yaml"), []byte("name: custom-review\nextends: reviewer\ncapability: strongest\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Files[".aih/roles/custom.yaml"]; !ok {
		t.Fatal("custom role was omitted from local configuration")
	}
}
