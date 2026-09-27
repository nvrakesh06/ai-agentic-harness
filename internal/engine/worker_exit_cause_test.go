package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
)

func TestClassifyWorkerExitUsesLiveErrorSemantics(t *testing.T) {
	process := &provider.InvocationError{Cause: errors.New("fixture process failure")}
	for _, test := range []struct {
		name string
		err  error
		want workerExitCause
	}{
		{"no error", nil, workerExitNone},
		{"deadline", context.DeadlineExceeded, workerExitDeadline},
		{"wrapped invocation deadline", fmt.Errorf("outer: %w", &provider.InvocationError{Cause: context.DeadlineExceeded}), workerExitDeadline},
		{"canceled", context.Canceled, workerExitCanceled},
		{"wrapped invocation cancellation", fmt.Errorf("outer: %w", &provider.InvocationError{Cause: context.Canceled}), workerExitCanceled},
		{"wrapped invocation process failure", fmt.Errorf("outer: %w", process), workerExitProcessFailure},
		{"untyped adapter failure", errors.New("structured result unavailable"), workerExitAdapterFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyWorkerExit(test.err); got != test.want {
				t.Fatalf("classifyWorkerExit(%v) = %q, want %q", test.err, got, test.want)
			}
		})
	}
}
