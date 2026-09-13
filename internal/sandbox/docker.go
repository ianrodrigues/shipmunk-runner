package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const (
	defaultMemoryBytes    = 256 << 20
	defaultWorkspaceBytes = 128 << 20
	maxAgentInputBytes    = 1 << 20
	maxOutputBytes        = 2 << 20
	maxCommandOutputBytes = 4 << 20
)

var (
	// ErrCreateUncertain marks a Docker create request whose daemon-side result
	// cannot be proved from the client response. The persisted name must remain
	// reserved until an owned container is observed or an operator resolves it.
	ErrCreateUncertain   = errors.New("sandbox create outcome is uncertain")
	ulidPattern          = regexp.MustCompile(`^[0-7][0-9a-hjkmnp-tv-z]{25}$`)
	containerIDPattern   = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
	containerNamePattern = regexp.MustCompile(`^shipmunk-[0-7][0-9a-hjkmnp-tv-z]{25}-[1-9][0-9]{0,15}$`)
	profileNamePattern   = regexp.MustCompile(`^shipmunk-profile-[0-7][0-9a-hjkmnp-tv-z]{25}$`)
	codexNamePattern     = regexp.MustCompile(`^shipmunk-codex-[0-7][0-9a-hjkmnp-tv-z]{25}-[1-9][0-9]{0,15}$`)
)

// Config controls the Docker sandbox and the executable used by the independent
// watchdog. Child processes receive only ClientEnvironment's Docker-client
// configuration allowlist, not the supervisor's full environment.
type Config struct {
	Image              string
	DockerExecutable   string
	WatchdogExecutable string
	CreateTimeout      time.Duration
	CommandTimeout     time.Duration
	PollInterval       time.Duration
	StopGrace          time.Duration
	MemoryBytes        int64
	WorkspaceBytes     int64
	PIDs               int
	CPUs               string
}

// Docker creates and reconciles isolated repository execution containers.
type Docker struct {
	config Config
}

// New validates the runtime configuration and applies conservative defaults.
func New(config Config) (*Docker, error) {
	if strings.TrimSpace(config.Image) == "" {
		return nil, errors.New("sandbox image is required")
	}
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
	if config.MemoryBytes == 0 {
		config.MemoryBytes = defaultMemoryBytes
	}
	if config.WorkspaceBytes == 0 {
		config.WorkspaceBytes = defaultWorkspaceBytes
	}
	if config.PIDs == 0 {
		config.PIDs = 64
	}
	if config.CPUs == "" {
		config.CPUs = "1.0"
	}
	if config.MemoryBytes < 32<<20 || config.MemoryBytes > 1<<50 ||
		config.WorkspaceBytes < 8<<20 || config.WorkspaceBytes > 1<<50 || config.PIDs < 8 || config.PIDs > 1_000_000 {
		return nil, errors.New("sandbox resource limits are outside the supported range")
	}
	cpuCount, err := strconv.ParseFloat(config.CPUs, 64)
	if err != nil || math.IsNaN(cpuCount) || math.IsInf(cpuCount, 0) || cpuCount <= 0 || cpuCount > 32 {
		return nil, errors.New("sandbox CPU limit is invalid")
	}
	return &Docker{config: config}, nil
}

// Name returns the stable Docker name that the coordinator must journal before
// it calls Create. It deliberately matches the PHP runner's attempt/fence name.
func (docker *Docker) Name(claim protocol.Claim) (string, error) {
	if !ulidPattern.MatchString(claim.RunID) || !ulidPattern.MatchString(claim.AttemptID) || claim.Fence < 1 || claim.Fence > protocol.MaxSafeInteger {
		return "", errors.New("claim identity is invalid for sandbox naming")
	}
	name := fmt.Sprintf("shipmunk-%s-%d", claim.AttemptID, claim.Fence)
	if !containerNamePattern.MatchString(name) {
		return "", errors.New("claim identity creates an unsafe sandbox name")
	}
	return name, nil
}

// Create reserves the already-journaled deterministic name and creates a
// stopped container. The caller must arm a watchdog before this operation.
func (docker *Docker) Create(ctx context.Context, claim protocol.Claim, agentInput map[string]any, workspace string) (*Process, error) {
	name, err := docker.Name(claim)
	if err != nil {
		return nil, err
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox workspace: %w", err)
	}
	workspaceInfo, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox workspace: %w", err)
	}
	workspace = workspaceInfo
	if workspace == string(filepath.Separator) {
		return nil, errors.New("sandbox workspace cannot be a filesystem root")
	}
	if err := validateWorkspaceDirectory(workspace); err != nil {
		return nil, err
	}
	input, err := json.Marshal(agentInput)
	if err != nil {
		return nil, fmt.Errorf("encode sandbox agent input: %w", err)
	}
	if len(input) > maxAgentInputBytes {
		return nil, errors.New("sandbox agent input exceeds 1 MiB")
	}
	inspection, err := docker.inspect(ctx, name)
	if err == nil {
		if !ownsSandbox(inspection, claim, name) {
			return nil, errors.New("sandbox name is occupied by an unrelated container")
		}
		return nil, errors.New("sandbox name is already present; reconcile it before creating")
	}
	if !errors.Is(err, errContainerAbsent) {
		return nil, fmt.Errorf("check sandbox name before creation: %w", err)
	}
	arguments := []string{
		"create",
		"--name", name,
		"--label", "shipmunk.runner=true",
		"--label", "shipmunk.run=" + claim.RunID,
		"--label", "shipmunk.attempt=" + claim.AttemptID,
		"--label", "shipmunk.fence=" + strconv.FormatInt(claim.Fence, 10),
		"--read-only",
		"--user", "65532:65532",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--network", "none",
		"--memory", strconv.FormatInt(docker.config.MemoryBytes, 10),
		"--memory-swap", strconv.FormatInt(docker.config.MemoryBytes, 10),
		"--cpus", docker.config.CPUs,
		"--pids-limit", strconv.Itoa(docker.config.PIDs),
		"--ulimit", "nofile=256:256",
		"--stop-timeout", strconv.Itoa(max(1, int(docker.config.StopGrace.Seconds()))),
		"--log-driver", "json-file",
		"--log-opt", "compress=false",
		"--log-opt", "max-size=1m",
		"--log-opt", "max-file=1",
		"--tmpfs", fmt.Sprintf("/workspace:rw,exec,nosuid,nodev,size=%d,uid=65532,gid=65532,mode=700", docker.config.WorkspaceBytes),
		"--tmpfs", "/run/shipmunk:rw,noexec,nosuid,nodev,size=1m,uid=65532,gid=65532,mode=700",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16m,uid=65532,gid=65532,mode=700",
		"--env", "HOME=/workspace",
		"--env", "PATH=/usr/local/bin:/usr/bin:/bin",
		"--env", "HTTP_PROXY=",
		"--env", "http_proxy=",
		"--env", "HTTPS_PROXY=",
		"--env", "https_proxy=",
		"--env", "FTP_PROXY=",
		"--env", "ftp_proxy=",
		"--env", "ALL_PROXY=",
		"--env", "all_proxy=",
		"--env", "NO_PROXY=",
		"--env", "no_proxy=",
		"--workdir", "/workspace",
		docker.config.Image,
		claim.RunID,
		claim.AttemptID,
		strconv.FormatInt(claim.Fence, 10),
	}
	result, err := docker.run(ctx, docker.config.CreateTimeout, maxCommandOutputBytes, arguments...)
	if err != nil {
		return nil, fmt.Errorf("%w: create sandbox: %w", ErrCreateUncertain, err)
	}
	containerID := strings.TrimSpace(result.stdout)
	if !containerIDPattern.MatchString(containerID) {
		return nil, fmt.Errorf("%w: Docker returned an invalid sandbox identifier", ErrCreateUncertain)
	}
	created, err := docker.inspect(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("%w: verify created sandbox: %w", ErrCreateUncertain, err)
	}
	if !ownsSandbox(created, claim, name) || created.Config.Image != docker.config.Image || !strings.HasPrefix(created.ID, containerID) {
		return nil, fmt.Errorf("%w: Docker created a sandbox with unexpected ownership or image", ErrCreateUncertain)
	}
	return &Process{docker: docker, name: name, id: created.ID, claim: claim, workspace: workspace, agentInput: input}, nil
}

// Reconcile stops and removes a Shipmunk-owned container, then verifies that
// Docker confirms its absence. Both deterministic names and legacy hex IDs are
// accepted for the persisted identifier.
func (docker *Docker) Reconcile(ctx context.Context, identifier string) error {
	if !containerNamePattern.MatchString(identifier) && !containerIDPattern.MatchString(identifier) {
		return errors.New("unsafe sandbox identifier")
	}
	inspection, err := docker.inspect(ctx, identifier)
	if errors.Is(err, errContainerAbsent) {
		if containerNamePattern.MatchString(identifier) {
			return fmt.Errorf("%w: reserved sandbox name is absent", ErrCreateUncertain)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect sandbox during reconciliation: %w", err)
	}
	if !ownsSandboxIdentifier(inspection, identifier) {
		return errors.New("refusing to reconcile a container not owned by Shipmunk")
	}
	removeIdentifier := identifier
	if containerNamePattern.MatchString(identifier) {
		removeIdentifier = inspection.ID
	}
	return docker.removeOwned(ctx, removeIdentifier, inspection)
}

func (docker *Docker) removeOwned(ctx context.Context, identifier string, inspection containerInspection) error {
	return docker.removeOwnedWith(ctx, identifier, inspection, ownsSandboxIdentifier)
}

func (docker *Docker) removeOwnedWith(
	ctx context.Context,
	identifier string,
	inspection containerInspection,
	owns func(containerInspection, string) bool,
) error {
	if !owns(inspection, identifier) {
		return errors.New("refusing to remove a container not owned by Shipmunk")
	}
	if inspection.State.Running {
		_, _ = docker.run(ctx, docker.config.CommandTimeout, maxCommandOutputBytes, "stop", "--time", strconv.Itoa(max(1, int(docker.config.StopGrace.Seconds()))), identifier)
		current, inspectErr := docker.inspect(ctx, identifier)
		if errors.Is(inspectErr, errContainerAbsent) {
			return nil
		}
		if inspectErr != nil {
			return fmt.Errorf("verify sandbox after stop: %w", inspectErr)
		}
		if !owns(current, identifier) {
			return errors.New("sandbox ownership changed during stop")
		}
		if current.State.Running {
			_, killErr := docker.run(ctx, docker.config.CommandTimeout, maxCommandOutputBytes, "kill", identifier)
			current, inspectErr = docker.inspect(ctx, identifier)
			if errors.Is(inspectErr, errContainerAbsent) {
				return nil
			}
			if inspectErr != nil {
				return fmt.Errorf("verify sandbox after kill: %w", inspectErr)
			}
			if !owns(current, identifier) {
				return errors.New("sandbox ownership changed during kill")
			}
			if current.State.Running {
				if killErr != nil {
					return fmt.Errorf("sandbox survived stop and kill: %w", killErr)
				}
				return errors.New("sandbox process tree survived stop and kill")
			}
		}
	}
	if _, err := docker.run(ctx, docker.config.CommandTimeout, maxCommandOutputBytes, "rm", "--force", identifier); err != nil {
		if _, inspectErr := docker.inspect(ctx, identifier); !errors.Is(inspectErr, errContainerAbsent) {
			return fmt.Errorf("remove sandbox: %w", err)
		}
	}
	if _, err := docker.inspect(ctx, identifier); !errors.Is(err, errContainerAbsent) {
		if err == nil {
			return errors.New("Docker did not confirm sandbox removal")
		}
		return fmt.Errorf("confirm sandbox removal: %w", err)
	}
	return nil
}

type commandResult struct {
	stdout string
	stderr string
}

func (docker *Docker) run(ctx context.Context, timeout time.Duration, outputLimit int, arguments ...string) (commandResult, error) {
	commandContext := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		commandContext, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	command := exec.CommandContext(commandContext, docker.config.DockerExecutable, arguments...)
	configureDockerCommand(command)
	command.Env = ClientEnvironment()
	var stdout, stderr boundedBuffer
	stdout.limit = outputLimit
	stderr.limit = outputLimit
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := runDockerCommand(command)
	result := commandResult{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		if errors.Is(commandContext.Err(), context.DeadlineExceeded) && timeout > 0 {
			return result, fmt.Errorf("docker command exceeded %s", timeout)
		}
		return result, fmt.Errorf("docker %s failed: %s: %w", arguments[0], strings.TrimSpace(result.stderr), err)
	}
	return result, nil
}

// ClientEnvironment returns the minimal environment shared by Docker client
// subprocesses and command-boundary preflight checks. Provider and
// control-plane credentials are intentionally excluded.
func ClientEnvironment() []string {
	environment := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C"}
	for _, name := range []string{
		"HOME",
		"DOCKER_HOST",
		"DOCKER_CONTEXT",
		"DOCKER_CONFIG",
		"DOCKER_CERT_PATH",
		"DOCKER_TLS_VERIFY",
	} {
		if value, exists := os.LookupEnv(name); exists {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 || len(value) > remaining {
		if remaining > 0 {
			_, _ = buffer.Buffer.Write(value[:remaining])
			return remaining, errors.New("subprocess output exceeded its byte limit")
		}
		return 0, errors.New("subprocess output exceeded its byte limit")
	}
	return buffer.Buffer.Write(value)
}

type containerInspection struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	} `json:"State"`
}

var errContainerAbsent = errors.New("container absent")

func (docker *Docker) inspect(ctx context.Context, identifier string) (containerInspection, error) {
	result, err := docker.run(ctx, docker.config.CommandTimeout, maxCommandOutputBytes, "inspect", identifier)
	if err != nil {
		if containerIsAbsent(result.stderr + result.stdout) {
			return containerInspection{}, errContainerAbsent
		}
		return containerInspection{}, err
	}
	var inspections []containerInspection
	if err := json.Unmarshal([]byte(result.stdout), &inspections); err != nil || len(inspections) != 1 {
		return containerInspection{}, errors.New("Docker returned invalid sandbox inspection data")
	}
	return inspections[0], nil
}

func containerIsAbsent(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "no such object") || strings.Contains(lower, "no such container")
}

func ownsSandbox(inspection containerInspection, claim protocol.Claim, name string) bool {
	labels := inspection.Config.Labels
	return strings.TrimPrefix(inspection.Name, "/") == name && labels["shipmunk.runner"] == "true" &&
		labels["shipmunk.run"] == claim.RunID && labels["shipmunk.attempt"] == claim.AttemptID &&
		labels["shipmunk.fence"] == strconv.FormatInt(claim.Fence, 10)
}

func ownsSandboxIdentifier(inspection containerInspection, identifier string) bool {
	labels := inspection.Config.Labels
	if labels["shipmunk.runner"] != "true" || !ulidPattern.MatchString(labels["shipmunk.run"]) ||
		!ulidPattern.MatchString(labels["shipmunk.attempt"]) {
		return false
	}
	fence, err := strconv.ParseInt(labels["shipmunk.fence"], 10, 64)
	if err != nil || fence < 1 || fence > protocol.MaxSafeInteger {
		return false
	}
	expectedName := fmt.Sprintf("shipmunk-%s-%d", labels["shipmunk.attempt"], fence)
	if strings.TrimPrefix(inspection.Name, "/") != expectedName {
		return false
	}
	if containerNamePattern.MatchString(identifier) {
		return identifier == expectedName
	}
	return containerIDPattern.MatchString(identifier) && (inspection.ID == identifier || strings.HasPrefix(inspection.ID, identifier))
}
