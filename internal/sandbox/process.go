package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

// Process is one stopped or running Docker sandbox owned by a claim.
type Process struct {
	docker     *Docker
	name       string
	id         string
	claim      protocol.Claim
	workspace  string
	agentInput []byte
}

// ID is the deterministic Docker name persisted by the coordinator.
func (process *Process) ID() string {
	return process.name
}

// ContainerID returns Docker's immutable full container ID. The coordinator
// should persist it after Create succeeds; Reconcile can safely confirm an
// absent immutable ID even if the deterministic name is later reused.
func (process *Process) ContainerID() string {
	return process.id
}

// Start starts the container, copies the workspace and agent input, then marks
// the execution inputs ready for the pinned sandbox entrypoint.
func (process *Process) Start(ctx context.Context) error {
	if _, err := process.inspectOwned(ctx); err != nil {
		return fmt.Errorf("verify sandbox before start: %w", err)
	}
	archive, err := workspaceArchive(process.workspace, process.docker.config.WorkspaceBytes)
	if err != nil {
		return err
	}
	if _, err := process.docker.run(ctx, process.docker.config.CommandTimeout, maxCommandOutputBytes, "start", process.id); err != nil {
		return fmt.Errorf("start sandbox: %w", err)
	}
	if _, err := process.docker.runInput(ctx, process.docker.config.CommandTimeout, maxCommandOutputBytes, bytes.NewReader(archive), "exec", "--interactive", process.id, "tar", "-xf", "-", "-C", "/workspace"); err != nil {
		return fmt.Errorf("copy sandbox workspace: %w", err)
	}
	if _, err := process.docker.runInput(ctx, process.docker.config.CommandTimeout, maxCommandOutputBytes, bytes.NewReader(process.agentInput), "exec", "--interactive", process.id,
		"sh", "-c", "umask 077; cat > /run/shipmunk/agent-input.json.tmp && mv /run/shipmunk/agent-input.json.tmp /run/shipmunk/agent-input.json"); err != nil {
		return fmt.Errorf("copy sandbox agent input: %w", err)
	}
	if _, err := process.docker.run(ctx, process.docker.config.CommandTimeout, maxCommandOutputBytes, "exec", process.id,
		"sh", "-c", "touch /run/shipmunk/ready.tmp && mv /run/shipmunk/ready.tmp /run/shipmunk/ready"); err != nil {
		return fmt.Errorf("mark sandbox inputs ready: %w", err)
	}
	return nil
}

// Wait waits for the container command to finish and returns its exit code and
// bounded combined output. Context cancellation does not remove the sandbox;
// the caller must stop and remove it before disarming the watchdog.
func (process *Process) Wait(ctx context.Context) (int, []byte, error) {
	if _, err := process.inspectOwned(ctx); err != nil {
		return 0, nil, fmt.Errorf("verify sandbox before wait: %w", err)
	}
	result, err := process.docker.run(ctx, 0, maxCommandOutputBytes, "wait", process.id)
	if err != nil {
		return 0, nil, fmt.Errorf("wait for sandbox: %w", err)
	}
	text := strings.TrimSpace(result.stdout)
	exitCode, err := strconv.Atoi(text)
	if err != nil || exitCode < 0 {
		return 0, nil, errors.New("Docker returned an invalid sandbox exit code")
	}
	inspection, err := process.inspectOwned(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("verify sandbox after wait: %w", err)
	}
	if inspection.State.Running {
		return 0, nil, errors.New("Docker reported completion while the sandbox was still running")
	}
	logs, err := process.docker.run(ctx, process.docker.config.CommandTimeout, maxOutputBytes, "logs", process.id)
	if err != nil {
		return 0, nil, fmt.Errorf("read sandbox output: %w", err)
	}
	output := append([]byte(logs.stdout), []byte(logs.stderr)...)
	if len(output) > maxOutputBytes {
		return 0, nil, errors.New("sandbox output exceeded 2 MiB")
	}
	return exitCode, output, nil
}

// Stop sends TERM through Docker's stop operation, escalates to KILL if needed,
// and verifies that the sandbox is no longer running.
func (process *Process) Stop(ctx context.Context) error {
	inspection, err := process.inspectOwned(ctx)
	if errors.Is(err, errContainerAbsent) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect sandbox before stop: %w", err)
	}
	if !inspection.State.Running {
		return nil
	}
	_, stopErr := process.docker.run(ctx, process.docker.config.CommandTimeout, maxCommandOutputBytes,
		"stop", "--time", fmt.Sprintf("%d", max(1, int(process.docker.config.StopGrace.Seconds()))), process.id)
	current, inspectErr := process.docker.inspect(ctx, process.id)
	if errors.Is(inspectErr, errContainerAbsent) {
		return nil
	}
	if inspectErr != nil {
		return fmt.Errorf("verify sandbox after stop: %w", inspectErr)
	}
	if !ownsSandbox(current, process.claim, process.name) || current.ID != process.id {
		return errors.New("sandbox ownership changed during stop")
	}
	if !current.State.Running {
		return nil
	}
	if _, err := process.docker.run(ctx, process.docker.config.CommandTimeout, maxCommandOutputBytes, "kill", process.id); err != nil {
		return fmt.Errorf("kill sandbox after stop failure (%v): %w", stopErr, err)
	}
	current, err = process.docker.inspect(ctx, process.id)
	if errors.Is(err, errContainerAbsent) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify sandbox after kill: %w", err)
	}
	if !ownsSandbox(current, process.claim, process.name) || current.ID != process.id {
		return errors.New("sandbox ownership changed during kill")
	}
	if current.State.Running {
		return errors.New("sandbox process tree survived TERM and KILL")
	}
	return nil
}

// Remove stops and removes the sandbox, then verifies Docker confirms absence.
func (process *Process) Remove(ctx context.Context) error {
	if err := process.Stop(ctx); err != nil {
		return err
	}
	return process.docker.Reconcile(ctx, process.id)
}

func (process *Process) inspectOwned(ctx context.Context) (containerInspection, error) {
	inspection, err := process.docker.inspect(ctx, process.id)
	if err != nil {
		return containerInspection{}, err
	}
	if !ownsSandbox(inspection, process.claim, process.name) ||
		(process.id != "" && inspection.ID != process.id) {
		return containerInspection{}, errors.New("sandbox no longer matches its recorded claim or container ID")
	}
	return inspection, nil
}

func (docker *Docker) runInput(ctx context.Context, timeout time.Duration, outputLimit int, stdin io.Reader, arguments ...string) (commandResult, error) {
	if timeout <= 0 {
		timeout = docker.config.CommandTimeout
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandContext, docker.config.DockerExecutable, arguments...)
	configureDockerCommand(command)
	command.Env = ClientEnvironment()
	command.Stdin = stdin
	var stdout, stderr boundedBuffer
	stdout.limit = outputLimit
	stderr.limit = outputLimit
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := runDockerCommand(command)
	result := commandResult{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("docker command exceeded %s", timeout)
		}
		return result, fmt.Errorf("docker %s failed: %s: %w", arguments[0], strings.TrimSpace(result.stderr), err)
	}
	return result, nil
}

func workspaceArchive(workspace string, limit int64) ([]byte, error) {
	info, err := os.Lstat(workspace)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("sandbox workspace must be an existing real directory")
	}
	archive := &limitedArchiveBuffer{limit: limit + (4 << 20)}
	writer := tar.NewWriter(archive)
	var totalBytes int64
	err = filepath.WalkDir(workspace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == workspace {
			return nil
		}
		relative, err := filepath.Rel(workspace, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return errors.New("sandbox workspace contains an unsafe path")
		}
		fileInfo, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if fileInfo.Mode()&os.ModeSymlink != 0 || (!fileInfo.IsDir() && !fileInfo.Mode().IsRegular()) {
			return fmt.Errorf("sandbox workspace contains unsupported file %q", relative)
		}
		name := filepath.ToSlash(relative)
		header, err := tar.FileInfoHeader(fileInfo, "")
		if err != nil {
			return err
		}
		header.Name = name
		if fileInfo.IsDir() {
			header.Name += "/"
		} else {
			totalBytes += fileInfo.Size()
			if totalBytes > limit {
				return errors.New("sandbox workspace exceeds its configured byte limit")
			}
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if fileInfo.IsDir() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		openedInfo, err := file.Stat()
		if err != nil || !os.SameFile(fileInfo, openedInfo) {
			_ = file.Close()
			return fmt.Errorf("sandbox workspace file changed while archiving %q", relative)
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("archive sandbox workspace: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finish sandbox workspace archive: %w", err)
	}
	return archive.Bytes(), nil
}

type limitedArchiveBuffer struct {
	bytes.Buffer
	limit int64
}

func (buffer *limitedArchiveBuffer) Write(value []byte) (int, error) {
	if int64(buffer.Len())+int64(len(value)) > buffer.limit {
		return 0, errors.New("sandbox workspace archive exceeds its byte limit")
	}
	return buffer.Buffer.Write(value)
}

func validateWorkspaceDirectory(workspace string) error {
	info, err := os.Lstat(workspace)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("sandbox workspace must be an existing real directory")
	}
	return nil
}
