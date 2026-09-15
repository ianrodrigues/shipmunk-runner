package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	worker, closeState, err := prepareFixtureRunner(ctx, options)
	if err != nil {
		fmt.Fprintln(stderr, "Runner setup failed. Check the private token, state directory, Docker engine, image, and adjacent shipmunk-watchdog executable.")
		return 1
	}
	defer closeState()
	return runSupervised(ctx, worker, options.Once, stdout, stderr)
}

func prepareFixtureRunner(ctx context.Context, options RunnerOptions) (*supervisor.Supervisor, func() error, error) {
	store, err := attemptstate.Open(filepath.Join(options.StateDir, "active-attempt.json"))
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	client, err := protocol.NewHTTPClient(options.BaseURL, token, nil)
	if err != nil {
		return nil, nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, nil, err
	}
	watchdog := filepath.Join(filepath.Dir(executable), "shipmunk-watchdog")
	info, err := os.Lstat(watchdog)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return nil, nil, errors.New("independent watchdog executable is unavailable or unsafe")
	}
	dockerExecutable, err := exec.LookPath("docker")
	if err != nil {
		return nil, nil, err
	}
	platform, err := dockerPreflight(ctx, dockerExecutable, "version", "--format", "{{.Server.Os}}")
	if err != nil || platform != "linux" {
		return nil, nil, errors.New("a Linux Docker engine is required")
	}
	image, err := dockerPreflight(ctx, dockerExecutable, "image", "inspect", "--format", "{{.Id}}", "--", options.Image)
	if err != nil || !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(image) {
		return nil, nil, errors.New("runtime image must exist locally")
	}
	config := sandbox.Config{Image: image, DockerExecutable: dockerExecutable, WatchdogExecutable: watchdog}
	docker, err := sandbox.New(config)
	if err != nil {
		return nil, nil, err
	}
	workspaces, err := workspace.New(filepath.Join(options.StateDir, "workspaces"))
	if err != nil {
		return nil, nil, err
	}
	ready = true
	return &supervisor.Supervisor{Client: client, State: store, Workspaces: workspaces, Sandbox: supervisor.DockerBackend{Docker: docker}, Watchdog: supervisor.HostWatchdog{Watchdog: sandbox.NewWatchdog(config)}}, store.Close, nil
}

type attemptRunner interface {
	RunOnce(context.Context) (supervisor.Outcome, error)
}

func runSupervised(ctx context.Context, worker attemptRunner, once bool, stdout, stderr io.Writer) int {
	for {
		outcome, err := worker.RunOnce(ctx)
		if err != nil {
			fmt.Fprintln(stderr, supervisionFailure(err))
			return 1
		}
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
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return 1
			case <-timer.C:
			}
		}
	}
}

// supervisionFailure names the condition that ended supervision, without repeating an internal error.
func supervisionFailure(err error) string {
	var requestError *protocol.ControlPlaneError
	var refused *supervisor.RefusedStoppedError
	switch {
	case errors.As(err, &refused):
		return fmt.Sprintf("The application refused the stopped acknowledgement (%d of %d); the attempt journal is kept for recovery and clears on its own once that bound is reached. Run 'run --discard-attempt' to discard the local journal immediately instead; the application's own capacity reservation is released separately.", refused.Count, refused.Threshold)
	case errors.Is(err, supervisor.ErrCleanupUnconfirmed):
		return "Runner could not confirm sandbox cleanup, so the attempt journal is kept for recovery. Check the Docker engine and the application."
	case errors.Is(err, supervisor.ErrLeaseExpired):
		return "Runner attempt lease expired before a result could be reported."
	case errors.Is(err, supervisor.ErrStopped):
		return "The application revoked this attempt before it completed."
	case errors.As(err, &requestError):
		return fmt.Sprintf("Runner request failed with HTTP %d. Check the runner connection and authorization.", requestError.StatusCode)
	case errors.Is(err, context.Canceled):
		return "Runner stopped on request before an attempt completed."
	case errors.Is(err, context.DeadlineExceeded):
		return "Runner attempt passed its deadline before a result could be reported."
	default:
		return "Runner stopped before a completed result could be reported. Check the application run and runner configuration."
	}
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
