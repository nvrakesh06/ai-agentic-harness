package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
)

// checkPermit limits native checks independently of advisory readers and writers.
// A heavy slot is also an OS lock shared by project controllers in one AIH home;
// process death releases it, so recovery never inherits a stale running owner.
func (c *Controller) checkPermit(ctx context.Context, check config.Check) (func(), error) {
	class := check.Class
	if class == "" {
		class = "heavy"
	} // Legacy checks get the safe class.
	limit := c.heavyChecks
	if class == "light" {
		limit = c.lightChecks
	}
	select {
	case limit <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	releaseLocal := func() { <-limit }
	if class == "light" {
		return releaseLocal, nil
	}

	dir := filepath.Join(c.P.Home, "verification")
	if err := os.MkdirAll(dir, 0700); err != nil {
		releaseLocal()
		return nil, err
	}
	slots := c.P.Machine.MaxHeavyChecks
	if slots == 0 {
		slots = 1
	}
	if slots < 1 || slots > 8 {
		releaseLocal()
		return nil, fmt.Errorf("invalid machine heavy-check capacity %d", slots)
	}
	for {
		for i := 0; i < slots; i++ {
			lock, err := platform.Acquire(filepath.Join(dir, fmt.Sprintf("heavy-%d.lock", i)))
			if err == nil {
				return func() { _ = lock.Close(); releaseLocal() }, nil
			}
			if !errors.Is(err, platform.ErrLocked) {
				releaseLocal()
				return nil, err
			}
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			releaseLocal()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
