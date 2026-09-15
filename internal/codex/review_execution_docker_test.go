package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/codexsession"
	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

// The test builds its own uniquely tagged image and removes it afterward,
// instead of adding a persistent make target.
func TestDockerReviewExecutionProducesACharterCompliantResult(t *testing.T) {
	if os.Getenv("SHIPMUNK_CODEX_DOCKER_TEST") != "1" {
		t.Skip("set SHIPMUNK_CODEX_DOCKER_TEST=1 for the credential-free Linux Docker regression")
	}
	baseImage := os.Getenv("SHIPMUNK_CODEX_TEST_IMAGE")
	if baseImage == "" {
		baseImage = "shipmunk-profile-native-test:local"
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("shipmunk-codex-review-fixture-%d:local", time.Now().UnixNano())
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "docker", "build", "--pull=false", "--build-arg", "CODEX_FIXTURE_BASE="+baseImage,
		"--tag", tag, "--file", filepath.Join(repoRoot, "runner/tests/fixtures/codex-review/Dockerfile"), repoRoot)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build synthetic review image: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		rmCtx, cancelRM := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelRM()
		if output, err := exec.CommandContext(rmCtx, "docker", "rmi", "-f", tag).CombinedOutput(); err != nil {
			t.Errorf("remove synthetic review image: %v\n%s", err, output)
		}
	})

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	profileHome := filepath.Join(root, "profile")
	workspace := filepath.Join(root, "workspace")
	baseline, head := filepath.Join(workspace, "sources", "0"), filepath.Join(workspace, "sources", "1")
	for _, dir := range []string{profileHome, baseline, head} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(baseline, "README.md"): "old\n",
		filepath.Join(head, "README.md"):     "new\n",
		filepath.Join(head, "added.txt"):     "content\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}

	sessions, err := codexsession.Open(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewExecutor(ExecutorConfig{
		ProfileHome: profileHome, NativeImage: tag, RepositoryImage: tag, Sessions: sessions,
		DockerExecutable: "docker", CommandTimeout: 30 * time.Second,
		NewTransport: func(cfg TransportConfig) (agentTransport, error) { return NewDockerTransport(cfg) },
	})
	if err != nil {
		t.Fatal(err)
	}

	claim := protocol.Claim{
		RunID: "01k4w000000000000000000010", AttemptID: "01k4w000000000000000000011", Fence: 1,
		LeaseExpiresAt: time.Now().Add(2 * time.Minute), Deadline: time.Now().Add(5 * time.Minute),
		Manifest: map[string]any{
			"agent": "codex", "runtime_version": profile.CodexVersion, "kind": "review",
			"source_artifacts": sourceReferences(2), "repository_id": 1, "target_sha": strings.Repeat("9", 40),
			"diff_base_sha": sampleBaselineSHA, "head_sha": sampleHeadSHA, "profile_id": "01k4w000000000000000000003",
			"task_context":     "Review carefully.",
			"effective_config": map[string]any{"model": "gpt-5", "instructions": "Stay focused.", "max_turns": json.Number("20")},
			"supervisor":       map[string]any{"credential_reference": "credential:test"},
		},
	}

	// Registered before Execute: several of its error paths return after
	// transport.Start has already created a container, and t.Fatal below
	// must not skip past cleanup for those.
	t.Cleanup(func() {
		if err := executor.Cleanup(context.Background(), claim); err != nil {
			t.Error(err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	execution, err := executor.Execute(ctx, claim, nil, workspace)
	if err != nil {
		t.Fatal(err)
	}

	if execution.Result["outcome"] != "no_findings" || execution.Result["charter_version"] != ReviewCharterVersion || execution.Result["verification_state"] != "none" {
		t.Fatalf("unexpected review result: %#v", execution.Result)
	}
	coverage, ok := execution.Result["coverage"].(map[string]any)
	if !ok {
		t.Fatalf("coverage missing or malformed: %#v", execution.Result["coverage"])
	}
	files, _ := coverage["files"].([]any)
	covered := make(map[string]bool, len(files))
	for _, raw := range files {
		file, ok := raw.(map[string]any)
		if !ok || file["status"] != "reviewed" {
			t.Fatalf("unexpected coverage entry: %#v", raw)
		}
		covered[file["path"].(string)] = true
	}
	if !covered["README.md"] || !covered["added.txt"] || len(covered) != 2 {
		t.Fatalf("coverage did not match the real changed-file set: %#v", files)
	}

	raw, err := json.Marshal(execution.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.Validate("result", raw); err != nil {
		t.Fatalf("full review result violates the wire contract: %v\n%s", err, raw)
	}
}
