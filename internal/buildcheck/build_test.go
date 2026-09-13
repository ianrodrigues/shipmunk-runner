package buildcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBuildTargetPropagatesEachCommandFailure(t *testing.T) {
	makefile, err := filepath.Abs("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"all-succeed", "shipmunk-runner", "shipmunk-profile", "shipmunk-setup"} {
		t.Run(command, func(t *testing.T) {
			directory := t.TempDir()
			fakeGo := "#!/bin/sh\ncase \"$*\" in\n*./cmd/" + command + ") exit 17;;\nesac\nexit 0\n"
			if err := os.WriteFile(filepath.Join(directory, "go"), []byte(fakeGo), 0700); err != nil {
				t.Fatal(err)
			}
			process := exec.Command("make", "--no-print-directory", "-f", makefile, "go-build")
			process.Dir = directory
			process.Env = []string{"PATH=" + directory + string(os.PathListSeparator) + os.Getenv("PATH"), "TMPDIR=" + directory}
			output, err := process.CombinedOutput()
			if command == "all-succeed" && err != nil {
				t.Fatalf("successful build commands failed: %v: %s", err, output)
			}
			if command != "all-succeed" && err == nil {
				t.Fatalf("build failure was hidden for %s: %s", command, output)
			}
		})
	}
}
