package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AcquireSlot takes one named lock from a shared, bounded slot set. It lets
// independent native-check callers coordinate on one machine.
func AcquireSlot(ctx context.Context, dir, name string, slots int) (func(), error) {
	if slots < 1 || slots > 8 {
		return nil, fmt.Errorf("invalid shared slot capacity %d", slots)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	for {
		for i := 0; i < slots; i++ {
			lock, err := Acquire(filepath.Join(dir, fmt.Sprintf("%s-%d.lock", name, i)))
			if err == nil {
				return func() { _ = lock.Close() }, nil
			}
			if !errors.Is(err, ErrLocked) {
				return nil, err
			}
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
