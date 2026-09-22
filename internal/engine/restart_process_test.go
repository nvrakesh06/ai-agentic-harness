package engine_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

func init() {
	if os.Getenv("AIH_RESTART_PROCESS_HELPER") != "1" || len(os.Args) != 3 || os.Args[1] != "_restart" {
		return
	}
	root := os.Args[2]
	cfg, err := config.ParseLocal(filepath.Join(root, "repo"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	f := &demo.Fixture{Root: root, Source: filepath.Join(root, "repo"), Remote: filepath.Join(root, "origin.git"), Project: cfg.Project, Provider: &demo.Worker{}, Hub: &demo.Hub{}}
	p, err := f.Open(context.Background(), filepath.Join(root, "machine-a"))
	if err == nil {
		err = engine.New(p).Serve(context.Background())
		_ = p.DB.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestProcessCrashAndPersistentQueueRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	t.Setenv("AIH_RESTART_PROCESS_HELPER", "1")
	exe, _ := os.Executable()
	start := func() (*exec.Cmd, *bytes.Buffer, <-chan error) {
		out := &bytes.Buffer{}
		cmd := exec.CommandContext(ctx, exe, "_restart", f.Root)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, out, done
	}
	waitPID := func(pid int) {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if f.P.DB.Get("pid") == strconv.Itoa(pid) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("subprocess did not acquire the controller")
	}
	a, _, done := start()
	waitPID(a.Process.Pid)
	if err := a.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	// A process-manager restart must NOT steal even this machine's live lease.
	_, out, done := start()
	if err := <-done; err == nil {
		t.Fatal("crashed owner's live lease was overridden", out.String())
	}
	s, head, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	epoch := s.Controller.Epoch
	// Advance only the test lease clock, avoiding a real 65-second sleep.
	s.Controller.Expires = time.Now().Add(-time.Minute)
	next, err := f.P.Git.StateCommit(ctx, head, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: head, New: next}}); err != nil {
		t.Fatal(err)
	}
	id := model.ID()
	if err = f.P.DB.Submit(store.Command{ID: id, Kind: "improvement", Payload: "queued while supervisor was down"}); err != nil {
		t.Fatal(err)
	}
	b, out, done := start()
	waitPID(b.Process.Pid)
	for {
		s, _, err = f.P.DB.Load()
		if err == nil && s.Applied[id] {
			break
		}
		select {
		case err := <-done:
			t.Fatal("restart exited", err, out.String())
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if s.Controller.Epoch <= epoch || len(s.Improvements) != 1 {
		t.Fatal("restart lost queue or fencing", s.Controller)
	}
	if err = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err, out.String())
	}
	s, _, err = f.P.Git.Load(ctx)
	if err != nil || s.Controller.Owner != "" {
		t.Fatal("shutdown did not release ownership", err)
	}
}
