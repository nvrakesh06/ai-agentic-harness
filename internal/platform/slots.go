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
	for {
		release, acquired, err := TryAcquireSlot(dir, name, slots)
		if err != nil {
			return nil, err
		}
		if acquired {
			return release, nil
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

// TryAcquireSlot makes one nonblocking pass over the bounded slot set.
// Callers that implement admission priorities can recheck demand between passes.
func TryAcquireSlot(dir, name string, slots int) (func(), bool, error) {
	if err := validSlotCapacity(slots); err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, false, err
	}
	for i := 0; i < slots; i++ {
		lock, err := Acquire(filepath.Join(dir, fmt.Sprintf("%s-%d.lock", name, i)))
		if err == nil {
			return func() { _ = lock.Close() }, true, nil
		}
		if !errors.Is(err, ErrLocked) {
			return nil, false, err
		}
	}
	return nil, false, nil
}

const priorityWaiterSlots = 8

// RegisterPriorityWaiter records live, high-priority demand while a native
// supervisor check is waiting for a heavy slot. The registration is an owned
// lock, so cancellation and process death cannot leave stale demand behind.
func RegisterPriorityWaiter(ctx context.Context, dir, name string) (func(), error) {
	return AcquireSlot(ctx, dir, name+"-priority-waiter", priorityWaiterSlots)
}

// HasPriorityWaiter reports whether any live supervisor check is waiting.
func HasPriorityWaiter(dir, name string) (bool, error) {
	return anySlotHeld(dir, name+"-priority-waiter", priorityWaiterSlots)
}

// HasAvailableSlot reports whether the bounded slot set has spare capacity.
// It acquires and immediately releases one slot only for admission decisions.
func HasAvailableSlot(dir, name string, slots int) (bool, error) {
	if err := validSlotCapacity(slots); err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	for i := 0; i < slots; i++ {
		lock, err := Acquire(filepath.Join(dir, fmt.Sprintf("%s-%d.lock", name, i)))
		if err == nil {
			_ = lock.Close()
			return true, nil
		}
		if !errors.Is(err, ErrLocked) {
			return false, err
		}
	}
	return false, nil
}

func anySlotHeld(dir, name string, slots int) (bool, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	for i := 0; i < slots; i++ {
		lock, err := Acquire(filepath.Join(dir, fmt.Sprintf("%s-%d.lock", name, i)))
		if err == nil {
			_ = lock.Close()
			continue
		}
		if errors.Is(err, ErrLocked) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func validSlotCapacity(slots int) error {
	if slots < 1 || slots > 8 {
		return fmt.Errorf("invalid shared slot capacity %d", slots)
	}
	return nil
}
