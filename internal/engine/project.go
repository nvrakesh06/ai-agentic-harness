package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/github"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
)

type Project struct {
	Home, Dir, Root, Repo, Remote string
	Machine                       config.Machine
	Git                           gitx.Git
	Config                        config.Effective
	DB                            *store.Store
	Hub                           github.Service
	Provider                      provider.Provider
}

func Init(ctx context.Context, root, home, selected string) error {
	root, remote, e := gitx.Discover(ctx, root)
	if e != nil {
		return e
	}
	repo, e := config.GitHubRepo(remote)
	if e != nil {
		return e
	}
	if e = outsideSource(root, home); e != nil {
		return e
	}
	hub := github.Client{Repo: repo, Dir: root}
	if e = hub.Capabilities(ctx); e != nil {
		return e
	}
	if _, e = config.Install(home); e != nil {
		return e
	}
	p := config.Defaults()
	existing, readErr := os.ReadFile(filepath.Join(root, ".aih", "project.yaml"))
	if readErr == nil {
		p.ID = ""
		if e = config.DecodeProject(existing, &p); e != nil {
			return e
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	} else if selected != "" {
		p.Provider = selected
	}
	if readErr != nil {
		p.Checks = config.DetectChecks(root)
	}
	if e = p.Validate(); e != nil {
		return e
	}
	if e = provider.New(p.Provider).Validate(ctx); e != nil {
		return e
	}
	dir, e := config.ProjectDir(home, p.ID)
	if e != nil {
		return e
	}
	g, e := gitx.OpenControl(ctx, filepath.Join(dir, "control.git"), remote)
	if e != nil {
		return e
	}
	if e = g.Fetch(ctx); e != nil {
		return e
	}
	if _, e = g.SHA(ctx, "refs/remotes/origin/main"); e != nil {
		return errors.New("origin/main must exist before aih init")
	}
	if _, _, e = g.Load(ctx); e == nil {
		return errors.New("repository already enabled; restore/commit its .aih configuration and use aih attach")
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	files := map[string]any{"project.yaml": p, "policies.yaml": config.DefaultPolicy(), "harness.lock": config.DefaultLock()}
	if e = os.MkdirAll(filepath.Join(root, ".aih", "roles"), 0755); e != nil {
		return e
	}
	for name, value := range files {
		if _, se := os.Stat(filepath.Join(root, ".aih", name)); se == nil {
			continue
		}
		b, _ := yaml.Marshal(value)
		if e = os.WriteFile(filepath.Join(root, ".aih", name), b, 0644); e != nil {
			return e
		}
	}
	if _, e = os.Stat(filepath.Join(root, "AGENTS.md")); os.IsNotExist(e) {
		e = os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Project instructions\n\nRepository: "+repo+"\n\nSee `.aih/project.yaml` for verification commands. Add project-specific architecture and invariants here.\n"), 0644)
		if e != nil {
			return e
		}
	}
	s := model.NewSnapshot(p.ID)
	s.Capacity = configuredCapacity(p, s.Capacity)
	s.Revision = 1
	h, e := g.StateCommit(ctx, "", s)
	if e != nil {
		return e
	}
	if e = g.Publish(ctx, []gitx.Update{{Branch: "aih-state", New: h}}); e != nil {
		return e
	}
	if e = config.Register(home, config.Registration{ProjectID: p.ID, Repository: repo, Remote: remote, Root: root}); e != nil {
		return e
	}
	db, e := store.Open(filepath.Join(dir, "state.db"))
	if e != nil {
		return e
	}
	defer db.Close()
	return db.Save(h, s)
}
func Open(ctx context.Context, root, home string, refresh bool) (*Project, error) {
	root, remote, e := gitx.Discover(ctx, root)
	if e != nil {
		return nil, e
	}
	if e = outsideSource(root, home); e != nil {
		return nil, e
	}
	repo, e := config.GitHubRepo(remote)
	if e != nil {
		return nil, e
	}
	b, e := os.ReadFile(filepath.Join(root, ".aih", "project.yaml"))
	if e != nil {
		return nil, fmt.Errorf("read project configuration (run aih init/clone configured main): %w", e)
	}
	var local config.Project
	if e = config.Decode(b, &local); e != nil {
		return nil, e
	}
	dir, e := config.ProjectDir(home, local.ID)
	if e != nil {
		return nil, e
	}
	machine, e := config.Install(home)
	if e != nil {
		return nil, e
	}
	g, e := gitx.OpenControl(ctx, filepath.Join(dir, "control.git"), remote)
	if e != nil {
		return nil, e
	}
	if refresh {
		if e = g.Fetch(ctx); e != nil {
			return nil, e
		}
	}
	effective, e := Canonical(ctx, g)
	if e != nil {
		return nil, e
	}
	if effective.Project.ID != local.ID {
		return nil, errors.New("local and canonical project identities disagree")
	}
	db, e := store.Open(filepath.Join(dir, "state.db"))
	if e != nil {
		return nil, e
	}
	p := &Project{Home: home, Dir: dir, Root: root, Repo: repo, Remote: remote, Machine: machine, Git: g, Config: effective, DB: db, Hub: github.Client{Repo: repo, Dir: root}, Provider: provider.New(effective.Project.Provider)}
	if e = config.Register(home, config.Registration{ProjectID: local.ID, Repository: repo, Remote: remote, Root: root}); e != nil {
		db.Close()
		return nil, e
	}
	return p, nil
}
func Canonical(ctx context.Context, g gitx.Git) (config.Effective, error) {
	ref, e := g.SHA(ctx, "refs/remotes/origin/main")
	if e != nil {
		return config.Effective{}, e
	}
	names, e := g.Files(ctx, ref, "")
	if e != nil {
		return config.Effective{}, e
	}
	files := map[string]string{}
	for _, n := range names {
		if strings.HasPrefix(n, ".aih/") || filepath.Base(n) == "AGENTS.md" {
			b, e := g.Show(ctx, ref, n)
			if e != nil {
				return config.Effective{}, e
			}
			if len(b) > 128*1024 {
				return config.Effective{}, fmt.Errorf("canonical instruction file too large: %s", n)
			}
			files[n] = b
		}
	}
	effective, e := config.Parse(files)
	if e != nil {
		return effective, e
	}
	all, e := roles.Load(files)
	if e != nil {
		return effective, e
	}
	for _, role := range all {
		for _, name := range role.Context {
			if strings.HasPrefix(name, "/") || strings.Contains(name, "..") || strings.ContainsAny(name, `\:`) {
				return effective, fmt.Errorf("invalid role context path %q", name)
			}
			if _, ok := files[name]; ok {
				continue
			}
			b, err := g.Show(ctx, ref, name)
			if err != nil {
				return effective, fmt.Errorf("required context %s: %w", name, err)
			}
			if len(b) > 128*1024 {
				return effective, fmt.Errorf("context too large: %s", name)
			}
			files[name] = b
		}
	}
	effective, e = config.Parse(files)
	effective.BaseSHA = ref
	return effective, e
}
func (p *Project) Attach(ctx context.Context) error {
	if e := p.Provider.Validate(ctx); e != nil {
		return e
	}
	if e := p.Git.Fetch(ctx); e != nil {
		return e
	}
	s, h, e := p.Git.Load(ctx)
	if e != nil {
		return e
	}
	if s.Project != p.Config.Project.ID {
		return errors.New("remote project identity mismatch")
	}
	if _, e = p.Hub.Issues(ctx); e != nil {
		return e
	}
	// Observation never changes the remote lease or rewinds a local branch.
	for _, t := range model.Ordered(s) {
		if t.State == model.Done || t.HeadSHA == "" {
			continue
		}
		remoteHead, e := p.Git.RemoteHead(ctx, t.Branch)
		if e != nil {
			return e
		}
		if remoteHead != t.HeadSHA {
			return fmt.Errorf("task %s branch diverged from its durable checkpoint", t.ID)
		}
		if e = p.Git.Worktree(ctx, p.TaskPath(t), t.Branch, t.HeadSHA); e != nil {
			return e
		}
		if t.PR != 0 {
			if _, e = p.Hub.Pull(ctx, t.PR); e != nil {
				return e
			}
		}
	}
	return p.DB.Save(h, s)
}
func (p *Project) TaskPath(t *model.Task) string { return filepath.Join(p.Dir, "worktrees", t.ID) }

// ValidDisposableReviewWorktreePath returns the sole path a read-only role may
// use for its throwaway checkout. Run IDs are generated by AIH, but validation
// still rejects a corrupt value or a root/child junction before Git is allowed
// to create or later force-remove a worktree.
func (p *Project) ValidDisposableReviewWorktreePath(runID string) (string, error) {
	_, target, err := p.disposableReviewWorktreeTarget(runID)
	if err != nil {
		return "", err
	}
	return target, nil
}

// RemoveDisposableReviewWorktree discards only an AIH-created, validated
// detached review checkout. It deliberately owns --force here rather than in
// gitx so arbitrary paths can never receive forced worktree removal.
func (p *Project) RemoveDisposableReviewWorktree(ctx context.Context, runID string) error {
	root, target, err := p.disposableReviewWorktreeTarget(runID)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect disposable review root: %w", err)
	}
	if _, err = os.Lstat(target); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect disposable review worktree: %w", err)
	}
	// Re-resolve immediately before the forced operation so a swapped root or
	// target junction fails closed instead of redirecting cleanup outside AIH.
	if _, _, err = p.disposableReviewWorktreeTarget(runID); err != nil {
		return err
	}
	if _, err = p.Git.Run(ctx, "", "worktree", "remove", "--force", target); err != nil {
		return fmt.Errorf("remove disposable review worktree: %w", err)
	}
	return nil
}

func (p *Project) disposableReviewWorktreeTarget(runID string) (string, string, error) {
	if !safeTaskScratchID(runID) {
		return "", "", errors.New("unsafe disposable review worktree identifier")
	}
	project, err := filepath.EvalSymlinks(p.Dir)
	if err != nil {
		return "", "", fmt.Errorf("resolve project directory: %w", err)
	}
	root := filepath.Join(project, "review-worktrees")
	target := filepath.Join(root, runID)
	if err = mustResolveTo(root, root); err != nil {
		return "", "", fmt.Errorf("unsafe disposable review root: %w", err)
	}
	if err = mustResolveTo(target, target); err != nil {
		return "", "", fmt.Errorf("unsafe disposable review worktree: %w", err)
	}
	return root, target, nil
}

// TaskScratchPath is machine-local disposable storage for worker tooling and
// caches. It intentionally lives outside the checkpointed source worktree and
// is derived from the durable task identity, so a resumed task keeps its cache.
func (p *Project) TaskScratchPath(t *model.Task) string {
	return filepath.Join(p.Dir, "scratch", t.ID)
}

func (p *Project) ValidTaskScratchPath(t *model.Task) (string, error) {
	_, target, err := p.taskScratchTarget(t)
	if err != nil {
		return "", err
	}
	return target, nil
}

// PrepareTaskScratch creates a task's scratch directory only after resolving
// both the scratch root and target to their intended project-local paths.
// MkdirAll would follow a root junction before the provider gets a chance to
// reject it, so the two known path components are created and checked one at a
// time.
func (p *Project) PrepareTaskScratch(t *model.Task) (string, error) {
	root, target, err := p.taskScratchTarget(t)
	if err != nil {
		return "", err
	}
	if err = os.Mkdir(root, 0700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create task scratch root: %w", err)
	}
	if _, _, err = p.taskScratchTarget(t); err != nil {
		return "", err
	}
	if err = os.Mkdir(target, 0700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create task scratch directory: %w", err)
	}
	_, target, err = p.taskScratchTarget(t)
	if err != nil {
		return "", err
	}
	return target, nil
}

// RemoveTaskScratch removes only a resolved task directory below this project's
// scratch root. Task IDs originate in durable state, so a corrupted value must
// fail closed rather than allow a path traversal or junction escape.
func (p *Project) RemoveTaskScratch(t *model.Task) error {
	root, target, err := p.taskScratchTarget(t)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(root); os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect task scratch root: %w", err)
	}
	if _, err = os.Lstat(target); os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect task scratch path: %w", err)
	}
	// Resolve again immediately before the recursive operation. RemoveAll does
	// not receive an unchecked lexical path that a root or task junction can
	// redirect outside this project.
	if _, _, err = p.taskScratchTarget(t); err != nil {
		return err
	}
	return os.RemoveAll(target)
}

func (p *Project) taskScratchTarget(t *model.Task) (string, string, error) {
	if t == nil || !safeTaskScratchID(t.ID) {
		return "", "", errors.New("unsafe task scratch identifier")
	}
	project, err := filepath.EvalSymlinks(p.Dir)
	if err != nil {
		return "", "", fmt.Errorf("resolve project directory: %w", err)
	}
	root := filepath.Join(project, "scratch")
	target := filepath.Join(root, t.ID)
	if err = mustResolveTo(root, root); err != nil {
		return "", "", fmt.Errorf("unsafe task scratch root: %w", err)
	}
	if err = mustResolveTo(target, target); err != nil {
		return "", "", fmt.Errorf("unsafe task scratch path: %w", err)
	}
	return root, target, nil
}

// mustResolveTo resolves existing components and appends only genuinely missing
// suffixes. A direct Lstat rejects dangling links, which EvalSymlinks alone
// would otherwise report as a missing path.
func mustResolveTo(path, want string) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("symbolic link or junction is not allowed")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	resolved, err := resolvePath(path)
	if err != nil {
		return err
	}
	if filepath.Clean(resolved) != filepath.Clean(want) {
		return fmt.Errorf("resolves to %q, want %q", resolved, want)
	}
	return nil
}

func resolvePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	missing := []string{}
	for {
		resolved, err := filepath.EvalSymlinks(abs)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", err
		}
		missing = append(missing, filepath.Base(abs))
		abs = parent
	}
}

func safeTaskScratchID(id string) bool {
	return id != "" && id != "." && id != ".." && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\:`)
}

func outsideSource(root, home string) error {
	return config.OutsideSource(root, home)
}
