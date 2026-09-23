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
