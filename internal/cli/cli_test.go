package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

func init() {
	if os.Getenv("AIH_DOCTOR_HELPER") != "1" {
		return
	}
	args := strings.Join(os.Args[1:], " ")
	if strings.Contains(args, "--help") {
		fmt.Print("--output-schema --output-last-message --sandbox --json-schema --output-format --permission-mode --safe-mode")
		os.Exit(0)
	}
	if os.Getenv("AIH_DOCTOR_AUTH_FAIL") == "1" {
		fmt.Print("sensitive-auth-output")
		os.Exit(1)
	}
	os.Exit(0)
}

func TestDoctorAuthenticationAndStorageWithoutNetwork(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := platform.CopyFile(filepath.Join(bin, name), exe, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_BINARY", exe)
	t.Setenv("AIH_DOCTOR_HELPER", "1")
	for _, failure := range []string{"0", "1"} {
		t.Setenv("AIH_DOCTOR_AUTH_FAIL", failure)
		var out bytes.Buffer
		cmd := New()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		state := t.TempDir()
		cmd.SetArgs([]string{"doctor", "--machine", "--repo", t.TempDir(), "--home", state})
		err := cmd.ExecuteContext(context.Background())
		if (err != nil) != (failure == "1") {
			t.Fatal("wrong readiness status", err, out.String())
		}
		if !strings.Contains(out.String(), "SQLite WAL available") || strings.Contains(out.String(), "sensitive-auth-output") {
			t.Fatal(out.String())
		}
		files, _ := os.ReadDir(state)
		if len(files) != 0 {
			t.Fatal("doctor left disposable database files", files)
		}
	}
}

func TestInspectionWithoutProject(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"version"}, "AIH 1.0.0"}, {[]string{"rules", "--core"}, "Understand before changing"}, {[]string{"--help"}, "handoff"}, {[]string{"roles", "assign", "--help"}, "task-id"}} {
		var b bytes.Buffer
		c := New()
		c.SetOut(&b)
		c.SetErr(&b)
		c.SetArgs(tc.args)
		if e := c.ExecuteContext(context.Background()); e != nil {
			t.Fatal(tc.args, e)
		}
		if !strings.Contains(b.String(), tc.want) {
			t.Fatal(tc.args, b.String())
		}
	}
}

func TestStatusShowsVerificationRetryRoute(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := model.NewSnapshot("project123")
	snapshot.Tasks["task"] = &model.Task{ID: "task", Title: "verify", State: model.SyncRequired, Verification: &model.Verification{Environment: "windows/native/check", HeadSHA: strings.Repeat("c", 40), Fingerprint: strings.Repeat("b", 64), Attempts: 1, NativeOnly: true}}
	if err = db.Save(strings.Repeat("a", 40), snapshot); err != nil {
		t.Fatal(err)
	}
	p := &engine.Project{Dir: dir, DB: db}
	var out bytes.Buffer
	cmd := New()
	cmd.SetOut(&out)
	if err = showStatus(cmd, p, false, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"supervisor-native verification only", "windows/native/check", "attempt 1"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status omitted %q: %s", want, out.String())
		}
	}
}

func TestStatusShowsWorkerDeadlineLifecycle(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := model.NewSnapshot("deadline-project")
	if err = db.Save("0123456789abcdef", snapshot); err != nil {
		t.Fatal(err)
	}
	if err = db.Event("task", "run", "implementer", "codex", "worker_checkpoint_requested", "soft deadline reached"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := New()
	cmd.SetOut(&out)
	if err = showStatus(cmd, &engine.Project{DB: db, Dir: dir}, false, false); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"Worker deadline", "task", "worker_checkpoint_requested", "soft deadline reached"} {
		if !strings.Contains(out.String(), value) {
			t.Fatalf("status missing %q: %s", value, out.String())
		}
	}
}

func TestDoctorPolicyDriftIgnoresAdditionalContext(t *testing.T) {
	local := config.Effective{Files: map[string]string{".aih/project.yaml": "provider: codex\r\nbase_branch: main\r\n"}}
	canonical := config.Effective{Files: map[string]string{".aih/project.yaml": "provider: codex\nbase_branch: main", "AGENTS.md": "Project instructions"}}
	if localPolicyDiffers(local, canonical) {
		t.Fatal("line endings and additional canonical context are not policy drift")
	}
	canonical.Files[".aih/project.yaml"] = "provider: claude-code"
	if !localPolicyDiffers(local, canonical) {
		t.Fatal("provider drift was missed")
	}
}

func TestDoctorRejectsNonExecutableCheck(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "check"), 0700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	d := &diagnostics{out: &out}
	checkExecutables(d, config.Effective{Project: config.Project{Checks: []config.Check{{Name: "build", Command: []string{"./check"}}}}}, dir)
	if !d.failed {
		t.Fatal("directory accepted as a check executable")
	}
}
