package main

import (
	"context"
	"fmt"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
)

// releaseUnitWatchdog bounds one protected release turn. It includes the
// existing 15 minute go-test timeout and a small allowance for compilation,
// JSON draining, identity checks, and managed-child shutdown. It never extends
// the test binary timeout.
const releaseUnitWatchdog = 17 * time.Minute

const releaseNoProgressLimit = 3

// releaseInvocationProgress is deliberately in-memory. It carries a single
// release invocation across cooperative permit waits without creating a cache
// that a later invocation could reuse, especially for browser-sensitive groups.
type releaseInvocationProgress struct {
	completed  map[string]bool
	noProgress int
	handoffs   int
	identity   *releaseReceiptIdentity
	groups     []releaseTestGroup
}

type releaseBoundaryKey struct{}

type releaseBoundary struct {
	dir      string
	machine  config.Machine
	progress *releaseInvocationProgress
}

var releasePriorityDemandFn = releasePriorityDemand

func withReleaseBoundary(ctx context.Context, dir string, machine config.Machine, progress *releaseInvocationProgress) context.Context {
	return context.WithValue(ctx, releaseBoundaryKey{}, releaseBoundary{dir: dir, machine: machine, progress: progress})
}

// releaseBoundaryHandoff is called only after a unit's process has returned,
// its terminal events were validated, and any receipt/identity work completed.
// It never cancels an owned child in response to priority demand.
func releaseBoundaryHandoff(ctx context.Context, id string) (bool, error) {
	b, ok := ctx.Value(releaseBoundaryKey{}).(releaseBoundary)
	if !ok {
		return false, nil
	}
	if b.progress == nil {
		return false, fmt.Errorf("release boundary has no invocation progress")
	}
	progress := b.progress.completedFirst(id)
	demand, err := releasePriorityDemandFn(b.dir, b.machine)
	if err != nil || !demand {
		return false, err
	}
	if err := b.progress.handoff(progress); err != nil {
		return false, err
	}
	return true, nil
}

func releasePriorityDemand(dir string, machine config.Machine) (bool, error) {
	waiting, err := platform.HasPriorityWaiter(dir, "heavy")
	if err != nil || !waiting {
		return false, err
	}
	spare, err := platform.HasAvailableSlot(dir, "heavy", machine.MaxHeavyChecks)
	if err != nil || spare {
		return false, err
	}
	return true, nil
}

func newReleaseInvocationProgress() *releaseInvocationProgress {
	return &releaseInvocationProgress{completed: map[string]bool{}}
}

func (p *releaseInvocationProgress) completedFirst(id string) bool {
	if p.completed[id] {
		return false
	}
	p.completed[id] = true
	return true
}

// bindIdentity establishes the one immutable release identity for this
// invocation. A cooperative capacity wait is never permission to adopt a new
// source, environment, toolchain, resource policy, or inventory baseline.
func (p *releaseInvocationProgress) bindIdentity(identity releaseReceiptIdentity, groups []releaseTestGroup) error {
	if p.identity == nil {
		copyIdentity := identity
		p.identity = &copyIdentity
		p.groups = append([]releaseTestGroup(nil), groups...)
		return nil
	}
	if *p.identity != identity {
		return fmt.Errorf("release invocation identity changed during cooperative handoff")
	}
	return nil
}

func releaseInvocationIdentity(ctx context.Context) (*releaseInvocationProgress, error) {
	b, ok := ctx.Value(releaseBoundaryKey{}).(releaseBoundary)
	if !ok || b.progress == nil || b.progress.identity == nil {
		return nil, nil
	}
	current, err := releaseReceiptIdentityFor(ctx, b.progress.groups)
	if err != nil {
		return nil, err
	}
	if *b.progress.identity != current {
		return nil, fmt.Errorf("release invocation identity changed during cooperative handoff")
	}
	return b.progress, nil
}

// handoff records only validated, previously uncounted completion as progress.
// Replaying a cached prefix, discovery, logging, or a repeated browser run can
// never reset the bounded consecutive no-progress guard.
func (p *releaseInvocationProgress) handoff(progress bool) error {
	p.handoffs++
	if progress {
		p.noProgress = 0
		return nil
	}
	p.noProgress++
	if p.noProgress >= releaseNoProgressLimit {
		return fmt.Errorf("release gate made no unique validated progress across %d consecutive cooperative handoffs", releaseNoProgressLimit)
	}
	return nil
}
