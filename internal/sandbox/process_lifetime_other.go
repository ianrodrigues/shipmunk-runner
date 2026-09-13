//go:build !linux

package sandbox

import "os/exec"

func configureDockerCommand(_ *exec.Cmd) {}

func runDockerCommand(command *exec.Cmd) error {
	return command.Run()
}
