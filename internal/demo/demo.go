// Package demo runs the real supervisor against local Git and deterministic
// provider/GitHub adapters. It never makes a paid model or GitHub call.
package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/github"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Fixture struct {
	Root, Remote, Source, Home string
	Project                    config.Project
	Hub                        *Hub
	Provider                   *Worker
	P                          *engine.Project
}

func New(ctx context.Context, root string, checks []string) (*Fixture, error) {
	f := &Fixture{Root: root, Remote: filepath.Join(root, "origin.git"), Source: filepath.Join(root, "repo"), Home: filepath.Join(root, "machine-a"), Project: config.Defaults(), Provider: &Worker{}}
	for _, d := range []string{f.Remote, f.Source} {
		if e := os.MkdirAll(d, 0700); e != nil {
			return nil, e
		}
	}
	if _, e := (gitx.Git{Dir: f.Remote}).Run(ctx, "", "init", "--bare", "-b", "main"); e != nil {
		return nil, e
	}
	g := gitx.Git{Dir: f.Source}
	if _, e := g.Run(ctx, "", "init", "-b", "main"); e != nil {
		return nil, e
	}
	if _, e := g.Run(ctx, "", "remote", "add", "origin", f.Remote); e != nil {
		return nil, e
	}
	f.Project.LeaseSeconds = 60
	f.Project.WorkerSeconds = 30
	f.Project.Checks = []config.Check{{Name: "fixture acceptance", Command: checks, Timeout: 30}}
	if e := os.MkdirAll(filepath.Join(f.Source, ".aih"), 0700); e != nil {
		return nil, e
	}
	for name, v := range map[string]any{"project.yaml": f.Project, "policies.yaml": config.DefaultPolicy(), "harness.lock": config.DefaultLock()} {
		b, _ := yaml.Marshal(v)
		if e := os.WriteFile(filepath.Join(f.Source, ".aih", name), b, 0600); e != nil {
			return nil, e
		}
	}
	if e := os.WriteFile(filepath.Join(f.Source, "README.md"), []byte("AIH deterministic fixture\n"), 0600); e != nil {
		return nil, e
	}
	if _, e := g.Run(ctx, "", "add", "--all"); e != nil {
		return nil, e
	}
	if _, e := g.Run(ctx, "", "commit", "-m", "Fixture baseline"); e != nil {
		return nil, e
	}
	if _, e := g.Run(ctx, "", "push", "origin", "HEAD:main"); e != nil {
		return nil, e
	}
	f.Hub = &Hub{Remote: gitx.Git{Dir: f.Remote}, issues: map[int]github.Issue{}, pulls: map[int]github.Pull{}}
	p, e := f.Open(ctx, f.Home)
	if e != nil {
		return nil, e
	}
	s := model.NewSnapshot(f.Project.ID)
	s.Revision = 1
	h, e := p.Git.StateCommit(ctx, "", s)
	if e != nil {
		return nil, e
	}
	if e = p.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", New: h}}); e != nil {
		return nil, e
	}
	if e = p.DB.Save(h, s); e != nil {
		return nil, e
	}
	f.P = p
	return f, nil
}
func (f *Fixture) Open(ctx context.Context, home string) (*engine.Project, error) {
	m, e := config.Install(home)
	if e != nil {
		return nil, e
	}
	dir, e := config.ProjectDir(home, f.Project.ID)
	if e != nil {
		return nil, e
	}
	g, e := gitx.OpenControl(ctx, filepath.Join(dir, "control.git"), f.Remote)
	if e != nil {
		return nil, e
	}
	if e = g.Fetch(ctx); e != nil {
		return nil, e
	}
	cfg, e := engine.Canonical(ctx, g)
	if e != nil {
		return nil, e
	}
	db, e := store.Open(filepath.Join(dir, "state.db"))
	if e != nil {
		return nil, e
	}
	return &engine.Project{Home: home, Dir: dir, Root: f.Source, Repo: "fixture/demo", Remote: f.Remote, Machine: m, Git: g, Config: cfg, DB: db, Hub: f.Hub, Provider: f.Provider}, nil
}

type Hub struct {
	mu     sync.Mutex
	Remote gitx.Git
	issues map[int]github.Issue
	pulls  map[int]github.Pull
	seq    int
}

func (h *Hub) EnsureIssue(_ context.Context, key, title, body string) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for n, i := range h.issues {
		if strings.Contains(i.Body, github.Marker(key)) {
			return n, nil
		}
	}
	h.seq++
	h.issues[h.seq] = github.Issue{Number: h.seq, Body: github.Marker(key) + "\n" + body, State: "open"}
	return h.seq, nil
}
func (h *Hub) UpdateIssue(_ context.Context, n int, body string, closed bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := h.issues[n]
	i.Body = body
	if closed {
		i.State = "closed"
	} else {
		i.State = "open"
	}
	h.issues[n] = i
	return nil
}
func (h *Hub) Issues(_ context.Context) ([]github.Issue, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []github.Issue{}
	for _, i := range h.issues {
		out = append(out, i)
	}
	return out, nil
}
func (h *Hub) EnsurePR(_ context.Context, branch, base, title, body string) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for n, p := range h.pulls {
		if p.Head.Ref == branch {
			return n, nil
		}
	}
	h.seq++
	p := github.Pull{Number: h.seq, State: "open"}
	p.Head.Ref = branch
	p.Base.Ref = base
	h.pulls[h.seq] = p
	return p.Number, nil
}
func (h *Hub) UpdatePR(context.Context, int, string) error { return nil }
func (h *Hub) Pull(ctx context.Context, n int) (github.Pull, error) {
	h.mu.Lock()
	p, ok := h.pulls[n]
	h.mu.Unlock()
	if !ok {
		return p, errors.New("missing mock PR")
	}
	var e error
	p.Head.SHA, e = h.Remote.SHA(ctx, "refs/heads/"+p.Head.Ref)
	if e != nil {
		return p, e
	}
	p.Base.SHA, e = h.Remote.SHA(ctx, "refs/heads/"+p.Base.Ref)
	if e != nil {
		return p, e
	}
	p.Merged = h.Remote.Ancestor(ctx, p.Head.SHA, p.Base.SHA)
	if p.Merged {
		p.State = "closed"
	}
	return p, nil
}

type Worker struct {
	Active   atomic.Int32
	Max      atomic.Int32
	mu       sync.Mutex
	Reviews  []string
	Failures map[string]int
}

func (w *Worker) Name() string                   { return "codex" }
func (w *Worker) Validate(context.Context) error { return nil }
func (w *Worker) Run(ctx context.Context, r provider.Request) (provider.Result, error) {
	result := provider.Result{Schema: 1, Status: "completed", Summary: "Deterministic independent fixture check passed."}
	if r.Role == "orchestrator" {
		for _, key := range []string{"alpha", "beta", "human", "dependent"} {
			p := model.PlanTask{Key: key, Title: key, Objective: "Create " + key + " fixture", Acceptance: []string{"feature-" + key + ".txt contains implemented"}, Areas: []string{key}, Domains: []string{key}, Risk: "low"}
			if key == "dependent" {
				p.Dependencies = []string{"alpha"}
			}
			result.Plan = append(result.Plan, p)
		}
		return result, nil
	}
	task, e := Task(r.Prompt)
	if e != nil {
		return result, e
	}
	if r.Role == "implementer" {
		n := w.Active.Add(1)
		defer w.Active.Add(-1)
		for {
			old := w.Max.Load()
			if n <= old || w.Max.CompareAndSwap(old, n) {
				break
			}
		}
		if task.Title == "human" && len(task.Decisions) == 0 {
			result.Status = "blocked"
			result.Question = "May the fixture create feature-human.txt?"
			return result, nil
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		w.mu.Lock()
		fail := w.Failures[task.Title] > 0
		if fail {
			w.Failures[task.Title]--
		}
		w.mu.Unlock()
		if fail {
			return result, errors.New("injected provider failure")
		}
		if e = os.WriteFile(filepath.Join(r.Directory, "feature-"+task.Title+".txt"), []byte("implemented\n"), 0600); e != nil {
			return result, e
		}
		result.Summary = "Created feature-" + task.Title + ".txt"
		return result, nil
	}
	if r.Role == "reviewer" || r.Role == "qa" {
		b, e := os.ReadFile(filepath.Join(r.Directory, "feature-"+task.Title+".txt"))
		if e != nil || strings.TrimSpace(string(b)) != "implemented" {
			return result, errors.New("mock reviewer observed missing feature")
		}
		w.mu.Lock()
		w.Reviews = append(w.Reviews, r.Role+":"+task.Title+":"+task.BaseSHA)
		w.mu.Unlock()
	}
	return result, nil
}
func Task(prompt string) (*model.Task, error) {
	_, after, ok := strings.Cut(prompt, "ASSIGNED TASK\n")
	if !ok {
		return nil, errors.New("mock missing task context")
	}
	var t model.Task
	e := json.NewDecoder(strings.NewReader(after)).Decode(&t)
	return &t, e
}
func Check(dir string) error {
	files, e := filepath.Glob(filepath.Join(dir, "feature-*.txt"))
	if e != nil {
		return e
	}
	for _, name := range files {
		b, e := os.ReadFile(name)
		if e != nil || strings.TrimSpace(string(b)) != "implemented" {
			return fmt.Errorf("fixture acceptance failed for %s", filepath.Base(name))
		}
	}
	return nil
}
func Run(ctx context.Context, out io.Writer, checks []string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	root, e := os.MkdirTemp("", "aih-demo-")
	if e != nil {
		return "", e
	}
	f, e := New(ctx, root, checks)
	if e != nil {
		return root, e
	}
	defer func() {
		if f.P != nil {
			_ = f.P.DB.Close()
		}
	}()
	if e = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "run", Payload: "Create independent fixtures, one blocked task, and one dependent task."}); e != nil {
		return root, e
	}
	controller := engine.New(f.P)
	done := make(chan error, 1)
	go func() { done <- controller.Serve(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	fmt.Fprintln(out, "Running real supervisor with mock intelligence and GitHub, using local Git.")
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var saved *model.Snapshot
	for saved == nil {
		select {
		case e = <-done:
			return root, fmt.Errorf("supervisor ended before demo completed: %w", e)
		case <-ctx.Done():
			return root, ctx.Err()
		case <-ticker.C:
			s, _, se := f.P.DB.Load()
			if se != nil {
				continue
			}
			complete, blocked := 0, 0
			for _, t := range s.Tasks {
				if t.State == model.Done {
					complete++
				}
				if t.State == model.Blocked {
					blocked++
				}
			}
			if complete == 3 && blocked == 1 {
				saved = s
			}
		}
	}
	if e = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"}); e != nil {
		return root, e
	}
	select {
	case e = <-done:
		if e != nil {
			return root, e
		}
	case <-ctx.Done():
		return root, ctx.Err()
	}
	fmt.Fprintln(out, "Three tasks DONE; one BLOCKED_HUMAN; dependencies and merge train completed.")
	projectDir := f.P.Dir
	if e = f.P.DB.Close(); e != nil {
		return root, e
	}
	f.P = nil
	rel, e := filepath.Rel(root, projectDir)
	if e != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return root, errors.New("unsafe demo recovery deletion target")
	}
	if e = os.RemoveAll(projectDir); e != nil {
		return root, e
	}
	p, e := f.Open(ctx, filepath.Join(root, "machine-b"))
	if e != nil {
		return root, e
	}
	defer p.DB.Close()
	if e = p.Attach(ctx); e != nil {
		return root, e
	}
	recovered, _, e := p.DB.Load()
	if e != nil {
		return root, e
	}
	for id, t := range saved.Tasks {
		if recovered.Tasks[id] == nil || recovered.Tasks[id].State != t.State {
			return root, errors.New("recovery lost logical task state")
		}
	}
	fmt.Fprintln(out, "Deleted the complete machine-A project directory; machine B reconstructed every task and blocker.")
	return root, nil
}
