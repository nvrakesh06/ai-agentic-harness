package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"gopkg.in/yaml.v3"
)

func configureSingleWriterProviderAdmissionFixture(t *testing.T, ctx context.Context, f *demo.Fixture) {
	t.Helper()
	f.Project.MaxWriters = 1
	f.Project.Scheduling.TargetWriters = 1
	encoded, err := yaml.Marshal(f.Project)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.Source, ".aih", "project.yaml"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	source := gitx.Git{Dir: f.Source}
	for _, args := range [][]string{{"add", ".aih/project.yaml"}, {"commit", "-m", "Configure one writer fixture"}, {"push", "origin", "HEAD:main"}} {
		if _, err = source.Run(ctx, "", args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.P.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	f.P.Config.Project.MaxWriters = 1
	f.P.Config.Project.Scheduling.TargetWriters = 1
}

func waitProviderAdmissionHold(t *testing.T, ctx context.Context, f *demo.Fixture) *model.Snapshot {
	t.Helper()
	for {
		s, _, err := f.P.DB.Load()
		if err == nil && len(s.ProviderAdmissionHolds) != 0 {
			return s
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%v waiting for provider admission hold", ctx.Err())
		case <-time.After(40 * time.Millisecond):
		}
	}
}

func TestProviderAdmissionHoldSuppressesWriterAndRestartWithoutBudgets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	configureSingleWriterProviderAdmissionFixture(t, ctx, f)
	f.Provider.RequestRejections = map[string]int{"first": 1}
	seedReadyTask(t, ctx, f, "first")
	seedReadyTask(t, ctx, f, "second")

	firstCtx, stopFirst := context.WithCancel(ctx)
	firstDone := make(chan error, 1)
	go func() { firstDone <- engine.New(f.P).Serve(firstCtx) }()
	s := waitProviderAdmissionHold(t, ctx, f)
	first := s.Tasks["first"]
	if first == nil || first.State != model.Ready || first.Attempts != 0 || len(first.FixCycles) != 0 || first.AdvisorUsed {
		t.Fatalf("initial request rejection did not preserve pre-provider checkpoint: %+v", first)
	}
	failedRun := false
	for _, run := range s.Runs {
		failedRun = failedRun || (run.Task == "first" && run.Role == "implementer" && run.Outcome == "failed")
	}
	if !failedRun {
		t.Fatalf("rejected invocation was not durably recorded: %#v", s.Runs)
	}
	// More scheduler ticks must not admit the queued independent writer once the
	// shared schema hold exists.
	time.Sleep(1200 * time.Millisecond)
	if got := f.Provider.ProviderCallCount("second"); got != 0 {
		t.Fatalf("second task made %d provider calls after shared hold", got)
	}
	stopFirst()
	if err = <-firstDone; err != nil {
		t.Fatal(err)
	}

	secondCtx, stopSecond := context.WithCancel(ctx)
	secondDone := make(chan error, 1)
	go func() { secondDone <- engine.New(f.P).Serve(secondCtx) }()
	time.Sleep(1200 * time.Millisecond)
	if got := f.Provider.ProviderCallCount("first"); got != 1 {
		t.Fatalf("restart repeated rejected provider call %d times", got)
	}
	if got := f.Provider.ProviderCallCount("second"); got != 0 {
		t.Fatalf("restart admitted second provider call %d times", got)
	}
	stopSecond()
	if err = <-secondDone; err != nil {
		t.Fatal(err)
	}
	if s, _, loadErr := f.P.DB.Load(); loadErr != nil || len(s.ProviderAdmissionHolds) != 1 {
		t.Fatalf("restart lost portable hold: holds=%#v err=%v", s.ProviderAdmissionHolds, loadErr)
	}
}
