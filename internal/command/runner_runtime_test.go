package command

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/supervisor"
)

type scriptedRunner struct {
	results []supervisor.Outcome
	err     error
	calls   int
	cancel  context.CancelFunc
}

func (runner *scriptedRunner) RunOnce(ctx context.Context) (supervisor.Outcome, error) {
	runner.calls++
	if err := ctx.Err(); err != nil {
		return supervisor.Outcome{}, err
	}
	if len(runner.results) > 0 {
		out := runner.results[0]
		runner.results = runner.results[1:]
		return out, nil
	}
	if runner.cancel != nil {
		runner.cancel()
	}
	return supervisor.Outcome{}, runner.err
}

func TestSupervisedOnceExitAndSafeOutput(t *testing.T) {
	for _, test := range []struct {
		outcome string
		code    int
	}{{"no_findings", 0}, {"findings", 0}, {"changes_proposed", 0}, {"incomplete", 1}, {"needs_input", 1}} {
		t.Run(test.outcome, func(t *testing.T) {
			worker := &scriptedRunner{results: []supervisor.Outcome{{Worked: true, RunID: testRunnerID, AttemptID: testOperation, Result: test.outcome}}}
			var stdout, stderr bytes.Buffer
			code := runSupervised(context.Background(), worker, true, &stdout, &stderr)
			if code != test.code || worker.calls != 1 || stderr.Len() != 0 || !strings.Contains(stdout.String(), test.outcome) {
				t.Fatalf("code=%d calls=%d stdout=%q stderr=%q", code, worker.calls, stdout.String(), stderr.String())
			}
		})
	}
}

func TestSupervisedIdleAndContinuousPolling(t *testing.T) {
	var stdout, stderr bytes.Buffer
	worker := &scriptedRunner{}
	if code := runSupervised(context.Background(), worker, true, &stdout, &stderr); code != 0 || worker.calls != 1 || !strings.Contains(stdout.String(), "No eligible queued work") {
		t.Fatalf("idle code=%d stdout=%q", code, stdout.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker = &scriptedRunner{results: []supervisor.Outcome{{Worked: true, RunID: testRunnerID, AttemptID: testOperation, Result: "incomplete"}, {Worked: true, RunID: testRunnerID, AttemptID: testOperation, Result: "no_findings"}}, cancel: cancel}
	stdout.Reset()
	stderr.Reset()
	if code := runSupervised(ctx, worker, false, &stdout, &stderr); code != 1 || worker.calls != 3 || !strings.Contains(stdout.String(), "incomplete") || !strings.Contains(stdout.String(), "no_findings") {
		t.Fatalf("continuous code=%d calls=%d stdout=%q", code, worker.calls, stdout.String())
	}
}

func TestSupervisedErrorsNeverExposeRawMessages(t *testing.T) {
	for _, failure := range []error{errors.New("SYNTHETIC_SECRET: raw native output"), &protocol.ControlPlaneError{StatusCode: 403}, supervisor.ErrCleanupUnconfirmed} {
		var stdout, stderr bytes.Buffer
		if code := runSupervised(context.Background(), &scriptedRunner{err: failure}, true, &stdout, &stderr); code != 1 || stdout.Len() != 0 || strings.Contains(stderr.String(), "SYNTHETIC_SECRET") || stderr.Len() == 0 {
			t.Fatalf("unsafe error code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}
}

func TestNativeRunnerRefusesBeforeReadingCredentials(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunRunner([]string{"--base-url=https://runner.example", "--token-file=/nonexistent/private.token", "--state-dir=/nonexistent/state", "--image=fixture", "--driver=codex", "--profiles-dir=/nonexistent/profiles"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "Runner setup failed") || stdout.Len() != 0 {
		t.Fatalf("native boundary code=%d stderr=%q", code, stderr.String())
	}
}

func TestRunnerTokenReadIsPrivateBoundedAndRejectsLinks(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "runner.token")
	if err := os.WriteFile(path, []byte("123|synthetic-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if token, err := readRunnerToken(path); err != nil || token != "123|synthetic-token" {
		t.Fatalf("valid token rejected: %q %v", token, err)
	}
	for _, test := range []struct {
		name, value string
		mode        os.FileMode
	}{{"broad", "synthetic", 0644}, {"empty", "", 0600}, {"newline", "secret\nsecond", 0600}, {"oversized", strings.Repeat("s", 4097), 0600}} {
		t.Run(test.name, func(t *testing.T) {
			target := filepath.Join(directory, test.name)
			if err := os.WriteFile(target, []byte(test.value), test.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := readRunnerToken(target); err == nil {
				t.Fatal("accepted unsafe token")
			}
		})
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRunnerToken(link); err == nil {
		t.Fatal("accepted token symlink")
	}
	hardlink := filepath.Join(directory, "hardlink")
	if err := os.Link(path, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readRunnerToken(hardlink); err == nil {
		t.Fatal("accepted token hardlink")
	}
	if _, err := readRunnerToken(directory); err == nil {
		t.Fatal("accepted token directory")
	}
}

func TestRunnerTokenRejectsPathReplacementDuringRead(t *testing.T) {
	for _, phase := range []string{"after open", "before read", "after read"} {
		t.Run(phase, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "runner.token")
			if err := os.WriteFile(path, []byte("123|synthetic-token"), 0600); err != nil {
				t.Fatal(err)
			}
			replacePath := func() {
				t.Helper()
				oldPath := filepath.Join(directory, "opened-token")
				if err := os.Rename(path, oldPath); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("123|replacement-token"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			hooks := runnerTokenReadHooks{}
			switch phase {
			case "after open":
				hooks.afterOpen = replacePath
			case "before read":
				hooks.beforeRead = replacePath
			case "after read":
				hooks.afterRead = replacePath
			}
			token, err := readRunnerTokenWithHooks(path, hooks)
			if err == nil || token != "" {
				t.Fatal("accepted a token after its path was replaced")
			}
			if strings.Contains(err.Error(), "synthetic-token") || strings.Contains(err.Error(), "replacement-token") {
				t.Fatal("token content appeared in a read error")
			}
		})
	}
}

func TestFileIsTerminalRejectsNullDeviceAndPipes(t *testing.T) {
	null, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Fatal("treated /dev/null as a terminal")
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if isTerminal(reader) || isTerminal(writer) {
		t.Fatal("treated a pipe as a terminal")
	}
}

func TestDockerPreflightUsesDockerConfigurationWithoutProviderCredentials(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///synthetic/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "synthetic-context")
	t.Setenv("OPENAI_API_KEY", "synthetic-provider-secret")
	executable := filepath.Join(t.TempDir(), "docker-fixture")
	script := "#!/bin/sh\nprintf '%s|%s|%s' \"$DOCKER_HOST\" \"$DOCKER_CONTEXT\" \"${OPENAI_API_KEY-unset}\"\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	output, err := dockerPreflight(context.Background(), executable, "version")
	if err != nil || output != "unix:///synthetic/docker.sock|synthetic-context|unset" {
		t.Fatalf("preflight lost Docker configuration or exposed credentials: %q %v", output, err)
	}
}
