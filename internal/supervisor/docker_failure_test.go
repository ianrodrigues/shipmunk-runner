package supervisor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
	"github.com/ianrodrigues/shipmunk-runner/internal/workspace"
)

func dockerFailureConfig(t *testing.T) sandbox.Config {
	t.Helper()
	if os.Getenv("SHIPMUNK_SANDBOX_DOCKER_TEST") != "1" {
		t.Skip("requires designated Linux Docker engine and synthetic runtime image")
	}
	executable := os.Getenv("SHIPMUNK_DOCKER_EXECUTABLE")
	if executable == "" {
		executable = "docker"
	}
	executable, err := exec.LookPath(executable)
	if err != nil {
		t.Fatal(err)
	}
	config := sandbox.Config{Image: os.Getenv("SHIPMUNK_RUNNER_IMAGE"), DockerExecutable: executable, WatchdogExecutable: os.Getenv("SHIPMUNK_WATCHDOG_BINARY"), PollInterval: 50 * time.Millisecond, StopGrace: time.Second}
	if config.Image == "" || config.WatchdogExecutable == "" {
		t.Fatal("fixture image and watchdog executable are required")
	}
	return config
}

func uniqueDockerClaim(t *testing.T) protocol.Claim {
	t.Helper()
	claim := fixtureClaim(t)
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	claim.Manifest["attempt_id"] = "01" + hex.EncodeToString(random[:])
	claim.Manifest["deadline"] = time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339)
	claim, err := protocol.ClaimFromManifest(claim.Manifest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

// The Docker process is real. Only the temporary CLI outage and fenced HTTP
// responses are synthetic. Neither failure may erase recovery evidence.
func TestDockerRecoveryRetainsStateAcrossFailureAndFreshSupervisor(t *testing.T) {
	config := dockerFailureConfig(t)
	for _, failure := range []string{"docker_unavailable", "stopped_ack_stale_fence"} {
		t.Run(failure, func(t *testing.T) {
			claim := uniqueDockerClaim(t)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			workspaces, err := workspace.New(filepath.Join(root, "workspaces"))
			if err != nil {
				t.Fatal(err)
			}
			path := workspaces.Path(claim)
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			docker, err := sandbox.New(config)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			process, err := docker.Create(ctx, claim, map[string]any{"fake_mode": "sleep"}, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cleanupCancel()
				if err := process.Remove(cleanupCtx); err != nil {
					t.Error(err)
				}
			})
			if err := process.Start(ctx); err != nil {
				t.Fatal(err)
			}
			id := process.ContainerID()
			journal := filepath.Join(root, "active-attempt.json")
			store, err := attemptstate.Open(journal)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			if err := store.Save(attemptstate.State{RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, SandboxID: &id, LeaseExpiresAt: claim.LeaseExpiresAt, Deadline: claim.Deadline, Workspace: path}); err != nil {
				t.Fatal(err)
			}
			var acknowledge, claims atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/heartbeat"):
					count := acknowledge.Add(1)
					if failure == "stopped_ack_stale_fence" && count == 1 {
						w.WriteHeader(http.StatusConflict)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				case r.URL.Path == "/runner/v1/claims":
					claims.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected recovery endpoint: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			client, err := protocol.NewHTTPClient(server.URL, "synthetic-recovery-token", nil)
			if err != nil {
				t.Fatal(err)
			}
			outage := filepath.Join(root, "docker-unavailable")
			recoveryDocker := docker
			if failure == "docker_unavailable" {
				quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
				wrapper := filepath.Join(root, "docker-wrapper")
				script := "#!/bin/sh\nif [ -e " + quote(outage) + " ]; then exit 125; fi\nexec " + quote(config.DockerExecutable) + " \"$@\"\n"
				if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(outage, nil, 0600); err != nil {
					t.Fatal(err)
				}
				faultConfig := config
				faultConfig.DockerExecutable = wrapper
				recoveryDocker, err = sandbox.New(faultConfig)
				if err != nil {
					t.Fatal(err)
				}
			}
			makeSupervisor := func() *Supervisor {
				return &Supervisor{Client: client, State: store, Workspaces: workspaces, Sandbox: DockerBackend{Docker: recoveryDocker}, Watchdog: HostWatchdog{Watchdog: sandbox.NewWatchdog(config)}}
			}
			if _, err := makeSupervisor().RunOnce(ctx); !errors.Is(err, ErrCleanupUnconfirmed) {
				t.Fatalf("recovery failure was not retained: %v", err)
			}
			if saved, err := store.Load(); err != nil || saved == nil || saved.SandboxID == nil || *saved.SandboxID != id || claims.Load() != 0 {
				t.Fatalf("failure lost identity or admitted claim: %+v %v claims=%d", saved, err, claims.Load())
			}
			if failure == "docker_unavailable" {
				if acknowledge.Load() != 0 {
					t.Fatal("Docker outage allowed a stopped acknowledgement")
				}
				if err := os.Remove(outage); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = attemptstate.Open(journal)
			if err != nil {
				t.Fatal(err)
			}
			if out, err := makeSupervisor().RunOnce(ctx); err != nil || out.Worked {
				t.Fatalf("fresh supervisor failed cleanup retry: %+v %v", out, err)
			}
			if saved, err := store.Load(); err != nil || saved != nil || claims.Load() != 1 {
				t.Fatalf("confirmed recovery did not clear state: %+v %v claims=%d", saved, err, claims.Load())
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("workspace remains after recovery: %v", err)
			}
			if err := docker.Reconcile(ctx, id); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDockerWatchdogLeaseAndDeadlineExpiry(t *testing.T) {
	config := dockerFailureConfig(t)
	for _, scenario := range []string{"renewal_loss", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			claim := uniqueDockerClaim(t)
			docker, err := sandbox.New(config)
			if err != nil {
				t.Fatal(err)
			}
			name, err := docker.Name(claim)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Minute)
			if scenario == "deadline" {
				deadline = time.Now().Add(10 * time.Second)
			}
			lease, err := sandbox.NewWatchdog(config).Arm(name, time.Now().Add(10*time.Second), deadline)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := lease.CreateStarted(); err != nil {
				t.Fatal(err)
			}
			process, err := docker.Create(ctx, claim, map[string]any{"fake_mode": "sleep"}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cleanupCancel()
				_ = process.Remove(cleanupCtx)
				_ = lease.Disarm()
			})
			if err := lease.CreateFinished(process.ContainerID()); err != nil {
				t.Fatal(err)
			}
			if err := process.Start(ctx); err != nil {
				t.Fatal(err)
			}
			expires := time.Now().Add(time.Minute)
			if scenario == "renewal_loss" {
				expires = time.Now().Add(15 * time.Second)
			}
			if err := lease.Renew(expires); err != nil {
				t.Fatal(err)
			}
			for {
				containers, err := exec.CommandContext(ctx, config.DockerExecutable, "container", "ls", "--all", "--filter", "id="+process.ContainerID(), "--format", "{{.ID}}").Output()
				if err != nil {
					t.Fatal(err)
				}
				if len(bytes.TrimSpace(containers)) == 0 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(fmt.Errorf("watchdog did not remove container after %s: %w", scenario, ctx.Err()))
				case <-time.After(100 * time.Millisecond):
				}
			}
		})
	}
}

func TestDockerStopEscalatesWhenContainerIgnoresTerm(t *testing.T) {
	config := dockerFailureConfig(t)
	docker, err := sandbox.New(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	process, err := docker.Create(ctx, uniqueDockerClaim(t), map[string]any{"fake_mode": "ignore_term"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if err := process.Remove(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
	if err := process.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// Wait for the actual PID 1 signal disposition, not a timing guess about
	// whether the fixture shell has installed its ignored TERM handler.
	for {
		status, err := exec.CommandContext(ctx, config.DockerExecutable, "exec", process.ContainerID(), "cat", "/proc/1/status").Output()
		if err != nil {
			t.Fatal(err)
		}
		ignoresTerm := false
		for _, line := range strings.Split(string(status), "\n") {
			if value, ok := strings.CutPrefix(line, "SigIgn:"); ok {
				mask, parseErr := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				ignoresTerm = mask&(1<<14) != 0
			}
		}
		if ignoresTerm {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture did not begin ignoring TERM")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := process.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	exitCode, err := exec.CommandContext(ctx, config.DockerExecutable, "inspect", "--format", "{{.State.ExitCode}}", process.ContainerID()).Output()
	if err != nil || strings.TrimSpace(string(exitCode)) != "137" {
		t.Fatalf("TERM-ignoring container was not killed: exit=%q error=%v", exitCode, err)
	}
}
