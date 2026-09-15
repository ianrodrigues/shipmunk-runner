package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	return watchdog.arm(name, lease, deadline, false, false)
}

// ArmProfile starts an independent watchdog for a profile credential
// container. Its ownership checks use the dedicated profile-runtime label and
// deterministic profile sandbox name rather than attempt labels.
func (watchdog *Watchdog) ArmProfile(name string, lease, deadline time.Time) (*Lease, error) {
	return watchdog.arm(name, lease, deadline, true, false)
}

// ArmCodex owns the deterministic native, repository, collector, and workspace
// volume topology for one Codex attempt.
func (watchdog *Watchdog) ArmCodex(name string, lease, deadline time.Time) (*Lease, error) {
	return watchdog.arm(name, lease, deadline, false, true)
}

func (watchdog *Watchdog) arm(name string, lease, deadline time.Time, profile, codex bool) (*Lease, error) {
	validName := containerNamePattern.MatchString(name)
	if profile {
		validName = profileNamePattern.MatchString(name)
	} else if codex {
		validName = codexNamePattern.MatchString(name)
	}
	if !validName || deadline.IsZero() || lease.IsZero() ||
		!time.Now().Before(deadline) || !time.Now().Before(lease) {
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
	pollInterval := watchdog.config.PollInterval
	if pollInterval <= 0 {
		pollInterval = 100 * time.Millisecond
	}
	command := exec.Command(executable,
		"--docker", dockerExecutable,
		"--name", name,
		"--profile-mode", strconv.FormatBool(profile),
		"--codex-mode", strconv.FormatBool(codex),
		"--lease", strconv.FormatInt(lease.UnixNano(), 10),
		"--deadline", strconv.FormatInt(deadline.UnixNano(), 10),
		"--poll-interval", pollInterval.String(),
	)
	command.Env = ClientEnvironment()
	command.Stderr = io.Discard
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create watchdog readiness channel: %w", err)
	}
	// os.Pipe keeps Disarm as the only owner of the control pipe.
	// StdinPipe lets Wait close the same pipe, and the two closes race.
	controlRead, control, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create watchdog control channel: %w", err)
	}
	command.Stdin = controlRead
	if err := command.Start(); err != nil {
		_ = controlRead.Close()
		_ = control.Close()
		return nil, fmt.Errorf("start independent watchdog: %w", err)
	}
	_ = controlRead.Close()
	reader := bufio.NewReaderSize(stdout, 32)
	leaseHandle := &Lease{docker: &Docker{config: watchdog.config}, name: name, profile: profile, codex: codex, stdin: control, done: make(chan struct{}), responses: make(chan string, 16)}
	go func() {
		err := command.Wait()
		leaseHandle.mu.Lock()
		leaseHandle.waitErr = err
		leaseHandle.mu.Unlock()
		close(leaseHandle.done)
	}()
	ready := make(chan error, 1)
	go func() {
		line, err := readBoundedLine(reader, 128)
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
			_ = leaseHandle.waitForExitBounded(watchdog.config.CommandTimeout)
			return nil, fmt.Errorf("independent watchdog did not arm: %w", err)
		}
	case <-leaseHandle.done:
		err := leaseHandle.waitForExitBounded(0)
		_ = control.Close()
		return nil, fmt.Errorf("independent watchdog exited before arming: %w", err)
	case <-time.After(watchdog.config.CommandTimeout):
		_ = command.Process.Kill()
		_ = control.Close()
		_ = leaseHandle.waitForExitBounded(watchdog.config.CommandTimeout)
		return nil, errors.New("independent watchdog did not arm before its startup timeout")
	}
	go leaseHandle.readResponses(reader)
	return leaseHandle, nil
}

// Lease controls one watchdog process. Renewal is monotonic inside the child;
// its wall-clock input is converted once for each renewal.
type Lease struct {
	docker         *Docker
	name           string
	profile        bool
	codex          bool
	stdin          io.WriteCloser
	done           chan struct{}
	mu             sync.Mutex
	closed         bool
	createInFlight bool
	createFinished bool
	phaseUnknown   bool
	phaseSequence  uint64
	responses      chan string
	confirmedID    string
	waitErr        error
}

// Renew updates the watchdog lease, capped by the immutable claim deadline.
func (lease *Lease) Renew(expiry time.Time) error {
	if expiry.IsZero() {
		return errors.New("watchdog renewal expiry is invalid")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.writeMessageLocked(fmt.Sprintf("renew:%d\n", expiry.UnixNano()))
}

// CreateStarted tells the independent watchdog that a Docker create request is
// about to be issued. If the parent dies before CreateFinished, absence cannot
// prove that the daemon did not accept the request, so cleanup remains active.
func (lease *Lease) CreateStarted() error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.createFinished {
		return errors.New("watchdog create phase is already finished")
	}
	if lease.createInFlight && !lease.phaseUnknown {
		return nil
	}
	lease.phaseUnknown = true
	if err := lease.sendPhaseLocked("started", ""); err != nil {
		return err
	}
	lease.createInFlight = true
	lease.phaseUnknown = false
	return nil
}

// CreateFinished confirms a definitive create response. Pass the full Docker
// ID on success or an empty ID only when no container could have been created.
// Do not call this after Create returns ErrCreateUncertain.
func (lease *Lease) CreateFinished(containerID string) error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if containerID != "" && (len(containerID) != 64 || !containerIDPattern.MatchString(containerID)) {
		return errors.New("confirmed Docker container ID is invalid")
	}
	if lease.createFinished {
		if lease.confirmedID == containerID {
			return nil
		}
		return errors.New("watchdog create phase was finished with a different outcome")
	}
	if !lease.createInFlight && !lease.phaseUnknown {
		return errors.New("watchdog has no create request in flight")
	}
	lease.phaseUnknown = true
	if err := lease.sendPhaseLocked("finished", containerID); err != nil {
		return err
	}
	lease.createInFlight = false
	lease.phaseUnknown = false
	lease.createFinished = true
	lease.confirmedID = containerID
	return nil
}

// Disarm confirms the sandbox is absent before closing the watchdog channel.
func (lease *Lease) Disarm() error {
	ctx, cancel := context.WithTimeout(context.Background(), lease.docker.config.CommandTimeout)
	defer cancel()
	lease.mu.Lock()
	if lease.createInFlight || lease.phaseUnknown {
		lease.mu.Unlock()
		return errors.New("cannot disarm watchdog while create outcome is uncertain")
	}
	identifier := lease.confirmedID
	lease.mu.Unlock()
	if identifier == "" {
		identifier = lease.name
	}
	if lease.codex {
		if err := lease.docker.cleanupCodexWatchdog(lease.name, false); err != nil {
			return fmt.Errorf("cannot disarm Codex watchdog: %w", err)
		}
	} else if _, err := lease.docker.inspect(ctx, identifier); !errors.Is(err, errContainerAbsent) {
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
	writeErr := lease.writeMessageLocked("disarm\n")
	lease.closed = true
	closeErr := lease.stdin.Close()
	lease.mu.Unlock()
	waitErr := lease.waitForExitBounded(lease.docker.config.CommandTimeout)
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

func (lease *Lease) sendPhaseLocked(phase, containerID string) error {
	lease.phaseSequence++
	message := fmt.Sprintf("phase:%d:%s:%s\n", lease.phaseSequence, phase, containerID)
	if err := lease.writeMessageLocked(message); err != nil {
		return err
	}
	timer := time.NewTimer(lease.docker.config.CommandTimeout)
	defer timer.Stop()
	for {
		select {
		case response := <-lease.responses:
			if response == fmt.Sprintf("ACK:%d", lease.phaseSequence) {
				return nil
			}
			if response == "EOF" || strings.HasPrefix(response, "ERROR:") {
				return fmt.Errorf("watchdog phase transition failed: %s", response)
			}
		case <-lease.done:
			return errors.New("independent watchdog exited before acknowledging phase transition")
		case <-timer.C:
			return errors.New("independent watchdog phase acknowledgment timed out; retain sandbox state")
		}
	}
}

func (lease *Lease) readResponses(reader *bufio.Reader) {
	for {
		line, err := readBoundedLine(reader, 128)
		if line != "" {
			lease.responses <- strings.TrimSpace(line)
		}
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				lease.responses <- "ERROR:oversized watchdog response"
			} else if !errors.Is(err, io.EOF) {
				lease.responses <- "ERROR:watchdog response stream failed"
			} else {
				lease.responses <- "EOF"
			}
			return
		}
	}
}

func (lease *Lease) waitForExitBounded(timeout time.Duration) error {
	if timeout <= 0 {
		<-lease.done
	} else {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-lease.done:
		case <-timer.C:
			return errors.New("watchdog shutdown is still pending; cleanup process was left running")
		}
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.waitErr
}

func (lease *Lease) writeMessageLocked(message string) error {
	if lease.closed {
		return errors.New("watchdog lease is closed")
	}
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(lease.stdin, message)
		written <- err
	}()
	timer := time.NewTimer(lease.docker.config.CommandTimeout)
	defer timer.Stop()
	select {
	case err := <-written:
		if err != nil {
			return fmt.Errorf("write watchdog control message: %w", err)
		}
		return nil
	case <-timer.C:
		lease.closed = true
		_ = lease.stdin.Close()
		return errors.New("watchdog control channel write timed out; cleanup process was notified by EOF")
	}
}

// RunWatchdog monitors the parent control channel while discarding protocol
// responses. EOF or expiry triggers cleanup retries until the exact owned
// container is absent.
func RunWatchdog(arguments []string, input io.Reader) error {
	return RunWatchdogCommand(arguments, input, io.Discard)
}

// RunWatchdogCommand writes readiness and phase acknowledgments to output while
// monitoring the parent control channel. EOF or expiry triggers cleanup retries
// until the exact owned container is absent.
func RunWatchdogCommand(arguments []string, input io.Reader, output io.Writer) error {
	options, err := parseWatchdogArguments(arguments)
	if err != nil {
		return err
	}
	deadlineExpiry := expiryFromWall(time.Unix(0, options.deadline))
	leaseExpiry := earlierExpiry(expiryFromWall(time.Unix(0, options.lease)), deadlineExpiry)
	if !time.Now().Before(leaseExpiry) {
		return errors.New("watchdog lease is already expired")
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
	messages := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(input)
		for {
			line, err := readBoundedLine(reader, 128)
			if err == nil {
				messages <- strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			} else if errors.Is(err, errLineTooLong) {
				messages <- "protocol-error"
				return
			}
			if err != nil {
				messages <- "eof"
				return
			}
		}
	}()

	createInFlight := false
	createPhaseFinished := false
	lastPhaseSequence := uint64(0)
	confirmedID := ""
	for {
		remaining := time.Until(leaseExpiry)
		if remaining <= 0 {
			return docker.cleanupWatchdog(options, createInFlight, confirmedID)
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
				return docker.cleanupWatchdog(options, createInFlight, confirmedID)
			}
			if strings.HasPrefix(message, "phase:") {
				parts := strings.SplitN(strings.TrimPrefix(message, "phase:"), ":", 3)
				if len(parts) != 3 {
					return docker.cleanupWatchdog(options, true, confirmedID)
				}
				sequence, parseErr := strconv.ParseUint(parts[0], 10, 64)
				if parseErr != nil || sequence == 0 || sequence <= lastPhaseSequence {
					return docker.cleanupWatchdog(options, true, confirmedID)
				}
				lastPhaseSequence = sequence
				switch parts[1] {
				case "started":
					if parts[2] != "" || createPhaseFinished {
						return docker.cleanupWatchdog(options, true, confirmedID)
					}
					createInFlight = true
				case "finished":
					if (!createInFlight && !createPhaseFinished) ||
						(parts[2] != "" && (len(parts[2]) != 64 || !containerIDPattern.MatchString(parts[2]))) ||
						(createPhaseFinished && parts[2] != confirmedID) {
						return docker.cleanupWatchdog(options, true, confirmedID)
					}
					createInFlight = false
					createPhaseFinished = true
					confirmedID = parts[2]
				default:
					return docker.cleanupWatchdog(options, true, confirmedID)
				}
				if _, err := fmt.Fprintf(output, "ACK:%d\n", sequence); err != nil {
					return fmt.Errorf("acknowledge watchdog phase: %w", err)
				}
				continue
			}
			if message == "protocol-error" {
				return docker.cleanupWatchdog(options, true, confirmedID)
			}
			if message == "disarm" {
				if createInFlight {
					continue
				}
				identifier := confirmedID
				if identifier == "" {
					identifier = options.name
				}
				ctx, cancel := context.WithTimeout(context.Background(), docker.config.CommandTimeout)
				_, inspectErr := docker.inspect(ctx, identifier)
				cancel()
				if errors.Is(inspectErr, errContainerAbsent) {
					return nil
				}
				continue
			}
			if strings.HasPrefix(message, "renew:") {
				unixNano, err := strconv.ParseInt(strings.TrimPrefix(message, "renew:"), 10, 64)
				if err == nil {
					candidate := earlierExpiry(expiryFromWall(time.Unix(0, unixNano)), deadlineExpiry)
					if candidate.After(leaseExpiry) {
						leaseExpiry = candidate
					}
				}
			}
		case <-timer.C:
		}
	}
}

type watchdogOptions struct {
	docker       string
	name         string
	profile      bool
	codex        bool
	lease        int64
	deadline     int64
	pollInterval time.Duration
}

var errLineTooLong = errors.New("watchdog control frame exceeds 128 bytes")

func readBoundedLine(reader *bufio.Reader, maxBytes int) (string, error) {
	var line strings.Builder
	for {
		fragment, err := reader.ReadSlice('\n')
		if line.Len()+len(fragment) > maxBytes {
			return "", errLineTooLong
		}
		line.Write(fragment)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line.String(), err
	}
}

func parseWatchdogArguments(arguments []string) (watchdogOptions, error) {
	options := watchdogOptions{pollInterval: 100 * time.Millisecond}
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
		case "--profile-mode":
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return watchdogOptions{}, errors.New("invalid watchdog ownership mode")
			}
			options.profile = parsed
		case "--codex-mode":
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return watchdogOptions{}, errors.New("invalid watchdog ownership mode")
			}
			options.codex = parsed
		case "--lease":
			options.lease, _ = strconv.ParseInt(value, 10, 64)
		case "--deadline":
			options.deadline, _ = strconv.ParseInt(value, 10, 64)
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
	validName := containerNamePattern.MatchString(options.name)
	if options.profile {
		validName = profileNamePattern.MatchString(options.name)
	} else if options.codex {
		validName = codexNamePattern.MatchString(options.name)
	}
	if options.profile && options.codex || options.docker == "" || !validName || options.lease == 0 || options.deadline == 0 {
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

func (docker *Docker) cleanupWatchdogSandbox(name string, createInFlight bool, confirmedID string) error {
	return docker.cleanupWatchdogSandboxWithOwner(name, createInFlight, confirmedID, ownsSandboxIdentifier)
}

func (docker *Docker) cleanupWatchdog(options watchdogOptions, createInFlight bool, confirmedID string) error {
	if options.codex {
		return docker.cleanupCodexWatchdog(options.name, createInFlight)
	}
	if options.profile {
		return docker.cleanupProfileWatchdogSandbox(options.name, createInFlight, confirmedID)
	}
	return docker.cleanupWatchdogSandbox(options.name, createInFlight, confirmedID)
}

func (docker *Docker) cleanupProfileWatchdogSandbox(name string, createInFlight bool, confirmedID string) error {
	return docker.cleanupWatchdogSandboxWithOwner(name, createInFlight, confirmedID, ownsProfileSandboxIdentifier)
}

func (docker *Docker) cleanupCodexWatchdog(name string, createInFlight bool) error {
	for {
		for _, suffix := range []string{"", "-repo", "-diff"} {
			resource := name + suffix
			owner := func(info containerInspection, identifier string) bool {
				return info.Config.Labels["shipmunk.codex"] == "true" &&
					info.Config.Labels["shipmunk.codex-owner"] == name &&
					(identifier == resource || containerIDPattern.MatchString(identifier) &&
						(info.ID == identifier || strings.HasPrefix(info.ID, identifier)))
			}
			if err := docker.cleanupWatchdogSandboxWithOwner(resource, false, "", owner); err != nil {
				return err
			}
		}
		volume := name + "-workspace"
		ctx, cancel := context.WithTimeout(context.Background(), docker.config.CommandTimeout)
		label, absent, err := docker.codexVolumeLabel(ctx, volume)
		if err != nil {
			cancel()
			return err
		}
		if absent {
			cancel()
			if createInFlight {
				time.Sleep(docker.config.PollInterval)
				continue
			}
			return nil
		}
		if label != name {
			cancel()
			return errors.New("watchdog refused to remove an unrelated Codex volume")
		}
		if err := docker.runCodexDocker(ctx, "volume", "rm", volume); err != nil {
			cancel()
			return err
		}
		_, absent, err = docker.codexVolumeLabel(ctx, volume)
		cancel()
		if err != nil || !absent {
			return errors.New("watchdog could not confirm Codex volume absence")
		}
		// The volume is the topology's first Docker side effect. Observing its
		// owned label and then confirming its removal proves the pending create
		// sequence was accepted and has been fenced by cleanup.
		createInFlight = false
		continue
	}
}

func (docker *Docker) codexVolumeLabel(ctx context.Context, name string) (string, bool, error) {
	command := exec.CommandContext(ctx, docker.config.DockerExecutable, "volume", "inspect", "--format", `{{index .Labels "shipmunk.codex-owner"}}`, name)
	command.Env = ClientEnvironment()
	var stdout, stderr boundedBuffer
	stdout.limit, stderr.limit = 4096, 4096
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if exit := new(exec.ExitError); errors.As(err, &exit) && exit.ExitCode() != 0 && strings.Contains(strings.ToLower(stderr.String()), "no such volume") {
		return "", true, nil
	}
	if err != nil {
		return "", false, errors.New("watchdog could not inspect Codex volume")
	}
	return strings.TrimSpace(stdout.String()), false, nil
}

func (docker *Docker) runCodexDocker(ctx context.Context, arguments ...string) error {
	command := exec.CommandContext(ctx, docker.config.DockerExecutable, arguments...)
	command.Env = ClientEnvironment()
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Run(); err != nil {
		return errors.New("watchdog could not remove Codex volume")
	}
	return nil
}

func (docker *Docker) cleanupWatchdogSandboxWithOwner(
	name string,
	createInFlight bool,
	confirmedID string,
	owns func(containerInspection, string) bool,
) error {
	observed := false
	observedID := confirmedID
	for {
		identifier := observedID
		if identifier == "" {
			identifier = name
		}
		ctx, cancel := context.WithTimeout(context.Background(), docker.config.CommandTimeout)
		inspection, err := docker.inspect(ctx, identifier)
		cancel()
		if errors.Is(err, errContainerAbsent) {
			if !createInFlight || observed {
				return nil
			}
			time.Sleep(docker.config.PollInterval)
			continue
		}
		if err != nil {
			time.Sleep(docker.config.PollInterval)
			continue
		}
		if inspection.Config.Labels["shipmunk.profile-create-reservation"] == "true" &&
			inspection.Config.Labels["shipmunk.profile-runtime"] == "true" &&
			inspection.Config.Labels["shipmunk.profile-sandbox"] == name &&
			strings.TrimPrefix(inspection.Name, "/") == name {
			// An uncertain-create tombstone must retain the operation-scoped
			// name permanently so an older delayed create cannot commit later.
			return nil
		}
		if !owns(inspection, identifier) || strings.TrimPrefix(inspection.Name, "/") != name {
			return errors.New("watchdog refused to remove an unrelated container")
		}
		observed = true
		if observedID == "" {
			observedID = inspection.ID
		}
		ctx, cancel = context.WithTimeout(context.Background(), docker.config.CommandTimeout*3)
		removeErr := docker.removeOwnedWith(ctx, inspection.ID, inspection, owns)
		cancel()
		if removeErr == nil {
			return nil
		}
		if removeErr != nil {
			time.Sleep(docker.config.PollInterval)
		}
	}
}

func ownsProfileSandboxIdentifier(inspection containerInspection, identifier string) bool {
	name := strings.TrimPrefix(inspection.Name, "/")
	if inspection.Config.Labels["shipmunk.profile-runtime"] != "true" ||
		inspection.Config.Labels["shipmunk.profile-sandbox"] != name || !profileNamePattern.MatchString(name) {
		return false
	}
	if profileNamePattern.MatchString(identifier) {
		return identifier == name
	}
	return containerIDPattern.MatchString(identifier) &&
		(inspection.ID == identifier || strings.HasPrefix(inspection.ID, identifier))
}
