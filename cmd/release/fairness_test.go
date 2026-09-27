package main

import "testing"

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
