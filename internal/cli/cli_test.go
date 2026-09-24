package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/buildinfo"
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

func TestGuideOperatorFlagsAndSizeRejectBeforeProjectOpen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "guidance.txt")
	if err := os.WriteFile(file, []byte("use the bounded contract"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"guide", "task", "--operator", "--from", "source", "--file", file}, "exactly one of --from or --operator"},
		{[]string{"guide", "task", "--file", file}, "exactly one of --from or --operator"},
	} {
		cmd := New()
		cmd.SetArgs(tc.args)
		if err := cmd.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: %v", tc.args, err)
		}
	}
	if err := os.WriteFile(file, []byte(strings.Repeat("x", model.MaxGuidanceBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := New()
	cmd.SetArgs([]string{"guide", "task", "--operator", "--file", file})
	if err := cmd.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "1..1600 UTF-8 bytes") {
		t.Fatalf("oversize guidance error = %v", err)
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

func TestStatusShowsResolvedModelInHumanAndJSONOutput(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := model.NewSnapshot("model-project")
	snapshot.Runs = []model.Run{{ID: "run", Role: "reviewer", Capability: "strong", EffectiveModel: "provider-specific-model", Outcome: "completed"}}
	if err = db.Save(strings.Repeat("a", 40), snapshot); err != nil {
		t.Fatal(err)
	}
	p := &engine.Project{DB: db, Dir: dir}
	var human, machine bytes.Buffer
	cmd := New()
	cmd.SetOut(&human)
	if err = showStatus(cmd, p, false, false); err != nil {
		t.Fatal(err)
	}
	cmd.SetOut(&machine)
	if err = showStatus(cmd, p, false, true); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{human.String(), machine.String()} {
		if !strings.Contains(out, "capability=strong") && !strings.Contains(out, `"capability": "strong"`) || !strings.Contains(out, "provider-specific-model") {
			t.Fatalf("status omitted model evidence: %s", out)
		}
	}
}

func TestStatusBoundsHistoricalWorkerRunsButKeepsActive(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := model.NewSnapshot("bounded-runs")
	for i := 0; i < 12; i++ {
		snapshot.Runs = append(snapshot.Runs, model.Run{ID: fmt.Sprintf("old-%02d", i), Role: "reviewer", Outcome: "completed"})
	}
	snapshot.Runs[0].Outcome = "running"
	if err = db.Save(strings.Repeat("a", 40), snapshot); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := New()
	cmd.SetOut(&out)
	if err = showStatus(cmd, &engine.Project{DB: db, Dir: dir}, false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "active plus latest 10 of 12") || !strings.Contains(out.String(), "old-00") || strings.Contains(out.String(), "old-01") {
		t.Fatalf("historical runs were not bounded correctly: %s", out.String())
	}
}

func TestStatusShowsReviewCoordinationAndEvidenceRefresh(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := model.NewSnapshot("review-project")
	snapshot.Tasks["task"] = &model.Task{ID: "task", Title: "review", State: model.Review}
	if err = db.Save("0123456789abcdef", snapshot); err != nil {
		t.Fatal(err)
	}
	if err = db.Event("task", "run", "qa", "codex", "review_evidence_refresh_requested", "roles=qa head=abc"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := New()
	cmd.SetOut(&out)
	if err = showStatus(cmd, &engine.Project{DB: db, Dir: dir}, false, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"independent peer roles", "Review lifecycle", "review_evidence_refresh_requested", "roles=qa"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("review status missing %q: %s", want, out.String())
		}
	}
}

func TestStatusDistinguishesLocalAndDurableLeaseHeartbeats(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	snapshot := model.NewSnapshot("project123")
	snapshot.Controller = model.Lease{Machine: "machine-a", Heartbeat: now.Add(-time.Minute), Expires: now.Add(time.Minute)}
	if err = db.Save(strings.Repeat("a", 40), snapshot); err != nil {
		t.Fatal(err)
	}
	if err = db.Set(engine.LocalLeaseHeartbeatKey, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	lock, err := platform.Acquire(filepath.Join(dir, "supervisor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	var out bytes.Buffer
	cmd := New()
	cmd.SetOut(&out)
	if err = showStatus(cmd, &engine.Project{Dir: dir, DB: db}, false, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"durable heartbeat 2026-09-23T11:59:00Z", "local heartbeat 2026-09-23T12:00:00Z", "durable renewals are coalesced"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status omitted %q: %s", want, out.String())
		}
	}
}

func TestStatusShowsActiveBuildAndInvokingBinaryMismatch(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Save(strings.Repeat("a", 40), model.NewSnapshot("project123")); err != nil {
		t.Fatal(err)
	}
	invoked := buildinfo.Current()
	other := buildinfo.Identity{Version: invoked.Version, StateSchema: invoked.StateSchema + 1, Commit: strings.Repeat("f", 40)}
	if other.Commit == invoked.Commit {
		other.Commit = strings.Repeat("e", 40)
	}
	recorded, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Set(engine.LocalSupervisorBuildKey, string(recorded)); err != nil {
		t.Fatal(err)
	}
	lock, err := platform.Acquire(filepath.Join(dir, "supervisor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	cmd := New()
	var human bytes.Buffer
	cmd.SetOut(&human)
	if err = showStatus(cmd, &engine.Project{Dir: dir, DB: db}, false, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Active supervisor build: " + other.Label(), "Invoked binary build:", "Build mismatch:", "controlled handoff"} {
		if !strings.Contains(human.String(), want) {
			t.Fatalf("human status omitted %q: %s", want, human.String())
		}
	}
	var machine bytes.Buffer
	cmd.SetOut(&machine)
	if err = showStatus(cmd, &engine.Project{Dir: dir, DB: db}, false, true); err != nil {
		t.Fatal(err)
	}
	var result struct {
		ActiveSupervisorBuild *buildinfo.Identity `json:"active_supervisor_build"`
		InvokedBinaryBuild    buildinfo.Identity  `json:"invoked_binary_build"`
		BuildsDiffer          bool                `json:"builds_differ"`
	}
	if err = json.Unmarshal(machine.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ActiveSupervisorBuild == nil || result.ActiveSupervisorBuild.Commit != other.Commit || result.InvokedBinaryBuild != invoked || !result.BuildsDiffer {
		t.Fatalf("wrong machine build status: %+v", result)
	}
}

func TestStatusShowsMachineReadableCapacityAndHumanReason(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot := model.NewSnapshot("capacity-project")
	snapshot.Capacity = model.Capacity{ActiveWriters: 1, TargetWriters: 2, MaxWriters: 3, ActiveReaders: 2, MaxReaders: 4, GraceSeconds: 30, BacklogSource: "queued_objectives", State: "underutilized", ReasonCode: "dependencies", Reason: "3 tasks wait on unfinished dependencies", NextSafeWork: "task-b after task-a"}
	snapshot.Capacity.MaxHeavyChecks = 1
	snapshot.Capacity.MaxLightChecks = 2
	snapshot.Tasks["task-a"] = &model.Task{ID: "task-a", State: model.Verifying, Title: "verify release"}
	snapshot.Capacity.Verification = []model.VerificationCheck{{Task: "task-a", Check: "release", Class: "heavy", Phase: "queued", QueuedAt: time.Now().Add(-time.Minute)}}
	snapshot.Capacity.ActivePreflights = 1
	snapshot.Tasks["ui"] = &model.Task{ID: "ui", Title: "UI task", State: model.Ready, Preflight: &model.Preflight{Phase: "waiting", BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Config: fmt.Sprintf("%064x", 1), Rules: fmt.Sprintf("%064x", 2), ReuseCount: 1, ReuseReason: "reused unchanged bounded FIX guidance"}}
	if err = db.Save("0123456789abcdef", snapshot); err != nil {
		t.Fatal(err)
	}
	p := &engine.Project{DB: db, Dir: dir}
	var human bytes.Buffer
	cmd := New()
	cmd.SetOut(&human)
	if err = showStatus(cmd, p, false, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Writers: 1 active / 2 target / 3 max", "Readers: 2 active / 4 max", "Checks: 0 heavy / 1 project max / 1 machine max", "WAITING_CHECK_CAPACITY", "waiting for a verification slot", "Preflights: 1 active", "PREFLIGHT_WAITING", "reused unchanged bounded FIX guidance", "dependencies", "Next safe work: task-b after task-a"} {
		if !strings.Contains(human.String(), want) {
			t.Fatalf("human status omitted %q: %s", want, human.String())
		}
	}
	var machine bytes.Buffer
	cmd.SetOut(&machine)
	if err = showStatus(cmd, p, false, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(machine.String(), `"target_active_writers": 2`) || !strings.Contains(machine.String(), `"underutilization_reason_code": "dependencies"`) || !strings.Contains(machine.String(), `"phase": "queued"`) || !strings.Contains(machine.String(), `"machine_max_heavy_checks": 1`) {
		t.Fatalf("JSON status omitted capacity fields: %s", machine.String())
	}
}

func TestSupervisorStartupWaitsForDelayedAcknowledgement(t *testing.T) {
	if startupAckTimeout < 30*time.Second {
		t.Fatal("startup deadline is too short for lease acquisition on a loaded machine")
	}
	probes := 0
	err := waitForSupervisorStart(context.Background(), 3*time.Second, time.Millisecond,
		func() bool {
			probes++
			return probes > 35
		},
		func() string { return "" },
	)
	if err != nil {
		t.Fatal("delayed supervisor acknowledgement was reported as failure:", err)
	}
	if probes <= 35 {
		t.Fatalf("startup returned before acknowledgement after %d probes", probes)
	}
}

func TestSupervisorStartupReportsChildFailure(t *testing.T) {
	probes := 0
	err := waitForSupervisorStart(context.Background(), 3*time.Second, time.Millisecond,
		func() bool { return false },
		func() string {
			probes++
			if probes >= 3 {
				return "lease refused"
			}
			return ""
		},
	)
	if err == nil || !strings.Contains(err.Error(), "lease refused") {
		t.Fatalf("child failure was not reported: %v", err)
	}
}

func TestSupervisorStartupReportsDelayedAcknowledgement(t *testing.T) {
	err := waitForSupervisorStart(context.Background(), 20*time.Millisecond, time.Millisecond,
		func() bool { return false },
		func() string { return "" },
	)
	if !errors.Is(err, errStartupAckTimeout) {
		t.Fatalf("startup timeout was not distinguishable from a child failure: %v", err)
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
