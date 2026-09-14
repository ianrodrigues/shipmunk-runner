package codex

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
)

const transportCommandLimit = 4 << 20

// ErrTransportCleanupUnconfirmed means startup crossed a Docker side-effect
// boundary and the transport could not prove every deterministic resource absent.
var ErrTransportCleanupUnconfirmed = errors.New("Codex transport cleanup is unconfirmed")

var transportNamePattern = regexp.MustCompile(`^shipmunk-codex-[0-7][0-9a-hjkmnp-tv-z]{25}-[1-9][0-9]{0,15}$`)

// TransportConfig describes the three-container Codex boundary. Image names are
// resolved to immutable IDs before any container is created.
type TransportConfig struct {
	Name, ProfileHome, Source, NativeImage, RepositoryImage string
	DockerExecutable                                        string
	MaxCommands                                             int
	CommandTimeout                                          time.Duration
	sourceHandle                                            *os.File
}

type transportResult struct {
	stdout, stderr []byte
	exitCode       int
}

type dockerCommand interface {
	Run(context.Context, time.Duration, int, io.Reader, ...string) (transportResult, error)
}

// DockerTransport keeps provider credentials out of the network-disabled
// repository and collector containers.
type DockerTransport struct {
	cfg             TransportConfig
	docker          dockerCommand
	mu              sync.Mutex
	started, sealed bool
	bridge          string
	workspaceVolume string
	mediator        *Mediator
	patch           *Patch
	patchCollected  bool
	fence           int64
	profileHandle   *os.File
	sourceHandle    *os.File
}

func NewDockerTransport(cfg TransportConfig) (*DockerTransport, error) {
	return newDockerTransport(cfg, execDockerCommand{executable: cfg.DockerExecutable})
}

// CleanupDockerTransport reconciles deterministic resources after a restart,
// without reopening source or profile paths.
func CleanupDockerTransport(ctx context.Context, cfg TransportConfig) error {
	if !transportNamePattern.MatchString(cfg.Name) {
		return errors.New("Codex transport name is invalid")
	}
	if cfg.DockerExecutable == "" {
		cfg.DockerExecutable = "docker"
	}
	if cfg.CommandTimeout <= 0 {
		cfg.CommandTimeout = 30 * time.Second
	}
	t := &DockerTransport{cfg: cfg, docker: execDockerCommand{executable: cfg.DockerExecutable}, workspaceVolume: cfg.Name + "-workspace"}
	return t.cleanup(ctx)
}

func newDockerTransport(cfg TransportConfig, docker dockerCommand) (*DockerTransport, error) {
	if !transportNamePattern.MatchString(cfg.Name) || cfg.MaxCommands < 1 || docker == nil ||
		cfg.NativeImage == "" || cfg.RepositoryImage == "" {
		return nil, errors.New("Codex transport configuration is invalid")
	}
	if cfg.DockerExecutable == "" {
		cfg.DockerExecutable = "docker"
	}
	fence, err := strconv.ParseInt(cfg.Name[strings.LastIndexByte(cfg.Name, '-')+1:], 10, 64)
	if err != nil || fence < 1 || fence > protocol.MaxSafeInteger {
		return nil, errors.New("Codex transport fence is invalid")
	}
	if cfg.CommandTimeout <= 0 {
		cfg.CommandTimeout = 30 * time.Second
	}
	var handles [2]*os.File
	closeHandles := func() {
		for _, handle := range handles {
			if handle != nil {
				_ = handle.Close()
			}
		}
	}
	for index, root := range []string{cfg.ProfileHome, cfg.Source} {
		if index == 1 && cfg.sourceHandle != nil {
			handle := cfg.sourceHandle
			info, statErr := handle.Stat()
			if statErr != nil || !info.IsDir() {
				closeHandles()
				return nil, errors.New("Codex transport input directory is invalid")
			}
			handles[index] = handle
			stable := handle.Name()
			if runtime.GOOS == "linux" {
				stable = fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), handle.Fd())
			} else if runtime.GOOS == "darwin" {
				stable = fmt.Sprintf("/dev/fd/%d", handle.Fd())
			}
			cfg.Source = stable
			continue
		}
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			closeHandles()
			return nil, errors.New("Codex transport input directory is invalid")
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			closeHandles()
			return nil, errors.New("Codex transport input directory is invalid")
		}
		handle, openErr := os.Open(resolved)
		if openErr != nil {
			for _, opened := range handles {
				if opened != nil {
					_ = opened.Close()
				}
			}
			return nil, errors.New("Codex transport input directory is invalid")
		}
		openedInfo, statErr := handle.Stat()
		if statErr != nil || !os.SameFile(info, openedInfo) {
			_ = handle.Close()
			for _, opened := range handles {
				if opened != nil {
					_ = opened.Close()
				}
			}
			return nil, errors.New("Codex transport input directory changed while opening")
		}
		handles[index] = handle
		stable := resolved
		if runtime.GOOS == "linux" {
			stable = fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), handle.Fd())
		} else if index == 1 && runtime.GOOS == "darwin" {
			stable = fmt.Sprintf("/dev/fd/%d", handle.Fd())
		}
		if index == 0 {
			cfg.ProfileHome = stable
		} else {
			cfg.Source = stable
		}
	}
	if _, err := os.Lstat(filepath.Join(cfg.Source, ".git")); err == nil || !errors.Is(err, os.ErrNotExist) {
		closeHandles()
		return nil, errors.New("repository input must not contain Git metadata")
	}
	bridge, err := os.MkdirTemp("", "shipmunk-codex-bridge-")
	if err != nil {
		closeHandles()
		return nil, errors.New("cannot create protected command bridge")
	}
	if err := os.Chmod(bridge, 0700); err != nil {
		closeHandles()
		_ = os.RemoveAll(bridge)
		return nil, errors.New("cannot protect command bridge")
	}
	t := &DockerTransport{cfg: cfg, docker: docker, bridge: bridge, workspaceVolume: cfg.Name + "-workspace", fence: fence, profileHandle: handles[0], sourceHandle: handles[1]}
	mediator, err := NewMediator(fence, cfg.MaxCommands, t)
	if err != nil {
		closeHandles()
		_ = os.RemoveAll(bridge)
		return nil, err
	}
	t.mediator = mediator
	return t, nil
}

// Start creates and starts all execution boundaries and copies source bytes into
// the repository's memory-backed workspace. Failure always attempts full cleanup.
func (t *DockerTransport) Start(ctx context.Context) (err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return errors.New("Codex transport is already started")
	}
	defer func() {
		if err != nil {
			if cleanup := t.cleanup(context.Background()); cleanup != nil {
				err = errors.Join(err, fmt.Errorf("%w: Codex setup cleanup was not confirmed: %v", ErrTransportCleanupUnconfirmed, cleanup))
			}
		}
	}()
	nativeID, err := t.imageID(ctx, t.cfg.NativeImage)
	if err != nil {
		return err
	}
	repoID, err := t.imageID(ctx, t.cfg.RepositoryImage)
	if err != nil {
		return err
	}
	uid, gid := os.Geteuid(), os.Getegid()
	if _, err = t.run(ctx, nil, "volume", "create", "--label", "shipmunk.codex=true", "--label", "shipmunk.codex-owner="+t.cfg.Name, "--driver", "local", "--opt", "type=tmpfs", "--opt", "device=tmpfs", "--opt", fmt.Sprintf("o=size=256m,uid=%d,gid=%d,mode=0700,nosuid,nodev", uid, gid), t.workspaceVolume); err != nil {
		return errors.New("cannot create repository memory volume")
	}
	common := []string{"--label", "shipmunk.codex=true", "--label", "shipmunk.codex-owner=" + t.cfg.Name, "--read-only", "--user", fmt.Sprintf("%d:%d", uid, gid), "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "536870912", "--memory-swap", "536870912", "--cpus", "1", "--ulimit", "nofile=1024:1024", "--log-driver", "none", "--stop-timeout", "2"}
	native := append([]string{"create", "--name", t.cfg.Name}, common...)
	native = append(native, "--pids-limit", "128", "--network", "bridge", "--workdir", "/empty", "--mount", "type=bind,src="+t.cfg.ProfileHome+",dst=/profile", "--mount", "type=bind,src="+t.bridge+",dst=/bridge", "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=64m,mode=1777", "--entrypoint", "/usr/bin/env", nativeID, "-i", "PATH=/usr/local/bin:/usr/bin:/bin", "/bin/sleep", "1800")
	if _, err = t.run(ctx, nil, native...); err != nil {
		return errors.New("cannot create native container")
	}
	repo := append([]string{"create", "--name", t.cfg.Name + "-repo"}, common...)
	repo = append(repo, "--pids-limit", "64", "--network", "none", "--workdir", "/workspace", "--mount", "type=volume,src="+t.workspaceVolume+",dst=/workspace", "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=64m,mode=1777", "--entrypoint", "/usr/bin/env", repoID, "-i", "PATH=/usr/local/bin:/usr/bin:/bin", "/bin/sleep", "1800")
	if _, err = t.run(ctx, nil, repo...); err != nil {
		return errors.New("cannot create repository container")
	}
	for _, name := range []string{t.cfg.Name, t.cfg.Name + "-repo"} {
		if _, err = t.run(ctx, nil, "start", name); err != nil {
			return errors.New("cannot start Codex boundary")
		}
	}
	archive, err := archiveDirectory(t.cfg.Source, MaxSnapshotBytes)
	if err != nil {
		return err
	}
	if _, err = t.run(ctx, bytes.NewReader(archive), t.repoExec("tar", "-xf", "-", "-C", "/workspace")...); err != nil {
		return errors.New("cannot populate repository container")
	}
	if _, err = t.run(ctx, nil, t.repoExec("sh", "-c", "test ! -e .git && git -c core.hooksPath=/dev/null init -q && git -c core.hooksPath=/dev/null add --all && git -c core.hooksPath=/dev/null -c user.name=Shipmunk -c user.email=runner@shipmunk.local commit -qm baseline --allow-empty")...); err != nil {
		return errors.New("cannot initialize protected repository baseline")
	}
	t.started = true
	return nil
}

// RunNative executes the pinned native command while servicing fenced bridge requests.
func (t *DockerTransport) RunNative(ctx context.Context, argv []string, stdin []byte) (CommandResult, error) {
	t.mu.Lock()
	if !t.started || t.sealed {
		t.mu.Unlock()
		return CommandResult{}, errors.New("Codex transport is not ready")
	}
	t.mu.Unlock()
	if len(argv) == 0 {
		return CommandResult{}, errors.New("native command is empty")
	}
	for _, v := range argv {
		if strings.IndexByte(v, 0) >= 0 {
			return CommandResult{}, errors.New("native command arguments are invalid")
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	var result transportResult
	var runErr error
	go func() {
		arguments := append([]string{"exec", "-i", t.cfg.Name, "/usr/bin/env", "-i", "HOME=/profile", "CODEX_HOME=/profile/.codex", "XDG_CONFIG_HOME=/profile/.config", "XDG_CACHE_HOME=/profile/.cache", "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "TERM=dumb", "/bin/sh", "-c", `umask 077; exec "$@"`, "native-agent"}, argv...)
		result, runErr = t.docker.Run(runCtx, 30*time.Minute, MaxOutputBytes, bytes.NewReader(stdin), arguments...)
		close(done)
	}()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-done
			_ = t.Stop(context.Background())
			return CommandResult{}, ctx.Err()
		case <-done:
			if runErr != nil {
				return CommandResult{}, errors.New("native execution failed")
			}
			return CommandResult{Stdout: string(result.stdout), Stderr: string(result.stderr), ExitCode: result.exitCode}, nil
		case <-ticker.C:
			if err := t.serviceBridge(ctx); err != nil {
				cancel()
				<-done
				_ = t.Stop(context.Background())
				return CommandResult{}, err
			}
		}
	}
}

func (t *DockerTransport) serviceBridge(ctx context.Context) error {
	if _, err := os.Lstat(filepath.Join(t.bridge, "response.json")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("repository command response is invalid")
	}
	root, err := os.OpenRoot(t.bridge)
	if err != nil {
		return errors.New("repository command bridge is invalid")
	}
	defer root.Close()
	info, err := root.Lstat("request.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !validBridgeRequestInfo(info) {
		return errors.New("repository command request is invalid")
	}
	request, err := root.OpenFile("request.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("repository command request is invalid")
	}
	raw, readErr := io.ReadAll(io.LimitReader(request, MaxCommandRequestBytes+1))
	opened, statErr := request.Stat()
	closeErr := request.Close()
	if readErr != nil || statErr != nil || closeErr != nil || !os.SameFile(info, opened) || len(raw) > MaxCommandRequestBytes {
		return errors.New("repository command request is invalid")
	}
	// The bundled bridge omits the host-only fence; bind it before strict decoding.
	value, err := decodeBridgeRequest(raw)
	if err != nil {
		return err
	}
	framed, err := json.Marshal(commandRequest{Fence: t.fence, ID: value.ID, Command: value.Command})
	if err != nil {
		return errors.New("repository command request schema is invalid")
	}
	response, err := t.mediator.Handle(ctx, framed)
	if err != nil {
		return err
	}
	var host commandResponse
	if err = json.Unmarshal(response, &host); err != nil {
		return errors.New("repository command response is invalid")
	}
	wire, err := json.Marshal(struct {
		ID       int64  `json:"id"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}{host.ID, host.Stdout, host.Stderr, host.ExitCode})
	if err != nil {
		return errors.New("repository command response is invalid")
	}
	tmp, err := root.OpenFile("response.tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("cannot write repository response")
	}
	if _, err = tmp.Write(wire); err == nil {
		err = tmp.Sync()
	}
	responseCloseErr := tmp.Close()
	if err != nil || responseCloseErr != nil {
		return errors.New("cannot write repository response")
	}
	if err = root.Rename("response.tmp", "response.json"); err != nil {
		return errors.New("cannot publish repository response")
	}
	return nil
}

func validBridgeRequestInfo(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > MaxCommandRequestBytes {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

type bridgeRequest struct {
	ID      int64
	Command string
}

func decodeBridgeRequest(raw []byte) (bridgeRequest, error) {
	// Reuse strict request validation by injecting the attempt fence.
	value, err := protocol.Decode(raw, MaxCommandRequestBytes)
	object, ok := value.(map[string]any)
	if err != nil || !ok || len(object) != 2 {
		return bridgeRequest{}, errors.New("repository command request schema is invalid")
	}
	id, ok := safeInteger(object["id"])
	cmd, ok2 := object["command"].(string)
	if !ok || !ok2 || cmd == "" || len(cmd) > MaxCommandBytes || strings.IndexByte(cmd, 0) >= 0 {
		return bridgeRequest{}, errors.New("repository command request schema is invalid")
	}
	return bridgeRequest{id, cmd}, nil
}

func (t *DockerTransport) ExecuteRepositoryCommand(ctx context.Context, command string, limit int) (CommandResult, error) {
	r, err := t.rawRun(ctx, nil, t.repoExec("sh", "-c", command)...)
	if err != nil {
		return CommandResult{}, err
	}
	if len(r.stdout)+len(r.stderr) > limit {
		return CommandResult{}, errors.New("repository command output exceeds limit")
	}
	return CommandResult{string(r.stdout), string(r.stderr), r.exitCode}, nil
}

func (t *DockerTransport) repoExec(argv ...string) []string {
	return append([]string{"exec", "-i", t.cfg.Name + "-repo", "/usr/bin/env", "-i", "HOME=/workspace", "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "TERM=dumb", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}, argv...)
}

// CollectPatch freezes repository writers and snapshots through a separate,
// network-disabled, read-only collector mount before trusted host verification.
func (t *DockerTransport) CollectPatch(ctx context.Context) (_ *Patch, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started {
		return nil, errors.New("Codex transport is not ready")
	}
	if t.patchCollected {
		return t.patch, nil
	}
	t.sealed = true
	t.mediator.Seal()
	defer func() {
		if err != nil {
			if cleanup := t.cleanup(context.Background()); cleanup != nil {
				err = errors.Join(err, fmt.Errorf("Codex patch cleanup was not confirmed: %w", cleanup))
			}
		}
	}()
	if _, err = t.run(ctx, nil, "pause", t.cfg.Name+"-repo"); err != nil {
		return nil, errors.New("cannot freeze repository writers")
	}
	repoID, err := t.imageID(ctx, t.cfg.RepositoryImage)
	if err != nil {
		return nil, err
	}
	uid, gid := os.Geteuid(), os.Getegid()
	args := []string{"create", "--name", t.cfg.Name + "-diff", "--label", "shipmunk.codex=true", "--read-only", "--user", fmt.Sprintf("%d:%d", uid, gid), "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--pids-limit", "64", "--memory", "536870912", "--memory-swap", "536870912", "--cpus", "1", "--ulimit", "nofile=1024:1024", "--log-driver", "none", "--stop-timeout", "2", "--network", "none", "--workdir", "/empty", "--mount", "type=volume,src=" + t.workspaceVolume + ",dst=/snapshot-source,readonly", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777", "--entrypoint", "/usr/bin/env", repoID, "-i", "PATH=/usr/local/bin:/usr/bin:/bin", "/bin/sleep", "1800"}
	args = append(args[:5], append([]string{"--label", "shipmunk.codex-owner=" + t.cfg.Name}, args[5:]...)...)
	if _, err = t.run(ctx, nil, args...); err != nil {
		return nil, errors.New("cannot create patch collector")
	}
	if _, err = t.run(ctx, nil, "start", t.cfg.Name+"-diff"); err != nil {
		return nil, errors.New("cannot start patch collector")
	}
	r, err := t.docker.Run(ctx, 2*time.Minute, int(MaxSnapshotBytes)+(4<<20), nil, "exec", t.cfg.Name+"-diff", "/usr/bin/env", "-i", "PATH=/usr/local/bin:/usr/bin:/bin", "tar", "-C", "/snapshot-source", "--exclude=.git", "-cf", "-", ".")
	if err != nil || r.exitCode != 0 {
		return nil, errors.New("cannot collect repository snapshot")
	}
	temporary, err := os.MkdirTemp("", "shipmunk-codex-snapshot-")
	if err != nil {
		return nil, errors.New("cannot create trusted snapshot")
	}
	defer os.RemoveAll(temporary)
	if err = extractSnapshot(bytes.NewReader(r.stdout), temporary); err != nil {
		return nil, err
	}
	patch, err := collectSnapshots(t.cfg.Source, temporary, t.sourceHandle)
	if err != nil {
		return nil, err
	}
	t.patch = patch
	t.patchCollected = true
	return patch, nil
}

func (t *DockerTransport) imageID(ctx context.Context, image string) (string, error) {
	r, err := t.run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", image)
	id := strings.TrimSpace(string(r.stdout))
	if err != nil || !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(id) {
		return "", errors.New("a preloaded immutable Codex image is required")
	}
	return id, nil
}
func (t *DockerTransport) run(ctx context.Context, stdin io.Reader, args ...string) (transportResult, error) {
	r, err := t.rawRun(ctx, stdin, args...)
	if err == nil && r.exitCode != 0 {
		return r, errors.New("Docker administrative command failed")
	}
	return r, err
}
func (t *DockerTransport) rawRun(ctx context.Context, stdin io.Reader, args ...string) (transportResult, error) {
	return t.docker.Run(ctx, t.cfg.CommandTimeout, transportCommandLimit, stdin, args...)
}

// Stop reconciles every sibling before removing the memory volume and bridge.
func (t *DockerTransport) Stop(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cleanup(ctx)
}
func (t *DockerTransport) cleanup(ctx context.Context) error {
	var failed bool
	for _, n := range []string{t.cfg.Name, t.cfg.Name + "-repo", t.cfg.Name + "-diff"} {
		owned, absent, err := t.ownedResource(ctx, n, false)
		if err != nil || !owned && !absent {
			failed = true
			continue
		}
		if absent {
			continue
		}
		_, _ = t.rawRun(ctx, nil, "stop", "--time", "2", n)
		_, _ = t.rawRun(ctx, nil, "rm", "--force", n)
		r, e := t.rawRun(ctx, nil, "inspect", n)
		if e != nil || r.exitCode == 0 || !strings.Contains(strings.ToLower(string(r.stdout)+string(r.stderr)), "no such") {
			failed = true
		}
	}
	if !failed {
		owned, absent, err := t.ownedResource(ctx, t.workspaceVolume, true)
		if err != nil || !owned && !absent {
			failed = true
		}
		if absent {
			goto bridgeCleanup
		}
		if !owned {
			goto finished
		}
		_, _ = t.rawRun(ctx, nil, "volume", "rm", t.workspaceVolume)
		r, e := t.rawRun(ctx, nil, "volume", "inspect", t.workspaceVolume)
		if e != nil || r.exitCode == 0 || !strings.Contains(strings.ToLower(string(r.stdout)+string(r.stderr)), "no such") {
			failed = true
		}
	}
bridgeCleanup:
	if !failed && t.bridge != "" {
		if err := os.RemoveAll(t.bridge); err != nil {
			failed = true
		}
	}
finished:
	t.started = false
	if t.profileHandle != nil {
		_ = t.profileHandle.Close()
		t.profileHandle = nil
	}
	if t.sourceHandle != nil {
		_ = t.sourceHandle.Close()
		t.sourceHandle = nil
	}
	if failed {
		return errors.New("cannot confirm all Codex process trees are absent")
	}
	return nil
}

func (t *DockerTransport) ownedResource(ctx context.Context, name string, volume bool) (owned, absent bool, err error) {
	args := []string{"inspect", "--format", `{{index .Config.Labels "shipmunk.codex"}}{{"\n"}}{{index .Config.Labels "shipmunk.codex-owner"}}`, name}
	if volume {
		args = []string{"volume", "inspect", "--format", `{{index .Labels "shipmunk.codex"}}{{"\n"}}{{index .Labels "shipmunk.codex-owner"}}`, name}
	}
	r, err := t.rawRun(ctx, nil, args...)
	if err != nil {
		return false, false, err
	}
	if r.exitCode != 0 {
		if strings.Contains(strings.ToLower(string(r.stdout)+string(r.stderr)), "no such") {
			return false, true, nil
		}
		return false, false, errors.New("cannot inspect Codex resource ownership")
	}
	lines := strings.Split(strings.TrimSpace(string(r.stdout)), "\n")
	return len(lines) == 2 && lines[0] == "true" && lines[1] == t.cfg.Name, false, nil
}

type execDockerCommand struct{ executable string }

func (e execDockerCommand) Run(ctx context.Context, timeout time.Duration, limit int, stdin io.Reader, args ...string) (transportResult, error) {
	if e.executable == "" {
		e.executable = "docker"
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.executable, args...)
	cmd.Env = sandbox.ClientEnvironment()
	cmd.Stdin = stdin
	var out, er limitedBuffer
	out.limit = limit
	er.limit = limit
	cmd.Stdout = &out
	cmd.Stderr = &er
	err := cmd.Run()
	result := transportResult{out.Bytes(), er.Bytes(), 0}
	if x := new(exec.ExitError); errors.As(err, &x) {
		result.exitCode = x.ExitCode()
		err = nil
	}
	if out.exceeded || er.exceeded {
		return result, errors.New("Docker output exceeds limit")
	}
	return result, err
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remain := b.limit - b.Len()
	if remain > 0 {
		_, _ = b.Buffer.Write(p[:min(remain, n)])
	}
	if n > remain {
		b.exceeded = true
	}
	return n, nil
}

func archiveDirectory(root string, limit int64) ([]byte, error) {
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, errors.New("cannot open repository source")
	}
	defer rootHandle.Close()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	var total int64
	err = fs.WalkDir(rootHandle.FS(), ".", func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == "." {
			return nil
		}
		rel := filepath.ToSlash(p)
		if !safeRepositoryPath(rel) {
			return errors.New("repository source path is invalid")
		}
		info, e := rootHandle.Lstat(p)
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New("repository source contains unsupported entries")
		}
		h, e := tar.FileInfoHeader(info, "")
		if e != nil {
			return e
		}
		h.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			h.Name += "/"
		}
		if e = tw.WriteHeader(h); e != nil {
			return e
		}
		if info.IsDir() {
			return nil
		}
		total += info.Size()
		if total > limit {
			return errors.New("repository source exceeds limit")
		}
		f, e := rootHandle.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if e != nil {
			return e
		}
		opened, statErr := f.Stat()
		if statErr != nil || !os.SameFile(info, opened) {
			_ = f.Close()
			return errors.New("repository source changed while opening")
		}
		_, ce := io.CopyN(tw, f, info.Size())
		finished, finishErr := f.Stat()
		closeErr := f.Close()
		if ce != nil || finishErr != nil || !os.SameFile(info, finished) || finished.Size() != info.Size() || !finished.ModTime().Equal(info.ModTime()) {
			return errors.New("repository source changed while reading")
		}
		return closeErr
	})
	if err != nil {
		return nil, err
	}
	if err = tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func extractSnapshot(input io.Reader, root string) error {
	tr := tar.NewReader(io.LimitReader(input, MaxSnapshotBytes+(4<<20)))
	var files int
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errors.New("repository snapshot archive is invalid")
		}
		name := strings.TrimPrefix(filepath.ToSlash(h.Name), "./")
		if name == "" {
			continue
		}
		if !safeRepositoryPath(name) || strings.Contains("/"+name+"/", "/.git/") {
			return errors.New("repository snapshot path is invalid")
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(target, 0700); err != nil {
				return errors.New("cannot materialize repository snapshot")
			}
		case tar.TypeReg, tar.TypeRegA:
			files++
			total += h.Size
			if files > MaxSnapshotFiles || h.Size < 0 || total > MaxSnapshotBytes {
				return errors.New("repository snapshot limits exceeded")
			}
			if h.Mode&^0777 != 0 || h.Mode&0111 != 0 && h.Mode&^0755 != 0 || h.Mode&0111 == 0 && h.Mode&^0644 != 0 {
				return errors.New("repository snapshot file mode is forbidden")
			}
			if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			f, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0777)
			if e != nil {
				return errors.New("cannot materialize repository snapshot")
			}
			_, ce := io.CopyN(f, tr, h.Size)
			closeErr := f.Close()
			if ce != nil || closeErr != nil {
				return errors.New("cannot materialize repository snapshot")
			}
		default:
			return errors.New("repository snapshot special files are forbidden")
		}
	}
}
