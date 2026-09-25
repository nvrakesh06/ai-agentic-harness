package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/buildinfo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrLease = errors.New("controller lease unavailable or lost")

const (
	// LocalLeaseHeartbeatKey is machine-local observability. The remote snapshot
	// remains the fencing authority; this value must never authorize publication.
	LocalLeaseHeartbeatKey = "lease_local_heartbeat"
	// LocalSupervisorBuildKey identifies the binary holding supervisor.lock.
	// This is local status evidence, never a fencing or publication authority.
	LocalSupervisorBuildKey = "supervisor_build"
	maxLocalLeasePulse      = 30 * time.Second
)

type Controller struct {
	P           *Project
	mu          sync.Mutex
	gitMu       sync.Mutex
	s           *model.Snapshot
	head, owner string
	ctx         context.Context
	cancel      context.CancelFunc
	fatal       chan error
	readers     chan struct{}
	heavyChecks chan struct{}
	lightChecks chan struct{}
	jobs        sync.WaitGroup
	now         func() time.Time
}

func New(p *Project) *Controller {
	return &Controller{P: p, owner: model.ID(), fatal: make(chan error, 1), readers: make(chan struct{}, p.Config.Project.MaxReaders), heavyChecks: make(chan struct{}, p.Config.Project.Resources.MaxHeavyChecks), lightChecks: make(chan struct{}, p.Config.Project.Resources.MaxLightChecks)}
}
func (c *Controller) Snapshot() *model.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return model.Clone(c.s)
}

func (c *Controller) nowUTC() time.Time {
	if c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

func (c *Controller) leaseDuration() time.Duration {
	return time.Duration(c.P.Config.Project.LeaseSeconds) * time.Second
}

func leasePulseInterval(lease time.Duration) time.Duration {
	interval := lease / 6
	if interval > maxLocalLeasePulse {
		return maxLocalLeasePulse
	}
	if interval < time.Second {
		return time.Second
	}
	return interval
}

func leaseRenewalDue(lease model.Lease, now time.Time, duration time.Duration) bool {
	return !lease.Expires.After(now.Add(duration / 2))
}

func (c *Controller) acquire(ctx context.Context) error {
	s, h, e := c.P.Git.Load(ctx)
	if e != nil {
		return e
	}
	if s.Project != c.P.Config.Project.ID {
		return errors.New("remote project identity mismatch")
	}
	now := c.nowUTC()
	if s.Controller.Owner != "" && s.Controller.Expires.Add(5*time.Second).After(now) {
		return fmt.Errorf("%w: held by %s until %s", ErrLease, s.Controller.Machine, s.Controller.Expires)
	}
	s.Controller = model.Lease{Machine: c.P.Machine.ID, Owner: c.owner, Epoch: s.Controller.Epoch + 1, Heartbeat: now, Expires: now.Add(c.leaseDuration())}
	s.Capacity = configuredCapacity(c.P.Config.Project, s.Capacity)
	s.Revision++
	next, e := c.P.Git.StateCommit(ctx, h, s)
	if e != nil {
		return e
	}
	if e = c.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: h, New: next}}); e != nil {
		return e
	}
	c.s = s
	c.head = next
	if e = c.P.DB.Save(next, s); e != nil {
		return e
	}
	return c.P.DB.Set(LocalLeaseHeartbeatKey, now.Format(time.RFC3339Nano))
}
func (c *Controller) save(ctx context.Context, fn func(*model.Snapshot) error, updates ...gitx.Update) error {
	_, e := c.persist(ctx, fn, updates...)
	return e
}

// persist publishes meaningful state immediately. A semantic no-op only
// publishes when the durable lease has reached half-life, so frequent local
// pulses and duplicate mutations do not create remote history.
func (c *Controller) persist(ctx context.Context, fn func(*model.Snapshot) error, updates ...gitx.Update) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s == nil {
		return false, ErrLease
	}
	now := c.nowUTC()
	if c.s.Controller.Owner != c.owner || !c.s.Controller.Expires.After(now) {
		return false, ErrLease
	}
	next := model.Clone(c.s)
	if e := fn(next); e != nil {
		return false, e
	}
	due := leaseRenewalDue(c.s.Controller, now, c.leaseDuration())
	if len(updates) == 0 && reflect.DeepEqual(c.s, next) {
		if !due {
			return false, nil
		}
		return c.renewLeaseLocked(ctx, now)
	}
	// Keep runtime paths out of portable diagnostic/result text.
	portable, e := json.Marshal(next)
	if e != nil {
		return false, e
	}
	if e = json.Unmarshal([]byte(c.portable(string(portable))), next); e != nil {
		return false, e
	}
	if len(updates) == 0 && reflect.DeepEqual(c.s, next) {
		if !due {
			return false, nil
		}
		return c.renewLeaseLocked(ctx, now)
	}
	if next.Controller.Owner == c.owner {
		next.Controller.Heartbeat = now
		next.Controller.Expires = now.Add(c.leaseDuration())
	}
	next.Revision = c.s.Revision + 1
	newHead, e := c.P.Git.StateCommit(ctx, c.head, next)
	if e != nil {
		return false, e
	}
	all := append([]gitx.Update{{Branch: "aih-state", Old: c.head, New: newHead}}, updates...)
	if e = c.P.Git.Publish(ctx, all); e != nil {
		return false, e
	}
	for id, task := range next.Tasks {
		if old := c.s.Tasks[id]; old == nil || old.State != task.State {
			from := ""
			if old != nil {
				from = string(old.State)
			}
			_ = c.P.DB.Event(id, task.RunID, "", "", "state_transition", from+" -> "+string(task.State))
		}
	}
	c.s = next
	c.head = newHead
	if e = c.P.DB.Save(newHead, next); e != nil {
		return true, e
	}
	_ = c.P.DB.Set(LocalLeaseHeartbeatKey, now.Format(time.RFC3339Nano))
	return true, nil
}

// renewLeaseLocked publishes a dedicated lease commit while c.mu is held. The
// cached snapshot adopts the authoritative renewed lease without changing its
// user-significant state revision.
func (c *Controller) renewLeaseLocked(ctx context.Context, now time.Time) (bool, error) {
	next := model.Clone(c.s)
	next.Controller.Heartbeat = now
	next.Controller.Expires = now.Add(c.leaseDuration())
	newHead, e := c.P.Git.LeaseCommit(ctx, c.head, next)
	if e != nil {
		return false, e
	}
	if e = c.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: c.head, New: newHead}}); e != nil {
		return false, e
	}
	c.s = next
	c.head = newHead
	if e = c.P.DB.Save(newHead, next); e != nil {
		return true, e
	}
	_ = c.P.DB.Set(LocalLeaseHeartbeatKey, now.Format(time.RFC3339Nano))
	return true, nil
}
func (c *Controller) mutate(fn func(*model.Snapshot) error) error {
	e := c.save(c.ctx, fn)
	if e != nil {
		c.fail(e)
	}
	return e
}
func (c *Controller) fail(e error) {
	select {
	case c.fatal <- e:
	default:
	}
	if c.cancel != nil {
		c.cancel()
	}
}
func (c *Controller) fetch(ctx context.Context) error {
	c.gitMu.Lock()
	defer c.gitMu.Unlock()
	return c.P.Git.Fetch(ctx)
}
func (c *Controller) pulseLease(ctx context.Context) (bool, error) {
	now := c.nowUTC()
	if e := c.P.DB.Set(LocalLeaseHeartbeatKey, now.Format(time.RFC3339Nano)); e != nil {
		return false, e
	}
	published, e := c.persist(ctx, func(*model.Snapshot) error { return nil })
	if e != nil {
		return false, e
	}
	if published {
		expires := c.Snapshot().Controller.Expires.Format(time.RFC3339)
		_ = c.P.DB.Event("", "", "", "", "lease_renewed", "durable controller lease extended to "+expires)
	}
	return published, nil
}

func (c *Controller) heartbeat(ctx context.Context) {
	interval := leasePulseInterval(c.leaseDuration())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if _, e := c.pulseLease(ctx); e != nil {
		c.fail(e)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, e := c.pulseLease(ctx); e != nil {
				c.fail(e)
				return
			}
		}
	}
}
func (c *Controller) recover() error {
	return c.mutate(recoverSnapshot)
}
func recoverSnapshot(s *model.Snapshot) error {
	// An interrupted controller no longer owns these commands. The ordinary
	// task recovery route re-runs verification after acquiring fresh slots.
	s.Capacity.Verification = nil
	for _, t := range s.Tasks {
		switch t.State {
		case model.Running:
			if _, assigned := model.ImmutableAreas(t); !assigned {
				model.Block(t, "This interrupted legacy task has no immutable area assignment. Replan it before resuming.", "AIH refuses to reconstruct a started task's historical write scope.", model.Ready)
			} else {
				t.State = model.Ready
			}
		case model.Implemented, model.Verifying, model.Review, model.MergeTrain:
			t.State = model.SyncRequired
		}
		t.RunID = ""
		resetInterruptedPreflight(t.Preflight)
	}
	for i := range s.Runs {
		if s.Runs[i].Outcome == "running" {
			s.Runs[i].Outcome = "interrupted"
		}
	}
	return nil
}
func (c *Controller) Serve(parent context.Context) error {
	lock, e := platform.Acquire(filepath.Join(c.P.Dir, "supervisor.lock"))
	if e != nil {
		return e
	}
	defer lock.Close()
	defer func() {
		if e != nil {
			_ = c.P.DB.Set("last_error", safety.Redact(e.Error()))
		}
	}()
	if e = c.P.Provider.Validate(parent); e != nil {
		return e
	}
	preflightRoles, e := roles.Load(c.P.Config.Files)
	if e != nil {
		return e
	}
	c.ctx, c.cancel = context.WithCancel(parent)
	defer c.cancel()
	if e = c.acquire(parent); e != nil {
		return e
	}
	log.Printf("AIH supervisor started: project=%s epoch=%d", c.s.Project, c.s.Controller.Epoch)
	identity, _ := json.Marshal(buildinfo.Current())
	if e = c.P.DB.Set(LocalSupervisorBuildKey, string(identity)); e != nil {
		return e
	}
	_ = c.P.DB.Set("pid", strconv.Itoa(os.Getpid()))
	_ = c.P.DB.Set("last_error", "")
	defer c.P.DB.Set("pid", "")
	if e = c.recover(); e != nil {
		return e
	}
	hbCtx, hbCancel := context.WithCancel(context.Background())
	hbDone := make(chan struct{})
	defer func() { hbCancel(); <-hbDone }()
	go func() { defer close(hbDone); c.heartbeat(hbCtx) }()
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	active := map[string]bool{}
	guidedPreflights := map[string]bool{}
	done := make(chan string, 64)
	merging := false
	planning := false
	stopping := false
	for !stopping {
		select {
		case <-parent.Done():
			stopping = true
			c.cancel()
		case e = <-c.fatal:
			stopping = true
			c.cancel()
		case id := <-done:
			delete(active, id)
			delete(guidedPreflights, id)
			if strings.HasPrefix(id, "@merge:") {
				delete(active, strings.TrimPrefix(id, "@merge:"))
				merging = false
			}
			if id == "@plan" {
				planning = false
			}
		case <-ticker.C:
			stop, ce := c.commands()
			if ce != nil {
				e = ce
				stopping = true
				break
			}
			if stop {
				stopping = true
				c.cancel()
				break
			}
			s := c.Snapshot()
			capacity := decideCapacity(s, active, c.P.Config.Project, planning, len(c.readers), time.Now().UTC())
			if ce = c.persistCapacity(capacity.status, capacity.planObjective); ce != nil {
				e = ce
				stopping = true
				c.cancel()
				break
			}
			for _, t := range capacity.writers {
				if hasActive(active, t.ID) {
					continue
				}
				id := t.ID
				admitted, ae := c.admitWriter(id, active)
				if ae != nil {
					ce = ae
					break
				}
				if !admitted {
					continue
				}
				active[id] = true
				c.launch(func() { c.work(id, true); done <- id })
			}
			for _, candidate := range selectPreflights(c.Snapshot(), active, guidedPreflights, c.P.Config.Project.MaxReaders, c.P.Config.Project.MaxWriters, preflightRoles) {
				id := candidate.task.ID
				active[id] = false
				guidedPreflights[id] = candidate.guided
				c.launch(func() { c.preflight(id); done <- id })
			}
			if capacity.planObjective != "" {
				id := capacity.planObjective
				planning = true
				c.launch(func() { c.plan(id); done <- "@plan" })
			}
			for _, t := range model.Ordered(s) {
				if hasActive(active, t.ID) {
					continue
				}
				if !dependenciesComplete(s, t) {
					continue
				}
				switch t.State {
				case model.Implemented, model.SyncRequired, model.Verifying, model.Review:
					id := t.ID
					active[id] = true
					c.launch(func() { c.work(id, false); done <- id })
				}
			}
			if !merging {
				for _, t := range model.Ordered(s) {
					if hasActive(active, t.ID) {
						continue
					}
					if t.State == model.PostVerify || (t.State == model.MergeReady && s.IntegrationBlocked == "") {
						id := t.ID
						merging = true
						// Reserve the task for the whole merge workflow, including
						// any fresh-main review. Otherwise the scheduler can launch
						// another review while integration is in progress.
						active[id] = true
						c.launch(func() { c.integrate(id); done <- "@merge:" + id })
						break
					}
				}
			}
		}
	}
	c.cancel()
	c.jobs.Wait()
	// Shutdown checkpoints only after all writers exited. If authority was lost,
	// leave edits local and report them; never publish using a stale epoch.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if e == nil {
		for _, t := range model.Ordered(c.Snapshot()) {
			if t.State == model.Running || t.State == model.Ready || t.State == model.Fix {
				if _, se := os.Stat(c.P.TaskPath(t)); se == nil {
					if se = c.checkpoint(ctx, t.ID); se != nil {
						e = errors.Join(e, se)
					}
				}
			}
		}
	}
	if e == nil {
		for _, task := range model.Ordered(c.Snapshot()) {
			c.mirrorWith(ctx, task.ID)
		}
		hbCancel()
		<-hbDone
		e = c.save(ctx, func(s *model.Snapshot) error {
			s.Controller.Owner = ""
			s.Controller.Expires = time.Now().UTC()
			s.Capacity.ActiveWriters = 0
			s.Capacity.ActivePreflights = 0
			s.Capacity.ActiveReaders = 0
			s.Capacity.Verification = nil
			s.Capacity.State = "stopped"
			s.Capacity.ReasonCode = "supervisor_stopped"
			s.Capacity.Reason = "the local supervisor is stopped"
			s.Capacity.NextSafeWork = ""
			for _, t := range s.Tasks {
				if t.State == model.Running {
					t.State = model.Ready
				}
				resetInterruptedPreflight(t.Preflight)
			}
			for i := range s.Runs {
				if s.Runs[i].Outcome == "running" {
					s.Runs[i].Outcome = "interrupted"
				}
			}
			return nil
		})
	}
	if e != nil {
		_ = c.P.DB.Event("", "", "", "", "supervisor_stopped", safety.Redact(e.Error()))
		return fmt.Errorf("stopped with potentially unpersisted work in %s: %w", filepath.Join(c.P.Dir, "worktrees"), e)
	}
	log.Printf("AIH supervisor stopped: checkpoints persisted and controller lease released")
	return nil
}

func dependenciesComplete(s *model.Snapshot, t *model.Task) bool {
	if t == nil {
		return false
	}
	for _, id := range t.Dependencies {
		if s.Tasks[id] == nil || s.Tasks[id].State != model.Done {
			return false
		}
	}
	return true
}
func (c *Controller) launch(fn func()) { c.jobs.Add(1); go func() { defer c.jobs.Done(); fn() }() }

type guidanceCommand struct {
	Source   string `json:"source_task"`
	Operator bool   `json:"operator,omitempty"`
	Head     string `json:"head,omitempty"`
	Base     string `json:"base,omitempty"`
	Config   string `json:"config,omitempty"`
	Rules    string `json:"rules,omitempty"`
	Text     string `json:"text"`
}
type guidanceRejection struct{ error }

func validateGuidanceCommand(s *model.Snapshot, cmd store.Command, baseSHA, configHash, rulesHash string) (guidanceCommand, error) {
	var guidance guidanceCommand
	if err := json.Unmarshal([]byte(cmd.Payload), &guidance); err != nil {
		return guidance, errors.New("invalid guidance payload")
	}
	if err := safety.Check(guidance.Text); err != nil {
		return guidance, err
	}
	target := s.Tasks[cmd.Target]
	if guidance.Operator {
		if guidance.Base != baseSHA || guidance.Config != configHash || guidance.Rules != rulesHash {
			return guidance, errors.New("operator guidance policy scope is stale")
		}
		if target == nil {
			return guidance, errors.New("operator guidance requires known task ID")
		}
		probe := *target
		probe.Decisions = append([]string(nil), target.Decisions...)
		if err := model.QueueOperatorGuidance(&probe, cmd.ID, guidance.Head, guidance.Base, guidance.Config, guidance.Rules, guidance.Text); err != nil {
			return guidance, err
		}
		return guidance, nil
	}
	source := s.Tasks[guidance.Source]
	if target == nil || source == nil {
		return guidance, errors.New("guidance requires known task IDs")
	}
	probe := *target
	probe.Decisions = append([]string(nil), target.Decisions...)
	if err := model.QueueGuidance(&probe, source, cmd.ID, guidance.Text); err != nil {
		return guidance, err
	}
	return guidance, nil
}

func (c *Controller) commands() (bool, error) {
	commands, e := c.P.DB.Pending()
	if e != nil {
		return false, e
	}
	for _, cmd := range commands {
		if cmd.Kind == "stop" || cmd.Kind == "handoff" {
			_ = c.P.DB.Ack(cmd.ID, "")
			return true, nil
		}
		s := c.Snapshot()
		if s.Applied[cmd.ID] {
			_ = c.P.DB.Ack(cmd.ID, "")
			continue
		}
		if e = safety.Check(cmd.Payload); e != nil {
			_ = c.P.DB.Ack(cmd.ID, e.Error())
			continue
		}
		if cmd.Kind == "answer" {
			task := s.Tasks[cmd.Target]
			if task == nil {
				for _, candidate := range s.Tasks {
					if strconv.Itoa(candidate.Issue) == cmd.Target {
						task = candidate
						break
					}
				}
			}
			if task != nil {
				e = model.Answer(task, cmd.Payload)
			} else if objective := s.Objectives[cmd.Target]; objective == nil || objective.Blocker == "" {
				e = errors.New("unknown blocked task/objective")
			} else {
				e = validateObjectiveAnswer(cmd.Payload)
			}
			if e != nil {
				_ = c.P.DB.Ack(cmd.ID, e.Error())
				continue
			}
		}
		if cmd.Kind == "assign-role" {
			task := s.Tasks[cmd.Target]
			all, err := roles.Load(c.P.Config.Files)
			role, ok := all[cmd.Payload]
			if err != nil || !ok || (role.Stage != "review" && role.Stage != "pre-implementation") || task == nil || task.MergeSHA != "" || (task.State != model.Ready && task.State != model.Fix && task.State != model.Blocked) {
				_ = c.P.DB.Ack(cmd.ID, "assign a known specialist to a READY, FIX, or unmerged BLOCKED task using its task ID")
				continue
			}
		}
		if cmd.Kind == "guide" {
			effective, effectiveErr := c.effective(c.ctx)
			if effectiveErr != nil {
				_ = c.P.DB.Ack(cmd.ID, effectiveErr.Error())
				continue
			}
			guidance, err := validateGuidanceCommand(s, cmd, effective.BaseSHA, effective.Hash, roles.Hash())
			if err != nil {
				_ = c.P.DB.Ack(cmd.ID, err.Error())
				continue
			}
			err = c.save(c.ctx, func(s *model.Snapshot) error {
				var err error
				if guidance.Operator {
					err = model.QueueOperatorGuidance(s.Tasks[cmd.Target], cmd.ID, guidance.Head, guidance.Base, guidance.Config, guidance.Rules, guidance.Text)
				} else {
					err = model.QueueGuidance(s.Tasks[cmd.Target], s.Tasks[guidance.Source], cmd.ID, guidance.Text)
				}
				if err != nil {
					return guidanceRejection{err}
				}
				s.Applied[cmd.ID] = true
				return nil
			})
			if err != nil {
				var rejection guidanceRejection
				if errors.As(err, &rejection) {
					_ = c.P.DB.Ack(cmd.ID, err.Error())
					continue
				}
				c.fail(err)
				return false, err
			}
			if err = c.P.DB.Ack(cmd.ID, ""); err != nil {
				return false, err
			}
			_ = c.P.DB.Event(cmd.Target, "", "", "", "task_guidance_queued", "guidance command "+cmd.ID+" will reach the next implementer invocation")
			continue
		}
		e = c.mutate(func(s *model.Snapshot) error {
			switch cmd.Kind {
			case "run":
				s.Objectives[cmd.ID] = &model.Objective{ID: cmd.ID, Text: cmd.Payload}
				s.Backlog = append(s.Backlog, cmd.ID)
			case "answer":
				t := s.Tasks[cmd.Target]
				if t == nil {
					for _, candidate := range s.Tasks {
						if strconv.Itoa(candidate.Issue) == cmd.Target {
							t = candidate
							break
						}
					}
				}
				if t == nil {
					if o := s.Objectives[cmd.Target]; o != nil && o.Blocker != "" {
						if e := applyObjectiveAnswer(o, cmd.Payload); e != nil {
							return e
						}
						break
					}
					return errors.New("unknown blocked task/objective")
				}
				if e := model.Answer(t, cmd.Payload); e != nil {
					return e
				}
				// A human can point the supervisor at a concrete fix already in the
				// durable task checkpoint. Preserve prior guidance only when the
				// answer proves that exact head; ordinary answers can change the task
				// contract and must receive a fresh preflight.
				effective, effectiveErr := c.effective(c.ctx)
				preflightRoles, rolesErr := requiredPreflightRoles(effective, t)
				if effectiveErr == nil && rolesErr == nil && humanContinuationEvidence(t, cmd.Payload) && reusePreflightForContinuation(t.Preflight, t, effective, preflightRoles) {
					t.Decisions = append(t.Decisions, "Human checkpoint continuation: reused completed pre-implementation guidance at "+t.HeadSHA+".")
				} else {
					t.Preflight = nil
				}
			case "improvement":
				s.Improvements = append(s.Improvements, cmd.Payload)
			case "assign-role":
				task := s.Tasks[cmd.Target]
				found := false
				for _, name := range task.Roles {
					if name == cmd.Payload {
						found = true
					}
				}
				if !found {
					task.Roles = append(task.Roles, cmd.Payload)
				}
				task.Evidence = nil
				task.Preflight = nil
			default:
				return errors.New("unknown command")
			}
			s.Applied[cmd.ID] = true
			return nil
		})
		if e != nil {
			_ = c.P.DB.Ack(cmd.ID, e.Error())
			return false, e
		}
		if e = c.P.DB.Ack(cmd.ID, ""); e != nil {
			return false, e
		}
	}
	return false, nil
}
