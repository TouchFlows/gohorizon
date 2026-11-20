//go:build !unix

package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

func setProcessGroup(*exec.Cmd) {}

func signalTerminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(os.Interrupt)
}

func signalKillGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Kill(); err != nil {
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}
	return nil
}

func exitStatus(err error) (int, string) {
	if err == nil {
		return 0, "exited"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), "exited"
	}
	return -1, err.Error()
}
