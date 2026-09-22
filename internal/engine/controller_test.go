package engine_test

import (
	"context"
	"errors"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"path/filepath"
	"testing"
	"time"
)

func waitStarted(t *testing.T, p *engine.Project) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p.DB.Get("pid") != "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("controller failed to start")
}
func TestLiveLeaseRejectedAndExpiredLeaseRecovered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, e := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if e != nil {
		t.Fatal(e)
	}
	defer f.P.DB.Close()
	a := engine.New(f.P)
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	waitStarted(t, f.P)
	p2, e := f.Open(ctx, filepath.Join(f.Root, "machine-b"))
	if e != nil {
		t.Fatal(e)
	}
	defer p2.DB.Close()
	e = engine.New(p2).Serve(ctx)
	if !errors.Is(e, engine.ErrLease) {
		t.Fatal("live controller not rejected", e)
	}
	if e = f.P.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"}); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	s, h, e := p2.Git.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	oldEpoch := s.Controller.Epoch
	s.Controller.Owner = "dead-controller"
	s.Controller.Expires = time.Now().Add(-time.Minute)
	next, e := p2.Git.StateCommit(ctx, h, s)
	if e != nil {
		t.Fatal(e)
	}
	if e = p2.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: h, New: next}}); e != nil {
		t.Fatal(e)
	}
	go func() { done <- engine.New(p2).Serve(ctx) }()
	waitStarted(t, p2)
	fresh, _, e := p2.Git.Load(ctx)
	if e != nil || fresh.Controller.Epoch <= oldEpoch {
		t.Fatal("lease epoch not advanced", e)
	}
	_ = p2.DB.Submit(store.Command{ID: model.ID(), Kind: "handoff"})
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
