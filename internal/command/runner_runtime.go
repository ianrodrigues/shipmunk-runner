package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/codex"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/runlog"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
	"github.com/ianrodrigues/shipmunk-runner/internal/supervisor"
	"github.com/ianrodrigues/shipmunk-runner/internal/workspace"
)

var runnerStartupAfterAttemptLock = func() {}

func runFixtureRunner(options RunnerOptions, stdout, stderr io.Writer) int {
	if os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "Run the runner as a dedicated non-root account with Docker access.")
		return 1
	}
	// Opened before the attempt-state lock, so a losing concurrent process can
	// still log here; the resulting race is bounded since the lock failure
	// follows immediately.
	logger, _ := runlog.Open(options.StateDir)
	defer logger.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	worker, closeState, err := prepareFixtureRunner(ctx, options, logger)
	if err != nil {
		fmt.Fprintln(stderr, setupFailure("Check the private token, state directory, Docker engine, image, and adjacent shipmunk-watchdog executable.", logger))
		return 1
	}
	defer closeState()
	return runSupervised(ctx, worker, options.Once, stdout, stderr, logger.Path())
}

func prepareFixtureRunner(ctx context.Context, options RunnerOptions, logger *runlog.Logger) (*supervisor.Supervisor, func() error, error) {
	fail := func(err error, step string) (*supervisor.Supervisor, func() error, error) {
		logger.Event("setup_failed", map[string]any{"step": step, "error": err.Error()})
		return nil, nil, err
	}
	store, err := attemptstate.Open(filepath.Join(options.StateDir, "active-attempt.json"))
	if err != nil {
		return fail(err, "attempt_state")
	}
	ready := false
	defer func() {
		if !ready {
			_ = store.Close()
		}
	}()
	runnerStartupAfterAttemptLock()
	token, err := readRunnerToken(options.TokenFile)
	if err != nil {
		return fail(err, "read_token")
	}
	client, err := protocol.NewHTTPClient(options.BaseURL, token, nil)
	if err != nil {
		return fail(err, "http_client")
	}
	executable, err := os.Executable()
	if err != nil {
		return fail(err, "executable_path")
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return fail(err, "executable_path")
	}
	watchdog := filepath.Join(filepath.Dir(executable), "shipmunk-watchdog")
	info, err := os.Lstat(watchdog)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return fail(errors.New("independent watchdog executable is unavailable or unsafe"), "watchdog_executable")
	}
	dockerExecutable, err := exec.LookPath("docker")
	if err != nil {
		return fail(err, "docker_lookup")
	}
	platform, err := dockerPreflight(ctx, dockerExecutable, "version", "--format", "{{.Server.Os}}")
	if err != nil || platform != "linux" {
		return fail(errors.New("a Linux Docker engine is required"), "docker_platform")
	}
	image, err := dockerPreflight(ctx, dockerExecutable, "image", "inspect", "--format", "{{.Id}}", "--", options.Image)
	if err != nil || !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(image) {
		return fail(errors.New("runtime image must exist locally"), "docker_image")
	}
	config := sandbox.Config{Image: image, DockerExecutable: dockerExecutable, WatchdogExecutable: watchdog}
	docker, err := sandbox.New(config)
	if err != nil {
		return fail(err, "sandbox_init")
	}
	workspaces, err := workspace.New(filepath.Join(options.StateDir, "workspaces"))
	if err != nil {
		return fail(err, "workspace_init")
	}
	ready = true
	return &supervisor.Supervisor{Client: client, State: store, Workspaces: workspaces, Sandbox: supervisor.DockerBackend{Docker: docker}, Watchdog: supervisor.HostWatchdog{Watchdog: sandbox.NewWatchdog(config)}, Log: logger}, store.Close, nil
}

type attemptRunner interface {
	RunOnce(context.Context) (supervisor.Outcome, error)
}

// consecutiveRunnerFailureBound stops polling after this many consecutive
// runner-classified failures (see runnerClassifiedFailure). The count is
// in-memory only and resets on any restart; see docs/runner/failure-breaker.md.
const consecutiveRunnerFailureBound = 3

// supervisedBackoff is the supervised loop's base pause: an idle poll's
// fixed cadence, and the starting point a repeated non-runner failure backs
// off from (see transportBackoff). A package var so tests can zero it
// instead of sleeping for real.
var supervisedBackoff = 2 * time.Second

// supervisedBackoffCap bounds transportBackoff's exponential growth, so a
// long control-plane or network outage settles at a fixed retry cadence
// instead of spacing attempts out indefinitely. A package var for the same
// reason as supervisedBackoff.
var supervisedBackoffCap = 2 * time.Minute

// transportBackoff paces the cleanup-retry path's stderr and sleep for a
// repeated non-runner failure — a control-plane or network cause, which
// docs/runner/failure-breaker.md's consecutive-failure breaker never counts
// and so would otherwise retry forever at the fixed base cadence. The first
// occurrence of a given condition prints in full at the base pause; each
// identical repeat prints a short "still failing" line and doubles the pause
// up to supervisedBackoffCap. A delivered attempt or a successful idle poll
// resets it, since either is proof the transport recovered.
type transportBackoff struct {
	message string
	streak  int
}

func (backoff *transportBackoff) reset() {
	backoff.message = ""
	backoff.streak = 0
}

// report returns the stderr line for this occurrence of message: the
// message itself the first time it's seen, or a periodic "still failing"
// line while the identical condition repeats.
func (backoff *transportBackoff) report(message, logPath string) string {
	if message != backoff.message {
		backoff.message = message
		backoff.streak = 1
		return message
	}
	backoff.streak++
	return stillFailingMessage(backoff.streak, logPath)
}

// pause returns the sleep for the current streak: supervisedBackoff on the
// first occurrence, doubling on each repeat up to supervisedBackoffCap.
func (backoff *transportBackoff) pause() time.Duration {
	pause := supervisedBackoff
	for repeat := 1; repeat < backoff.streak && pause < supervisedBackoffCap; repeat++ {
		pause *= 2
	}
	if pause > supervisedBackoffCap {
		return supervisedBackoffCap
	}
	return pause
}

func runSupervised(ctx context.Context, worker attemptRunner, once bool, stdout, stderr io.Writer, logPath string) int {
	consecutiveRunnerFailures := 0
	backoff := transportBackoff{}
	for {
		outcome, err := worker.RunOnce(ctx)
		reason, classified := runnerClassifiedFailure(outcome, err)
		switch {
		case classified:
			consecutiveRunnerFailures++
		case err == nil && outcome.Worked:
			// Reset only here: a delivered, non-runner-classified attempt is the
			// only proof the runner is healthy again — an idle poll or a
			// transient claim/network error must not mask a real streak.
			consecutiveRunnerFailures = 0
		}
		if consecutiveRunnerFailures >= consecutiveRunnerFailureBound {
			fmt.Fprintln(stderr, consecutiveRunnerFailureMessage(reason, consecutiveRunnerFailures, logPath))
			return 1
		}
		if err != nil {
			if retryableCleanupFailure(err) && !once {
				fmt.Fprintln(stderr, backoff.report(supervisionFailure(err, logPath), logPath))
				if !sleepOrDone(ctx, backoff.pause()) {
					return 1
				}
				continue
			}
			fmt.Fprintln(stderr, supervisionFailure(err, logPath))
			return 1
		}
		// A successful claim — delivered or idle — is proof the transport
		// recovered, so it always clears any streak from an earlier retry.
		backoff.reset()
		code := 0
		if outcome.Worked {
			switch outcome.Result {
			case "findings", "no_findings", "changes_proposed":
			case "incomplete", "needs_input":
				code = 1
			default:
				fmt.Fprintln(stderr, "Runner returned an unsupported outcome.")
				return 1
			}
			fmt.Fprintf(stdout, "Run %s, attempt %s: %s.\n", outcome.RunID, outcome.AttemptID, outcome.Result)
		} else if once {
			fmt.Fprintln(stdout, "No eligible queued work was returned for this runner.")
		}
		if once {
			return code
		}
		if !outcome.Worked {
			if !sleepOrDone(ctx, supervisedBackoff) {
				return 1
			}
		}
	}
}

// retryableCleanupFailure reports whether a cleanup-unconfirmed error is
// safe to retry on the next poll. A refused stopped acknowledgement never
// retries here: each reconcile attempt would advance the server's
// three-refusal settle count, so an in-process loop could exhaust it in
// seconds instead of requiring separate operator relaunches. An uncertain
// sandbox create on a reserved name never retries either: it recurs
// identically on every poll until an operator or the original watchdog
// resolves it, so retrying in-process would only spin.
func retryableCleanupFailure(err error) bool {
	if !errors.Is(err, supervisor.ErrCleanupUnconfirmed) {
		return false
	}
	var refused *supervisor.RefusedStoppedError
	if errors.As(err, &refused) {
		return false
	}
	return !errors.Is(err, sandbox.ErrCreateUncertain)
}

// sleepOrDone waits out the supervised loop's idle backoff, reporting false
// if the context ended first so the caller can stop instead of continuing.
func sleepOrDone(ctx context.Context, backoff time.Duration) bool {
	// A context that ended before the wait must win even when the backoff is
	// zero, or select could pick the expired timer and poll once more.
	if ctx.Err() != nil {
		return false
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// runnerClassifiedFailure reports whether an attempt failure points at the
// runner itself, not the queued work, server, or network (see
// docs/runner/failure-breaker.md for the full definition and rationale).
// Callers must reset their consecutive count only on a delivered,
// non-runner-classified result — never on an idle poll or an unclassified
// error (see runSupervised).
func runnerClassifiedFailure(outcome supervisor.Outcome, err error) (string, bool) {
	if err != nil {
		if !errors.Is(err, supervisor.ErrCleanupUnconfirmed) {
			return "", false
		}
		var refused *supervisor.RefusedStoppedError
		var controlPlaneErr *protocol.ControlPlaneError
		var networkErr net.Error
		if errors.As(err, &refused) || errors.As(err, &controlPlaneErr) || errors.As(err, &networkErr) {
			return "", false
		}
		return "cleanup_unconfirmed", true
	}
	if outcome.Worked {
		switch outcome.FailureReason {
		case string(codex.FailureMalformedOutput), string(codex.FailureMissingResult),
			string(codex.FailureInvalidResult), string(codex.FailureInvalidOutputSchema):
			return outcome.FailureReason, true
		}
	}
	return "", false
}

// consecutiveRunnerFailureMessage names the bound and the last reason that
// tripped it, and points at the runner log the same way supervisionFailure does.
func consecutiveRunnerFailureMessage(reason string, count int, logPath string) string {
	message := fmt.Sprintf("Runner stopped polling after %d consecutive runner-classified failures (last reason: %s); this looks like a defect in the runner itself, not the queued work.", count, reason)
	return withLogPointer(message, logPath)
}

// stillFailingMessage is the periodic line transportBackoff prints for a
// repeated identical non-runner failure, in place of re-printing the full
// condition on every retry.
func stillFailingMessage(streak int, logPath string) string {
	return withLogPointer(fmt.Sprintf("Runner is still failing, %d attempts.", streak), logPath)
}

// supervisionFailure names the condition that ended supervision, without
// repeating an internal error, and points at the runner log where the
// classified reason and surrounding events are recorded.
func supervisionFailure(err error, logPath string) string {
	var requestError *protocol.ControlPlaneError
	var refused *supervisor.RefusedStoppedError
	var timeout *protocol.HTTPTimeoutError
	var message string
	switch {
	case errors.As(err, &refused):
		message = fmt.Sprintf("The application refused the stopped acknowledgement (%d of %d); the attempt journal is kept for recovery and clears on its own once that bound is reached. Run 'run --discard-attempt' to discard the local journal immediately instead; the application's own capacity reservation is released separately.", refused.Count, refused.Threshold)
	case errors.Is(err, supervisor.ErrCleanupUnconfirmed):
		message = "Runner could not confirm sandbox cleanup, so the attempt journal is kept for recovery. Check the Docker engine and the application."
	case errors.Is(err, supervisor.ErrLeaseExpired):
		message = "Runner attempt lease expired before a result could be reported."
	case errors.Is(err, supervisor.ErrStopped):
		message = "The application revoked this attempt before it completed."
	case errors.As(err, &requestError):
		message = fmt.Sprintf("Runner request failed with HTTP %d. Check the runner connection and authorization.", requestError.StatusCode)
	// This case must precede the bare context.DeadlineExceeded case below:
	// HTTPTimeoutError unwraps to context.DeadlineExceeded so an HTTP call's
	// own budget elapsing is never reported as the attempt's deadline.
	case errors.As(err, &timeout):
		message = fmt.Sprintf("Runner request to %s exceeded its %s budget before a response was received.", timeout.Endpoint, timeout.Budget)
	case errors.Is(err, context.Canceled):
		message = "Runner stopped on request before an attempt completed."
	case errors.Is(err, context.DeadlineExceeded):
		message = "Runner attempt passed its deadline before a result could be reported."
	default:
		message = "Runner stopped before a completed result could be reported. Check the application run and runner configuration."
	}
	return withLogPointer(message, logPath)
}

// setupFailure names the generic setup boundary that failed, without
// repeating the internal error (setup happens before the token and
// connection are trusted), and points at the runner log for the recorded step.
func setupFailure(hint string, logger *runlog.Logger) string {
	return withLogPointer("Runner setup failed. "+hint, logger.Path())
}

func withLogPointer(message, logPath string) string {
	if logPath == "" {
		return message
	}
	return fmt.Sprintf("%s Check the runner log for the recorded condition: %s", message, logPath)
}

type preflightBuffer struct{ bytes.Buffer }

func (buffer *preflightBuffer) Write(value []byte) (int, error) {
	if buffer.Len()+len(value) > 4096 {
		return 0, errors.New("Docker preflight output exceeded limit")
	}
	return buffer.Buffer.Write(value)
}

func dockerPreflight(parent context.Context, executable string, arguments ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Env = sandbox.ClientEnvironment()
	var output preflightBuffer
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", errors.New("Docker preflight failed")
	}
	return strings.TrimSpace(output.String()), nil
}
