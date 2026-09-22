package platform

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type limitedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
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
	return n, nil
}
func Run(ctx context.Context, dir string, env []string, input string, name string, args ...string) (string, error) {
	cmd, err := command(name, args)
	if err != nil {
		return "", err
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
		return "", e
	}
	defer lifeline()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	cleanup, kill, err := own(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", err
	}
	defer cleanup()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		kill()
		<-done
		err = ctx.Err()
	}
	if err != nil {
		return output.b.String() + stderr.b.String(), fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return output.b.String(), nil
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
