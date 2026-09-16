package command

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
)

func TestRunCLIDispatchesLauncherAndManualModes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("installed commands refuse root")
	}
	for name, test := range map[string]struct {
		args       []string
		wantCode   int
		wantStdout []string
		wantStderr []string
	}{
		"empty args": {
			args:       nil,
			wantCode:   2,
			wantStderr: []string{"Usage: shipmunk-runner"},
		},
		"unknown command": {
			args:       []string{"bogus"},
			wantCode:   2,
			wantStderr: []string{"Unknown command"},
		},
		"top-level help": {
			args:       []string{"--help"},
			wantCode:   0,
			wantStdout: []string{"Usage: shipmunk-runner <command>", "disconnect"},
		},
		"top-level version": {
			args:       []string{"--version"},
			wantCode:   0,
			wantStdout: []string{"shipmunk-runner development\n"},
		},
		"run with no root argument uses the raw flag surface": {
			args:       []string{"run"},
			wantCode:   2,
			wantStderr: []string{"Missing required --base-url"},
		},
		"run --once with no root argument uses the raw flag surface": {
			args:       []string{"run", "--once"},
			wantCode:   2,
			wantStderr: []string{"Missing required --base-url"},
		},
		"run with a positional root reaches the installed dispatch": {
			args:       []string{"run", "/does/not/exist/as/an/installation"},
			wantCode:   1,
			wantStderr: []string{"Installed runner configuration is unsafe or incomplete."},
		},
		"connect with a positional root reaches the installed dispatch": {
			args:       []string{"connect", "/does/not/exist/as/an/installation"},
			wantCode:   1,
			wantStderr: []string{"Installed runner configuration is unsafe or incomplete."},
		},
		"probe with a positional root reaches the installed dispatch": {
			args:       []string{"probe", "/does/not/exist/as/an/installation"},
			wantCode:   1,
			wantStderr: []string{"Installed runner configuration is unsafe or incomplete."},
		},
		"connect with raw flags uses the manual flag surface": {
			args:       []string{"connect"},
			wantCode:   2,
			wantStderr: []string{"Missing required --base-url"},
		},
		"probe with raw flags uses the manual flag surface": {
			args:       []string{"probe"},
			wantCode:   2,
			wantStderr: []string{"Missing required --base-url"},
		},
		"disconnect with raw flags uses the manual flag surface": {
			args:       []string{"disconnect"},
			wantCode:   2,
			wantStderr: []string{"Missing required --base-url"},
		},
		"connect --help names the connect subcommand": {
			args:       []string{"connect", "--help"},
			wantCode:   0,
			wantStdout: []string{"Usage: shipmunk-runner connect"},
		},
		"probe --help names the probe subcommand": {
			args:       []string{"probe", "--help"},
			wantCode:   0,
			wantStdout: []string{"Usage: shipmunk-runner probe"},
		},
		"disconnect --help names the disconnect subcommand": {
			args:       []string{"disconnect", "--help"},
			wantCode:   0,
			wantStdout: []string{"Usage: shipmunk-runner disconnect"},
		},
		"run --help names the run subcommand": {
			args:       []string{"run", "--help"},
			wantCode:   0,
			wantStdout: []string{"Usage: shipmunk-runner run"},
		},
		"setup dispatches to guided setup": {
			args:       []string{"setup"},
			wantCode:   2,
			wantStderr: []string{"Invalid setup options"},
		},
		"connect rejects a caller-supplied --operation": {
			args:       []string{"connect", "--operation=disconnect"},
			wantCode:   2,
			wantStderr: []string{"sets --operation itself"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunCLI(test.args, &stdout, &stderr)
			if code != test.wantCode {
				t.Fatalf("code = %d, want %d; stdout %q, stderr %q", code, test.wantCode, stdout.String(), stderr.String())
			}
			for _, want := range test.wantStdout {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("stdout %q missing %q", stdout.String(), want)
				}
			}
			for _, want := range test.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr %q missing %q", stderr.String(), want)
				}
			}
		})
	}
}

func TestRunCLIManualModeSetsOperationFromTheSubcommand(t *testing.T) {
	original := runNativeProfile
	t.Cleanup(func() { runNativeProfile = original })
	for subcommand, wantOperation := range map[string]string{"probe": "probe", "disconnect": "disconnect"} {
		t.Run(subcommand, func(t *testing.T) {
			var gotOperation string
			runNativeProfile = func(options ProfileOptions, _ io.Writer) (profile.Health, error) {
				gotOperation = options.Operation
				return profile.Health{Health: "disconnected"}, nil
			}
			args := []string{
				subcommand,
				"--base-url", "https://runner.example", "--token-file", "/private/profile.token",
				"--profiles-dir", "/private/profiles", "--image", "shipmunk:local",
				"--profile", testProfileID, "--operation-id", testOperation,
			}
			var stdout, stderr bytes.Buffer
			if code := RunCLI(args, &stdout, &stderr); code != 0 {
				t.Fatalf("%s code = %d, stdout %q, stderr %q", subcommand, code, stdout.String(), stderr.String())
			}
			if gotOperation != wantOperation {
				t.Fatalf("%s set operation %q, want %q", subcommand, gotOperation, wantOperation)
			}
		})
	}
}
