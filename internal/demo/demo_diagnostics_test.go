package demo

import (
	"context"
	"errors"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"path/filepath"
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

func TestDemoTimeoutReturnsWhenStoreConnectionIsHeld(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn, err := s.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	started := time.Now()
	err = demoTimeout(context.DeadlineExceeded, "waiting for native verification", started, &engine.Project{DB: s})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout diagnostics waited %s with the sole store connection held", elapsed)
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("timeout diagnostics did not report bounded store read failure:\n%s", err)
	}
}
