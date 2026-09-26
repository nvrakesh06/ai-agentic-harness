package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

func TestAnswerPolicyResolutionDoesNotHoldControllerMutex(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	if err := os.MkdirAll(remote, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := (gitx.Git{Dir: remote}).Run(ctx, "", "init", "--bare", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	project := config.Defaults()
	project.ID = "command-lock-test"
	project.LeaseSeconds = 60
	p := leaseTestProject(t, ctx, root, remote, "machine-a", project)
	c := New(p)
	c.ctx = ctx
	now := time.Now().UTC()
	c.now = func() time.Time { return now }
	s := model.NewSnapshot(project.ID)
	s.Controller = model.Lease{Owner: c.owner, Machine: "machine-a", Epoch: 1, Heartbeat: now, Expires: now.Add(time.Minute)}
	s.Tasks["task"] = &model.Task{ID: "task", State: model.Blocked, Blocker: &model.Blocker{Question: "Resume?", Resume: model.Fix}}
	head, err := p.Git.StateCommit(ctx, "", s)
	if err != nil {
		t.Fatal(err)
	}
	c.s, c.head = s, head
	s.Tasks["task"].Preflight = &model.Preflight{Phase: "ready", BaseSHA: head, Config: strings.Repeat("a", 64), Rules: strings.Repeat("b", 64)}
	c.publish = func(context.Context, []gitx.Update) error { return nil }
	if err = p.DB.Submit(store.Command{ID: "answer", Kind: "answer", Target: "task", Payload: "Resume the bounded task."}); err != nil {
		t.Fatal(err)
	}
	if err = p.DB.Submit(store.Command{ID: "stop", Kind: "stop"}); err != nil {
		t.Fatal(err)
	}

	// Model a concurrent worktree operation holding gitMu. The resolver barrier
	// proves that the answer has reached canonical lookup before testing liveness.
	c.gitMu.Lock()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(c.gitMu.Unlock) }
	defer release()
	entered := make(chan struct{})
	c.commandEffective = func(context.Context) (config.Effective, error) {
		close(entered)
		c.gitMu.Lock()
		defer c.gitMu.Unlock()
		return config.Effective{}, errors.New("fixture canonical policy unavailable")
	}
	type result struct {
		stop bool
		err  error
	}
	done := make(chan result, 1)
	go func() { stop, err := c.commands(); done <- result{stop, err} }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("answer never reached canonical policy lookup")
	}

	snapshotDone := make(chan struct{}, 1)
	go func() { c.Snapshot(); snapshotDone <- struct{}{} }()
	select {
	case <-snapshotDone:
	case <-time.After(time.Second):
		t.Fatal("answer policy lookup held controller mutex while waiting for Git")
	}
	pulseDone := make(chan error, 1)
	go func() {
		published, err := c.pulseLease(ctx)
		if published && err == nil {
			err = errors.New("early pulse unexpectedly published")
		}
		pulseDone <- err
	}()
	select {
	case err = <-pulseDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("lease pulse blocked behind answer policy lookup")
	}
	release()
	select {
	case got := <-done:
		if got.err != nil || !got.stop {
			t.Fatalf("answer/stop result = %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("answer and queued stop did not complete")
	}
	final := c.Snapshot()
	if !final.Applied["answer"] || final.Tasks["task"].State != model.Fix || final.Tasks["task"].Blocker != nil || final.Tasks["task"].Preflight != nil {
		t.Fatalf("answer was not applied once with policy-failure fallback: %+v", final.Tasks["task"])
	}
	pending, err := p.DB.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatalf("commands not acknowledged: %+v, %v", pending, err)
	}
}
