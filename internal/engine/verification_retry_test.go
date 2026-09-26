package engine_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureSupervisorDrainTimeout = 2*time.Minute + 5*time.Second

var errFixtureSupervisorUndrained = errors.New("fixture supervisor did not drain")

// fixtureSupervisor owns the child context started by a real-Git fixture. A
// fixture must drain it before deferred SQLite cleanup can run.
type fixtureSupervisor struct {
	cancel         context.CancelFunc
	done           <-chan error
	drainTimeout   time.Duration
	watchdog       func(string)
	completionOnce sync.Once
	completed      chan struct{}
	resultMu       sync.Mutex
	result         error
}

func newFixtureSupervisor(parent context.Context, project *engine.Project) *fixtureSupervisor {
	return newFixtureSupervisorForController(parent, engine.New(project))
}

// newFixtureSupervisorForController starts a preconfigured controller while
// keeping the ordinary fixture path above unchanged. Tests use it only for
// narrow controller synchronization seams configured before Serve.
func newFixtureSupervisorForController(parent context.Context, controller *engine.Controller) *fixtureSupervisor {
	child, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- controller.Serve(child) }()
	return &fixtureSupervisor{
		cancel:       cancel,
		done:         done,
		drainTimeout: fixtureSupervisorDrainTimeout,
		watchdog: func(diagnostic string) {
			// Do not touch SQLite here: its owner did not drain. Let process exit
			// release handles rather than returning into a deferred DB.Close race.
			fmt.Fprintln(os.Stderr, "fixture supervisor drain watchdog:", diagnostic)
			os.Exit(1)
		},
	}
}

func TestFixtureSupervisorDrainCancelsAndReturnsChildResult(t *testing.T) {
	done := make(chan error, 1)
	done <- errors.New("child stopped")
	cancelled := false
	supervisor := &fixtureSupervisor{
		cancel:       func() { cancelled = true },
		done:         done,
		drainTimeout: time.Second,
		watchdog:     func(string) { t.Fatal("watchdog ran after child exit") },
	}
	if err := supervisor.drain("status=fixture"); err == nil || err.Error() != "child stopped" {
		t.Fatalf("drain error = %v", err)
	}
	if !cancelled {
		t.Fatal("fixture drain did not cancel its child context")
	}
}

func TestFixtureSupervisorDrainWatchdogIsInjectable(t *testing.T) {
	done := make(chan error)
	var diagnostic string
	supervisor := &fixtureSupervisor{
		cancel:       func() {},
		done:         done,
		drainTimeout: time.Millisecond,
		watchdog:     func(value string) { diagnostic = value },
	}
	if err := supervisor.drain("status=bounded-safe-fields"); !errors.Is(err, errFixtureSupervisorUndrained) {
		t.Fatalf("undrained fixture error = %v", err)
	}
	if diagnostic != "status=bounded-safe-fields" {
		t.Fatalf("watchdog diagnostic = %q", diagnostic)
	}
}

func TestFixtureSupervisorWaitHandoffDoesNotCancelHealthyChild(t *testing.T) {
	done := make(chan error, 1)
	done <- nil
	cancelled := false
	supervisor := &fixtureSupervisor{
		cancel:       func() { cancelled = true },
		done:         done,
		drainTimeout: time.Second,
		watchdog:     func(string) { t.Fatal("watchdog ran after healthy handoff") },
	}
	if err := supervisor.waitHandoff("status=fixture"); err != nil {
		t.Fatal(err)
	}
	if cancelled {
		t.Fatal("healthy handoff cancelled its child before completion")
	}
}

func TestFixtureSupervisorHandoffTimeoutCancelsBeforeWatchdog(t *testing.T) {
	done := make(chan error)
	cancelled := false
	var diagnostic string
	supervisor := &fixtureSupervisor{
		cancel:       func() { cancelled = true },
		done:         done,
		drainTimeout: time.Millisecond,
		watchdog:     func(value string) { diagnostic = value },
	}
	if err := supervisor.waitHandoff("status=bounded-safe-fields"); !errors.Is(err, errFixtureSupervisorUndrained) {
		t.Fatalf("handoff timeout error = %v", err)
	}
	if !cancelled || diagnostic != "status=bounded-safe-fields" {
		t.Fatalf("handoff timeout cancellation=%t watchdog=%q", cancelled, diagnostic)
	}
}

func TestFixtureSupervisorCachesNonNilChildResultAcrossCleanup(t *testing.T) {
	done := make(chan error, 1)
	done <- errors.New("child failed")
	cancelled := false
	supervisor := &fixtureSupervisor{
		cancel:       func() { cancelled = true },
		done:         done,
		drainTimeout: time.Second,
		watchdog:     func(string) { t.Fatal("watchdog ran after child exit") },
	}
	if err := supervisor.waitHandoff("status=fixture"); err == nil || err.Error() != "child failed" {
		t.Fatalf("handoff error = %v", err)
	}
	if err := supervisor.drain("status=fixture"); err == nil || err.Error() != "child failed" {
		t.Fatalf("cached cleanup error = %v", err)
	}
	if !cancelled {
		t.Fatal("cleanup did not cancel after failed handoff")
	}
}

// drain cancels the child on failure cleanup, then waits through the
// controller's bounded shutdown allowance. The watchdog is process-level by
// design: returning would allow a caller's deferred DB.Close to race Serve.
func (s *fixtureSupervisor) drain(diagnostic string) error {
	if s == nil {
		return nil
	}
	s.cancel()
	return s.waitAfterCancel(diagnostic)
}

func (s *fixtureSupervisor) completion() <-chan struct{} {
	s.completionOnce.Do(func() {
		s.completed = make(chan struct{})
		go func() {
			err := <-s.done
			s.resultMu.Lock()
			s.result = err
			s.resultMu.Unlock()
			close(s.completed)
		}()
	})
	return s.completed
}

func (s *fixtureSupervisor) completedResult() error {
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	return s.result
}

// waitHandoff preserves a queued handoff's cooperative shutdown path. If that
// bounded phase does not finish, cancellation starts the same drain path used
// for failures before the watchdog can terminate the fixture process.
func (s *fixtureSupervisor) waitHandoff(diagnostic string) error {
	if s == nil {
		return nil
	}
	timer := time.NewTimer(s.drainTimeout)
	defer timer.Stop()
	select {
	case <-s.completion():
		return s.completedResult()
	case <-timer.C:
		s.cancel()
		return s.waitAfterCancel(diagnostic)
	}
}

func (s *fixtureSupervisor) waitAfterCancel(diagnostic string) error {
	timer := time.NewTimer(s.drainTimeout)
	defer timer.Stop()
	select {
	case <-s.completion():
		return s.completedResult()
	case <-timer.C:
		s.watchdog(diagnostic)
		return errFixtureSupervisorUndrained
	}
}

func init() {
	if len(os.Args) > 1 && os.Args[1] == "_aih-native-source-failure" {
		fmt.Fprintln(os.Stderr, "src/studio-server/http.ts(291,69): TS2740: cannot use Duplex as Socket")
		fmt.Fprintln(os.Stderr, "token=sk-abcdefghijklmnopqrstuvwxyz012345")
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "_aih-native-windows-npm-lock" {
		fmt.Fprintln(os.Stderr, "npm ERR! code EPERM")
		fmt.Fprintln(os.Stderr, "npm ERR! syscall unlink")
		fmt.Fprintln(os.Stderr, "npm ERR! path C:\\fixture\\node_modules\\rollup.win32-x64-msvc.node")
		fmt.Fprintln(os.Stderr, "npm ERR! EPERM: operation not permitted, unlink 'node_modules/rollup.win32-x64-msvc.node'")
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "_aih-native-timeout" {
		time.Sleep(2 * time.Second)
		os.Exit(0)
	}
	if len(os.Args) > 2 && os.Args[1] == "_aih-native-timeout-once" {
		if _, err := os.Stat(os.Args[2]); os.IsNotExist(err) {
			if err := os.WriteFile(os.Args[2], []byte("first"), 0600); err != nil {
				os.Exit(2)
			}
			time.Sleep(2 * time.Second)
		}
		os.Exit(0)
	}
	if len(os.Args) > 2 && os.Args[1] == "_aih-native-timeout-then-npm-lock" {
		if _, err := os.Stat(os.Args[2]); os.IsNotExist(err) {
			if err := os.WriteFile(os.Args[2], []byte("first"), 0600); err != nil {
				os.Exit(2)
			}
			time.Sleep(2 * time.Second)
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "npm ERR! code EPERM")
		fmt.Fprintln(os.Stderr, "npm ERR! syscall unlink")
		fmt.Fprintln(os.Stderr, "npm ERR! path C:\\fixture\\node_modules\\rollup.win32-x64-msvc.node")
		os.Exit(1)
	}
	if len(os.Args) > 2 && os.Args[1] == "_aih-native-npm-lock-then-source" {
		if _, err := os.Stat(os.Args[2]); os.IsNotExist(err) {
			if err := os.WriteFile(os.Args[2], []byte("first"), 0600); err != nil {
				os.Exit(2)
			}
			fmt.Fprintln(os.Stderr, "npm ERR! code EPERM")
			fmt.Fprintln(os.Stderr, "npm ERR! syscall unlink")
			fmt.Fprintln(os.Stderr, "npm ERR! path C:\\fixture\\node_modules\\rollup.win32-x64-msvc.node")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "src/studio-server/http.ts(291,69): TS2740: cannot use Duplex as Socket")
		os.Exit(1)
	}
}

func setFixtureCheck(t *testing.T, ctx context.Context, f *demo.Fixture, command []string, timeout int) {
	t.Helper()
	setFixtureChecks(t, ctx, f, []config.Check{{Name: "fixture acceptance", Command: command, Timeout: timeout}})
}

func setFixtureChecks(t *testing.T, ctx context.Context, f *demo.Fixture, checks []config.Check) {
	t.Helper()
	f.Project.Checks = checks
	encoded, err := yaml.Marshal(f.Project)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.Source, ".aih", "project.yaml"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	source := gitx.Git{Dir: f.Source}
	if _, err = source.Run(ctx, "", "add", ".aih/project.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Run(ctx, "", "commit", "-m", "Configure fixture check"); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Run(ctx, "", "push", "origin", "HEAD:main"); err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
}

func seedReadyTask(t *testing.T, ctx context.Context, f *demo.Fixture, title string) {
	t.Helper()
	snapshot, head, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Objectives["objective"] = &model.Objective{ID: "objective", Text: "verification routing", Planned: true}
	snapshot.Tasks[title] = &model.Task{
		ID: title, ObjectiveID: "objective", Issue: 1, Title: title,
		Objective: "Create the fixture and verify it", Acceptance: []string{"fixture exists"},
		// demo.Worker writes feature-<title>.txt. This remains an unstarted
		// fixture so normal canonical hydration classifies the explicit file.
		Areas: []string{"feature-" + title + ".txt"}, Domains: []string{title}, Risk: "low", State: model.Ready,
		Branch: "aih/" + title, FixCycles: map[string]int{},
	}
	next, err := f.P.Git.StateCommit(ctx, head, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: head, New: next}}); err != nil {
		t.Fatal(err)
	}
}

func runUntilTaskState(t *testing.T, ctx context.Context, f *demo.Fixture, id string, wanted ...model.State) *model.Task {
	t.Helper()
	supervisor := newFixtureSupervisor(ctx, f.P)
	drained := false
	defer func() {
		if !drained {
			if err := supervisor.drain("fixture cleanup"); err != nil {
				t.Errorf("fixture supervisor drain during cleanup: %v", err)
			}
			drained = true
		}
	}()
	wantedStates := map[model.State]bool{}
	for _, state := range wanted {
		wantedStates[state] = true
	}
	for {
		snapshot, _, err := f.P.DB.Load()
		if err == nil && snapshot.Tasks[id] != nil && wantedStates[snapshot.Tasks[id].State] {
			if err = f.P.DB.Submit(storeCommand("handoff")); err != nil {
				t.Fatal(err)
			}
			handoffErr := supervisor.waitHandoff("fixture cooperative handoff")
			drained = true
			if handoffErr != nil {
				t.Fatal(handoffErr, retryFixtureStatus(f, id))
			}
			return snapshot.Tasks[id]
		}
		select {
		case <-supervisor.completion():
			drained = true
			t.Fatal("supervisor stopped before target state", supervisor.completedResult(), retryFixtureStatus(f, id))
		case <-ctx.Done():
			stopErr := supervisor.drain("fixture workflow deadline")
			drained = true
			diagnostic := retryFixtureStatus(f, id)
			t.Fatalf("%v waiting for %s; supervisor=%v %s", ctx.Err(), strings.Join(func() []string {
				states := make([]string, 0, len(wanted))
				for _, state := range wanted {
					states = append(states, string(state))
				}
				return states
			}(), ","), stopErr, diagnostic)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func storeCommand(kind string) store.Command {
	return store.Command{ID: model.ID(), Kind: kind}
}

func eventCount(t *testing.T, f *demo.Fixture, kind string) int {
	t.Helper()
	var count int
	if err := f.P.DB.DB.QueryRow("SELECT COUNT(*) FROM events WHERE kind=?", kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestEnvironmentOnlyImplementerBlockRoutesToNativeVerification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.Provider.EnvironmentBlocks = map[string]int{"environment": 1}
	seedReadyTask(t, ctx, f, "environment")
	task := runUntilTaskState(t, ctx, f, "environment", model.Review, model.MergeReady, model.Done)
	if got := f.Provider.ImplementationCount("environment"); got != 1 {
		t.Fatalf("implementer ran %d times, want one native-verification reroute", got)
	}
	if task.Verification != nil {
		t.Fatal("successful native verification left a retry guard")
	}
	if got := eventCount(t, f, "verification_rerouted"); got != 1 {
		t.Fatalf("verification reroute events = %d, want 1", got)
	}
}

func TestMissingNativeCapabilityBlocksWithoutImplementerRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"aih-command-that-does-not-exist"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	seedReadyTask(t, ctx, f, "missing-tool")
	task := runUntilTaskState(t, ctx, f, "missing-tool", model.Blocked)
	if got := f.Provider.ImplementationCount("missing-tool"); got != 1 {
		t.Fatalf("implementer ran %d times after a missing native tool", got)
	}
	if task.AdvisorUsed || len(task.FixCycles) != 0 || task.Blocker == nil || task.Blocker.Origin != model.BlockerOriginVerificationOnly || task.Blocker.Resume != model.SyncRequired || task.Verification == nil || !task.Verification.NativeOnly {
		t.Fatalf("missing capability was not durably routed to native verification: %+v", task)
	}
	if got := eventCount(t, f, "retry_suppressed"); got != 1 {
		t.Fatalf("suppressed retry events = %d, want 1", got)
	}
	if err = f.P.DB.Close(); err != nil {
		t.Fatal(err)
	}
	recoveredProject, err := f.Open(ctx, filepath.Join(f.Root, "machine-b"))
	if err != nil {
		t.Fatal(err)
	}
	defer recoveredProject.DB.Close()
	if err = recoveredProject.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := recoveredProject.DB.Load()
	if err != nil || recovered.Tasks["missing-tool"].Blocker == nil || recovered.Tasks["missing-tool"].Blocker.Origin != model.BlockerOriginVerificationOnly {
		t.Fatalf("cross-machine recovery lost verification-only blocker provenance: %#v err=%v", recovered.Tasks["missing-tool"], err)
	}
}

func TestWriterCheckpointMakesPreflightExactForMissingCapabilityContinuation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"aih-command-that-does-not-exist"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	seedReadyTask(t, ctx, f, "writer-checkpoint")
	snapshot, head, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Tasks["writer-checkpoint"].UI = true
	snapshot.Tasks["writer-checkpoint"].Areas = []string{"feature-writer-checkpoint.txt"}
	task := snapshot.Tasks["writer-checkpoint"]
	// StateCommit uses the aih-state revision as its CAS parent, but task scope
	// must be based on the code revision used to create the worktree.
	base, err := f.P.Git.RemoteHead(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	task.BaseSHA = base
	task.AssignedAreas = []string{"feature-writer-checkpoint.txt"}
	task.AssignedAreaKinds = map[string]string{"feature-writer-checkpoint.txt": model.AreaFile}
	if err = f.P.Git.Worktree(ctx, f.P.TaskPath(task), task.Branch, "refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.P.TaskPath(task), "feature-writer-checkpoint.txt"), []byte("prior checkpoint\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prior, err := f.P.Git.Checkpoint(ctx, f.P.TaskPath(task), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: task.Branch, New: prior}}); err != nil {
		t.Fatal(err)
	}
	task.HeadSHA = prior
	next, err := f.P.Git.StateCommit(ctx, head, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: head, New: next}}); err != nil {
		t.Fatal(err)
	}
	blocked := runUntilTaskState(t, ctx, f, "writer-checkpoint", model.Blocked)
	if blocked.Blocker == nil || blocked.Blocker.Origin != model.BlockerOriginVerificationOnly || blocked.Preflight == nil || blocked.Preflight.HeadSHA != blocked.HeadSHA {
		t.Fatalf("normal writer checkpoint did not preserve exact preflight identity for verification recovery: blocker=%#v task=%#v", blocked.Blocker, blocked)
	}
	if err = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "answer", Target: "writer-checkpoint", Payload: "AIH-CONTINUE CHECKPOINT " + blocked.HeadSHA}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(ctx) }()
	var resumed *model.Task
	deadline := time.NewTimer(75 * time.Second)
	defer deadline.Stop()
	for resumed == nil {
		current, _, loadErr := f.P.DB.Load()
		if loadErr == nil {
			task := current.Tasks["writer-checkpoint"]
			if task != nil && f.Provider.ImplementationCount("writer-checkpoint") == 2 && task.State == model.Blocked {
				resumed = task
			}
		}
		select {
		case serveErr := <-done:
			t.Fatalf("supervisor stopped before exact checkpoint writer admission: %v", serveErr)
		case <-deadline.C:
			t.Fatal("exact checkpoint continuation did not reach its retained-guidance writer pass")
		case <-time.After(25 * time.Millisecond):
		}
	}
	if got := f.Provider.ImplementationCount("writer-checkpoint"); got != 2 {
		t.Fatalf("exact checkpoint continuation ran implementer %d times, want the initial writer plus one retained-guidance pass", got)
	}
	if resumed.Preflight == nil || resumed.Preflight.ReuseCount != 1 || !strings.Contains(resumed.Preflight.ReuseReason, "human checkpoint") {
		t.Fatalf("normal writer checkpoint lost retained guidance after exact verification unblock: %#v", resumed.Preflight)
	}
	if err = f.P.DB.Submit(storeCommand("handoff")); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNativeOnlySourceFailureRoutesToFixAndPersistsRedactedEvidence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := demo.New(ctx, t.TempDir(), []string{exe, "_aih-native-source-failure"})
	if err != nil {
		t.Fatal(err)
	}
	f.Provider.EnvironmentBlocks = map[string]int{"native-source": 1}
	seedReadyTask(t, ctx, f, "native-source")
	task := runUntilTaskState(t, ctx, f, "native-source", model.Blocked)
	if got := f.Provider.ImplementationCount("native-source"); got != 2 {
		t.Fatalf("implementer ran %d times, want initial implementation plus one native source-failure fix", got)
	}
	if task.Blocker == nil || task.Blocker.Resume != model.Fix || task.Verification == nil || !task.Verification.NativeOnly || task.Verification.Attempts != 2 {
		t.Fatalf("native source failure did not become a bounded FIX recovery: %+v", task)
	}
	if !strings.Contains(task.Blocker.Reason, "fixture acceptance failed") || !strings.Contains(task.Blocker.Reason, "TS2740") || strings.Contains(task.Blocker.Reason, "sk-abcdefghijklmnopqrstuvwxyz012345") {
		t.Fatalf("native source evidence was not named and redacted: %q", task.Blocker.Reason)
	}
	pulls, _ := f.Hub.Pulls()
	if len(pulls) != 1 || !strings.Contains(pulls[0].Body, "fixture acceptance failed") || strings.Contains(pulls[0].Body, "sk-abcdefghijklmnopqrstuvwxyz012345") {
		t.Fatalf("draft PR did not preserve bounded redacted check evidence: %#v", pulls)
	}
	if err = f.P.DB.Close(); err != nil {
		t.Fatal(err)
	}
	recoveredProject, err := f.Open(ctx, filepath.Join(f.Root, "machine-b"))
	if err != nil {
		t.Fatal(err)
	}
	defer recoveredProject.DB.Close()
	if err = recoveredProject.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := recoveredProject.DB.Load()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Tasks["native-source"].State != model.Blocked || recovered.Tasks["native-source"].Blocker == nil || recovered.Tasks["native-source"].Blocker.Resume != model.Fix || recovered.Tasks["native-source"].Verification == nil {
		t.Fatalf("restart recovery lost bounded native source-failure state: %+v", recovered.Tasks["native-source"])
	}
}

func TestRepeatedNativeFailureAtSameHeadStopsEquivalentWriterLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "rev-parse", "--verify", "refs/heads/missing-native-check"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	seedReadyTask(t, ctx, f, "persistent-check")
	task := runUntilTaskState(t, ctx, f, "persistent-check", model.Blocked)
	if got := f.Provider.ImplementationCount("persistent-check"); got != 2 {
		t.Fatalf("implementer ran %d times, want initial implementation plus one bounded fix", got)
	}
	if task.Verification == nil || task.Verification.Attempts != 2 || task.Verification.NativeOnly {
		t.Fatalf("repeated verification failure was not fingerprinted: %+v", task.Verification)
	}
	if task.Blocker == nil || task.Blocker.Resume != model.Fix {
		t.Fatalf("repeated verification failure did not produce a durable implementer recovery blocker: %+v", task.Blocker)
	}
}

func TestTransientWindowsNPMLockRetriesWithoutWriterOrAdvisor(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows npm lock classification is platform-specific")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := demo.New(ctx, t.TempDir(), []string{exe, "_aih-native-windows-npm-lock"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	seedReadyTask(t, ctx, f, "transient-npm-lock")
	task := runUntilTaskState(t, ctx, f, "transient-npm-lock", model.Blocked)
	if got := f.Provider.ImplementationCount("transient-npm-lock"); got != 1 {
		t.Fatalf("transient npm lock ran implementer %d times, want one", got)
	}
	if task.AdvisorUsed || len(task.FixCycles) != 0 || task.Verification == nil || task.Verification.Attempts != 2 || task.Verification.Classification != "windows-npm-eperm-unlink" {
		t.Fatalf("transient npm lock consumed source recovery budget: %+v", task)
	}
	if task.Blocker == nil || task.Blocker.Origin != model.BlockerOriginVerificationOnly || task.Blocker.Resume != model.SyncRequired || !strings.Contains(task.Blocker.Reason, "classification=windows-npm-eperm-unlink") || !strings.Contains(task.Blocker.Reason, "command_id=") || !strings.Contains(task.Blocker.Reason, "head=") {
		t.Fatalf("second transient npm lock was not a durable verification-only blocker: %+v", task.Blocker)
	}
	if got := eventCount(t, f, "transient_retry_queued"); got != 1 {
		t.Fatalf("transient npm lock retry queue events = %d, want 1", got)
	}
	if got := eventCount(t, f, "transient_retry_suppressed"); got != 1 {
		t.Fatalf("transient npm lock retry suppression events = %d, want 1", got)
	}
}

func TestTransientNativeTimeoutRetriesAtExactHeadWithoutWriterOrAdvisor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := demo.New(ctx, t.TempDir(), []string{exe, "_aih-native-timeout"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	setFixtureCheck(t, ctx, f, []string{exe, "_aih-native-timeout"}, 1)
	seedReadyTask(t, ctx, f, "transient-timeout")
	task := runUntilTaskState(t, ctx, f, "transient-timeout", model.Blocked)
	if got := f.Provider.ImplementationCount("transient-timeout"); got != 1 {
		t.Fatalf("transient timeout ran implementer %d times, want one", got)
	}
	if task.AdvisorUsed || len(task.FixCycles) != 0 || task.Verification == nil || task.Verification.Attempts != 2 || task.Verification.Classification != "timeout" {
		t.Fatalf("transient timeout consumed source recovery budget: %+v", task)
	}
	branchHead, err := f.P.Git.RemoteHead(ctx, task.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if task.HeadSHA != branchHead || task.Verification.HeadSHA != task.HeadSHA {
		t.Fatalf("transient timeout retry did not keep its exact durable head: task=%s guard=%s branch=%s", task.HeadSHA, task.Verification.HeadSHA, branchHead)
	}
	if task.Blocker == nil || task.Blocker.Origin != model.BlockerOriginVerificationOnly || task.Blocker.Resume != model.SyncRequired || !strings.Contains(task.Blocker.Reason, "classification=timeout") {
		t.Fatalf("second timeout was not a durable verification-only blocker: %+v", task.Blocker)
	}
	if got := eventCount(t, f, "transient_retry_queued"); got != 1 {
		t.Fatalf("transient timeout retry queue events = %d, want 1", got)
	}
	if got := eventCount(t, f, "transient_retry_suppressed"); got != 1 {
		t.Fatalf("transient timeout retry suppression events = %d, want 1", got)
	}
}

func TestAlternatingTransientFailuresConsumeOneRetry(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows npm lock classification is platform-specific")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "first-attempt")
	f, err := demo.New(ctx, t.TempDir(), []string{exe, "_aih-native-timeout-then-npm-lock", marker})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	setFixtureCheck(t, ctx, f, []string{exe, "_aih-native-timeout-then-npm-lock", marker}, 1)
	seedReadyTask(t, ctx, f, "alternating-transient")
	task := runUntilTaskState(t, ctx, f, "alternating-transient", model.Blocked)
	if task.Verification == nil || task.Verification.Attempts != 2 || task.Verification.Classification != "timeout" {
		t.Fatalf("alternating transient failures did not retain the first bounded classification: %+v", task.Verification)
	}
	if task.Blocker == nil || task.Blocker.Origin != model.BlockerOriginVerificationOnly || !strings.Contains(task.Blocker.Reason, "First transient classification") || !strings.Contains(task.Blocker.Reason, "timeout") {
		t.Fatalf("alternating transient failures did not produce a bounded blocker: %+v", task.Blocker)
	}
	if task.AdvisorUsed || len(task.FixCycles) != 0 || f.Provider.ImplementationCount("alternating-transient") != 1 {
		t.Fatalf("alternating transient failures consumed source recovery budget: %+v", task)
	}
}

func TestTransientRetryIsBoundedAcrossConfiguredChecks(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows npm lock classification is platform-specific")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "first-check")
	f, err := demo.New(ctx, t.TempDir(), []string{exe, "_aih-native-timeout-once", marker})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	setFixtureChecks(t, ctx, f, []config.Check{
		{Name: "timeout once", Command: []string{exe, "_aih-native-timeout-once", marker}, Timeout: 1},
		{Name: "npm lock", Command: []string{exe, "_aih-native-windows-npm-lock"}, Timeout: 1},
	})
	seedReadyTask(t, ctx, f, "two-check-transient")
	task := runUntilTaskState(t, ctx, f, "two-check-transient", model.Blocked)
	if task.Verification == nil || task.Verification.Attempts != 2 || task.Verification.Classification != "timeout" || task.Verification.CheckID == "" {
		t.Fatalf("two configured transient checks did not retain one plan allowance: %+v", task.Verification)
	}
	if task.Blocker == nil || task.Blocker.Origin != model.BlockerOriginVerificationOnly || task.Blocker.Resume != model.SyncRequired || !strings.Contains(task.Blocker.Reason, "check=npm lock") {
		t.Fatalf("second configured transient check did not stop at verification-only blocker: %+v", task.Blocker)
	}
	if task.AdvisorUsed || len(task.FixCycles) != 0 || f.Provider.ImplementationCount("two-check-transient") != 1 {
		t.Fatalf("two configured transient checks consumed source recovery budget: %+v", task)
	}
	if got := eventCount(t, f, "transient_retry_queued"); got != 1 {
		t.Fatalf("two-check retry queue events = %d, want 1", got)
	}
	if got := eventCount(t, f, "transient_retry_suppressed"); got != 1 {
		t.Fatalf("two-check retry suppression events = %d, want 1", got)
	}
}

func TestTransientFailureThenSourceFailureGetsFirstFix(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows npm lock classification is platform-specific")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "first-attempt")
	f, err := demo.New(ctx, t.TempDir(), []string{exe, "_aih-native-npm-lock-then-source", marker})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	seedReadyTask(t, ctx, f, "transient-then-source")
	task := runUntilTaskState(t, ctx, f, "transient-then-source", model.Blocked)
	if got := f.Provider.ImplementationCount("transient-then-source"); got != 2 {
		t.Fatalf("source failure after transient retry ran implementer %d times, want initial implementation plus one FIX", got)
	}
	if task.Verification == nil || task.Verification.Classification != "" || task.Verification.Attempts != 2 || task.Blocker == nil || task.Blocker.Resume != model.Fix || len(task.FixCycles) != 1 {
		t.Fatalf("source failure after transient retry did not receive its normal first FIX: %+v", task)
	}
}

func TestImplementationFailureKeepsNormalFixLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.Provider.Failures = map[string]int{"code-defect": 1}
	seedReadyTask(t, ctx, f, "code-defect")
	runUntilTaskState(t, ctx, f, "code-defect", model.Implemented, model.Verifying, model.Review, model.MergeReady, model.Done)
	if got := f.Provider.ImplementationCount("code-defect"); got != 2 {
		t.Fatalf("code failure ran implementer %d times, want normal bounded retry", got)
	}
}

func TestProviderAuthenticationFailureAfterScopedWriteCheckpointsSharedHoldAcrossAttach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	// The worker writes first, then returns a typed authentication failure. The
	// shared hold must not strand that scoped edit in machine A's worktree.
	f.Provider.AuthFailuresAfterWrite = map[string]int{"auth-blocked": 1}
	seedReadyTask(t, ctx, f, "auth-blocked")
	preRunHead, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(ctx) }()
	// A poll can observe the hold either side of its follow-on checkpoint. The
	// canonical main head before the writer starts is the stable identity the
	// new durable task head must advance, independent of that observation race.
	_ = waitProviderAdmissionHold(t, ctx, f)
	if err = f.P.DB.Submit(storeCommand("handoff")); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := f.P.DB.Load()
	if err != nil {
		t.Fatal(err)
	}
	task := snapshot.Tasks["auth-blocked"]
	if task.Attempts != 0 || len(task.FixCycles) != 0 || task.AdvisorUsed {
		t.Fatalf("authentication failure consumed task recovery budget: %+v", task)
	}
	if task.State != model.Ready || task.Blocker != nil {
		t.Fatalf("authentication failure did not preserve schedulable implementer checkpoint: %+v", task)
	}
	if task.HeadSHA == "" || task.HeadSHA == preRunHead {
		t.Fatalf("authentication failure after a scoped write did not advance canonical pre-run head: before=%s after=%#v", preRunHead, task)
	}
	if len(snapshot.ProviderAdmissionHolds) != 1 {
		t.Fatalf("authentication failure did not create shared provider hold: %#v", snapshot.ProviderAdmissionHolds)
	}
	if got := f.Provider.ImplementationCount("auth-blocked"); got != 1 {
		t.Fatalf("implementer calls = %d, want one failed provider invocation", got)
	}
	if got := f.Provider.AdvisorCount("auth-blocked"); got != 0 {
		t.Fatalf("advisor calls = %d, want none after authentication failure", got)
	}
	if task.Blocker != nil && strings.Contains(task.Blocker.Reason, "fixture provider invocation failed") {
		t.Fatalf("raw provider diagnostic entered blocker: %q", task.Blocker.Reason)
	}

	// A separate machine must reconstruct the published source checkpoint from
	// state and the task branch; a provider hold cannot make local edits the
	// only surviving copy of work completed before authentication failed.
	replacement, err := f.Open(ctx, filepath.Join(f.Root, "auth-machine-b"))
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.DB.Close()
	if err = replacement.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := replacement.DB.Load()
	if err != nil {
		t.Fatal(err)
	}
	recoveredTask := recovered.Tasks["auth-blocked"]
	if recoveredTask == nil || recoveredTask.HeadSHA != task.HeadSHA {
		t.Fatalf("attach lost authentication checkpoint: recovered=%#v want_head=%s", recoveredTask, task.HeadSHA)
	}
	content, err := os.ReadFile(filepath.Join(replacement.TaskPath(recoveredTask), "feature-auth-blocked.txt"))
	if err != nil || strings.TrimSpace(string(content)) != "implemented" {
		t.Fatalf("attach lost source written before authentication failure: %q %v", content, err)
	}
}
