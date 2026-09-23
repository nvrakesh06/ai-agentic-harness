package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
)

// checkPermit limits native checks independently of advisory readers and writers.
// A heavy slot is also an OS lock shared by project controllers in one AIH home;
// process death releases it, so recovery never inherits a stale running owner.
func (c *Controller) checkPermit(ctx context.Context, taskID string, check config.Check) (func(), error) {
	class := check.Class
	if class == "" {
		class = "heavy"
	} // Legacy checks get the safe class.
	if err := c.verificationState(ctx, taskID, check.Name, class, "queued"); err != nil {
		return nil, err
	}
	clear := func() { _ = c.verificationState(c.ctx, taskID, check.Name, class, "") }
	limit := c.heavyChecks
	turns := cap(limit)
	if class == "light" {
		limit = c.lightChecks
		turns = cap(limit)
	} else if c.P.Machine.MaxHeavyChecks > 0 && c.P.Machine.MaxHeavyChecks < turns {
		turns = c.P.Machine.MaxHeavyChecks
	}
	if err := c.waitCheckTurn(ctx, taskID, check.Name, class, limit, turns); err != nil {
		clear()
		return nil, err
	}
	releaseLocal := func() { <-limit }
	if class == "light" {
		if err := c.verificationState(ctx, taskID, check.Name, class, "running"); err != nil {
			releaseLocal()
			clear()
			return nil, err
		}
		return func() { clear(); releaseLocal() }, nil
	}

	slots := c.P.Machine.MaxHeavyChecks
	if slots == 0 {
		slots = 1
	}
	if slots < 1 || slots > 8 {
		releaseLocal()
		clear()
		return nil, fmt.Errorf("invalid machine heavy-check capacity %d", slots)
	}
	releaseMachine, err := acquireMachineCheck(ctx, c.P.Home, slots)
	if err != nil {
		releaseLocal()
		clear()
		return nil, err
	}
	if err = c.verificationState(ctx, taskID, check.Name, class, "running"); err != nil {
		releaseMachine()
		releaseLocal()
		clear()
		return nil, err
	}
	return func() { clear(); releaseMachine(); releaseLocal() }, nil
}

// Reserve a local slot while holding the snapshot mutex. This prevents a
// later waiter from racing past an earlier queued check on this project.
func (c *Controller) waitCheckTurn(ctx context.Context, taskID, name, class string, limit chan struct{}, slots int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		rank := 0
		for _, check := range c.s.Capacity.Verification {
			if check.Phase != "queued" || check.Class != class {
				continue
			}
			if check.Task == taskID && check.Check == name {
				if rank == 0 && len(limit) < slots {
					limit <- struct{}{}
					c.mu.Unlock()
					return nil
				}
				break
			}
			rank++
		}
		c.mu.Unlock()
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func acquireMachineCheck(ctx context.Context, home string, slots int) (func(), error) {
	if slots < 1 || slots > 8 {
		return nil, fmt.Errorf("invalid machine heavy-check capacity %d", slots)
	}
	dir := filepath.Join(home, "verification")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	for {
		for i := 0; i < slots; i++ {
			lock, err := platform.Acquire(filepath.Join(dir, fmt.Sprintf("heavy-%d.lock", i)))
			if err == nil {
				return func() { _ = lock.Close() }, nil
			}
			if !errors.Is(err, platform.ErrLocked) {
				return nil, err
			}
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Controller) verificationState(ctx context.Context, taskID, name, class, phase string) error {
	return c.save(ctx, func(s *model.Snapshot) error {
		checks := s.Capacity.Verification[:0]
		var prior model.VerificationCheck
		for _, check := range s.Capacity.Verification {
			if check.Task == taskID && check.Check == name {
				prior = check
			} else {
				checks = append(checks, check)
			}
		}
		s.Capacity.Verification = checks
		if phase == "" {
			return nil
		}
		if prior.QueuedAt.IsZero() {
			prior.QueuedAt = c.nowUTC()
		}
		prior.Task, prior.Check, prior.Class, prior.Phase = taskID, name, class, phase
		if phase == "running" {
			prior.StartedAt = c.nowUTC()
		}
		s.Capacity.Verification = append(s.Capacity.Verification, prior)
		return nil
	})
}
