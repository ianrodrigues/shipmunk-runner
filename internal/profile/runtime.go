package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
)

const (
	nativeOutputLimit  = 16 << 10
	nativeCommandLimit = 30 * time.Second
	nativeLoginLimit   = 15 * time.Minute
	dockerCallLimit    = 10 * time.Second
	dockerOutputLimit  = 16 << 10
	profileUIDLabel    = "shipmunk.profile-runtime"
)

var (
	profileSandboxPattern = regexp.MustCompile(`^shipmunk-profile-[0-7][0-9a-hjkmnp-tv-z]{25}$`)
	imageIDPattern        = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	containerIDPattern    = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
)

// Checkpoint renews the operation lease or reports that the operation was
// revoked. Runtime methods call it before Docker work and while commands run.
type Checkpoint func() error

// Runtime manages a credential-only native profile container. Lifecycle and
// protected storage remain owned by the caller.
type Runtime interface {
	Start(context.Context, string, string, Checkpoint) error
	Run(context.Context, string, string, string, Checkpoint) (CommandResult, error)
	Stop(context.Context, string) error
}

// commandBoundary is deliberately narrow so runtime behavior can be tested
// without starting Docker or native clients.
type commandBoundary interface {
	Run(context.Context, []string, io.Reader, io.Writer, io.Writer, ...string) error
}

type osCommandBoundary struct{}

func (osCommandBoundary) Run(ctx context.Context, environment []string, stdin io.Reader, stdout, stderr io.Writer, arguments ...string) error {
	if len(arguments) == 0 {
		return errors.New("empty command")
	}
	command := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	command.Env = environment
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

type runtimeConfig struct {
	image              string
	docker             string
	uid                int
	gid                int
	commands           commandBoundary
	stdin              io.Reader
	stdout             io.Writer
	stderr             io.Writer
	dockerTimeout      time.Duration
	commandTimeout     time.Duration
	loginTimeout       time.Duration
	checkpointInterval time.Duration
}

// NewDockerRuntime creates a native runtime that resolves the configured image
// tag to a content-addressed local image before each container is created.
func NewDockerRuntime(image string) (Runtime, error) {
	return newDockerRuntime(runtimeConfig{
		image:              image,
		docker:             "docker",
		uid:                os.Geteuid(),
		gid:                os.Getegid(),
		commands:           osCommandBoundary{},
		stdin:              os.Stdin,
		stdout:             os.Stdout,
		stderr:             os.Stderr,
		dockerTimeout:      dockerCallLimit,
		commandTimeout:     nativeCommandLimit,
		loginTimeout:       nativeLoginLimit,
		checkpointInterval: 100 * time.Millisecond,
	})
}

func newDockerRuntime(config runtimeConfig) (*dockerRuntime, error) {
	if strings.TrimSpace(config.image) == "" || strings.TrimSpace(config.image) != config.image || strings.ContainsAny(config.image, "\r\n\x00 \t") || strings.HasPrefix(config.image, "-") {
		return nil, errors.New("native profile image is invalid")
	}
	if config.uid <= 0 || config.gid < 0 {
		return nil, errors.New("native profiles require a dedicated non-root runner account")
	}
	if config.docker == "" {
		config.docker = "docker"
	}
	if config.commands == nil {
		config.commands = osCommandBoundary{}
	}
	if config.dockerTimeout <= 0 {
		config.dockerTimeout = dockerCallLimit
	}
	if config.commandTimeout <= 0 {
		config.commandTimeout = nativeCommandLimit
	}
	if config.loginTimeout <= 0 {
		config.loginTimeout = nativeLoginLimit
	}
	if config.checkpointInterval <= 0 {
		config.checkpointInterval = 100 * time.Millisecond
	}
	if config.stdin == nil {
		config.stdin = os.Stdin
	}
	if config.stdout == nil {
		config.stdout = os.Stdout
	}
	if config.stderr == nil {
		config.stderr = os.Stderr
	}
	return &dockerRuntime{config: config}, nil
}

type dockerRuntime struct {
	config runtimeConfig
}

type dockerInspection struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
}

func (runtime *dockerRuntime) Start(ctx context.Context, sandboxName, home string, checkpoint Checkpoint) error {
	if err := validateSandboxName(sandboxName); err != nil {
		return err
	}
	if checkpoint == nil {
		return errors.New("profile operation checkpoint is required")
	}
	absoluteHome, err := filepath.Abs(home)
	if err != nil || !filepath.IsAbs(absoluteHome) || strings.ContainsAny(absoluteHome, ",\r\n\x00") {
		return errors.New("profile home path is invalid for a Docker mount")
	}
	info, err := os.Lstat(absoluteHome)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("profile home must be an existing real directory")
	}
	if err := checkpoint(); err != nil {
		return err
	}
	image, err := runtime.resolveImage(ctx, checkpoint)
	if err != nil {
		return err
	}
	inspection, err := runtime.inspect(ctx, sandboxName, checkpoint)
	if err == nil {
		if !runtime.owns(inspection, sandboxName) {
			return errors.New("profile sandbox name is occupied by an unrelated container")
		}
		return errors.New("profile sandbox already exists; reconcile it before starting")
	}
	if !errors.Is(err, errContainerAbsent) {
		return fmt.Errorf("inspect profile sandbox before creation: %w", err)
	}
	arguments := []string{
		"create", "--name", sandboxName, "--label", profileUIDLabel + "=true",
		"--init", "--read-only", "--user", strconv.Itoa(runtime.config.uid) + ":" + strconv.Itoa(runtime.config.gid),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", "64", "--memory", "512m", "--cpus", "1",
		"--ulimit", "nofile=1024:1024", "--log-driver", "none", "--network", "bridge",
		"--workdir", "/empty",
		"--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=64m,mode=1777",
		"--mount", "type=bind,src=" + absoluteHome + ",dst=/profile",
		"--tmpfs", "/profile/.codex/tmp:rw,nosuid,nodev,size=16m,mode=0700,uid=" + strconv.Itoa(runtime.config.uid) + ",gid=" + strconv.Itoa(runtime.config.gid),
		"--entrypoint", "/usr/bin/env", image,
		"-i", "PATH=/usr/local/bin:/usr/bin:/bin", "/bin/sh", "-c",
		"test ! -e /etc/claude-code && test ! -e /etc/codex && exec sleep 1800",
	}
	created, err := runtime.call(ctx, runtime.config.dockerTimeout, dockerOutputLimit, checkpoint, nil, false, arguments, true)
	if err != nil {
		return fmt.Errorf("create profile sandbox: %w", err)
	}
	containerID := strings.TrimSpace(created.stdout)
	if !containerIDPattern.MatchString(containerID) {
		return errors.New("Docker returned an invalid profile sandbox identifier")
	}
	inspection, err = runtime.inspect(ctx, containerID, checkpoint)
	if err != nil {
		return fmt.Errorf("verify created profile sandbox: %w", err)
	}
	if !runtime.owns(inspection, sandboxName) || inspection.Config.Image != image || !strings.HasPrefix(inspection.ID, containerID) || inspection.State.Running {
		return errors.New("Docker created a profile sandbox with unexpected identity or state")
	}
	if _, err := runtime.call(ctx, runtime.config.dockerTimeout, dockerOutputLimit, checkpoint, nil, false, []string{"start", inspection.ID}, true); err != nil {
		return fmt.Errorf("start profile sandbox: %w", err)
	}
	inspection, err = runtime.inspect(ctx, inspection.ID, checkpoint)
	if err != nil {
		return fmt.Errorf("verify started profile sandbox: %w", err)
	}
	if !runtime.owns(inspection, sandboxName) || inspection.Config.Image != image || !inspection.State.Running {
		return errors.New("Docker did not start the expected profile sandbox")
	}
	return nil
}

func (runtime *dockerRuntime) Run(ctx context.Context, sandboxName, agent, operation string, checkpoint Checkpoint) (CommandResult, error) {
	if err := validateSandboxName(sandboxName); err != nil {
		return CommandResult{}, err
	}
	command, ok := Command(agent, operation)
	if !ok {
		return CommandResult{}, errors.New("unsupported native profile command")
	}
	if checkpoint == nil {
		return CommandResult{}, errors.New("profile operation checkpoint is required")
	}
	if err := checkpoint(); err != nil {
		return CommandResult{}, err
	}
	if _, err := runtime.inspect(ctx, sandboxName, checkpoint); err != nil {
		return CommandResult{}, fmt.Errorf("verify profile sandbox before native command: %w", err)
	}
	interactive := operation == "login"
	limit := runtime.config.commandTimeout
	if interactive {
		limit = runtime.config.loginTimeout
	}
	arguments := []string{
		"exec", "-i", sandboxName, "/usr/bin/env", "-i",
		"HOME=/profile", "CODEX_HOME=/profile/.codex", "CLAUDE_CONFIG_DIR=/profile/.claude",
		"XDG_CONFIG_HOME=/profile/.config", "XDG_CACHE_HOME=/profile/.cache",
		"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "TERM=dumb",
		"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"/bin/sh", "-c", "umask 077; exec \"$@\"", "native-profile",
	}
	arguments = append(arguments, command...)
	var stdin io.Reader
	var stdoutWriter, stderrWriter io.Writer = io.Discard, io.Discard
	if interactive {
		stdin, stdoutWriter, stderrWriter = runtime.config.stdin, runtime.config.stdout, runtime.config.stderr
	}
	result, err := runtime.call(ctx, limit, nativeOutputLimit, checkpoint, stdin, true, arguments, !interactive, stdoutWriter, stderrWriter)
	if err != nil {
		return CommandResult{}, err
	}
	return CommandResult{ExitCode: result.exitCode, Stdout: result.stdout, Stderr: result.stderr}, nil
}

func (runtime *dockerRuntime) Stop(ctx context.Context, sandboxName string) error {
	if err := validateSandboxName(sandboxName); err != nil {
		return err
	}
	checkpoint := func() error { return nil }
	inspection, err := runtime.inspect(ctx, sandboxName, checkpoint)
	if errors.Is(err, errContainerAbsent) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect profile sandbox before stop: %w", err)
	}
	if !runtime.owns(inspection, sandboxName) {
		return errors.New("refusing to stop a profile sandbox not owned by Shipmunk")
	}
	if inspection.State.Running {
		_, stopErr := runtime.call(ctx, runtime.config.dockerTimeout, dockerOutputLimit, checkpoint, nil, false, []string{"stop", "--time", "2", inspection.ID}, true)
		current, inspectErr := runtime.inspect(ctx, inspection.ID, checkpoint)
		if errors.Is(inspectErr, errContainerAbsent) {
			return nil
		}
		if inspectErr != nil {
			return fmt.Errorf("verify profile sandbox after stop (%v): %w", stopErr, inspectErr)
		}
		if !runtime.owns(current, sandboxName) || current.ID != inspection.ID {
			return errors.New("profile sandbox identity changed during stop")
		}
		if current.State.Running {
			_, killErr := runtime.call(ctx, runtime.config.dockerTimeout, dockerOutputLimit, checkpoint, nil, false, []string{"kill", current.ID}, true)
			current, inspectErr = runtime.inspect(ctx, current.ID, checkpoint)
			if errors.Is(inspectErr, errContainerAbsent) {
				return nil
			}
			if inspectErr != nil {
				return fmt.Errorf("verify profile sandbox after kill (%v): %w", killErr, inspectErr)
			}
			if !runtime.owns(current, sandboxName) || current.ID != inspection.ID || current.State.Running {
				return errors.New("profile process tree survived stop and kill")
			}
		}
	}
	if _, err := runtime.call(ctx, runtime.config.dockerTimeout, dockerOutputLimit, checkpoint, nil, false, []string{"rm", "--force", inspection.ID}, true); err != nil {
		if _, inspectErr := runtime.inspect(ctx, inspection.ID, checkpoint); !errors.Is(inspectErr, errContainerAbsent) {
			return fmt.Errorf("remove profile sandbox: %w", err)
		}
	}
	if _, err := runtime.inspect(ctx, inspection.ID, checkpoint); !errors.Is(err, errContainerAbsent) {
		if err == nil {
			return errors.New("Docker did not confirm profile sandbox absence")
		}
		return fmt.Errorf("confirm profile sandbox absence: %w", err)
	}
	return nil
}

func (runtime *dockerRuntime) resolveImage(ctx context.Context, checkpoint Checkpoint) (string, error) {
	result, err := runtime.call(ctx, runtime.config.dockerTimeout, dockerOutputLimit, checkpoint, nil, false, []string{
		"image", "inspect", "--format", "{{.Id}}", "--", runtime.config.image}, true)
	if err != nil || !imageIDPattern.MatchString(strings.TrimSpace(result.stdout)) {
		return "", errors.New("native profile image must be present locally and resolve to an immutable image ID")
	}
	return strings.TrimSpace(result.stdout), nil
}

var errContainerAbsent = errors.New("profile container absent")

func (runtime *dockerRuntime) inspect(ctx context.Context, identifier string, checkpoint Checkpoint) (dockerInspection, error) {
	result, err := runtime.call(ctx, runtime.config.dockerTimeout, dockerOutputLimit, checkpoint, nil, false, []string{
		"inspect", "--format", "{{json .}}", "--", identifier}, true)
	if err != nil {
		if containerIsAbsent(result.stderr + result.stdout) {
			return dockerInspection{}, errContainerAbsent
		}
		return dockerInspection{}, err
	}
	var inspection dockerInspection
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.stdout)), &inspection); err != nil || !containerIDPattern.MatchString(inspection.ID) {
		return dockerInspection{}, errors.New("Docker returned invalid profile sandbox inspection data")
	}
	return inspection, nil
}

func (runtime *dockerRuntime) owns(inspection dockerInspection, sandboxName string) bool {
	return inspection.Name == "/"+sandboxName && inspection.Config.Labels[profileUIDLabel] == "true"
}

func validateSandboxName(value string) error {
	if !profileSandboxPattern.MatchString(value) {
		return errors.New("profile sandbox identifier is invalid")
	}
	return nil
}

type callResult struct {
	stdout   string
	stderr   string
	exitCode int
}

func (runtime *dockerRuntime) call(parent context.Context, timeout time.Duration, outputLimit int, checkpoint Checkpoint, stdin io.Reader, allowNonzero bool, arguments []string, capture bool, writers ...io.Writer) (callResult, error) {
	if len(writers) == 0 {
		writers = []io.Writer{io.Discard, io.Discard}
	}
	if len(writers) != 2 {
		return callResult{}, errors.New("invalid command output configuration")
	}
	if err := checkpoint(); err != nil {
		return callResult{}, err
	}
	stdoutDestination, stderrDestination := writers[0], writers[1]
	commandContext, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	limiter := &byteLimiter{remaining: outputLimit}
	var stdout, stderr *captureWriter
	var stdoutWriter, stderrWriter io.Writer = stdoutDestination, stderrDestination
	if capture {
		stdout = &captureWriter{limiter: limiter, destination: stdoutDestination}
		stderr = &captureWriter{limiter: limiter, destination: stderrDestination}
		stdoutWriter, stderrWriter = stdout, stderr
	}
	done := make(chan error, 1)
	go func() {
		done <- runtime.config.commands.Run(commandContext, sandbox.ClientEnvironment(), stdin, stdoutWriter, stderrWriter, append([]string{runtime.config.docker}, arguments...)...)
	}()
	ticker := time.NewTicker(runtime.config.checkpointInterval)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			result := callResult{}
			if capture {
				result.stdout, result.stderr = stdout.String(), stderr.String()
			}
			if limiter.overflowed() {
				return result, errors.New("native command output exceeded its byte limit")
			}
			if err != nil {
				if contextErr := commandContext.Err(); contextErr != nil {
					if errors.Is(contextErr, context.DeadlineExceeded) {
						return result, fmt.Errorf("Docker command exceeded %s", timeout)
					}
					return result, contextErr
				}
				var exitError interface{ ExitCode() int }
				if errors.As(err, &exitError) {
					result.exitCode = exitError.ExitCode()
					if allowNonzero {
						return result, nil
					}
					return result, fmt.Errorf("Docker %s failed with exit code %d", arguments[0], result.exitCode)
				}
				return result, fmt.Errorf("Docker %s command failed", arguments[0])
			}
			return result, nil
		case <-ticker.C:
			if err := checkpoint(); err != nil {
				cancel()
				<-done
				return callResult{}, err
			}
		case <-commandContext.Done():
			<-done
			if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
				return callResult{}, fmt.Errorf("Docker command exceeded %s", timeout)
			}
			return callResult{}, commandContext.Err()
		}
	}
}

type byteLimiter struct {
	mu        sync.Mutex
	remaining int
	overflow  bool
}

func (limiter *byteLimiter) write(value []byte, destination io.Writer) (int, error) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if len(value) > limiter.remaining {
		limiter.overflow = true
		return 0, errors.New("native command output exceeded its byte limit")
	}
	written, err := destination.Write(value)
	limiter.remaining -= written
	return written, err
}

func (limiter *byteLimiter) overflowed() bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return limiter.overflow
}

type captureWriter struct {
	mu          sync.Mutex
	buffer      bytes.Buffer
	limiter     *byteLimiter
	destination io.Writer
}

func (writer *captureWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.limiter.write(value, io.MultiWriter(&writer.buffer, writer.destination))
}

func (writer *captureWriter) String() string {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.buffer.String()
}

func containerIsAbsent(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "no such object") || strings.Contains(message, "no such container")
}
