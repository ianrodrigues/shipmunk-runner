//go:build linux

package sandbox

import (
	"os/exec"
	"syscall"
)

func configureDockerCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
