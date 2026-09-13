//go:build linux

package sandbox

import (
	"os/exec"
	"runtime"
	"syscall"
)

func configureDockerCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// runDockerCommand keeps Linux's Pdeathsig creating thread alive until the
// child exits. Linux sends Pdeathsig when that thread terminates, not merely
// when the Go process exits.
func runDockerCommand(command *exec.Cmd) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := command.Start(); err != nil {
		return err
	}
	return command.Wait()
}
