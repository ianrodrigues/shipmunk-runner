package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Watchdog launches a separate OS process that owns cleanup if the supervisor
// is killed. Its executable should be the built shipmunk-watchdog binary.
type Watchdog struct {
	config Config
}

// NewWatchdog creates a watchdog launcher using the same Docker command
// configuration as the sandbox.
func NewWatchdog(config Config) *Watchdog {
	if config.DockerExecutable == "" {
		config.DockerExecutable = "docker"
	}
	if config.CreateTimeout <= 0 {
		config.CreateTimeout = 30 * time.Second
	}
	if config.CommandTimeout <= 0 {
		config.CommandTimeout = 10 * time.Second
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 100 * time.Millisecond
	}
	if config.StopGrace <= 0 {
		config.StopGrace = 2 * time.Second
	}
	return &Watchdog{config: config}
}

// Arm starts the independent watchdog for an already-journaled deterministic
// sandbox name. The returned lease is the sole renewal/disarm control channel.
func (watchdog *Watchdog) Arm(name string, lease, deadline time.Time) (*Lease, error) {
	if !containerNamePattern.MatchString(name) || deadline.IsZero() || lease.IsZero() {
		return nil, errors.New("watchdog identity or expiry is invalid")
	}
	executable := watchdog.config.WatchdogExecutable
	if executable == "" {
		return nil, errors.New("watchdog executable is required")
	}
	dockerExecutable := watchdog.config.DockerExecutable
	if dockerExecutable == "" {
		dockerExecutable = "docker"
	}
	createTimeout := watchdog.config.CreateTimeout
	if createTimeout <= 0 {
		createTimeout = 30 * time.Second
	}
	pollInterval := watchdog.config.PollInterval
	if pollInterval <= 0 {
		pollInterval = 100 * time.Millisecond
	}
	command := exec.Command(executable,
		"--docker", dockerExecutable,
		"--name", name,
		"--lease", strconv.FormatInt(lease.UnixNano(), 10),
		"--deadline", strconv.FormatInt(deadline.UnixNano(), 10),
		"--create-timeout", createTimeout.String(),
		"--poll-interval", pollInterval.String(),
	)
	command.Env = minimalEnvironment()
	command.Stderr = io.Discard
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create watchdog readiness channel: %w", err)
	}
	control, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create watchdog control channel: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = control.Close()
		return nil, fmt.Errorf("start independent watchdog: %w", err)
	}
	leaseHandle := &Lease{docker: &Docker{config: watchdog.config}, name: name, stdin: control, done: make(chan error, 1)}
	go func() {
		leaseHandle.done <- command.Wait()
	}()
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && strings.TrimSpace(line) != "READY" {
			err = errors.New("watchdog returned an invalid readiness signal")
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = command.Process.Kill()
			_ = control.Close()
			_ = leaseHandle.waitForExit()
			return nil, fmt.Errorf("independent watchdog did not arm: %w", err)
		}
	case err := <-leaseHandle.done:
		leaseHandle.wait.Do(func() { leaseHandle.waitErr = err })
		_ = control.Close()
		return nil, fmt.Errorf("independent watchdog exited before arming: %w", err)
	case <-time.After(watchdog.config.CommandTimeout):
		_ = command.Process.Kill()
		_ = control.Close()
		_ = leaseHandle.waitForExit()
		return nil, errors.New("independent watchdog did not arm before its startup timeout")
	}
	return leaseHandle, nil
}

// Lease controls one watchdog process. Renewal is monotonic inside the child;
// its wall-clock input is converted once for each renewal.
type Lease struct {
	docker  *Docker
	name    string
	stdin   io.WriteCloser
	done    chan error
	mu      sync.Mutex
	closed  bool
	wait    sync.Once
	waitErr error
}

// Renew updates the watchdog lease, capped by the immutable claim deadline.
func (lease *Lease) Renew(expiry time.Time) error {
	if expiry.IsZero() {
		return errors.New("watchdog renewal expiry is invalid")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return errors.New("watchdog lease is closed")
	}
	if _, err := fmt.Fprintf(lease.stdin, "renew:%d\n", expiry.UnixNano()); err != nil {
		return fmt.Errorf("renew independent watchdog: %w", err)
	}
	return nil
}

// Disarm confirms the sandbox is absent before closing the watchdog channel.
func (lease *Lease) Disarm() error {
	ctx, cancel := context.WithTimeout(context.Background(), lease.docker.config.CommandTimeout)
	defer cancel()
	if _, err := lease.docker.inspect(ctx, lease.name); !errors.Is(err, errContainerAbsent) {
		if err == nil {
			return errors.New("cannot disarm watchdog while its sandbox exists")
		}
		return fmt.Errorf("cannot disarm watchdog without confirmed sandbox absence: %w", err)
	}
	lease.mu.Lock()
	if lease.closed {
		lease.mu.Unlock()
		return errors.New("watchdog lease is already closed")
	}
	lease.closed = true
	_, writeErr := io.WriteString(lease.stdin, "disarm\n")
	closeErr := lease.stdin.Close()
	lease.mu.Unlock()
	waitErr := lease.waitForExit()
	if writeErr != nil && waitErr != nil {
		return fmt.Errorf("watchdog exited while disarming (%v): %w", writeErr, waitErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close watchdog control channel: %w", closeErr)
	}
	if waitErr != nil {
		return fmt.Errorf("wait for watchdog shutdown: %w", waitErr)
	}
	return nil
}

func (lease *Lease) waitForExit() error {
	lease.wait.Do(func() {
		lease.waitErr = <-lease.done
	})
	return lease.waitErr
}

// RunWatchdog is the command body for cmd/shipmunk-watchdog. EOF is treated as
// parent death. Cleanup retries until the exact owned container is absent.
func RunWatchdog(arguments []string, input io.Reader) error {
	return RunWatchdogCommand(arguments, input, io.Discard)
}

// RunWatchdogCommand writes a readiness line before monitoring the control
// channel so the supervisor knows cleanup is armed before creating a container.
func RunWatchdogCommand(arguments []string, input io.Reader, output io.Writer) error {
	options, err := parseWatchdogArguments(arguments)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "READY"); err != nil {
		return fmt.Errorf("signal watchdog readiness: %w", err)
	}
	docker := &Docker{config: Config{
		DockerExecutable: options.docker,
		CommandTimeout:   10 * time.Second,
		PollInterval:     options.pollInterval,
		StopGrace:        2 * time.Second,
	}}
	deadlineExpiry := expiryFromWall(time.Unix(0, options.deadline))
	leaseExpiry := earlierExpiry(expiryFromWall(time.Unix(0, options.lease)), deadlineExpiry)
	messages := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(input)
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				messages <- strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			}
			if err != nil {
				messages <- "eof"
				return
			}
		}
	}()

	for {
		remaining := time.Until(leaseExpiry)
		if remaining <= 0 {
			return docker.cleanupWatchdogSandbox(options.name, options.createTimeout)
		}
		timer := time.NewTimer(min(remaining, options.pollInterval))
		select {
		case message := <-messages:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if message == "eof" {
				return docker.cleanupWatchdogSandbox(options.name, options.createTimeout)
			}
			if message == "disarm" {
				ctx, cancel := context.WithTimeout(context.Background(), docker.config.CommandTimeout)
				_, inspectErr := docker.inspect(ctx, options.name)
				cancel()
				if errors.Is(inspectErr, errContainerAbsent) {
					return nil
				}
				continue
			}
			if strings.HasPrefix(message, "renew:") {
				unixNano, err := strconv.ParseInt(strings.TrimPrefix(message, "renew:"), 10, 64)
				if err == nil {
					leaseExpiry = earlierExpiry(expiryFromWall(time.Unix(0, unixNano)), deadlineExpiry)
				}
			}
		case <-timer.C:
		}
	}
}

type watchdogOptions struct {
	docker        string
	name          string
	lease         int64
	deadline      int64
	createTimeout time.Duration
	pollInterval  time.Duration
}

func parseWatchdogArguments(arguments []string) (watchdogOptions, error) {
	options := watchdogOptions{createTimeout: 30 * time.Second, pollInterval: 100 * time.Millisecond}
	for index := 0; index < len(arguments); index += 2 {
		if index+1 >= len(arguments) {
			return watchdogOptions{}, errors.New("invalid watchdog arguments")
		}
		value := arguments[index+1]
		switch arguments[index] {
		case "--docker":
			options.docker = value
		case "--name":
			options.name = value
		case "--lease":
			options.lease, _ = strconv.ParseInt(value, 10, 64)
		case "--deadline":
			options.deadline, _ = strconv.ParseInt(value, 10, 64)
		case "--create-timeout":
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed <= 0 {
				return watchdogOptions{}, errors.New("invalid watchdog create timeout")
			}
			options.createTimeout = parsed
		case "--poll-interval":
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed <= 0 || parsed > time.Second {
				return watchdogOptions{}, errors.New("invalid watchdog poll interval")
			}
			options.pollInterval = parsed
		default:
			return watchdogOptions{}, errors.New("unknown watchdog argument")
		}
	}
	if options.docker == "" || !containerNamePattern.MatchString(options.name) || options.lease == 0 || options.deadline == 0 {
		return watchdogOptions{}, errors.New("watchdog configuration is incomplete")
	}
	return options, nil
}

func expiryFromWall(expiry time.Time) time.Time {
	remaining := time.Until(expiry)
	if remaining < 0 {
		remaining = 0
	}
	return time.Now().Add(remaining)
}

func earlierExpiry(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}

func (docker *Docker) cleanupWatchdogSandbox(name string, createTimeout time.Duration) error {
	settleUntil := time.Now().Add(createTimeout + time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), docker.config.CommandTimeout)
		inspection, err := docker.inspect(ctx, name)
		cancel()
		if errors.Is(err, errContainerAbsent) {
			if !time.Now().Before(settleUntil) {
				return nil
			}
			time.Sleep(docker.config.PollInterval)
			continue
		}
		if err != nil {
			time.Sleep(docker.config.PollInterval)
			continue
		}
		if !ownsSandboxIdentifier(inspection, name) {
			return errors.New("watchdog refused to remove an unrelated container")
		}
		ctx, cancel = context.WithTimeout(context.Background(), docker.config.CommandTimeout*3)
		removeErr := docker.removeOwned(ctx, name, inspection)
		cancel()
		if removeErr != nil {
			time.Sleep(docker.config.PollInterval)
		}
	}
}
