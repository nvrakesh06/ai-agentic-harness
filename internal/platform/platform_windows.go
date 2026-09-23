//go:build windows

package platform

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

func prepare(cmd *exec.Cmd, background bool) {
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP)
	if background {
		flags |= 0x00000008
	} else {
		// Own the process before its first instruction can spawn children.
		flags |= windows.CREATE_SUSPENDED
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
}

func supervise(cmd *exec.Cmd) (func(), error) { return func() {}, nil }
func own(cmd *exec.Cmd) (func(), func() error, error) {
	job, e := windows.CreateJobObject(nil, nil)
	if e != nil {
		return nil, nil, e
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, e = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if e != nil {
		windows.CloseHandle(job)
		return nil, nil, e
	}
	p, e := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if e == nil {
		e = windows.AssignProcessToJobObject(job, p)
		windows.CloseHandle(p)
	}
	if e != nil {
		windows.CloseHandle(job)
		return nil, nil, fmt.Errorf("own worker job: %w", e)
	}
	if e = resumePrimary(uint32(cmd.Process.Pid)); e != nil {
		windows.CloseHandle(job)
		return nil, nil, e
	}
	return func() { windows.CloseHandle(job) }, func() error { return windows.TerminateJobObject(job, 1) }, nil
}

func resumePrimary(pid uint32) error {
	snapshot, e := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if e != nil {
		return e
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for e = windows.Thread32First(snapshot, &entry); e == nil; e = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		_, err = windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		return err
	}
	return fmt.Errorf("cannot find suspended worker thread: %w", e)
}

// Execute known npm entrypoints through Node, preserving argv without cmd.exe
// expansion. Other batch scripts require an explicit shell command in config.
func command(name string, args []string) (*exec.Cmd, error) {
	resolved, e := exec.LookPath(name)
	if e != nil {
		return nil, e
	}
	ext := strings.ToLower(filepath.Ext(resolved))
	if ext != ".cmd" && ext != ".bat" {
		return exec.Command(resolved, args...), nil
	}
	entries := map[string]string{"npm": "npm/bin/npm-cli.js", "npx": "npm/bin/npx-cli.js", "codex": "@openai/codex/bin/codex.js", "claude": "@anthropic-ai/claude-code/cli.js", "pnpm": "pnpm/bin/pnpm.cjs"}
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(resolved)), ext)
	entry, ok := entries[base]
	if !ok {
		return nil, fmt.Errorf("batch executable %s requires an explicit cmd or PowerShell check; use a native executable for providers", base)
	}
	js := filepath.Join(filepath.Dir(resolved), "node_modules", filepath.FromSlash(entry))
	if _, e = os.Stat(js); e != nil {
		return nil, fmt.Errorf("npm shim %s has no known Node entrypoint: %w", base, e)
	}
	return exec.Command("node", append([]string{js}, args...)...), nil
}

type Lock struct {
	f  *os.File
	ov windows.Overlapped
}

func Acquire(path string) (*Lock, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	l := &Lock{f: f}
	e = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &l.ov)
	if e != nil {
		f.Close()
		if e == windows.ERROR_LOCK_VIOLATION {
			return nil, fmt.Errorf("%w: %v", ErrLocked, e)
		}
		return nil, fmt.Errorf("supervisor already active or lock unavailable: %w", e)
	}
	return l, nil
}
func (l *Lock) Close() error {
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, &l.ov)
	return l.f.Close()
}
