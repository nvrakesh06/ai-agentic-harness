//go:build !windows

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func command(name string, args []string) (*exec.Cmd, error) {
	cmd := exec.Command(name, args...)
	return cmd, cmd.Err
}

func prepare(cmd *exec.Cmd, background bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	if background {
		cmd.SysProcAttr.Setsid = true
	} else {
		cmd.SysProcAttr.Setpgid = true
	}
}

// A separate pipe, unrelated to prompt stdin, closes if the supervisor dies.
// Its watcher kills the process group, including grandchildren, even on macOS
// where Linux's parent-death signal is unavailable. Only fixed shell text runs;
// the command and its arguments are passed verbatim through "$@".
func supervise(cmd *exec.Cmd) (func(), error) {
	reader, writer, e := os.Pipe()
	if e != nil {
		return nil, e
	}
	argv := append([]string{cmd.Path}, cmd.Args[1:]...)
	script := "exec 4<&0\n\"$@\" <&4 &\nchild=$!\n( cat <&3 >/dev/null; kill -KILL -$$ 2>/dev/null ) >/dev/null 2>&1 &\nwait \"$child\"\nexit $?\n"
	cmd.Path = "/bin/sh"
	cmd.Args = append([]string{"sh", "-c", script, "aih-process"}, argv...)
	cmd.ExtraFiles = []*os.File{reader}
	return func() { _ = writer.Close(); _ = reader.Close() }, nil
}
func own(cmd *exec.Cmd) (func(), func() error, error) {
	kill := func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return func() { _ = kill() }, kill, nil
}

type Lock struct{ f *os.File }

func Acquire(path string) (*Lock, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		if e == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("%w: %v", ErrLocked, e)
		}
		return nil, fmt.Errorf("supervisor already active or lock unavailable: %w", e)
	}
	return &Lock{f}, nil
}
func (l *Lock) Close() error { _ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); return l.f.Close() }
