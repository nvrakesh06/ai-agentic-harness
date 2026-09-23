package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
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
	maxLocalLeasePulse     = 30 * time.Second
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
	jobs        sync.WaitGroup
	now         func() time.Time
}

func New(p *Project) *Controller {
	return &Controller{P: p, owner: model.ID(), fatal: make(chan error, 1), readers: make(chan struct{}, p.Config.Project.MaxReaders)}
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
	return c.mutate(func(s *model.Snapshot) error {
		for _, t := range s.Tasks {
			switch t.State {
			case model.Running:
				t.State = model.Ready
			case model.Implemented, model.Verifying, model.Review, model.MergeTrain:
				t.State = model.SyncRequired
			}
			t.RunID = ""
		}
		for i := range s.Runs {
			if s.Runs[i].Outcome == "running" {
				s.Runs[i].Outcome = "interrupted"
			}
		}
		return nil
	})
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
	c.ctx, c.cancel = context.WithCancel(parent)
	defer c.cancel()
	if e = c.acquire(parent); e != nil {
		return e
	}
	log.Printf("AIH supervisor started: project=%s epoch=%d", c.s.Project, c.s.Controller.Epoch)
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
			if !planning {
				for _, o := range s.Objectives {
					if (!o.Planned || needsProvision(s, o.ID)) && o.Blocker == "" {
						planning = true
						c.launch(func() { c.plan(o.ID); done <- "@plan" })
						break
					}
				}
			}
			writers := map[string]bool{}
			for id := range active {
				if t := s.Tasks[id]; t != nil && t.State == model.Running {
					writers[id] = true
				}
			}
			for _, t := range model.Runnable(s, writers, c.P.Config.Project.MaxWriters) {
				if active[t.ID] {
					continue
				}
				id := t.ID
				if ce = c.mutate(func(s *model.Snapshot) error { return model.Transition(s.Tasks[id], model.Running) }); ce != nil {
					break
				}
				active[id] = true
				c.launch(func() { c.work(id, true); done <- id })
			}
			for _, t := range model.Ordered(s) {
				if active[t.ID] {
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
					if active[t.ID] {
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
			for _, t := range s.Tasks {
				if t.State == model.Running {
					t.State = model.Ready
				}
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
func (c *Controller) launch(fn func()) { c.jobs.Add(1); go func() { defer c.jobs.Done(); fn() }() }
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
				e = nil
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
		e = c.mutate(func(s *model.Snapshot) error {
			switch cmd.Kind {
			case "run":
				s.Objectives[cmd.ID] = &model.Objective{ID: cmd.ID, Text: cmd.Payload}
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
						o.Text += "\nHuman answer: " + cmd.Payload
						o.Blocker = ""
						o.Attempts = 0
						break
					}
					return errors.New("unknown blocked task/objective")
				}
				if e := model.Answer(t, cmd.Payload); e != nil {
					return e
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
