package platform

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestPriorityWaiterProcessExitCleansUp(t *testing.T) {
	if os.Getenv("GO_WANT_PRIORITY_WAITER_HELPER") == "1" {
		clear, err := RegisterPriorityWaiter(context.Background(), os.Getenv("GO_WANT_PRIORITY_WAITER_DIR"), "heavy")
		if err != nil {
			os.Exit(2)
		}
		_ = clear
		fmt.Fprintln(os.Stdout, "ready")
		os.Exit(0) // Deliberately skip release; the OS must release this owned lock.
	}
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=TestPriorityWaiterProcessExitCleansUp", "--")
	child.Env = append(os.Environ(), "GO_WANT_PRIORITY_WAITER_HELPER=1", "GO_WANT_PRIORITY_WAITER_DIR="+dir)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("waiter helper failed: %v: %s", err, output)
	}
	waiting, err := HasPriorityWaiter(dir, "heavy")
	if err != nil || waiting {
		t.Fatalf("exited waiter remained live: waiting=%v err=%v", waiting, err)
	}
}

func TestPriorityWaiterIsLiveAndCancelsCleanly(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		clear, err := RegisterPriorityWaiter(ctx, dir, "heavy")
		if err == nil {
			<-ctx.Done()
			clear()
		}
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		waiting, err := HasPriorityWaiter(dir, "heavy")
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("priority waiter was not visible")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waiting, err := HasPriorityWaiter(dir, "heavy")
	if err != nil || waiting {
		t.Fatalf("cancelled waiter remained live: waiting=%v err=%v", waiting, err)
	}
}
