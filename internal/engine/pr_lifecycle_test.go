package engine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

func init() {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "_aih-pr-gated-check":
		if len(os.Args) != 4 || os.WriteFile(os.Args[2], []byte("started"), 0600) != nil {
			os.Exit(2)
		}
		for {
			if _, err := os.Stat(os.Args[3]); err == nil {
				os.Exit(0)
			}
			time.Sleep(10 * time.Millisecond)
		}
	case "_aih-pr-flaky-check":
		if len(os.Args) != 3 {
			os.Exit(2)
		}
		counter := os.Args[2]
		attempt := 0
		if data, err := os.ReadFile(counter); err == nil {
			attempt, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		attempt++
		if os.WriteFile(counter, []byte(strconv.Itoa(attempt)), 0600) != nil {
			os.Exit(2)
		}
		if attempt == 1 {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func waitForPR(t *testing.T, ctx context.Context, f *demo.Fixture, condition func([]demo.PullUpdate) bool) {
	t.Helper()
	for {
		_, updates := f.Hub.Pulls()
		if condition(updates) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func waitForMarker(ctx context.Context, marker string, done <-chan error) error {
	for {
		if _, err := os.Stat(marker); err == nil {
			return nil
		}
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("controller stopped before native verification: %w", err)
			}
			return errors.New("controller stopped before native verification")
		case <-ctx.Done():
			return fmt.Errorf("native verification did not start: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func stopController(t *testing.T, f *demo.Fixture, done <-chan error) {
	t.Helper()
	if err := f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("controller did not stop")
	}
}

func TestVerificationFailureKeepsOneDraftPRAndNoopStaysHidden(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	tmp := t.TempDir()
	counter := filepath.Join(tmp, "check-count")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := demo.New(ctx, filepath.Join(tmp, "fixture"), []string{exe, "_aih-pr-flaky-check", counter})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.Provider.NoChanges = map[string]bool{"no-op": true}
	seedReadyTask(t, ctx, f, "fix-loop")
	seedReadyTask(t, ctx, f, "no-op")
	done := make(chan error, 1)
	go func() { done <- engine.New(f.P).Serve(ctx) }()
	waitForPR(t, ctx, f, func(updates []demo.PullUpdate) bool {
		ready := false
		fixed := false
		for _, update := range updates {
			ready = ready || !update.Draft
			fixed = fixed || update.Draft && strings.Contains(update.Body, "AIH state: `FIX`")
		}
		snapshot, _, err := f.P.DB.Load()
		noOpRejected := err == nil && snapshot.Tasks["no-op"] != nil && snapshot.Tasks["no-op"].FixCycles["verification"] > 0
		return ready && fixed && noOpRejected
	})
	pulls, updates := f.Hub.Pulls()
	if len(pulls) != 1 || pulls[0].Head.Ref != "aih/fix-loop" {
		t.Fatalf("fix loop duplicated its PR or exposed the no-op task: %+v", pulls)
	}
	sawFix := false
	for _, update := range updates {
		if strings.Contains(update.Body, "AIH state: `FIX`") {
			sawFix = true
			if !update.Draft {
				t.Fatal("failed verification exposed a ready PR")
			}
		}
		if !update.Draft && !strings.Contains(update.Body, "exact-head verification") {
			t.Fatal("PR was promoted before final acceptance")
		}
	}
	if !sawFix || f.Provider.ImplementationCount("fix-loop") != 2 || f.Provider.ImplementationCount("no-op") < 1 {
		t.Fatalf("verification fix loop or no-op rejection was not exercised: fix=%t fix implementations=%d no-op implementations=%d", sawFix, f.Provider.ImplementationCount("fix-loop"), f.Provider.ImplementationCount("no-op"))
	}
	stopController(t, f, done)
}

func TestRecoveryReusesEarlyDraftPR(t *testing.T) {
	// This fixture deliberately blocks a real managed check while it records and
	// recovers a draft PR. Keep its short admission deadline independent from
	// sibling lifecycle fixtures in ordinary package test runs.
	ctx := contextWithTimeout(t, 90*time.Second)
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "check-started")
	release := filepath.Join(tmp, "check-release")
	t.Cleanup(func() { _ = os.WriteFile(release, []byte("continue"), 0600) })
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := demo.New(ctx, filepath.Join(tmp, "fixture"), []string{exe, "_aih-pr-gated-check", marker, release})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	seedReadyTask(t, ctx, f, "recover-draft")
	firstDone := make(chan error, 1)
	go func() { firstDone <- engine.New(f.P).Serve(ctx) }()
	waitForPR(t, contextWithTimeout(t, 30*time.Second), f, func(updates []demo.PullUpdate) bool { return len(updates) > 0 })
	if err = waitForMarker(ctx, marker, firstDone); err != nil {
		t.Fatal(err)
	}
	pulls, _ := f.Hub.Pulls()
	if len(pulls) != 1 || !pulls[0].Draft {
		t.Fatalf("recovery fixture did not create one early draft: %+v", pulls)
	}
	if !strings.Contains(pulls[0].Body, "AIH work in progress") || !strings.Contains(pulls[0].Body, "AIH state: `VERIFYING`") || strings.Contains(pulls[0].Body, "- [x]") {
		t.Fatalf("draft PR claimed acceptance while the native check was running:\n%s", pulls[0].Body)
	}
	snapshot, _, err := f.P.DB.Load()
	if err != nil || snapshot.Tasks["recover-draft"].PR != pulls[0].Number {
		t.Fatal("draft PR identity was not durably recorded", err)
	}
	firstNumber := pulls[0].Number
	stopController(t, f, firstDone)
	if err = os.WriteFile(release, []byte("continue"), 0600); err != nil {
		t.Fatal(err)
	}
	secondCtx, secondCancel := context.WithTimeout(ctx, 90*time.Second)
	defer secondCancel()
	secondDone := make(chan error, 1)
	go func() { secondDone <- engine.New(f.P).Serve(secondCtx) }()
	waitForPR(t, secondCtx, f, func(updates []demo.PullUpdate) bool {
		for _, update := range updates {
			if !update.Draft && strings.Contains(update.Body, "exact-head verification") {
				return true
			}
		}
		return false
	})
	pulls, updates := f.Hub.Pulls()
	if len(pulls) != 1 || pulls[0].Number != firstNumber || pulls[0].Draft {
		t.Fatalf("recovery did not reuse and promote the original PR: %+v", pulls)
	}
	for _, update := range updates {
		if !update.Draft && (!strings.Contains(update.Body, "exact-head verification") || !strings.Contains(update.Body, "- [x]")) {
			t.Fatalf("PR became ready without accepted exact-head evidence:\n%s", update.Body)
		}
	}
	stopController(t, f, secondDone)
}

func TestWaitForMarkerFailsAtDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitForMarker(ctx, filepath.Join(t.TempDir(), "never-created"), make(chan error))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("marker wait error = %v, want canceled context", err)
	}
}

func contextWithTimeout(t *testing.T, duration time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	t.Cleanup(cancel)
	return ctx
}
