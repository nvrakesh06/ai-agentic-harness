package engine_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

type heldPreflightProvider struct {
	designers atomic.Int32
	writers   atomic.Int32
}

type checkpointContinuationProvider struct {
	designers atomic.Int32
	writers   atomic.Int32
	recovered bool
}

func (*checkpointContinuationProvider) Name() string                   { return "codex" }
func (*checkpointContinuationProvider) Validate(context.Context) error { return nil }
func (p *checkpointContinuationProvider) Run(ctx context.Context, request provider.Request) (provider.Result, error) {
	switch request.Role {
	case "designer":
		p.designers.Add(1)
		return provider.Result{Schema: 1, Status: "completed", Summary: "Use the existing layout primitive."}, nil
	case "implementer":
		if p.writers.Add(1) == 1 {
			if err := os.WriteFile(filepath.Join(request.Directory, "checkpoint-continuation.txt"), []byte("durable checkpoint\n"), 0o644); err != nil {
				return provider.Result{}, err
			}
			return provider.Result{Schema: 1, Status: "in_progress", Summary: "A coherent source checkpoint is ready; continue the same repair.", RecoveredDeadlineHandoff: p.recovered}, nil
		}
		<-ctx.Done()
		return provider.Result{}, ctx.Err()
	default:
		return provider.Result{Schema: 1, Status: "completed"}, nil
	}
}

type evidenceLoopProvider struct {
	designers atomic.Int32
	writers   atomic.Int32
	decision  bool
}

func (p *evidenceLoopProvider) Name() string                   { return "codex" }
func (p *evidenceLoopProvider) Validate(context.Context) error { return nil }
func (p *evidenceLoopProvider) Run(ctx context.Context, request provider.Request) (provider.Result, error) {
	switch request.Role {
	case "designer":
		p.designers.Add(1)
		if p.decision {
			return provider.Result{Schema: 1, Status: "in_progress", Question: "Provide an exact-head screenshot, then choose whether the product should permit clipping this caption.", Summary: "A product decision is required before changing the presentation.", Findings: preflightEvidenceLoopFindings()}, nil
		}
		return provider.Result{Schema: 1, Status: "in_progress", Question: "Please provide an exact-head rendered frame or Playwright screenshot for this visual review.", Summary: "The restricted designer cannot launch Playwright, but found two source defects.", Findings: preflightEvidenceLoopFindings()}, nil
	case "implementer":
		p.writers.Add(1)
		<-ctx.Done()
		return provider.Result{}, ctx.Err()
	default:
		return provider.Result{Schema: 1, Status: "completed"}, nil
	}
}

func preflightEvidenceLoopFindings() []model.Finding {
	return []model.Finding{
		{Severity: "medium", Category: "layout validation", Location: "src/engine/layout.ts:462", Reason: "The measured label path does not reject a narrow overflow.", Resolution: "Add the existing narrow-width validation before rendering the label."},
		{Severity: "medium", Category: "schema compatibility", Location: "src/project-model/schemas.ts:26", Reason: "The scene schema omits the compatible text-fit field used by the renderer.", Resolution: "Add the compatible optional field and validate it with the existing schema test."},
		{Severity: "high", Category: "visual verification", Location: "Rendered-frame evidence for head dae939776385f468aaf0818940925f384927782e", Reason: "No exact-head rendered frames or browser capture were available.", Resolution: "Have the supervisor supply native captures of healthy, timeout, failure, and rebalance frames for final visual review."},
	}
}

func TestPreflightEvidenceLoopAdmitsOneFixWithoutRepeatingDesigner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	s, old, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := engine.Canonical(ctx, f.P.Git)
	if err != nil {
		t.Fatal(err)
	}
	base := effective.BaseSHA
	task := &model.Task{ID: "ui", Title: "ui", Objective: "Repair the caption layout", Acceptance: []string{"caption fits"}, Areas: []string{"src/labels.tsx"}, Domains: []string{"ui"}, Risk: "low", UI: true, State: model.Ready, Branch: "aih/ui", BaseSHA: base}
	s.Tasks[task.ID] = task
	if err = f.P.Git.Worktree(ctx, f.P.TaskPath(task), task.Branch, base); err != nil {
		t.Fatal(err)
	}
	next, err := f.P.Git.StateCommit(ctx, old, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: old, New: next}}); err != nil {
		t.Fatal(err)
	}
	workers := &evidenceLoopProvider{}
	f.P.Provider = workers
	c := engine.New(f.P)
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx) }()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for workers.writers.Load() == 0 {
		select {
		case err := <-done:
			t.Fatalf("supervisor exited before admitting evidence-guided implementer fix: %v", err)
		case <-deadline.C:
			current := c.Snapshot().Tasks[task.ID]
			t.Fatalf("designer calls=%d writer calls=%d state=%s preflight=%+v findings=%+v decisions=%q", workers.designers.Load(), workers.writers.Load(), current.State, current.Preflight, current.Findings, current.Decisions)
		case <-time.After(25 * time.Millisecond):
		}
	}
	current := c.Snapshot().Tasks[task.ID]
	if workers.designers.Load() != 1 || current.Preflight == nil || current.Preflight.Config != effective.Hash || current.VisualRequired == nil || current.VisualRequired.Head != base || !containsDecision(current.Decisions, "Final visual review must still use rendered evidence") || len(current.Findings) != 2 || current.Findings[1].Category != "schema compatibility" {
		t.Fatalf("evidence-guided fix did not preserve exact-head guidance or bounded admission: designers=%d task=%#v", workers.designers.Load(), current)
	}
	cancel()
	<-done
	persisted, _, err := f.P.Git.Load(context.Background())
	if err != nil || persisted.Tasks[task.ID].VisualRequired == nil || persisted.Tasks[task.ID].VisualRequired.Head == "" {
		t.Fatalf("headless task lost a durable exact-head visual requirement after recovery: %#v %v", persisted.Tasks[task.ID], err)
	}
}

func TestPreflightEvidenceLoopBlocksProductDecisionWithoutImplementer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	s, old, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := engine.Canonical(ctx, f.P.Git)
	if err != nil {
		t.Fatal(err)
	}
	base := effective.BaseSHA
	task := &model.Task{ID: "ui", Title: "ui", Objective: "Repair the caption layout", Acceptance: []string{"caption fits"}, Areas: []string{"src/labels.tsx"}, Domains: []string{"ui"}, Risk: "low", UI: true, State: model.Fix, Branch: "aih/ui", BaseSHA: base, HeadSHA: base, Attempts: 1}
	s.Tasks[task.ID] = task
	if err = f.P.Git.Worktree(ctx, f.P.TaskPath(task), task.Branch, base); err != nil {
		t.Fatal(err)
	}
	next, err := f.P.Git.StateCommit(ctx, old, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: old, New: next}}); err != nil {
		t.Fatal(err)
	}
	workers := &evidenceLoopProvider{decision: true}
	f.P.Provider = workers
	c := engine.New(f.P)
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx) }()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		snapshot := c.Snapshot()
		if snapshot == nil {
			continue
		}
		current := snapshot.Tasks[task.ID]
		if current == nil {
			continue
		}
		if current.State == model.Blocked {
			if workers.designers.Load() != 1 || workers.writers.Load() != 0 {
				t.Fatalf("product decision retried specialist or admitted writer: designers=%d writers=%d task=%#v", workers.designers.Load(), workers.writers.Load(), current)
			}
			cancel()
			<-done
			return
		}
		select {
		case err := <-done:
			t.Fatalf("supervisor exited before preserving product decision: %v", err)
		case <-deadline.C:
			t.Fatalf("product decision was not blocked: designers=%d writers=%d snapshot=%#v", workers.designers.Load(), workers.writers.Load(), current)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func containsDecision(decisions []string, want string) bool {
	for _, decision := range decisions {
		if strings.Contains(decision, want) {
			return true
		}
	}
	return false
}

func TestRecoveredCompletedDesignerIsNotRunAgain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	s, old, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := engine.Canonical(ctx, f.P.Git)
	if err != nil {
		t.Fatal(err)
	}
	base := effective.BaseSHA
	s.Tasks["ui"] = &model.Task{ID: "ui", Title: "ui", Objective: "Fixture UI", Acceptance: []string{"works"},
		Areas: []string{"ui"}, Domains: []string{"ui"}, Risk: "low", UI: true,
		State: model.Ready, Branch: "aih/ui", BaseSHA: base, HeadSHA: base,
		Preflight: &model.Preflight{Phase: "waiting", BaseSHA: base, HeadSHA: base,
			Config: effective.Hash, Rules: roles.Hash(), Completed: []string{"designer"}}}
	if err = f.P.Git.Worktree(ctx, f.P.TaskPath(s.Tasks["ui"]), s.Tasks["ui"].Branch, base); err != nil {
		t.Fatal(err)
	}
	next, err := f.P.Git.StateCommit(ctx, old, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: old, New: next}}); err != nil {
		t.Fatal(err)
	}
	workers := &heldPreflightProvider{}
	f.P.Provider = workers
	c := engine.New(f.P)
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx) }()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for workers.writers.Load() == 0 {
		select {
		case err := <-done:
			t.Fatalf("supervisor exited before resumed writer: %v", err)
		case <-deadline.C:
			t.Fatal("prepared UI task did not resume after restart")
		case <-time.After(25 * time.Millisecond):
		}
	}
	if workers.designers.Load() != 0 {
		t.Fatal("completed designer guidance was duplicated")
	}
	cancel()
	<-done
}

func TestHumanExactHeadCheckpointContinuationReusesPreflightOnlyForStructuredAcknowledgement(t *testing.T) {
	for _, test := range []struct {
		name            string
		answer          func(string) string
		wantDesigners   int32
		wantWriters     int32
		wantReuseReason string
		blockerOrigin   string
		resume          model.State
	}{
		{"exact verification acknowledgement", func(head string) string { return "AIH-CONTINUE CHECKPOINT " + head }, 0, 1, "human checkpoint", model.BlockerOriginVerificationOnly, model.SyncRequired},
		{"freeform verification answer", func(string) string { return "The verification environment is repaired; recheck." }, 1, 0, "", model.BlockerOriginVerificationOnly, model.Ready},
		{"exact acknowledgement cannot bypass implementer decision", func(head string) string { return "AIH-CONTINUE CHECKPOINT " + head }, 1, 0, "", model.BlockerOriginImplementerDecision, model.Ready},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
			if err != nil {
				t.Fatal(err)
			}
			defer f.P.DB.Close()
			s, old, err := f.P.Git.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			effective, err := engine.Canonical(ctx, f.P.Git)
			if err != nil {
				t.Fatal(err)
			}
			base := effective.BaseSHA
			task := &model.Task{ID: "ui", Title: "ui", Objective: "Repair the caption layout", Acceptance: []string{"caption fits"}, Areas: []string{"src/labels.tsx"}, AssignedAreas: []string{"src/labels.tsx"}, AssignedAreaKinds: map[string]string{"src/labels.tsx": model.AreaFile}, Domains: []string{"ui"}, Risk: "low", UI: true, State: model.Blocked, Branch: "aih/ui", BaseSHA: base, HeadSHA: base,
				Blocker: &model.Blocker{Question: "Confirm the verification-only unblock.", Reason: "Native verification was interrupted.", Resume: test.resume, Origin: test.blockerOrigin}}
			task.Preflight = &model.Preflight{Phase: "writing", BaseSHA: base, HeadSHA: base, Config: effective.Hash, Rules: roles.Hash(), Scope: engine.PreflightScopeForTest(task, effective), Completed: []string{"designer"}}
			s.Tasks[task.ID] = task
			if err = f.P.Git.Worktree(ctx, f.P.TaskPath(task), task.Branch, base); err != nil {
				t.Fatal(err)
			}
			next, err := f.P.Git.StateCommit(ctx, old, s)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: old, New: next}}); err != nil {
				t.Fatal(err)
			}
			workers := &heldPreflightProvider{}
			f.P.Provider = workers
			c := engine.New(f.P)
			done := make(chan error, 1)
			go func() { done <- c.Serve(ctx) }()
			if err = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "answer", Target: task.ID, Payload: test.answer(base)}); err != nil {
				t.Fatal(err)
			}
			deadline := time.NewTimer(30 * time.Second)
			defer deadline.Stop()
			for workers.designers.Load() < test.wantDesigners || workers.writers.Load() < test.wantWriters {
				select {
				case err := <-done:
					t.Fatalf("supervisor exited before resumed work: %v", err)
				case <-deadline.C:
					current := c.Snapshot().Tasks[task.ID]
					t.Fatalf("human unblock did not take the expected route: designers=%d writers=%d task=%#v", workers.designers.Load(), workers.writers.Load(), current)
				case <-time.After(25 * time.Millisecond):
				}
			}
			current := c.Snapshot().Tasks[task.ID]
			if workers.designers.Load() != test.wantDesigners || workers.writers.Load() != test.wantWriters || (test.wantReuseReason != "" && (current.Preflight == nil || !strings.Contains(current.Preflight.ReuseReason, test.wantReuseReason))) {
				t.Fatalf("human unblock did not preserve the intended preflight policy: designers=%d writers=%d task=%#v", workers.designers.Load(), workers.writers.Load(), current)
			}
			cancel()
			<-done
		})
	}
}

func TestImplementerCheckpointContinuesWithoutRepeatingPreflight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	s, old, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := engine.Canonical(ctx, f.P.Git)
	if err != nil {
		t.Fatal(err)
	}
	task := &model.Task{ID: "ui", Title: "ui", Objective: "Repair the caption layout", Acceptance: []string{"caption fits"}, Areas: []string{"src/labels.tsx"}, Domains: []string{"ui"}, Risk: "low", UI: true, State: model.Ready, Branch: "aih/ui", BaseSHA: effective.BaseSHA}
	s.Tasks[task.ID] = task
	if err = f.P.Git.Worktree(ctx, f.P.TaskPath(task), task.Branch, effective.BaseSHA); err != nil {
		t.Fatal(err)
	}
	next, err := f.P.Git.StateCommit(ctx, old, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: old, New: next}}); err != nil {
		t.Fatal(err)
	}
	workers := &checkpointContinuationProvider{}
	f.P.Provider = workers
	c := engine.New(f.P)
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx) }()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for workers.writers.Load() < 2 {
		select {
		case err := <-done:
			t.Fatalf("supervisor exited before checkpoint continuation: %v", err)
		case <-deadline.C:
			current := c.Snapshot().Tasks[task.ID]
			t.Fatalf("checkpoint did not resume writer directly: designers=%d writers=%d task=%#v", workers.designers.Load(), workers.writers.Load(), current)
		case <-time.After(25 * time.Millisecond):
		}
	}
	current := c.Snapshot().Tasks[task.ID]
	if workers.designers.Load() != 1 || current.Preflight == nil || current.Preflight.Phase != "writing" || !strings.Contains(current.Preflight.ReuseReason, "checkpoint") || !containsDecision(current.Decisions, "Checkpoint continuation") {
		t.Fatalf("checkpoint continuation lost durable guidance provenance: designers=%d task=%#v", workers.designers.Load(), current)
	}
	cancel()
	<-done
}

func TestRecoveredDeadlineCheckpointContinuesWithoutRepeatingPreflight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	s, old, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := engine.Canonical(ctx, f.P.Git)
	if err != nil {
		t.Fatal(err)
	}
	task := &model.Task{ID: "ui", Title: "ui", Objective: "Repair the caption layout", Acceptance: []string{"caption fits"}, Areas: []string{"src/labels.tsx"}, Domains: []string{"ui"}, Risk: "low", UI: true, State: model.Ready, Branch: "aih/ui", BaseSHA: effective.BaseSHA}
	s.Tasks[task.ID] = task
	if err = f.P.Git.Worktree(ctx, f.P.TaskPath(task), task.Branch, effective.BaseSHA); err != nil {
		t.Fatal(err)
	}
	next, err := f.P.Git.StateCommit(ctx, old, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: old, New: next}}); err != nil {
		t.Fatal(err)
	}
	workers := &checkpointContinuationProvider{recovered: true}
	f.P.Provider = workers
	c := engine.New(f.P)
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx) }()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for workers.writers.Load() < 2 {
		select {
		case err := <-done:
			t.Fatalf("supervisor exited before recovered checkpoint continuation: %v", err)
		case <-deadline.C:
			current := c.Snapshot().Tasks[task.ID]
			t.Fatalf("recovered checkpoint did not resume writer directly: designers=%d writers=%d task=%#v", workers.designers.Load(), workers.writers.Load(), current)
		case <-time.After(25 * time.Millisecond):
		}
	}
	current := c.Snapshot().Tasks[task.ID]
	if workers.designers.Load() != 1 || current.Preflight == nil || current.Preflight.Phase != "writing" || !containsDecision(current.Decisions, "Recovered checkpoint continuation") {
		t.Fatalf("recovered checkpoint lost durable guidance provenance: designers=%d task=%#v", workers.designers.Load(), current)
	}
	cancel()
	<-done
}

func (*heldPreflightProvider) Name() string                   { return "codex" }
func (*heldPreflightProvider) Validate(context.Context) error { return nil }
func (p *heldPreflightProvider) Run(ctx context.Context, request provider.Request) (provider.Result, error) {
	switch request.Role {
	case "designer":
		p.designers.Add(1)
	case "implementer":
		p.writers.Add(1)
	default:
		return provider.Result{Schema: 1, Status: "completed"}, nil
	}
	<-ctx.Done()
	return provider.Result{}, ctx.Err()
}

func TestPreflightReaderQueueLeavesIndependentWriterSlotsAvailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.P.Config.Project.MaxWriters = 2
	f.P.Config.Project.Scheduling.TargetWriters = 2
	s, old, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id string
		ui bool
	}{
		{"ui_a", true}, {"ui_b", true}, {"ui_c", true}, {"code_a", false}, {"code_b", false},
	} {
		s.Tasks[item.id] = &model.Task{ID: item.id, Title: item.id, Objective: "Fixture task " + item.id,
			Acceptance: []string{"fixture succeeds"}, Areas: []string{item.id}, Domains: []string{item.id},
			Risk: "low", UI: item.ui, State: model.Ready, Branch: "aih/" + item.id,
			BaseSHA: base, HeadSHA: base}
		if err = f.P.Git.Worktree(ctx, f.P.TaskPath(s.Tasks[item.id]), s.Tasks[item.id].Branch, base); err != nil {
			t.Fatal(err)
		}
	}
	next, err := f.P.Git.StateCommit(ctx, old, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: old, New: next}}); err != nil {
		t.Fatal(err)
	}
	workers := &heldPreflightProvider{}
	f.P.Provider = workers
	c := engine.New(f.P)
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- c.Serve(ctx) }()
	readerDeadline := time.NewTimer(90 * time.Second)
	defer readerDeadline.Stop()
	for workers.designers.Load() < 2 {
		select {
		case err := <-done:
			t.Fatalf("supervisor exited before reader pressure was established: %v", err)
		case <-readerDeadline.C:
			t.Fatalf("reader pressure was not established: designers=%d writers=%d", workers.designers.Load(), workers.writers.Load())
		case <-time.After(25 * time.Millisecond):
		}
	}
	readerPressureAt := time.Now()
	t.Logf("preflight admission fixture: first two designers active after %s", readerPressureAt.Sub(started).Round(time.Millisecond))
	// Start the starvation observation once reader pressure exists. Real Git and
	// state setup may be slow under load, but it must not consume the interval
	// that proves independent writer admission. A reader-only scheduler cannot
	// satisfy this condition because the held designers never release capacity.
	writerDeadline := time.NewTimer(45 * time.Second)
	defer writerDeadline.Stop()
	for {
		current := c.Snapshot()
		if current != nil && workers.designers.Load() == 2 && workers.writers.Load() == 2 {
			waiting := 0
			for _, task := range current.Tasks {
				if task.UI && task.Preflight != nil && task.Preflight.Phase == "waiting" {
					waiting++
				}
			}
			if waiting > 0 && current.Capacity.ActiveWriters == 2 && current.Capacity.ActiveReaders == 2 {
				t.Logf("preflight admission fixture: two independent writers active %s after reader pressure", time.Since(readerPressureAt).Round(time.Millisecond))
				cancel()
				<-done
				return
			}
		}
		select {
		case err := <-done:
			t.Fatalf("supervisor exited before independent writers started: %v", err)
		case <-writerDeadline.C:
			t.Fatal(fmt.Sprintf("reader queue suppressed writers after reader pressure: designers=%d writers=%d snapshot=%#v", workers.designers.Load(), workers.writers.Load(), current))
		case <-time.After(25 * time.Millisecond):
		}
	}
}
