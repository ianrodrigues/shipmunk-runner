package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/codex"
	"github.com/ianrodrigues/shipmunk-runner/internal/codexsession"
	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
	"github.com/ianrodrigues/shipmunk-runner/internal/supervisor"
	"github.com/ianrodrigues/shipmunk-runner/internal/workspace"
)

func runCodexRunner(options RunnerOptions, stdout, stderr io.Writer) int {
	if os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "Run the runner as a dedicated non-root account with Docker access.")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	worker, closeState, err := prepareCodexRunner(ctx, options)
	if err != nil {
		fmt.Fprintln(stderr, "Runner setup failed. Check the private token, profile directory, Docker engine, and pinned images.")
		return 1
	}
	defer closeState()
	return runSupervised(ctx, worker, options.Once, stdout, stderr)
}

func prepareCodexRunner(ctx context.Context, options RunnerOptions) (*supervisor.Supervisor, func() error, error) {
	state, err := attemptstate.Open(filepath.Join(options.StateDir, "active-attempt.json"))
	if err != nil {
		return nil, nil, err
	}
	ready := false
	defer func() {
		if !ready {
			_ = state.Close()
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
	dockerExecutable, err := exec.LookPath("docker")
	if err != nil {
		return nil, nil, err
	}
	watchdogExecutable, err := adjacentWatchdogExecutable()
	if err != nil {
		return nil, nil, err
	}
	if err := codex.RequireDocker(ctx, dockerExecutable); err != nil {
		return nil, nil, err
	}
	for _, image := range []string{options.Image, options.RepositoryImage} {
		id, e := dockerPreflight(ctx, dockerExecutable, "image", "inspect", "--format", "{{.Id}}", "--", image)
		if e != nil || !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(id) {
			return nil, nil, errors.New("Codex images must exist locally")
		}
	}
	workspaces, err := workspace.New(filepath.Join(options.StateDir, "workspaces"))
	if err != nil {
		return nil, nil, err
	}
	mode := codexsession.Fresh
	if options.SessionMode == "resume" {
		mode = codexsession.Resume
	}
	router := &codexProfileRouter{profilesDir: options.ProfilesDir, nativeImage: options.Image, repositoryImage: options.RepositoryImage, dockerExecutable: dockerExecutable, watchdogExecutable: watchdogExecutable, active: make(map[string]supervisor.Executor), sessionMode: mode}
	ready = true
	return &supervisor.Supervisor{Client: client, State: state, Workspaces: workspaces, Executor: router}, state.Close, nil
}

type codexProfileRouter struct {
	profilesDir, nativeImage, repositoryImage, dockerExecutable string
	watchdogExecutable                                          string
	mu                                                          sync.Mutex
	active                                                      map[string]supervisor.Executor
	sessionMode                                                 codexsession.Mode
}

func (r *codexProfileRouter) build(claim protocol.Claim) (supervisor.Executor, error) {
	profileID, ok := claim.Manifest["profile_id"].(string)
	if !ok {
		return nil, errors.New("Codex claim has no profile")
	}
	store, err := profile.Open(r.profilesDir, profileID)
	if err != nil {
		return nil, err
	}
	sessions, err := codexsession.Open(filepath.Join(r.profilesDir, profileID, "sessions"))
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	watchdog := sandbox.NewWatchdog(sandbox.Config{DockerExecutable: r.dockerExecutable, WatchdogExecutable: r.watchdogExecutable})
	delegate, err := codex.NewExecutor(codex.ExecutorConfig{ProfileHome: store.Home(), NativeImage: r.nativeImage, RepositoryImage: r.repositoryImage, DockerExecutable: r.dockerExecutable, Sessions: sessions, SessionMode: r.sessionMode, Watchdog: watchdog})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	executor, err := newProfileExecutor(store, delegate)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return executor, nil
}
func (r *codexProfileRouter) Execute(ctx context.Context, claim protocol.Claim, input map[string]any, workspace string) (supervisor.Execution, error) {
	executor, err := r.build(claim)
	if err != nil {
		return supervisor.Execution{}, err
	}
	key := executionKey(claim)
	r.mu.Lock()
	r.active[key] = executor
	r.mu.Unlock()
	return executor.Execute(ctx, claim, input, workspace)
}
func (r *codexProfileRouter) Cleanup(ctx context.Context, claim protocol.Claim) error {
	key := executionKey(claim)
	r.mu.Lock()
	executor := r.active[key]
	r.mu.Unlock()
	if executor == nil {
		var err error
		executor, err = r.build(claim)
		if err != nil {
			return err
		}
	}
	if err := executor.Cleanup(ctx, claim); err != nil {
		return err
	}
	if owned, ok := executor.(*profileExecutor); ok {
		if err := owned.close(); err != nil {
			return err
		}
	}
	r.mu.Lock()
	delete(r.active, key)
	r.mu.Unlock()
	return nil
}
func (r *codexProfileRouter) Renew(claim protocol.Claim, expiry time.Time) error {
	r.mu.Lock()
	executor := r.active[executionKey(claim)]
	r.mu.Unlock()
	if lease, ok := executor.(supervisor.ExecutorLease); ok {
		return lease.Renew(claim, expiry)
	}
	return nil
}
func executionKey(c protocol.Claim) string { return fmt.Sprintf("%s-%d", c.AttemptID, c.Fence) }
