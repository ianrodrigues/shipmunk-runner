//go:build !linux

package sandbox

import "os/exec"

func configureDockerCommand(_ *exec.Cmd) {}
