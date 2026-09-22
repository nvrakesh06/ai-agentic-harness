package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func init() {
	if os.Getenv("AIH_PROCESS_HELPER") != "1" {
		return
	}
	switch os.Args[1] {
	case "echo":
		b, _ := io.ReadAll(os.Stdin)
		fmt.Print(string(b))
		os.Exit(0)
	case "active":
		for {
			fmt.Println("progress")
			time.Sleep(10 * time.Millisecond)
		}
	case "child":
		for {
			_ = os.WriteFile(os.Args[2], []byte(time.Now().Format(time.RFC3339Nano)), 0600)
			time.Sleep(20 * time.Millisecond)
		}
	case "tree":
		child := exec.Command(os.Args[0], "child", os.Args[2])
		child.Env = os.Environ()
		if child.Start() != nil {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Second)
		}
	case "supervisor":
		_, _ = Run(context.Background(), "", os.Environ(), "", os.Args[0], "tree", os.Args[2])
		os.Exit(0)
	}
	os.Exit(3)
}

func TestObservedProcessRecordsRecentOutput(t *testing.T) {
	t.Setenv("AIH_PROCESS_HELPER", "1")
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	observed, err := RunObserved(ctx, "", os.Environ(), "", exe, "active")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if observed.Output == "" || observed.Stdout == "" || observed.Stderr != "" || observed.LastActivity.IsZero() || time.Since(observed.LastActivity) > time.Second {
		t.Fatalf("missing recent activity evidence: %#v", observed)
	}
}

func TestTerminationFailureRemainsObservableAndBounded(t *testing.T) {
	done := make(chan error, 1)
	primaryErr := errors.New("job termination failed")
	fallbackErr := errors.New("root kill failed")
	started := time.Now()
	err := stopProcess(done, func() error { return primaryErr }, func() error { return fallbackErr })
	if !errors.Is(err, primaryErr) || !errors.Is(err, fallbackErr) || !errors.Is(err, errProcessTerminationTimeout) {
		t.Fatalf("termination errors were lost: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*processTerminationGrace+time.Second {
		t.Fatalf("termination fallback was not bounded: %s", elapsed)
	}
}

func TestRootKillFallbackUnblocksFailedTreeTermination(t *testing.T) {
	done := make(chan error, 1)
	primaryErr := errors.New("job termination failed")
	err := stopProcess(done, func() error { return primaryErr }, func() error {
		done <- errors.New("process exited after root kill")
		return nil
	})
	if !errors.Is(err, primaryErr) || errors.Is(err, errProcessTerminationTimeout) {
		t.Fatalf("unexpected bounded fallback result: %v", err)
	}
}

func TestPromptStdinAndTimeout(t *testing.T) {
	t.Setenv("AIH_PROCESS_HELPER", "1")
	exe, _ := os.Executable()
	out, e := Run(context.Background(), "", os.Environ(), "hello\nworld", exe, "echo")
	if e != nil || out != "hello\nworld" {
		t.Fatalf("stdin corrupted: %q %v", out, e)
	}
	marker := filepath.Join(t.TempDir(), "pulse")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, e = Run(ctx, "", os.Environ(), "", exe, "tree", marker)
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	assertStopped(t, marker)
}
func TestSupervisorCrashKillsDescendants(t *testing.T) {
	t.Setenv("AIH_PROCESS_HELPER", "1")
	exe, _ := os.Executable()
	marker := filepath.Join(t.TempDir(), "pulse")
	cmd := exec.Command(exe, "supervisor", marker)
	cmd.Env = os.Environ()
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(marker); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e := cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
	assertStopped(t, marker)
}
func assertStopped(t *testing.T, marker string) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	a, e := os.ReadFile(marker)
	if e != nil {
		t.Fatal("worker never started", e)
	}
	time.Sleep(150 * time.Millisecond)
	b, e := os.ReadFile(marker)
	if e != nil || string(a) != string(b) {
		t.Fatal("descendant outlived its supervisor", e)
	}
}
func TestExclusiveLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	a, e := Acquire(path)
	if e != nil {
		t.Fatal(e)
	}
	if b, e := Acquire(path); e == nil {
		b.Close()
		t.Fatal("duplicate lock acquired")
	}
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
	b, e := Acquire(path)
	if e != nil {
		t.Fatal(e)
	}
	b.Close()
}

func TestContextLockWaitsAndCancels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err = AcquireContext(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err = AcquireContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}
