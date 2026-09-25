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

// ErrProcessTerminationUncertain means cancellation could not prove that the
// owned child tree stopped. Callers must not repeat a mutating command because
// the original process may still be executing it.
var ErrProcessTerminationUncertain = errors.New("process termination did not complete within the bounded fallback")

// Kept as an internal alias for existing process tests and diagnostics.
var errProcessTerminationTimeout = ErrProcessTerminationUncertain

const processTerminationGrace = 500 * time.Millisecond

const maxCapturedOutputBytes = 8 * 1024 * 1024

const failureCaptureMarker = "\n[... earlier process output omitted; showing first and last captured output ...]\n"

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
	tail         []byte
	tailStart    int
	tailLen      int
	lastActivity time.Time
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	left := maxCapturedOutputBytes - b.b.Len()
	if left > 0 {
		if len(p) > left {
			_, _ = b.b.Write(p[:left])
			p = p[left:]
		} else {
			_, _ = b.b.Write(p)
			p = nil
		}
	}
	if len(p) > 0 {
		if b.tail == nil {
			b.tail = make([]byte, maxCapturedOutputBytes)
			copy(b.tail, b.b.Bytes())
			b.tailLen = maxCapturedOutputBytes
		}
		b.appendTail(p)
	}
	if n > 0 {
		b.lastActivity = time.Now().UTC()
	}
	return n, nil
}

func (b *limitedBuffer) appendTail(p []byte) {
	if len(p) >= maxCapturedOutputBytes {
		copy(b.tail, p[len(p)-maxCapturedOutputBytes:])
		b.tailStart, b.tailLen = 0, maxCapturedOutputBytes
		return
	}
	start := (b.tailStart + b.tailLen) % maxCapturedOutputBytes
	copy(b.tail[start:], p)
	if remaining := len(p) - (maxCapturedOutputBytes - start); remaining > 0 {
		copy(b.tail, p[len(p)-remaining:])
	}
	if b.tailLen < maxCapturedOutputBytes {
		b.tailLen += len(p)
		if b.tailLen > maxCapturedOutputBytes {
			b.tailLen = maxCapturedOutputBytes
		}
	}
	b.tailStart = (b.tailStart + len(p)) % maxCapturedOutputBytes
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
	wait   chan struct{}
	mu     sync.Mutex
	err    error
	stderr *limitedBuffer
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
	p := &ManagedProcess{Stdout: stdout, cmd: cmd, done: make(chan error, 1), wait: make(chan struct{}), kill: kill, stderr: &stderr}
	p.stop = func() {
		_ = stopProcess(p.done, kill, cmd.Process.Kill)
	}
	p.clean = func() { cleanup(); lifeline() }
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.wait)
		p.done <- err
	}()
	return p, nil
}

// Wait returns the child exit result without consuming the termination signal
// that Close uses to prove an owned process tree has stopped.
func (p *ManagedProcess) Wait() error {
	if p == nil {
		return nil
	}
	<-p.wait
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Stderr returns diagnostics captured from the managed child.
func (p *ManagedProcess) Stderr() string {
	if p == nil || p.stderr == nil {
		return ""
	}
	text, _ := p.stderr.snapshot()
	return text
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

// failureSnapshot keeps the existing first-output behavior for successful
// commands while exposing the rolling tail when a command fails. That tail is
// where test runners usually print the decisive diagnostic after long success
// logs, and it remains bounded to the existing per-stream capture limit.
func (b *limitedBuffer) failureSnapshot() (string, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tail == nil {
		return b.b.String(), b.lastActivity
	}
	headBytes := maxCapturedOutputBytes / 4
	tailBytes := maxCapturedOutputBytes - headBytes - len(failureCaptureMarker)
	tail := string(b.tail[b.tailStart:]) + string(b.tail[:b.tailStart])
	return b.b.String()[:headBytes] + failureCaptureMarker + tail[len(tail)-tailBytes:], b.lastActivity
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
		stdout, stdoutAt := output.failureSnapshot()
		stderrText, stderrAt := stderr.failureSnapshot()
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
	return errors.Join(terminateErr, fallbackErr, ErrProcessTerminationUncertain)
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
