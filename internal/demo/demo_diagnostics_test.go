package demo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDemoTimeoutIncludesPhaseAndStateAvailability(t *testing.T) {
	phaseStarted := time.Date(2026, time.September, 24, 10, 0, 0, 0, time.UTC)
	err := demoTimeout(context.DeadlineExceeded, "running exact-head integration verification", phaseStarted, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error did not retain deadline cause: %v", err)
	}
	message := err.Error()
	for _, want := range []string{"demo timeout diagnostics", "running exact-head integration verification", "local project database is unavailable"} {
		if !strings.Contains(message, want) {
			t.Fatalf("timeout diagnostic omitted %q:\n%s", want, message)
		}
	}
}
