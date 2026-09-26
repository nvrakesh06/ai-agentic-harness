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
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxTimeoutDiagnosticBytes = 16 * 1024

const (
	timeoutDiagnosticDBBudget = 250 * time.Millisecond
	timeoutDiagnosticTextMax  = 512
	timeoutDiagnosticTasksMax = 32
	timeoutDiagnosticRunsMax  = 32
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
	f.Project.Scheduling.UnderutilizationGraceSeconds = 0
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
	mu      sync.Mutex
	Remote  gitx.Git
	issues  map[int]github.Issue
	pulls   map[int]github.Pull
	updates []PullUpdate
	seq     int
}

type PullUpdate struct {
	Number int
	Body   string
	Draft  bool
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
	p := github.Pull{Number: h.seq, State: "open", Draft: true, Body: body}
	p.Head.Ref = branch
	p.Base.Ref = base
	h.pulls[h.seq] = p
	h.updates = append(h.updates, PullUpdate{Number: p.Number, Body: p.Body, Draft: p.Draft})
	return p.Number, nil
}
func (h *Hub) UpdatePR(_ context.Context, n int, body string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.pulls[n]
	if !ok {
		return errors.New("missing mock PR")
	}
	p.Body = body
	h.pulls[n] = p
	h.updates = append(h.updates, PullUpdate{Number: p.Number, Body: p.Body, Draft: p.Draft})
	return nil
}
func (h *Hub) SetPRDraft(_ context.Context, n int, draft bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.pulls[n]
	if !ok {
		return errors.New("missing mock PR")
	}
	p.Draft = draft
	h.pulls[n] = p
	h.updates = append(h.updates, PullUpdate{Number: p.Number, Body: p.Body, Draft: p.Draft})
	return nil
}
func (h *Hub) Pulls() ([]github.Pull, []PullUpdate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pulls := make([]github.Pull, 0, len(h.pulls))
	for _, p := range h.pulls {
		pulls = append(pulls, p)
	}
	return pulls, append([]PullUpdate(nil), h.updates...)
}
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
	Active            atomic.Int32
	Max               atomic.Int32
	ReviewActive      atomic.Int32
	ReviewMax         atomic.Int32
	mu                sync.Mutex
	Reviews           []string
	Failures          map[string]int
	AuthFailures      map[string]int
	EnvironmentBlocks map[string]int
	Implementations   map[string]int
	Advisors          map[string]int
	NoChanges         map[string]bool
	ScratchTooling    map[string]bool
}

func (w *Worker) ImplementationCount(title string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Implementations[title]
}

func (w *Worker) AdvisorCount(title string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Advisors[title]
}

func (w *Worker) Name() string                   { return "codex" }
func (w *Worker) Validate(context.Context) error { return nil }
func (w *Worker) Run(ctx context.Context, r provider.Request) (provider.Result, error) {
	result := provider.Result{Schema: 1, Status: "completed", Summary: "Deterministic independent fixture check passed."}
	if r.Role == "orchestrator" {
		for _, key := range []string{"alpha", "beta", "human", "dependent"} {
			p := model.PlanTask{Key: key, Title: key, Objective: "Create " + key + " fixture", Acceptance: []string{"feature-" + key + ".txt contains implemented"}, Areas: []string{"feature-" + key + ".txt"}, Domains: []string{key}, Risk: "low"}
			// This recovery demo exercises a serial merge train. The separate
			// batch lifecycle fixture covers low-risk grouped integration.
			if key == "beta" {
				p.Risk = "medium"
			}
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
		if w.Implementations == nil {
			w.Implementations = map[string]int{}
		}
		w.Implementations[task.Title]++
		fail := w.Failures[task.Title] > 0
		if fail {
			w.Failures[task.Title]--
		}
		authFailure := w.AuthFailures[task.Title] > 0
		if authFailure {
			w.AuthFailures[task.Title]--
		}
		environmentBlock := w.EnvironmentBlocks[task.Title] > 0
		if environmentBlock {
			w.EnvironmentBlocks[task.Title]--
		}
		w.mu.Unlock()
		if fail {
			return result, errors.New("injected provider failure")
		}
		if authFailure {
			return result, &provider.InvocationError{Cause: errors.New("fixture provider invocation failed"), Failure: provider.FailureAuthentication}
		}
		if w.NoChanges[task.Title] {
			result.Summary = "Completed without source changes"
			return result, nil
		}
		if e = os.WriteFile(filepath.Join(r.Directory, "feature-"+task.Title+".txt"), []byte("implemented\n"), 0600); e != nil {
			return result, e
		}
		if w.ScratchTooling[task.Title] {
			if r.Scratch == "" {
				return result, errors.New("fixture worker was not given external scratch")
			}
			tooling := filepath.Join(r.Scratch, "npm-cache", "node_modules", "fixture-tool")
			if e = os.MkdirAll(tooling, 0700); e != nil {
				return result, e
			}
			if e = os.WriteFile(filepath.Join(tooling, "README.md"), []byte("token=sk-abcdefghijklmnopqrstuvwxyz012345"), 0600); e != nil {
				return result, e
			}
		}
		result.Summary = "Created feature-" + task.Title + ".txt"
		if environmentBlock {
			result.Status = "blocked"
			result.Summary += "; implementation is complete but the worker cannot run the required native verification"
			result.Risks = []string{"Supervisor-owned native verification remains."}
		}
		return result, nil
	}
	if r.Role == "advisor" {
		w.mu.Lock()
		if w.Advisors == nil {
			w.Advisors = map[string]int{}
		}
		w.Advisors[task.Title]++
		w.mu.Unlock()
	}
	if r.Role == "reviewer" || r.Role == "qa" {
		n := w.ReviewActive.Add(1)
		defer w.ReviewActive.Add(-1)
		for {
			old := w.ReviewMax.Load()
			if n <= old || w.ReviewMax.CompareAndSwap(old, n) {
				break
			}
		}
		deadline := time.NewTimer(5 * time.Second)
		ticker := time.NewTicker(10 * time.Millisecond)
	waitForPeer:
		for w.ReviewActive.Load() < 2 {
			select {
			case <-ctx.Done():
				deadline.Stop()
				ticker.Stop()
				return result, ctx.Err()
			case <-deadline.C:
				break waitForPeer
			case <-ticker.C:
			}
		}
		deadline.Stop()
		ticker.Stop()
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
	phase := "waiting for the initial workflow to complete"
	phaseStarted := time.Now().UTC()
	setPhase := func(next string) {
		phase = next
		phaseStarted = time.Now().UTC()
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var saved *model.Snapshot
	for saved == nil {
		select {
		case e = <-done:
			return root, fmt.Errorf("supervisor ended before demo completed: %w", e)
		case <-ctx.Done():
			return root, demoTimeout(ctx.Err(), phase, phaseStarted, f.P)
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
	if got := f.Provider.ReviewMax.Load(); got != int32(f.Project.MaxReaders) {
		return root, fmt.Errorf("parallel review roles used %d readers, want configured limit %d", got, f.Project.MaxReaders)
	}
	fmt.Fprintf(out, "Independent review roles used %d bounded reader slots.\n", f.Provider.ReviewMax.Load())
	setPhase("waiting for the supervisor handoff")
	if e = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"}); e != nil {
		return root, e
	}
	select {
	case e = <-done:
		if e != nil {
			return root, e
		}
	case <-ctx.Done():
		return root, demoTimeout(ctx.Err(), phase, phaseStarted, f.P)
	}
	fmt.Fprintln(out, "Three tasks DONE; one BLOCKED_HUMAN; dependencies and merge train completed.")
	setPhase("reconstructing the deleted machine-A project on machine B")
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

// demoTimeout adds the last portable durable state to the outer deadline. The
// deterministic demo intentionally drives concurrent controller paths, so this
// is the only evidence left when a worker, Git operation, or native check stops
// making progress before the normal state-transition assertions can run.
func demoTimeout(cause error, phase string, phaseStarted time.Time, p *engine.Project) error {
	diagnostic := map[string]any{
		"phase":         phase,
		"phase_started": phaseStarted.UTC().Format(time.RFC3339Nano),
		"timed_out_at":  time.Now().UTC().Format(time.RFC3339Nano),
	}
	if p == nil || p.DB == nil {
		diagnostic["state_error"] = "local project database is unavailable"
	} else {
		readCtx, cancel := context.WithTimeout(context.Background(), timeoutDiagnosticDBBudget)
		defer cancel()
		snapshot, head, err := p.DB.LoadContext(readCtx)
		if err != nil {
			diagnostic["state_error"] = diagnosticText(err.Error())
		} else {
			diagnostic["state_head"] = diagnosticText(head)
			diagnostic["snapshot"] = summarizeTimeoutSnapshot(snapshot)
			if lastError, readErr := p.DB.GetContext(readCtx, "last_error"); readErr != nil {
				diagnostic["runtime_error"] = diagnosticText(readErr.Error())
			} else {
				diagnostic["last_error"] = diagnosticText(lastError)
			}
			if heartbeat, readErr := p.DB.GetContext(readCtx, engine.LocalLeaseHeartbeatKey); readErr != nil {
				diagnostic["runtime_error"] = diagnosticText(readErr.Error())
			} else {
				diagnostic["lease_local_heartbeat"] = diagnosticText(heartbeat)
			}
		}
	}
	b, err := json.MarshalIndent(diagnostic, "", "  ")
	if err != nil {
		return fmt.Errorf("%w\ndemo timeout diagnostics could not be encoded: %v", cause, err)
	}
	text := safety.Redact(string(b))
	if len(text) > maxTimeoutDiagnosticBytes {
		text = text[:maxTimeoutDiagnosticBytes] + "\n[diagnostics truncated]"
	}
	return fmt.Errorf("%w\ndemo timeout diagnostics:\n%s", cause, text)
}

type timeoutSnapshot struct {
	Schema             int             `json:"state_schema"`
	Revision           uint64          `json:"revision"`
	Controller         timeoutLease    `json:"controller"`
	IntegrationBlocked string          `json:"integration_blocked,omitempty"`
	Capacity           timeoutCapacity `json:"capacity"`
	Tasks              []timeoutTask   `json:"tasks"`
	Runs               []timeoutRun    `json:"runs"`
	TaskCount          int             `json:"task_count"`
	RunCount           int             `json:"run_count"`
}

type timeoutCapacity struct {
	State        string                    `json:"state"`
	ReasonCode   string                    `json:"reason_code,omitempty"`
	Verification []model.VerificationCheck `json:"verification,omitempty"`
}

type timeoutLease struct {
	Machine   string    `json:"machine_id"`
	Owner     string    `json:"owner"`
	Epoch     uint64    `json:"lease_epoch"`
	Heartbeat time.Time `json:"last_heartbeat"`
	Expires   time.Time `json:"expires_at"`
}

type timeoutTask struct {
	ID      string      `json:"id"`
	State   model.State `json:"state"`
	RunID   string      `json:"run_id,omitempty"`
	Updated time.Time   `json:"updated"`
}

type timeoutRun struct {
	Task    string    `json:"task"`
	Role    string    `json:"role"`
	Started time.Time `json:"started"`
	Outcome string    `json:"outcome"`
}

func summarizeTimeoutSnapshot(snapshot *model.Snapshot) timeoutSnapshot {
	if snapshot == nil {
		return timeoutSnapshot{}
	}
	summary := timeoutSnapshot{
		Schema:   snapshot.Schema,
		Revision: snapshot.Revision,
		Controller: timeoutLease{
			Machine:   diagnosticText(snapshot.Controller.Machine),
			Owner:     diagnosticText(snapshot.Controller.Owner),
			Epoch:     snapshot.Controller.Epoch,
			Heartbeat: snapshot.Controller.Heartbeat,
			Expires:   snapshot.Controller.Expires,
		},
		IntegrationBlocked: diagnosticText(snapshot.IntegrationBlocked),
		Capacity: timeoutCapacity{
			State:      diagnosticText(snapshot.Capacity.State),
			ReasonCode: diagnosticText(snapshot.Capacity.ReasonCode),
		},
		TaskCount: len(snapshot.Tasks),
		RunCount:  len(snapshot.Runs),
	}
	for _, check := range snapshot.Capacity.Verification {
		if len(summary.Capacity.Verification) == timeoutDiagnosticTasksMax {
			break
		}
		check.Task = diagnosticText(check.Task)
		check.Check = diagnosticText(check.Check)
		check.Class = diagnosticText(check.Class)
		check.Phase = diagnosticText(check.Phase)
		summary.Capacity.Verification = append(summary.Capacity.Verification, check)
	}
	tasks := make([]*model.Task, 0, len(snapshot.Tasks))
	for _, task := range snapshot.Tasks {
		if task != nil {
			tasks = append(tasks, task)
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	for _, task := range tasks {
		if len(summary.Tasks) == timeoutDiagnosticTasksMax {
			break
		}
		summary.Tasks = append(summary.Tasks, timeoutTask{ID: diagnosticText(task.ID), State: task.State, RunID: diagnosticText(task.RunID), Updated: task.Updated})
	}
	start := len(snapshot.Runs) - timeoutDiagnosticRunsMax
	if start < 0 {
		start = 0
	}
	for _, run := range snapshot.Runs[start:] {
		summary.Runs = append(summary.Runs, timeoutRun{Task: diagnosticText(run.Task), Role: diagnosticText(run.Role), Started: run.Started, Outcome: diagnosticText(run.Outcome)})
	}
	return summary
}

func diagnosticText(value string) string {
	value = safety.Redact(value)
	if len(value) > timeoutDiagnosticTextMax {
		value = value[:timeoutDiagnosticTextMax] + "[truncated]"
	}
	return value
}
