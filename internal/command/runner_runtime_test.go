package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
	"github.com/ianrodrigues/shipmunk-runner/internal/supervisor"
)

// The supervised loop's idle/retry backoff would otherwise make these tests
// sleep for real; zero it for the whole package's test binary.
func init() {
	supervisedBackoff = time.Microsecond
}

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

// sequencedStep is one scripted RunOnce return for sequencedRunner: exactly
// one of outcome or err, used by the consecutive-failure breaker tests where
// scriptedRunner's cancel-on-exhaustion behavior would get in the way.
type sequencedStep struct {
	outcome supervisor.Outcome
	err     error
}

type sequencedRunner struct {
	steps []sequencedStep
	calls int
}

func (runner *sequencedRunner) RunOnce(context.Context) (supervisor.Outcome, error) {
	if runner.calls >= len(runner.steps) {
		runner.calls++
		return supervisor.Outcome{}, errors.New("sequencedRunner exhausted its scripted steps")
	}
	step := runner.steps[runner.calls]
	runner.calls++
	return step.outcome, step.err
}

func malformedOutputOutcome() supervisor.Outcome {
	return supervisor.Outcome{Worked: true, RunID: testRunnerID, AttemptID: testOperation, Result: "incomplete", FailureReason: "malformed_output"}
}

func TestSupervisedStopsPollingAfterThreeConsecutiveRunnerClassifiedFailures(t *testing.T) {
	runner := &sequencedRunner{steps: []sequencedStep{
		{outcome: malformedOutputOutcome()},
		{err: supervisor.ErrCleanupUnconfirmed},
		{outcome: malformedOutputOutcome()},
		{outcome: malformedOutputOutcome()}, // must not be reached
	}}
	var stdout, stderr bytes.Buffer
	code := runSupervised(context.Background(), runner, false, &stdout, &stderr, "/state/logs/runner.log")
	if code != 1 || runner.calls != 3 {
		t.Fatalf("code=%d calls=%d stderr=%q", code, runner.calls, stderr.String())
	}
	if !strings.Contains(stderr.String(), "3 consecutive runner-classified failures") || !strings.Contains(stderr.String(), "malformed_output") || !strings.Contains(stderr.String(), "/state/logs/runner.log") {
		t.Fatalf("breaker message missing bound, reason, or log path: %q", stderr.String())
	}
}

func TestSupervisedIdlePollsDoNotResetTheConsecutiveCount(t *testing.T) {
	runner := &sequencedRunner{steps: []sequencedStep{
		{outcome: malformedOutputOutcome()},
		{}, // idle poll: no work, no error
		{outcome: malformedOutputOutcome()},
		{},                                  // idle poll
		{outcome: malformedOutputOutcome()}, // trips the bound
		{outcome: malformedOutputOutcome()}, // must not be reached
	}}
	var stdout, stderr bytes.Buffer
	code := runSupervised(context.Background(), runner, false, &stdout, &stderr, "")
	if code != 1 || runner.calls != 5 {
		t.Fatalf("expected idle polls between failures not to reset the count: code=%d calls=%d stderr=%q", code, runner.calls, stderr.String())
	}
	if !strings.Contains(stderr.String(), "3 consecutive runner-classified failures") {
		t.Fatalf("expected the breaker to trip: %q", stderr.String())
	}
}

func TestSupervisedExcludesServerAndTransportCausesFromCleanupCount(t *testing.T) {
	networkErr := &net.DNSError{Err: "no such host", Name: "runner.example", IsTimeout: true}
	// Each cause is retested across four identical polls with once=false, so
	// the assertion actually proves the cause is never counted across polls
	// rather than merely on a single call. A refused stopped acknowledgement
	// never retries in-process (see retryableCleanupFailure), so it exits
	// after its one call; the other causes retry until sequencedRunner's
	// steps are exhausted, at which point the unretryable exhaustion
	// sentinel ends the loop on the fifth call.
	for name, testCase := range map[string]struct {
		failure error
		calls   int
	}{
		"refused stopped ack": {fmt.Errorf("%w: %w", supervisor.ErrCleanupUnconfirmed, &supervisor.RefusedStoppedError{Count: 1, Threshold: 3}), 1},
		"control plane error": {fmt.Errorf("%w: %w", supervisor.ErrCleanupUnconfirmed, &protocol.ControlPlaneError{StatusCode: 503}), 5},
		"network error":       {fmt.Errorf("%w: %w", supervisor.ErrCleanupUnconfirmed, networkErr), 5},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &sequencedRunner{steps: []sequencedStep{
				{err: testCase.failure}, {err: testCase.failure}, {err: testCase.failure}, {err: testCase.failure},
			}}
			var stdout, stderr bytes.Buffer
			code := runSupervised(context.Background(), runner, false, &stdout, &stderr, "")
			if code != 1 || runner.calls != testCase.calls {
				t.Fatalf("code=%d calls=%d want %d, stderr=%q", code, runner.calls, testCase.calls, stderr.String())
			}
			if strings.Contains(stderr.String(), "consecutive runner-classified failures") {
				t.Fatalf("server/transport cause must never count toward the runner-defect bound: %q", stderr.String())
			}
		})
	}
}

func TestSupervisedCountsInvalidOutputSchemaAsRunnerClassified(t *testing.T) {
	invalidSchema := supervisor.Outcome{Worked: true, RunID: testRunnerID, AttemptID: testOperation, Result: "incomplete", FailureReason: "invalid_output_schema"}
	runner := &sequencedRunner{steps: []sequencedStep{{outcome: invalidSchema}, {outcome: invalidSchema}, {outcome: invalidSchema}}}
	var stdout, stderr bytes.Buffer
	code := runSupervised(context.Background(), runner, false, &stdout, &stderr, "")
	if code != 1 || !strings.Contains(stderr.String(), "invalid_output_schema") {
		t.Fatalf("expected invalid_output_schema to trip the breaker: code=%d stderr=%q", code, stderr.String())
	}
}

func TestSupervisedNeverRetriesInProcessOnARefusedStoppedAcknowledgement(t *testing.T) {
	refused := fmt.Errorf("%w: %w", supervisor.ErrCleanupUnconfirmed, &supervisor.RefusedStoppedError{Count: 1, Threshold: 3})
	runner := &sequencedRunner{steps: []sequencedStep{{err: refused}, {err: refused}}}
	var stdout, stderr bytes.Buffer
	code := runSupervised(context.Background(), runner, false, &stdout, &stderr, "")
	if code != 1 || runner.calls != 1 {
		t.Fatalf("expected a refused stopped acknowledgement to end supervision without an in-process retry: code=%d calls=%d", code, runner.calls)
	}
	if !strings.Contains(stderr.String(), "refused the stopped acknowledgement (1 of 3)") {
		t.Fatalf("expected the refused-stopped message to survive: %q", stderr.String())
	}
}

func TestSupervisedNeverRetriesInProcessOnAnUncertainSandboxCreate(t *testing.T) {
	uncertain := fmt.Errorf("%w: %w", supervisor.ErrCleanupUnconfirmed, sandbox.ErrCreateUncertain)
	runner := &sequencedRunner{steps: []sequencedStep{{err: uncertain}, {err: uncertain}}}
	var stdout, stderr bytes.Buffer
	code := runSupervised(context.Background(), runner, false, &stdout, &stderr, "")
	if code != 1 || runner.calls != 1 {
		t.Fatalf("expected an uncertain sandbox create to end supervision without an in-process retry: code=%d calls=%d", code, runner.calls)
	}
	if !strings.Contains(stderr.String(), "could not confirm sandbox cleanup") {
		t.Fatalf("expected supervisionFailure to be printed: %q", stderr.String())
	}
}

func TestSupervisedRetriesOtherCleanupUnconfirmedCausesInProcess(t *testing.T) {
	runner := &sequencedRunner{steps: []sequencedStep{
		{err: supervisor.ErrCleanupUnconfirmed},
		{outcome: supervisor.Outcome{Worked: true, RunID: testRunnerID, AttemptID: testOperation, Result: "no_findings"}},
		{err: supervisor.ErrLeaseExpired},
	}}
	var stdout, stderr bytes.Buffer
	code := runSupervised(context.Background(), runner, false, &stdout, &stderr, "")
	if code != 1 || runner.calls != 3 {
		t.Fatalf("expected the retry to continue past the plain cleanup-unconfirmed failure to the delivered success: code=%d calls=%d stderr=%q", code, runner.calls, stderr.String())
	}
	if !strings.Contains(stderr.String(), "could not confirm sandbox cleanup") {
		t.Fatalf("expected supervisionFailure to be printed before the retry: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "no_findings") {
		t.Fatalf("expected the retried attempt's delivered outcome on stdout: %q", stdout.String())
	}
}

func TestSupervisedResetsConsecutiveCountOnSuccess(t *testing.T) {
	runner := &sequencedRunner{steps: []sequencedStep{
		{outcome: malformedOutputOutcome()},
		{outcome: malformedOutputOutcome()},
		{outcome: supervisor.Outcome{Worked: true, RunID: testRunnerID, AttemptID: testOperation, Result: "no_findings"}},
		{outcome: malformedOutputOutcome()},
		{outcome: malformedOutputOutcome()},
		{outcome: malformedOutputOutcome()},
	}}
	var stdout, stderr bytes.Buffer
	code := runSupervised(context.Background(), runner, false, &stdout, &stderr, "/state/logs/runner.log")
	if code != 1 || runner.calls != 6 {
		t.Fatalf("expected the success on call 3 to reset the count: code=%d calls=%d stderr=%q", code, runner.calls, stderr.String())
	}
}

func TestSupervisedResetsConsecutiveCountOnProviderClassifiedFailure(t *testing.T) {
	runner := &sequencedRunner{steps: []sequencedStep{
		{outcome: malformedOutputOutcome()},
		{outcome: malformedOutputOutcome()},
		{outcome: supervisor.Outcome{Worked: true, RunID: testRunnerID, AttemptID: testOperation, Result: "incomplete", FailureReason: "auth_expired"}},
		{outcome: malformedOutputOutcome()},
		{outcome: malformedOutputOutcome()},
		{outcome: malformedOutputOutcome()},
	}}
	var stdout, stderr bytes.Buffer
	code := runSupervised(context.Background(), runner, false, &stdout, &stderr, "/state/logs/runner.log")
	if code != 1 || runner.calls != 6 {
		t.Fatalf("expected the provider-classified failure on call 3 to reset the count: code=%d calls=%d", code, runner.calls)
	}
}

func TestSupervisedOnceStillReportsASingleRunnerClassifiedFailure(t *testing.T) {
	runner := &sequencedRunner{steps: []sequencedStep{{outcome: malformedOutputOutcome()}}}
	var stdout, stderr bytes.Buffer
	code := runSupervised(context.Background(), runner, true, &stdout, &stderr, "")
	if code != 1 || runner.calls != 1 {
		t.Fatalf("code=%d calls=%d", code, runner.calls)
	}
	if !strings.Contains(stdout.String(), "incomplete") {
		t.Fatalf("expected the single attempt's outcome on stdout: %q", stdout.String())
	}
}

func TestSupervisedOnceStillExitsBeforeReachingTheBreakerBound(t *testing.T) {
	runner := &sequencedRunner{steps: []sequencedStep{{err: supervisor.ErrCleanupUnconfirmed}}}
	var stdout, stderr bytes.Buffer
	code := runSupervised(context.Background(), runner, true, &stdout, &stderr, "")
	if code != 1 || runner.calls != 1 {
		t.Fatalf("code=%d calls=%d", code, runner.calls)
	}
	if strings.Contains(stderr.String(), "consecutive runner-classified failures") {
		t.Fatalf("once mode should not report the breaker on a single failure: %q", stderr.String())
	}
}

func TestSupervisedOnceExitAndSafeOutput(t *testing.T) {
	for _, test := range []struct {
		outcome string
		code    int
	}{{"no_findings", 0}, {"findings", 0}, {"changes_proposed", 0}, {"incomplete", 1}, {"needs_input", 1}} {
		t.Run(test.outcome, func(t *testing.T) {
			worker := &scriptedRunner{results: []supervisor.Outcome{{Worked: true, RunID: testRunnerID, AttemptID: testOperation, Result: test.outcome}}}
			var stdout, stderr bytes.Buffer
			code := runSupervised(context.Background(), worker, true, &stdout, &stderr, "")
			if code != test.code || worker.calls != 1 || stderr.Len() != 0 || !strings.Contains(stdout.String(), test.outcome) {
				t.Fatalf("code=%d calls=%d stdout=%q stderr=%q", code, worker.calls, stdout.String(), stderr.String())
			}
		})
	}
}

func TestSupervisedIdleAndContinuousPolling(t *testing.T) {
	var stdout, stderr bytes.Buffer
	worker := &scriptedRunner{}
	if code := runSupervised(context.Background(), worker, true, &stdout, &stderr, ""); code != 0 || worker.calls != 1 || !strings.Contains(stdout.String(), "No eligible queued work") {
		t.Fatalf("idle code=%d stdout=%q", code, stdout.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker = &scriptedRunner{results: []supervisor.Outcome{{Worked: true, RunID: testRunnerID, AttemptID: testOperation, Result: "incomplete"}, {Worked: true, RunID: testRunnerID, AttemptID: testOperation, Result: "no_findings"}}, cancel: cancel}
	stdout.Reset()
	stderr.Reset()
	if code := runSupervised(ctx, worker, false, &stdout, &stderr, ""); code != 1 || worker.calls != 3 || !strings.Contains(stdout.String(), "incomplete") || !strings.Contains(stdout.String(), "no_findings") {
		t.Fatalf("continuous code=%d calls=%d stdout=%q", code, worker.calls, stdout.String())
	}
}

func TestSupervisedErrorsNeverExposeRawMessages(t *testing.T) {
	for _, failure := range []error{errors.New("SYNTHETIC_SECRET: raw native output"), &protocol.ControlPlaneError{StatusCode: 403}, supervisor.ErrCleanupUnconfirmed} {
		var stdout, stderr bytes.Buffer
		if code := runSupervised(context.Background(), &scriptedRunner{err: failure}, true, &stdout, &stderr, ""); code != 1 || stdout.Len() != 0 || strings.Contains(stderr.String(), "SYNTHETIC_SECRET") || stderr.Len() == 0 {
			t.Fatalf("unsafe error code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}
}

func TestSupervisedFailuresNameTheirCondition(t *testing.T) {
	for name, test := range map[string]struct {
		failure error
		expect  string
	}{
		"unconfirmed cleanup": {supervisor.ErrCleanupUnconfirmed, "could not confirm sandbox cleanup"},
		"refused stopped ack": {fmt.Errorf("%w: %w: %w", supervisor.ErrCleanupUnconfirmed, &supervisor.RefusedStoppedError{Count: 1, Threshold: 3}, errors.New("conflict")), "refused the stopped acknowledgement (1 of 3)"},
		"expired lease":       {supervisor.ErrLeaseExpired, "lease expired"},
		"revoked attempt":     {supervisor.ErrStopped, "revoked this attempt"},
		"rejected request":    {&protocol.ControlPlaneError{StatusCode: 409}, "HTTP 409"},
		"operator stop":       {context.Canceled, "stopped on request"},
		"unclassified":        {errors.New("SYNTHETIC_SECRET"), "before a completed result"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runSupervised(context.Background(), &scriptedRunner{err: test.failure}, true, &stdout, &stderr, "")
			if code != 1 || !strings.Contains(stderr.String(), test.expect) || strings.Contains(stderr.String(), "SYNTHETIC_SECRET") {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
		})
	}
}

func TestSupervisionFailurePointsAtLogPath(t *testing.T) {
	message := supervisionFailure(supervisor.ErrCleanupUnconfirmed, "/state/logs/runner.log")
	if !strings.Contains(message, "could not confirm sandbox cleanup") || !strings.Contains(message, "/state/logs/runner.log") {
		t.Fatalf("expected the log path to be named alongside the condition: %q", message)
	}
	if without := supervisionFailure(supervisor.ErrCleanupUnconfirmed, ""); strings.Contains(without, "runner log") {
		t.Fatalf("expected no log pointer without a log path: %q", without)
	}
}

func TestRunSupervisedPointsAtLogPathOnFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSupervised(context.Background(), &scriptedRunner{err: supervisor.ErrLeaseExpired}, true, &stdout, &stderr, "/state/logs/runner.log")
	if code != 1 || !strings.Contains(stderr.String(), "/state/logs/runner.log") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestRunnerSetupFailureNamesLogPathAndRecordsStep(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the runner refuses to start as root, so setup never opens the runner log")
	}
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := RunRunner([]string{"--base-url=https://runner.example", "--token-file=" + filepath.Join(stateDir, "missing.token"), "--state-dir=" + stateDir, "--image=fixture"}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, stdout.String())
	}
	logPath := filepath.Join(stateDir, "logs", "runner.log")
	if !strings.Contains(stderr.String(), "Runner setup failed") || !strings.Contains(stderr.String(), logPath) {
		t.Fatalf("stderr does not name the runner log: %q", stderr.String())
	}
	contents, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected a runner log at %s: %v", logPath, err)
	}
	if !strings.Contains(string(contents), "setup_failed") || !strings.Contains(string(contents), "read_token") {
		t.Fatalf("expected the log to record the failing step: %s", contents)
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

func TestRunnerTokenRefusesIncompleteConfigurationActivation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "runner.token")
	if err := os.WriteFile(path, []byte("123|synthetic-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".activation.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRunnerToken(path); err == nil || !strings.Contains(err.Error(), "activation") {
		t.Fatalf("incomplete activation error = %v", err)
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
