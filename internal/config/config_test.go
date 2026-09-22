package config

import (
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
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
