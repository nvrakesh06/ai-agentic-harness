package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/github"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
)

type acquireTestProvider struct{}

func (acquireTestProvider) Name() string                   { return "test" }
func (acquireTestProvider) Validate(context.Context) error { return nil }
func (acquireTestProvider) Run(context.Context, provider.Request) (provider.Result, error) {
	return provider.Result{}, nil
}

type acquireTestHub struct{}

func (acquireTestHub) EnsureIssue(context.Context, string, string, string) (int, error) {
	return 0, nil
}
func (acquireTestHub) UpdateIssue(context.Context, int, string, bool) error { return nil }
func (acquireTestHub) EnsurePR(context.Context, string, string, string, string) (int, error) {
	return 0, nil
}
func (acquireTestHub) UpdatePR(context.Context, int, string) error    { return nil }
func (acquireTestHub) SetPRDraft(context.Context, int, bool) error    { return nil }
func (acquireTestHub) Pull(context.Context, int) (github.Pull, error) { return github.Pull{}, nil }
func (acquireTestHub) Issues(context.Context) ([]github.Issue, error) { return nil, nil }

func TestAcquireHydratesQueuedSchemaSixOwnershipBeforeBranchCreation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote, source := filepath.Join(root, "origin.git"), filepath.Join(root, "source")
	if err := os.MkdirAll(remote, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	remoteGit, sourceGit := gitx.Git{Dir: remote}, gitx.Git{Dir: source}
	if _, err := remoteGit.Run(ctx, "", "init", "--bare", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceGit.Run(ctx, "", "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "base"}, {"remote", "add", "origin", remote}, {"push", "origin", "HEAD:main"}} {
		if _, err := sourceGit.Run(ctx, "", args...); err != nil {
			t.Fatal(err)
		}
	}

	project := config.Defaults()
	project.ID = "acquire-test-project"
	project.LeaseSeconds = 60
	first := leaseTestProject(t, ctx, root, remote, "machine-a", project)
	first.Provider, first.Hub = acquireTestProvider{}, acquireTestHub{}
	if err := first.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	state := model.NewSnapshot(project.ID)
	state.Schema = 6
	state.Revision = 1
	state.Tasks["queued"] = &model.Task{ID: "queued", State: model.Ready, Areas: []string{"feature-queued.txt"}, Branch: "aih/queued", FixCycles: map[string]int{}}
	head, err := first.Git.StateCommit(ctx, "", state)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", New: head}}); err != nil {
		t.Fatal(err)
	}

	controller := New(first)
	if err = controller.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	durable, _, err := first.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queued := durable.Tasks["queued"]
	if queued.BaseSHA == "" || queued.HeadSHA != "" || queued.AssignedAreaKinds["feature-queued.txt"] != model.AreaFile {
		t.Fatalf("acquire did not preserve an attachable queued task: %#v", queued)
	}
	if branch, err := first.Git.RemoteHead(ctx, queued.Branch); err != nil || branch != "" {
		t.Fatalf("queued branch exists before dispatch: head=%q err=%v", branch, err)
	}

	second := leaseTestProject(t, ctx, root, remote, "machine-b", project)
	second.Provider, second.Hub = acquireTestProvider{}, acquireTestHub{}
	if err = second.Attach(ctx); err != nil {
		t.Fatalf("second-machine attach before branch creation failed: %v", err)
	}
	recovered, _, err := second.DB.Load()
	if err != nil {
		t.Fatal(err)
	}
	queued = recovered.Tasks["queued"]
	if queued.BaseSHA == "" || queued.HeadSHA != "" || queued.AssignedAreaKinds["feature-queued.txt"] != model.AreaFile {
		t.Fatalf("second-machine attach lost queued ownership or invented a branch checkpoint: %#v", queued)
	}

	if err = os.WriteFile(filepath.Join(source, "unrelated.txt"), []byte("advanced main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "unrelated.txt"}, {"commit", "-m", "advance main"}, {"push", "origin", "HEAD:main"}} {
		if _, err := sourceGit.Run(ctx, "", args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = first.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	advanced, err := first.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil || advanced == queued.BaseSHA {
		t.Fatalf("dispatch machine did not observe the advanced main: base=%q main=%q err=%v", queued.BaseSHA, advanced, err)
	}

	controller.ctx = ctx
	if err = controller.ensureWorktree("queued"); err != nil {
		t.Fatal(err)
	}
	hydrated := controller.Snapshot().Tasks["queued"]
	if hydrated.BaseSHA != queued.BaseSHA || hydrated.HeadSHA != queued.BaseSHA {
		t.Fatalf("queued worktree was not created at its durable base: %#v", hydrated)
	}
	worktreeHead, err := (gitx.Git{Dir: first.TaskPath(hydrated)}).SHA(ctx, "HEAD")
	if err != nil || worktreeHead != queued.BaseSHA {
		t.Fatalf("queued worktree head = %q, want durable base %q: %v", worktreeHead, queued.BaseSHA, err)
	}
	if err = second.Attach(ctx); err != nil {
		t.Fatalf("second-machine attach after worktree creation failed: %v", err)
	}
	recovered, _, err = second.DB.Load()
	if err != nil {
		t.Fatal(err)
	}
	queued = recovered.Tasks["queued"]
	if queued.BaseSHA != worktreeHead || queued.HeadSHA != worktreeHead || queued.AssignedAreaKinds["feature-queued.txt"] != model.AreaFile {
		t.Fatalf("second-machine attach did not recover the atomically published branch checkpoint: %#v", queued)
	}
	if err = os.WriteFile(filepath.Join(first.TaskPath(hydrated), "feature-queued.txt"), []byte("implemented\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = controller.checkpoint(ctx, "queued"); err != nil {
		t.Fatalf("narrow first checkpoint after main advance failed: %v", err)
	}
	checkpointed := controller.Snapshot().Tasks["queued"]
	if checkpointed.BaseSHA != queued.BaseSHA || checkpointed.HeadSHA == "" {
		t.Fatalf("checkpoint changed the durable base or omitted its head: %#v", checkpointed)
	}
}

func TestEnsureWorktreeRejectsStaleQueuedLocalBranchBeforePublication(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote, source := filepath.Join(root, "origin.git"), filepath.Join(root, "source")
	if err := os.MkdirAll(remote, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	remoteGit, sourceGit := gitx.Git{Dir: remote}, gitx.Git{Dir: source}
	if _, err := remoteGit.Run(ctx, "", "init", "--bare", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceGit.Run(ctx, "", "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "base"}, {"remote", "add", "origin", remote}, {"push", "origin", "HEAD:main"}} {
		if _, err := sourceGit.Run(ctx, "", args...); err != nil {
			t.Fatal(err)
		}
	}

	project := config.Defaults()
	project.ID = "stale-branch-project"
	project.LeaseSeconds = 60
	p := leaseTestProject(t, ctx, root, remote, "machine-a", project)
	if err := p.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	base, err := p.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "unrelated.txt"), []byte("advanced\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "unrelated.txt"}, {"commit", "-m", "advance main"}, {"push", "origin", "HEAD:main"}} {
		if _, err = sourceGit.Run(ctx, "", args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = p.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Git.Run(ctx, "", "branch", "aih/queued", "refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}

	snapshot := model.NewSnapshot(project.ID)
	snapshot.Tasks["queued"] = &model.Task{ID: "queued", State: model.Ready, Branch: "aih/queued", BaseSHA: base, AssignedAreas: []string{"feature-queued.txt"}, AssignedAreaKinds: map[string]string{"feature-queued.txt": model.AreaFile}}
	controller := &Controller{P: p, s: snapshot, ctx: ctx}
	if err = controller.ensureWorktree("queued"); err == nil {
		t.Fatal("stale local branch was accepted for initial publication")
	}
	if task := controller.Snapshot().Tasks["queued"]; task.HeadSHA != "" {
		t.Fatalf("stale local branch was published into durable state: %#v", task)
	}
}
