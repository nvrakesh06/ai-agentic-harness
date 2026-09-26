package engine

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func TestTransientNativeFailureClassificationIsNarrow(t *testing.T) {
	timeout := &checkFailure{err: context.DeadlineExceeded}
	if got := transientNativeFailure(timeout); got != transientNativeTimeout {
		t.Fatalf("timeout classification = %q", got)
	}
	if got := transientNativeFailure(&checkFailure{err: context.Canceled}); got != "" {
		t.Fatalf("canceled check was classified transient: %q", got)
	}
	lockOutput := "npm ERR! code EPERM\nnpm ERR! syscall unlink\nnpm ERR! path C:\\fixture\\node_modules\\package.node\nnpm ERR! EPERM: operation not permitted, unlink 'package.node'"
	if got := transientNativeFailure(&checkFailure{err: errors.New("exit status 1"), output: lockOutput}); runtime.GOOS == "windows" && got != transientWindowsNPMLock {
		t.Fatalf("windows npm lock classification = %q", got)
	} else if runtime.GOOS != "windows" && got != "" {
		t.Fatalf("non-windows lock signature was classified: %q", got)
	}
	npmErrorOutput := "npm error code EPERM\nnpm error syscall unlink\nnpm error path C:\\fixture\\node_modules\\package.node"
	if got := transientNativeFailure(&checkFailure{err: errors.New("exit status 1"), output: npmErrorOutput}); runtime.GOOS == "windows" && got != transientWindowsNPMLock {
		t.Fatalf("windows npm error lock classification = %q", got)
	} else if runtime.GOOS != "windows" && got != "" {
		t.Fatalf("non-windows npm error lock was classified: %q", got)
	}
	for _, failure := range []*checkFailure{
		{err: errors.New("exit status 1"), output: "npm ERR! EPERM: operation not permitted"},
		{err: errors.New("exit status 1"), output: "EPERM unlink package.node"},
		{err: errors.New("exit status 1"), output: "npm ERR! assertion failed"},
		{err: errors.New("exit status 1"), output: "assertion failed while comparing npm ERR! code EPERM, npm ERR! syscall unlink, and npm ERR! path C:\\fixture\\node_modules\\package.node"},
		{err: errors.New("exit status 1"), output: "src/server.go:12: undefined: MissingType"},
	} {
		if got := transientNativeFailure(failure); got != "" {
			t.Fatalf("source/near-miss failure was classified transient: %q", got)
		}
	}
}

func TestTransientVerificationIdentityResetsAllowance(t *testing.T) {
	guard := &model.Verification{Environment: "windows/native", HeadSHA: "head-a", CheckID: "diagnostic-only", Classification: transientNativeTimeout, Attempts: 1}
	if !sameTransientVerification(guard, "windows/native", "head-a") {
		t.Fatal("matching transient plan did not consume its one retry")
	}
	if !sameTransientVerification(guard, "windows/native", "head-a") {
		t.Fatal("a different transient check could bypass the same native plan")
	}
	if sameTransientVerification(guard, "windows/updated", "head-a") || sameTransientVerification(guard, "windows/native", "head-b") {
		t.Fatal("changed native plan or head reused transient retry allowance")
	}
}
