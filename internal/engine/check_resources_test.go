package engine

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func TestRecoveryClearsCheckOwnerBeforeRetry(t *testing.T) {
	s := model.NewSnapshot("project123")
	s.Tasks["one"] = &model.Task{ID: "one", State: model.Verifying}
	s.Capacity.Verification = []model.VerificationCheck{{Task: "one", Check: "release", Class: "heavy", Phase: "running", QueuedAt: time.Now(), StartedAt: time.Now()}}
	if err := recoverSnapshot(s); err != nil {
		t.Fatal(err)
	}
	if s.Tasks["one"].State != model.SyncRequired || len(s.Capacity.Verification) != 0 {
		t.Fatalf("unsafe recovery: %+v", s)
	}
}

func TestMachineCheckSlots(t *testing.T) {
	for _, slots := range []int{1, 2} {
		t.Run(strconv.Itoa(slots), func(t *testing.T) {
			home := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			releases := make([]func(), slots)
			for i := range releases {
				var err error
				releases[i], err = acquireMachineCheck(ctx, home, slots)
				if err != nil {
					t.Fatal(err)
				}
			}
			waiting, stop := context.WithTimeout(ctx, 100*time.Millisecond)
			defer stop()
			if release, err := acquireMachineCheck(waiting, home, slots); !errors.Is(err, context.DeadlineExceeded) {
				if release != nil {
					release()
				}
				t.Fatalf("third check acquired occupied capacity: %v", err)
			}
			releases[0]()
			release, err := acquireMachineCheck(ctx, home, slots)
			if err != nil {
				t.Fatal(err)
			}
			release()
			for _, held := range releases[1:] {
				held()
			}
		})
	}
}

func TestProjectCheckQueueReservesInOrder(t *testing.T) {
	for _, slots := range []int{1, 2} {
		t.Run(strconv.Itoa(slots), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			s := model.NewSnapshot("project123")
			for _, id := range []string{"first", "second", "third"} {
				s.Tasks[id] = &model.Task{ID: id, State: model.Verifying}
				s.Capacity.Verification = append(s.Capacity.Verification, model.VerificationCheck{Task: id, Check: "release", Class: "heavy", Phase: "queued", QueuedAt: time.Now()})
			}
			c := &Controller{s: s}
			limit := make(chan struct{}, slots)
			thirdDone := make(chan error, 1)
			go func() { thirdDone <- c.waitCheckTurn(ctx, "third", "release", "heavy", limit, slots) }()
			select {
			case err := <-thirdDone:
				t.Fatalf("third bypassed queue: %v", err)
			case <-time.After(75 * time.Millisecond):
			}
			if err := c.waitCheckTurn(ctx, "first", "release", "heavy", limit, slots); err != nil {
				t.Fatal(err)
			}
			c.mu.Lock()
			c.s.Capacity.Verification[0].Phase = "running"
			c.mu.Unlock()
			if slots == 2 {
				if err := c.waitCheckTurn(ctx, "second", "release", "heavy", limit, slots); err != nil {
					t.Fatal(err)
				}
				c.mu.Lock()
				c.s.Capacity.Verification[1].Phase = "running"
				c.mu.Unlock()
			} else {
				secondDone := make(chan error, 1)
				go func() { secondDone <- c.waitCheckTurn(ctx, "second", "release", "heavy", limit, slots) }()
				<-limit
				if err := <-secondDone; err != nil {
					t.Fatal(err)
				}
				c.mu.Lock()
				c.s.Capacity.Verification[1].Phase = "running"
				c.mu.Unlock()
			}
			select {
			case err := <-thirdDone:
				t.Fatalf("third acquired occupied capacity: %v", err)
			case <-time.After(75 * time.Millisecond):
			}
			<-limit
			if err := <-thirdDone; err != nil {
				t.Fatal(err)
			}
			<-limit
			if slots == 2 {
				<-limit
			}
		})
	}
}
