package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
)

func TestRunReleaseTestsCancelsAfterPackageFailure(t *testing.T) {
	if os.Getenv("GO_WANT_RELEASE_TEST_FAILURE_HELPER") == "1" {
		fmt.Fprintln(os.Stdout, `{"Time":"2026-01-01T00:00:00Z","Action":"output","Package":"example.com/first","Output":"ordinary test output containing FAIL must not drive cancellation\\n"}`)
		fmt.Fprintln(os.Stdout, `{"Time":"2026-01-01T00:00:01Z","Action":"fail","Package":"example.com/first"}`)
		select {}
	}
	t.Setenv("GO_WANT_RELEASE_TEST_FAILURE_HELPER", "1")
	original := newReleaseTestCommand
	newReleaseTestCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=TestRunReleaseTestsCancelsAfterPackageFailure", "--")
	}
	t.Cleanup(func() { newReleaseTestCommand = original })

	started := time.Now()
	err := runReleaseTests()
	if err == nil {
		t.Fatal("runReleaseTests succeeded after a package failure")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("runReleaseTests did not cancel promptly: %s", elapsed)
	}
	if got := err.Error(); !strings.Contains(got, "example.com/first") || !strings.Contains(got, "cancelled remaining package tests") {
		t.Fatalf("failure did not identify the failed package and cancellation: %v", err)
	}
}

func TestRunReleaseTestsDoesNotCancelForFailureTextInOutput(t *testing.T) {
	if os.Getenv("GO_WANT_RELEASE_TEST_LOG_HELPER") == "1" {
		fmt.Fprintln(os.Stdout, `{"Action":"output","Package":"example.com/first","Output":"FAIL: a test log only\\n"}`)
		time.Sleep(200 * time.Millisecond)
		return
	}
	t.Setenv("GO_WANT_RELEASE_TEST_LOG_HELPER", "1")
	original := newReleaseTestCommand
	newReleaseTestCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=TestRunReleaseTestsDoesNotCancelForFailureTextInOutput", "--")
	}
	t.Cleanup(func() { newReleaseTestCommand = original })
	if err := runReleaseTests(); err != nil {
		t.Fatalf("runReleaseTests treated test log output as a package failure: %v", err)
	}
}

func TestReleaseMachinePermitUsesCleanHomeWithoutInstallation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AIH_HOME", home)
	// Exercise the production acquisition wrapper; it tears down its temporary
	// interrupt handler before returning the held slot to the release workflow.
	release, err := acquireReleaseMachinePermit()
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
