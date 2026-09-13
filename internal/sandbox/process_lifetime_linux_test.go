//go:build linux

package sandbox

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestRunDockerCommandUsesLinuxParentDeathSignal(t *testing.T) {
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 0.05")
	configureDockerCommand(command)
	if command.SysProcAttr == nil || command.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("Docker command Pdeathsig = %#v, want SIGKILL", command.SysProcAttr)
	}
	if err := runDockerCommand(command); err != nil {
		t.Fatalf("run Docker command while retaining its creating OS thread: %v", err)
	}
}
