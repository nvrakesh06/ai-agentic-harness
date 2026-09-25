package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

func leaseTestProject(t *testing.T, ctx context.Context, root, remote, machine string, project config.Project) *Project {
	t.Helper()
	dir := filepath.Join(root, machine)
	g, err := gitx.OpenControl(ctx, filepath.Join(dir, "control.git"), remote)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Project{
		Dir:     dir,
		Home:    root,
		Machine: config.Machine{ID: machine, Platform: "test"},
		Git:     g,
		Config:  config.Effective{Project: project},
		DB:      db,
	}
}

func TestLeasePulsesCoalesceRemoteWritesAndFenceTakeover(t *testing.T) {
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
	project.ID = "lease-test-project"
	project.LeaseSeconds = 60
	a := leaseTestProject(t, ctx, root, remote, "machine-a", project)
	initial := model.NewSnapshot(project.ID)
	initial.Revision = 1
	head, err := a.Git.StateCommit(ctx, "", initial)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", New: head}}); err != nil {
		t.Fatal(err)
	}
	if err = a.DB.Save(head, initial); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	now := base
	controllerA := New(a)
	controllerA.now = func() time.Time { return now }
	if err = controllerA.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	assertRevision := func(want uint64) *model.Snapshot {
		t.Helper()
		snapshot, _, loadErr := a.Git.Load(ctx)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if snapshot.Revision != want {
			t.Fatalf("remote revision = %d, want %d", snapshot.Revision, want)
		}
		return snapshot
	}
	assertRevision(2) // initial state plus lease acquisition

	for _, elapsed := range []time.Duration{10 * time.Second, 20 * time.Second} {
		now = base.Add(elapsed)
		published, pulseErr := controllerA.pulseLease(ctx)
		if pulseErr != nil || published {
			t.Fatalf("early pulse published=%t err=%v", published, pulseErr)
		}
	}
	if got := a.DB.Get(LocalLeaseHeartbeatKey); got != now.Format(time.RFC3339Nano) {
		t.Fatalf("local heartbeat = %q, want %q", got, now.Format(time.RFC3339Nano))
	}
	assertRevision(2)

	// A meaningful transition publishes immediately and moves the durable lease
	// window, so the scheduled heartbeat at the old boundary is coalesced.
	if err = controllerA.save(ctx, func(s *model.Snapshot) error {
		s.Applied["meaningful-transition"] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	afterTransition := assertRevision(3)
	if afterTransition.Controller.Expires != now.Add(time.Minute) {
		t.Fatalf("meaningful save did not refresh lease: %#v", afterTransition.Controller)
	}
	if err = controllerA.save(ctx, func(*model.Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	assertRevision(3) // semantic no-op did not create a commit

	for _, elapsed := range []time.Duration{10 * time.Second, 20 * time.Second} {
		now = now.Add(10 * time.Second)
		published, pulseErr := controllerA.pulseLease(ctx)
		if pulseErr != nil || published {
			t.Fatalf("coalesced pulse at %s published=%t err=%v", elapsed, published, pulseErr)
		}
	}
	now = now.Add(10 * time.Second)
	_, beforeRenewalHead, err := a.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	published, err := controllerA.pulseLease(ctx)
	if err != nil || !published {
		t.Fatalf("half-life renewal published=%t err=%v", published, err)
	}
	renewed := assertRevision(3)
	_, afterRenewalHead, err := a.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterRenewalHead == beforeRenewalHead {
		t.Fatal("durable lease renewal did not advance aih-state")
	}
	if renewed.Controller.Expires != now.Add(time.Minute) {
		t.Fatalf("renewal expiry = %s, want %s", renewed.Controller.Expires, now.Add(time.Minute))
	}

	b := leaseTestProject(t, ctx, root, remote, "machine-b", project)
	controllerB := New(b)
	controllerB.now = func() time.Time { return now }
	if err = controllerB.acquire(ctx); !errors.Is(err, ErrLease) {
		t.Fatalf("second controller acquired live lease: %v", err)
	}
	oldEpoch := renewed.Controller.Epoch
	now = renewed.Controller.Expires.Add(4 * time.Second)
	if err = controllerB.acquire(ctx); !errors.Is(err, ErrLease) {
		t.Fatalf("second controller bypassed takeover grace: %v", err)
	}
	now = renewed.Controller.Expires.Add(6 * time.Second)
	if err = controllerB.acquire(ctx); err != nil {
		t.Fatalf("expired lease was not recoverable: %v", err)
	}
	taken, _, err := b.Git.Load(ctx)
	if err != nil || taken.Controller.Epoch != oldEpoch+1 || taken.Controller.Machine != "machine-b" {
		t.Fatalf("takeover did not advance fencing epoch: %#v err=%v", taken.Controller, err)
	}
}

func TestLeaseCadenceBounds(t *testing.T) {
	if got := leasePulseInterval(60 * time.Second); got != 10*time.Second {
		t.Fatalf("minimum lease pulse = %s", got)
	}
	if got := leasePulseInterval(180 * time.Second); got != 30*time.Second {
		t.Fatalf("default lease pulse = %s", got)
	}
	if got := leasePulseInterval(15 * time.Minute); got != maxLocalLeasePulse {
		t.Fatalf("long lease pulse = %s", got)
	}
	leaseDuration := 3 * time.Minute
	start := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	lease := model.Lease{Heartbeat: start, Expires: start.Add(leaseDuration)}
	publications := 0
	for elapsed := leasePulseInterval(leaseDuration); elapsed <= 30*time.Minute; elapsed += leasePulseInterval(leaseDuration) {
		now := start.Add(elapsed)
		if !leaseRenewalDue(lease, now, leaseDuration) {
			continue
		}
		publications++
		lease.Heartbeat = now
		lease.Expires = now.Add(leaseDuration)
	}
	if publications != 20 {
		t.Fatalf("30-minute steady state published %d lease renewals, want 20", publications)
	}
}

func TestLocalSupervisorHealthDetectsBlockedPublicationDespiteFreshAttempt(t *testing.T) {
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
	project.ID = "blocked-publication-health"
	project.LeaseSeconds = 60
	p := leaseTestProject(t, ctx, root, remote, "machine-a", project)
	initial := model.NewSnapshot(project.ID)
	initial.Revision = 1
	head, err := p.Git.StateCommit(ctx, "", initial)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", New: head}}); err != nil {
		t.Fatal(err)
	}
	if err = p.DB.Save(head, initial); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 9, 25, 16, 0, 0, 0, time.UTC)
	c := New(p)
	c.now = func() time.Time { return base }
	if err = c.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	enteredPublication := make(chan struct{})
	releasePublication := make(chan struct{})
	c.publish = func(context.Context, []gitx.Update) error {
		close(enteredPublication)
		<-releasePublication
		return nil
	}
	persistDone := make(chan error, 1)
	go func() {
		_, persistErr := c.persist(ctx, func(s *model.Snapshot) error {
			s.Applied["blocked-publication"] = true
			return nil
		})
		persistDone <- persistErr
	}()
	<-enteredPublication

	stalledAt := base.Add(SupervisorHealthWindow(time.Minute) + time.Second)
	c.now = func() time.Time { return stalledAt }
	pulseDone := make(chan error, 1)
	go func() {
		_, pulseErr := c.pulseLease(ctx)
		pulseDone <- pulseErr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for p.DB.Get(LocalLeaseHeartbeatKey) != stalledAt.Format(time.RFC3339Nano) {
		if time.Now().After(deadline) {
			t.Fatal("blocked heartbeat attempt did not record local evidence")
		}
		time.Sleep(time.Millisecond)
	}
	health := LocalSupervisorHealth(p.DB, time.Minute, stalledAt)
	if health.State != "stalled" || health.LastHeartbeat != stalledAt || health.LastProgress != base {
		t.Fatalf("blocked publication was reported as healthy: %#v", health)
	}
	if health.Stage != "persist: waiting for controller mutex" {
		t.Fatalf("blocked stage = %q, want controller-mutex diagnostic", health.Stage)
	}
	close(releasePublication)
	if err = <-persistDone; err != nil {
		t.Fatal(err)
	}
	if err = <-pulseDone; err != nil {
		t.Fatal(err)
	}
}
