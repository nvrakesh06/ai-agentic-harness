package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

var ErrLocked = errors.New("lock held by another process")

var errProcessTerminationTimeout = errors.New("process termination did not complete within the bounded fallback")

const processTerminationGrace = 500 * time.Millisecond

// AcquireContext serializes short local operations across CLI processes. The
// supervisor itself still uses fail-fast Acquire to reject duplicate owners.
func AcquireContext(ctx context.Context, path string) (*Lock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := Acquire(path)
		if !errors.Is(err, ErrLocked) {
			return lock, err
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

type limitedBuffer struct {
	mu           sync.Mutex
	b            bytes.Buffer
	lastActivity time.Time
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	left := 8*1024*1024 - b.b.Len()
	if left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = b.b.Write(p)
	}
	if n > 0 {
		b.lastActivity = time.Now().UTC()
	}
	return n, nil
}

type Observation struct {
	Output       string
	Stdout       string
	Stderr       string
	LastActivity time.Time
}

// ManagedProcess is an AIH-owned child process. Unlike Background, it remains
// in the supervisor's process tree and Close terminates all of its descendants.
// It is for short-lived project adapters whose lifetime is bounded by a check.
type ManagedProcess struct {
	Stdout io.ReadCloser
	cmd    *exec.Cmd
	done   chan error
	stop   func()
	kill   func() error
	clean  func()
	once   sync.Once
}

// StartManaged starts an owned process and exposes its stdout for a bounded
// readiness protocol. The caller must Close it, including on cancellation.
func StartManaged(ctx context.Context, dir string, env []string, name string, args ...string) (*ManagedProcess, error) {
	cmd, err := command(name, args)
	if err != nil {
		return nil, err
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir, cmd.Env = dir, env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	prepare(cmd, false)
	lifeline, err := supervise(cmd)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		lifeline()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		lifeline()
		return nil, err
	}
	cleanup, kill, err := own(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		lifeline()
		return nil, err
	}
	p := &ManagedProcess{Stdout: stdout, cmd: cmd, done: make(chan error, 1), kill: kill}
	p.stop = func() {
		_ = stopProcess(p.done, kill, cmd.Process.Kill)
	}
	p.clean = func() { cleanup(); lifeline() }
	go func() { p.done <- cmd.Wait() }()
	return p, nil
}

// Close kills the owned child tree and waits for it before releasing ownership.
func (p *ManagedProcess) Close() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		p.stop()
		p.clean()
	})
}

func (b *limitedBuffer) snapshot() (string, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String(), b.lastActivity
}

func RunObserved(ctx context.Context, dir string, env []string, input string, name string, args ...string) (Observation, error) {
	cmd, err := command(name, args)
	if err != nil {
		return Observation{}, err
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = bytes.NewBufferString(input)
	var output, stderr limitedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &stderr
	prepare(cmd, false)
	lifeline, e := supervise(cmd)
	if e != nil {
		return Observation{}, e
	}
	defer lifeline()
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	if err := cmd.Start(); err != nil {
		return Observation{}, err
	}
	cleanup, kill, err := own(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return Observation{}, err
	}
	defer cleanup()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
		if terminationErr := stopProcess(done, kill, cmd.Process.Kill); terminationErr != nil {
			err = errors.Join(err, terminationErr)
		}
	}
	if err != nil {
		stdout, stdoutAt := output.snapshot()
		stderrText, stderrAt := stderr.snapshot()
		if stderrAt.After(stdoutAt) {
			stdoutAt = stderrAt
		}
		return Observation{Output: stdout + stderrText, Stdout: stdout, Stderr: stderrText, LastActivity: stdoutAt}, fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	stdout, stdoutAt := output.snapshot()
	stderrText, stderrAt := stderr.snapshot()
	if stderrAt.After(stdoutAt) {
		stdoutAt = stderrAt
	}
	return Observation{Output: stdout + stderrText, Stdout: stdout, Stderr: stderrText, LastActivity: stdoutAt}, nil
}

func stopProcess(done <-chan error, terminate, fallback func() error) error {
	terminateErr := terminate()
	if processStopped(done, processTerminationGrace) {
		return terminateErr
	}
	fallbackErr := fallback()
	if processStopped(done, processTerminationGrace) {
		return errors.Join(terminateErr, fallbackErr)
	}
	return errors.Join(terminateErr, fallbackErr, errProcessTerminationTimeout)
}

func processStopped(done <-chan error, within time.Duration) bool {
	timer := time.NewTimer(within)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func Run(ctx context.Context, dir string, env []string, input string, name string, args ...string) (string, error) {
	result, err := RunObserved(ctx, dir, env, input, name, args...)
	if err != nil {
		return result.Output, err
	}
	return result.Stdout, nil
}
func Background(executable string, args []string, dir, log string) error {
	f, e := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	cmd := exec.Command(executable, args...)
	cmd.Dir = dir
	cmd.Stdout = f
	cmd.Stderr = f
	prepare(cmd, true)
	if e = cmd.Start(); e != nil {
		return e
	}
	return cmd.Process.Release()
}
func WithTimeout(parent context.Context, d time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	return fn(ctx)
}
func CopyFile(dst, src string, mode os.FileMode) error {
	in, e := os.Open(src)
	if e != nil {
		return e
	}
	defer in.Close()
	out, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if e != nil {
		return e
	}
	_, e = io.Copy(out, in)
	if e == nil {
		e = out.Sync()
	}
	ce := out.Close()
	if e != nil {
		return e
	}
	return ce
}
