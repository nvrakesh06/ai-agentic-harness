package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
)

func TestReleaseMachinePermitUsesCleanHomeWithoutInstallation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AIH_HOME", home)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := releaseMachinePermit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, name := range []string{"machine.yaml", "projects"} {
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("release permit initialized machine state %s: %v", name, err)
		}
	}
}

func TestReleaseMachinePermitWaitsForSharedHeavyCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AIH_HOME", home)
	held, err := platform.AcquireSlot(context.Background(), filepath.Join(home, "verification"), "heavy", 1)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		release, err := releaseMachinePermit(ctx)
		if err == nil {
			release()
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("release gate bypassed occupied heavy slot: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	held()
	if err := <-result; err != nil {
		t.Fatalf("release gate did not acquire released heavy slot: %v", err)
	}
}
