//go:build !windows

// Package proc holds the small set of OS process controls Komet needs:
// detaching supervisors, liveness checks, signals, priority and file locks.
package proc

import (
	"os"
	"os/exec"
	"syscall"
)

// detach starts the supervisor in its own session so it survives the CLI
// process exiting and is not killed by the terminal's Ctrl-C.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func Terminate(pid int) {
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

func SetNice(pid, nice int) error {
	return syscall.Setpriority(syscall.PRIO_PROCESS, pid, nice)
}

func Lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
