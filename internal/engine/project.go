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

func outsideSource(root, home string) error {
	return config.OutsideSource(root, home)
}
