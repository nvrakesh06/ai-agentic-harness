package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
)

func TestReleaseInvocationProgressAllowsProductiveHandoffsBeyondLegacyBudget(t *testing.T) {
	p := newReleaseInvocationProgress()
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		if !p.completedFirst(id) {
			t.Fatalf("first completion of %q was not progress", id)
		}
		if err := p.handoff(true); err != nil {
			t.Fatalf("productive handoff %d: %v", i+1, err)
		}
	}
}

func TestReleaseInvocationProgressBoundsRepeatedNonProgress(t *testing.T) {
	p := newReleaseInvocationProgress()
	if !p.completedFirst("group-a") || p.completedFirst("group-a") {
		t.Fatal("group identity was not monotonic")
	}
	for i := 0; i < releaseNoProgressLimit-1; i++ {
		if err := p.handoff(false); err != nil {
			t.Fatalf("handoff %d ended too early: %v", i+1, err)
		}
	}
	if err := p.handoff(false); err == nil {
		t.Fatal("repeated cached/no-progress handoffs were unbounded")
	}
}

func TestReleaseInvocationProgressResetsOnlyUniqueValidatedCompletion(t *testing.T) {
	p := newReleaseInvocationProgress()
	if err := p.handoff(false); err != nil {
		t.Fatal(err)
	}
	if p.completedFirst("browser") != true || p.completedFirst("browser") != false {
		t.Fatal("browser completion identity was not retained in this invocation")
	}
	if err := p.handoff(true); err != nil {
		t.Fatal(err)
	}
	if p.noProgress != 0 {
		t.Fatalf("unique completion did not reset guard: %d", p.noProgress)
	}
}

func TestReleaseInvocationProgressRejectsChangedImmutableIdentity(t *testing.T) {
	identity, group := receiptFixture()
	p := newReleaseInvocationProgress()
	if err := p.bindIdentity(identity, []releaseTestGroup{group}); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*releaseReceiptIdentity){
		func(v *releaseReceiptIdentity) { v.Head = "other-head" },
		func(v *releaseReceiptIdentity) { v.Environment = "other-environment" },
		func(v *releaseReceiptIdentity) { v.Inventory = "other-inventory" },
		func(v *releaseReceiptIdentity) { v.Resources = "other-resource-policy" },
	} {
		changed := identity
		change(&changed)
		if err := p.bindIdentity(changed, []releaseTestGroup{group}); err == nil {
			t.Fatal("changed identity was accepted after a cooperative handoff")
		}
	}
}

func TestReleaseResourcePolicyBindsUnitBoundaryRules(t *testing.T) {
	policy := releaseResourcePolicy()
	for _, want := range []string{"timeout=15m", "unit_watchdog=17m0s", "boundary_policy=completed-unit-v1", "no_progress_limit=3"} {
		if !strings.Contains(policy, want) {
			t.Fatalf("resource policy omitted %q: %s", want, policy)
		}
	}
}

func TestReleaseTailReacquireFailureReleasesOldPermitWithoutPanic(t *testing.T) {
	originalDemand, originalAcquire := releasePriorityDemandFn, acquireReleaseMachinePermitFn
	defer func() { releasePriorityDemandFn, acquireReleaseMachinePermitFn = originalDemand, originalAcquire }()
	releasePriorityDemandFn = func(string, config.Machine) (bool, error) { return true, nil }
	acquireReleaseMachinePermitFn = func(time.Duration) (func(), config.Machine, string, error) {
		return nil, config.Machine{}, "", errors.New("synthetic reacquire failure")
	}
	var releases atomic.Int32
	ctx := withReleaseBoundary(context.Background(), "unused", config.Machine{MaxHeavyChecks: 1}, newReleaseInvocationProgress())
	_, _, _, err := releaseTailHandoff(ctx, func() {}, func() { releases.Add(1) }, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "synthetic reacquire failure") {
		t.Fatalf("tail reacquire error = %v", err)
	}
	if releases.Load() != 1 {
		t.Fatalf("old permit release count = %d, want 1", releases.Load())
	}
}

func TestReleaseTailReacquireFailureReturnsFromReleaseWithoutDeferredPanic(t *testing.T) {
	originalDemand, originalAcquire := releasePriorityDemandFn, acquireReleaseMachinePermitFn
	originalComplete, originalUnit := runCompleteReleaseTestsFn, runReleaseBoundedUnitFn
	originalFlags, originalArgs := flag.CommandLine, append([]string(nil), os.Args...)
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join(originalDir, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer func() {
		releasePriorityDemandFn, acquireReleaseMachinePermitFn = originalDemand, originalAcquire
		runCompleteReleaseTestsFn, runReleaseBoundedUnitFn = originalComplete, originalUnit
		flag.CommandLine, os.Args = originalFlags, originalArgs
		_ = os.Chdir(originalDir)
	}()
	flag.CommandLine = flag.NewFlagSet("release-test", flag.ContinueOnError)
	os.Args = []string{"release-test"}
	releasePriorityDemandFn = func(string, config.Machine) (bool, error) { return true, nil }
	var attempts, releases atomic.Int32
	acquireReleaseMachinePermitFn = func(time.Duration) (func(), config.Machine, string, error) {
		if attempts.Add(1) == 1 {
			return func() { releases.Add(1) }, config.Machine{MaxHeavyChecks: 1}, "unused", nil
		}
		return nil, config.Machine{}, "", errors.New("synthetic tail reacquire failure")
	}
	runCompleteReleaseTestsFn = func(context.Context) error { return nil }
	runReleaseBoundedUnitFn = func(context.Context, []string, func() error, string, ...string) error { return nil }
	if err := release(); err == nil || !strings.Contains(err.Error(), "synthetic tail reacquire failure") {
		t.Fatalf("release tail error = %v", err)
	}
	if attempts.Load() != 2 || releases.Load() != 1 {
		t.Fatalf("tail ownership attempts=%d releases=%d, want 2/1", attempts.Load(), releases.Load())
	}
}
