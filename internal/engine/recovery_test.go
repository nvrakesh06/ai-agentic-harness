package engine_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type deadlineHandoffProvider struct {
	calls int
}

func (p *deadlineHandoffProvider) Name() string                   { return "codex" }
func (p *deadlineHandoffProvider) Validate(context.Context) error { return nil }
func (p *deadlineHandoffProvider) Run(ctx context.Context, request provider.Request) (provider.Result, error) {
	p.calls++
	if p.calls == 1 {
		if err := os.WriteFile(filepath.Join(request.Directory, "recovered.txt"), []byte("durable timeout work\n"), 0600); err != nil {
			return provider.Result{}, err
		}
		if err := os.MkdirAll(request.Runtime, 0700); err != nil {
			return provider.Result{}, err
		}
		logLine := `{"type":"item.completed","item":{"type":"command_execution","command":"go test ./internal/engine --api-key=must-not-persist","exit_code":0}}` + "\n"
		if err := os.WriteFile(filepath.Join(request.Runtime, "output.log"), []byte(logLine), 0600); err != nil {
			return provider.Result{}, err
		}
		<-ctx.Done()
		return provider.Result{}, &provider.InvocationError{Cause: ctx.Err(), LastActivity: time.Now().UTC(), OutputBytes: len(logLine)}
	}
	if p.calls == 2 {
		<-ctx.Done()
		return provider.Result{}, ctx.Err()
	}
	return provider.Result{}, errors.New("deadline fixture unexpectedly resumed")
}

func TestPartialGraphRecoversWithoutLocalProject(t *testing.T) {
	ctx := context.Background()
	f, e := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if e != nil {
		t.Fatal(e)
	}
	s, h, e := f.P.Git.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s.Objectives["objective"] = &model.Objective{ID: "objective", Text: "recover", Planned: true}
	base, e := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if e != nil {
		t.Fatal(e)
	}
	updates := []gitx.Update{}
	states := []model.State{model.Done, model.Running, model.Review, model.Blocked, model.Ready}
	for i, state := range states {
		id := string(rune('a' + i))
		task := &model.Task{ID: id, ObjectiveID: "objective", Title: id, State: state, HeadSHA: base, Branch: "aih/" + id, FixCycles: map[string]int{}}
		if state == model.Blocked {
			model.Block(task, "Choose", "decision", model.Ready)
		}
		s.Tasks[id] = task
		updates = append(updates, gitx.Update{Branch: task.Branch, New: base})
	}
	s.Tasks["c"].Verification = &model.Verification{
		Environment: "windows/native/check", HeadSHA: base,
		Fingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte("check failed"))), Attempts: 1,
	}
	next, e := f.P.Git.StateCommit(ctx, h, s)
	if e != nil {
		t.Fatal(e)
	}
	updates = append(updates, gitx.Update{Branch: "aih-state", Old: h, New: next})
	if e = f.P.Git.Publish(ctx, updates); e != nil {
		t.Fatal(e)
	}
	dir := f.P.Dir
	if e = f.P.DB.Close(); e != nil {
		t.Fatal(e)
	}
	rel, e := filepath.Rel(f.Root, dir)
	if e != nil || rel == "." || strings.HasPrefix(rel, "..") {
		t.Fatal("unsafe test deletion")
	}
	if e = os.RemoveAll(dir); e != nil {
		t.Fatal(e)
	}
	p, e := f.Open(ctx, filepath.Join(f.Root, "machine-b"))
	if e != nil {
		t.Fatal(e)
	}
	defer p.DB.Close()
	if e = p.Attach(ctx); e != nil {
		t.Fatal(e)
	}
	recovered, _, e := p.DB.Load()
	if e != nil {
		t.Fatal(e)
	}
	for id, task := range s.Tasks {
		if recovered.Tasks[id].State != task.State {
			t.Fatal("lost state", id)
		}
		if task.Verification != nil && (recovered.Tasks[id].Verification == nil || recovered.Tasks[id].Verification.Fingerprint != task.Verification.Fingerprint) {
			t.Fatal("lost verification retry guard", id)
		}
		if task.State != model.Done {
			if _, e = os.Stat(filepath.Join(p.TaskPath(task), "README.md")); e != nil {
				t.Fatal("missing recovered source", e)
			}
		}
	}
	// Stop is queued before scheduling: recovery transitions are still exercised.
	if e = p.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"}); e != nil {
		t.Fatal(e)
	}
	if e = engine.New(p).Serve(ctx); e != nil {
		t.Fatal(e)
	}
	recovered, _, e = p.DB.Load()
	if e != nil {
		t.Fatal(e)
	}
	if recovered.Tasks["b"].State != model.Ready || recovered.Tasks["c"].State != model.SyncRequired || recovered.Tasks["d"].Blocker == nil {
		t.Fatal("interruption recovery incorrect")
	}
	if recovered.Tasks["c"].Verification == nil {
		t.Fatal("interruption recovery lost verification retry guard")
	}
}

func TestAttachMigratesSchemaSixAreaOwnershipWithoutWideningStartedTasks(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()

	state, oldStateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Schema = 6
	state.Tasks["unstarted"] = &model.Task{ID: "unstarted", State: model.Ready, Areas: []string{"src/engine", "README.md"}}
	state.Tasks["started"] = &model.Task{ID: "started", State: model.Running, Areas: []string{"legacy-verification-output.txt"}}
	legacyStateHead, err := f.P.Git.StateCommit(ctx, oldStateHead, state)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: oldStateHead, New: legacyStateHead}}); err != nil {
		t.Fatal(err)
	}

	attached, err := f.Open(ctx, filepath.Join(f.Root, "machine-b"))
	if err != nil {
		t.Fatal(err)
	}
	defer attached.DB.Close()
	if err = attached.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := attached.DB.Load()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Schema != model.StateSchema {
		t.Fatalf("attach did not upgrade schema: %d", recovered.Schema)
	}
	if task := recovered.Tasks["unstarted"]; strings.Join(task.AssignedAreas, ",") != "src/engine,README.md" || task.AssignedAreaKinds["src/engine"] != model.AreaUnknown || task.AssignedAreaKinds["README.md"] != model.AreaUnknown {
		t.Fatalf("attach did not recover fail-closed immutable ownership: %#v", task)
	}
	if task := recovered.Tasks["started"]; len(task.AssignedAreas) != 0 || len(task.AssignedAreaKinds) != 0 || task.State != model.Running {
		t.Fatalf("attach widened or changed a started legacy task: %#v", task)
	}
}

func TestSchemaSixQueuedTaskHydratesOnResumeWhileStartedTaskBlocks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	s, stateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Schema = 6
	s.Tasks["queued"] = &model.Task{ID: "queued", Title: "queued", Objective: "resume queued fixture", State: model.Ready, Areas: []string{"feature-queued.txt"}, Branch: "aih/queued", FixCycles: map[string]int{}}
	s.Tasks["started"] = &model.Task{ID: "started", Title: "started", Objective: "do not widen started fixture", State: model.Running, Areas: []string{"feature-started.txt"}, Branch: "aih/started", FixCycles: map[string]int{}}
	legacyHead, err := f.P.Git.StateCommit(ctx, stateHead, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: legacyHead}}); err != nil {
		t.Fatal(err)
	}
	if err = f.P.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current, _, loadErr := f.P.DB.Load()
		if loadErr == nil && current.Tasks["queued"].AssignedAreaKinds["feature-queued.txt"] == model.AreaFile && current.Tasks["queued"].BaseSHA != "" && current.Tasks["started"].State == model.Blocked {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	current, _, err := f.P.DB.Load()
	if err != nil {
		t.Fatal(err)
	}
	if queued := current.Tasks["queued"]; queued.AssignedAreaKinds["feature-queued.txt"] != model.AreaFile || queued.BaseSHA == "" {
		t.Fatalf("queued task did not hydrate from durable base: %#v", queued)
	}
	if started := current.Tasks["started"]; started.State != model.Blocked || started.Blocker == nil {
		t.Fatalf("started task was widened or resumed: %#v", started)
	}
	if err = f.P.DB.Submit(store.Command{ID: "handoff-schema6", Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
}

func TestDeadlineCheckpointAndHandoffRecoverTogetherAfterMachineLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	f.Project.WorkerSeconds = 10
	projectYAML, _ := yaml.Marshal(f.Project)
	if err = os.WriteFile(filepath.Join(f.Source, ".aih", "project.yaml"), projectYAML, 0600); err != nil {
		t.Fatal(err)
	}
	sourceGit := gitx.Git{Dir: f.Source}
	if _, err = sourceGit.Run(ctx, "", "add", ".aih/project.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err = sourceGit.Run(ctx, "", "commit", "-m", "Short deadline fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err = sourceGit.Run(ctx, "", "push", "origin", "HEAD:main"); err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}

	snapshot, stateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Objectives["deadline-objective"] = &model.Objective{ID: "deadline-objective", Text: "recover timeout work", Planned: true}
	snapshot.Tasks["deadline"] = &model.Task{
		ID:          "deadline",
		ObjectiveID: "deadline-objective",
		Title:       "deadline",
		Objective:   "Write recovered.txt and preserve it across the deadline.",
		Acceptance:  []string{"recovered.txt is durable"},
		Areas:       []string{"recovered.txt"},
		Domains:     []string{"deadline-fixture"},
		Risk:        "low",
		State:       model.Ready,
		Branch:      "aih/deadline",
		BaseSHA:     base,
		HeadSHA:     base,
		Rotations:   23,
		FixCycles:   map[string]int{},
	}
	nextState, err := f.P.Git.StateCommit(ctx, stateHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih/deadline", New: base}, {Branch: "aih-state", Old: stateHead, New: nextState}}); err != nil {
		t.Fatal(err)
	}
	worker := &deadlineHandoffProvider{}
	f.P.Provider = worker
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(ctx) }()

	for {
		current, _, loadErr := f.P.DB.Load()
		if loadErr == nil {
			task := current.Tasks["deadline"]
			if task != nil && task.State == model.Blocked && task.Rotations == 24 {
				break
			}
		}
		select {
		case serveErr := <-done:
			t.Fatal("supervisor exited before recovered checkpoint publication", serveErr)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}

	remote, _, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	durable := remote.Tasks["deadline"]
	branchHead, err := f.P.Git.RemoteHead(ctx, durable.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if durable.HeadSHA == base || durable.HeadSHA != branchHead || !strings.Contains(durable.Summary, "Recovered worker timeout handoff") {
		t.Fatalf("source and handoff were not atomically published: %#v branch=%s", durable, branchHead)
	}
	if strings.Join(durable.ReportedTests, ",") != "go test" || len(durable.Risks) == 0 || len(durable.Decisions) == 0 {
		t.Fatalf("durable handoff fields are incomplete: %#v", durable)
	}
	portable, _ := json.Marshal(durable)
	if strings.Contains(string(portable), "must-not-persist") || strings.Contains(string(portable), "--api-key") {
		t.Fatalf("raw command arguments reached durable state: %s", portable)
	}

	projectDir := f.P.Dir
	if err = f.P.DB.Close(); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(f.Root, projectDir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		t.Fatal("unsafe test deletion target", projectDir)
	}
	if err = os.RemoveAll(projectDir); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.Open(ctx, filepath.Join(f.Root, "deadline-machine-b"))
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
	recoveredTask := recovered.Tasks["deadline"]
	if recoveredTask.HeadSHA != durable.HeadSHA || recoveredTask.Summary != durable.Summary || strings.Join(recoveredTask.ReportedTests, ",") != "go test" || len(recoveredTask.Risks) == 0 || len(recoveredTask.Decisions) == 0 {
		t.Fatalf("attach lost recovered handoff fields: %#v", recoveredTask)
	}
	content, err := os.ReadFile(filepath.Join(replacement.TaskPath(recoveredTask), "recovered.txt"))
	if err != nil || strings.TrimSpace(string(content)) != "durable timeout work" {
		t.Fatalf("attach lost recovered source: %q %v", content, err)
	}
}

func init() {
	if len(os.Args) > 1 && os.Args[1] == "_aih-post-check" {
		if os.Getenv("AIH_FAIL_POST_CHECK") == "1" {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestPostVerifyHoldAndHumanRetry(t *testing.T) {
	t.Setenv("AIH_FAIL_POST_CHECK", "1")
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, e := demo.New(ctx, t.TempDir(), []string{exe, "_aih-post-check"})
	if e != nil {
		t.Fatal(e)
	}
	defer f.P.DB.Close()
	s, h, e := f.P.Git.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	base, e := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if e != nil {
		t.Fatal(e)
	}
	s.Objectives["objective"] = &model.Objective{ID: "objective", Planned: true}
	s.Tasks["task"] = &model.Task{ID: "task", ObjectiveID: "objective", State: model.PostVerify, MergeSHA: base, HeadSHA: base, Branch: "aih/task", FixCycles: map[string]int{}}
	next, e := f.P.Git.StateCommit(ctx, h, s)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: h, New: next}}); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}()
	for {
		s, _, e = f.P.DB.Load()
		if e == nil && s.IntegrationBlocked == "task" {
			break
		}
		select {
		case e = <-done:
			t.Fatal("supervisor exited", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if s.Tasks["task"].State != model.Blocked || s.Tasks["task"].Blocker.Resume != model.PostVerify {
		t.Fatal("post-verify did not fail closed")
	}
	t.Setenv("AIH_FAIL_POST_CHECK", "0")
	if e = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "answer", Target: "task", Payload: "The verification environment is repaired; recheck."}); e != nil {
		t.Fatal(e)
	}
	for {
		s, _, e = f.P.DB.Load()
		if e == nil && s.Tasks["task"].State == model.Done {
			break
		}
		select {
		case e = <-done:
			t.Fatal("supervisor exited", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if s.IntegrationBlocked != "" {
		t.Fatal("hold not cleared")
	}
	_ = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"})
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
