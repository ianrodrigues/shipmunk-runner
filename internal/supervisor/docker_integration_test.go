package supervisor

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
	"github.com/ianrodrigues/shipmunk-runner/internal/workspace"
)

// This is the real HTTP -> journal -> workspace -> Docker -> watchdog ->
// completion -> stopped-ack path. Only the control plane and native CLI are fakes.
func TestDockerSupervisorEndToEnd(t *testing.T) {
	if os.Getenv("SHIPMUNK_SANDBOX_DOCKER_TEST") != "1" {
		t.Skip("requires designated Linux Docker engine and synthetic runtime image")
	}
	image := os.Getenv("SHIPMUNK_RUNNER_IMAGE")
	watchdogPath := os.Getenv("SHIPMUNK_WATCHDOG_BINARY")
	if image == "" || watchdogPath == "" {
		t.Fatal("SHIPMUNK_RUNNER_IMAGE and SHIPMUNK_WATCHDOG_BINARY are required")
	}
	dockerExecutable := os.Getenv("SHIPMUNK_DOCKER_EXECUTABLE")
	if dockerExecutable == "" {
		dockerExecutable = "docker"
	}
	for _, scenario := range []string{"completed", "completion_rejected", "stop_during_download"} {
		t.Run(scenario, func(t *testing.T) {
			claim := fixtureClaim(t)
			var random [12]byte
			if _, err := rand.Read(random[:]); err != nil {
				t.Fatal(err)
			}
			claim.Manifest["attempt_id"] = "01" + hex.EncodeToString(random[:])
			claim.Manifest["deadline"] = time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339)
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			content := []byte("synthetic repository contents\n")
			if err := writer.WriteHeader(&tar.Header{Name: "README.txt", Mode: 0600, Size: int64(len(content))}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(content); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			instructions := []byte(`{"instructions":"Synthetic instructions only."}`)
			sourceHash := sha256.Sum256(archive.Bytes())
			instructionsHash := sha256.Sum256(instructions)
			sourceID := "01k4w000000000000000000003"
			instructionsID := "01k4w000000000000000000004"
			claim.Manifest["source_artifacts"] = []any{map[string]any{"artifact_id": sourceID, "sha256": hex.EncodeToString(sourceHash[:])}}
			claim.Manifest["instruction_artifacts"] = []any{map[string]any{"artifact_id": instructionsID, "sha256": hex.EncodeToString(instructionsHash[:])}}
			var err error
			claim, err = protocol.ClaimFromManifest(claim.Manifest, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			state, err := attemptstate.Open(filepath.Join(root, "active-attempt.json"))
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			workspaces, err := workspace.New(filepath.Join(root, "workspaces"))
			if err != nil {
				t.Fatal(err)
			}
			var beats, acks, completions atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic-runner-token" {
					t.Error("missing synthetic authorization")
					w.WriteHeader(401)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/runner/v1/claims":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": claim.Manifest})
				case strings.HasSuffix(r.URL.Path, "/heartbeat"):
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if body["stopped"] == true {
						if _, err := os.Stat(workspaces.Path(claim)); !os.IsNotExist(err) {
							t.Error("stopped acknowledgement preceded workspace removal")
						}
						inspectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						containers, inspectErr := exec.CommandContext(inspectCtx, dockerExecutable, "container", "ls", "--all", "--filter", "label=shipmunk.attempt="+claim.AttemptID, "--format", "{{.ID}}").Output()
						cancel()
						if inspectErr != nil || len(bytes.TrimSpace(containers)) != 0 {
							t.Errorf("stopped acknowledgement without confirmed container absence: %q %v", containers, inspectErr)
						}
						acks.Add(1)
						w.WriteHeader(204)
						return
					}
					count := beats.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"protocol_version": protocol.Version, "attempt_id": claim.AttemptID, "fence": claim.Fence, "state": "running", "lease_expires_at": time.Now().Add(45 * time.Second).UTC().Format(time.RFC3339), "stop_requested": scenario == "stop_during_download" && count >= 2}})
				case strings.HasSuffix(r.URL.Path, "/input-artifacts/"+sourceID):
					if scenario == "stop_during_download" {
						<-r.Context().Done()
						return
					}
					w.Header().Set("X-Artifact-SHA256", hex.EncodeToString(sourceHash[:]))
					w.Header().Set("Content-Length", fmt.Sprint(archive.Len()))
					_, _ = w.Write(archive.Bytes())
				case strings.HasSuffix(r.URL.Path, "/input-artifacts/"+instructionsID):
					w.Header().Set("X-Artifact-SHA256", hex.EncodeToString(instructionsHash[:]))
					w.Header().Set("Content-Length", fmt.Sprint(len(instructions)))
					_, _ = w.Write(instructions)
				case strings.HasSuffix(r.URL.Path, "/completion"):
					completions.Add(1)
					if scenario == "completion_rejected" {
						w.WriteHeader(409)
					} else {
						w.WriteHeader(204)
					}
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			client, err := protocol.NewHTTPClient(server.URL, "synthetic-runner-token", nil)
			if err != nil {
				t.Fatal(err)
			}
			config := sandbox.Config{Image: image, DockerExecutable: os.Getenv("SHIPMUNK_DOCKER_EXECUTABLE"), WatchdogExecutable: watchdogPath}
			docker, err := sandbox.New(config)
			if err != nil {
				t.Fatal(err)
			}
			worker := &Supervisor{Client: client, State: state, Workspaces: workspaces, Sandbox: DockerBackend{Docker: docker}, Watchdog: HostWatchdog{Watchdog: sandbox.NewWatchdog(config)}, heartbeatInterval: 20 * time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			out, runErr := worker.RunOnce(ctx)
			if scenario == "completed" {
				if runErr != nil || !out.Worked || out.Result != "incomplete" {
					t.Fatalf("completion %+v %v", out, runErr)
				}
			} else if runErr == nil || out.Worked {
				t.Fatalf("failure reported as completion: %+v %v", out, runErr)
			}
			if acks.Load() != 1 {
				t.Fatalf("stopped acknowledgements=%d, run error=%v", acks.Load(), runErr)
			}
			if scenario == "stop_during_download" && (completions.Load() != 0 || beats.Load() < 2) {
				t.Fatal("stop did not cancel preparation")
			}
			if saved, err := state.Load(); err != nil || saved != nil {
				t.Fatalf("journal after cleanup=%+v %v", saved, err)
			}
		})
	}
}
