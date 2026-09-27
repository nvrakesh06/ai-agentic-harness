package main

import (
	"strings"
	"testing"
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
