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
