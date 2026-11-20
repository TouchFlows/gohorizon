//go:build unix

package process

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the worker in its own process group so an escalated SIGKILL
// cannot miss a child the worker spawned.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalTerminate asks the worker to finish its current job and exit.
// (exitStatus lives at the bottom of this file.)
// Laravel handles SIGTERM (as well as SIGINT/SIGQUIT) by setting shouldQuit and
// returning from the loop after the job in flight completes.
func signalTerminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// signalKillGroup escalates to SIGKILL on the process group.
func signalKillGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// exitStatus turns Wait's error into the exit code and a human reason. A signal death
// is reported as -1, so it can never be confused with Laravel's exit 1 (error) or
// exit 12 (memory).
func exitStatus(err error) (int, string) {
	if err == nil {
		return 0, "exited"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return -1, "killed by " + status.Signal().String()
		}
		return exitErr.ExitCode(), "exited"
	}
	return -1, err.Error()
}
