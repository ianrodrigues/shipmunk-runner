package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This fixture drives the shipped review MCP server and host review mediator
// against real Linux containers. Review mode registers no repository_command
// tool at all, so every inspection below goes through review_list,
// review_search, review_read or review_diff; the direct Docker exec probe is
// test instrumentation proving the container-level boundaries those tools
// rely on actually hold, not a capability ever granted to the model.
func TestDockerReviewToolsInspectOnlyAuthorizedSnapshots(t *testing.T) {
	if os.Getenv("SHIPMUNK_CODEX_DOCKER_TEST") != "1" {
		t.Skip("set SHIPMUNK_CODEX_DOCKER_TEST=1 for the credential-free Linux Docker regression")
	}
	image := os.Getenv("SHIPMUNK_CODEX_TEST_IMAGE")
	if image == "" {
		image = "shipmunk-profile-native-test:local"
	}
	workspace, claim := trustedInputFixture(t)
	claim.Manifest["kind"] = "review"
	claim.Manifest["source_artifacts"] = sourceReferences(2)
	base, head := filepath.Join(workspace, "sources", "0"), filepath.Join(workspace, "sources", "1")
	if err := os.MkdirAll(filepath.Join(head, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(head, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(base, "changed.txt"):         "base contents\n",
		filepath.Join(head, "changed.txt"):         "head contents\n",
		filepath.Join(base, "deleted.txt"):         "base-only file\n",
		filepath.Join(head, "added.txt"):           "head-only file\n",
		filepath.Join(base, "unchanged.txt"):       "same\n",
		filepath.Join(head, "unchanged.txt"):       "same\n",
		filepath.Join(head, "src", "app.go"):       "package main\n\nfunc main() {}\n",
		filepath.Join(head, ".codex", "auth.json"): "not a credential\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sources, _, err := executionInputs(claim, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer sources.close()

	// Synthetic canary credentials, shaped like a real profile's auth cache and
	// its environment-style export, planted where /profile is bind-mounted into
	// the native container. The review tool surface has no "profile" snapshot
	// and no filesystem access outside the two pinned snapshot maps, so it must
	// never see or echo these values.
	profileHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(profileHome, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	canary := "sk-canary-DO-NOT-USE-" + strings.Repeat("7", 24)
	if err := os.WriteFile(filepath.Join(profileHome, ".codex", "auth.json"), []byte(`{"OPENAI_API_KEY":"`+canary+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileHome, ".env"), []byte("OPENAI_API_KEY="+canary+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	name := fmt.Sprintf("shipmunk-codex-01k4w000000000000000000066-%d", time.Now().UnixNano()%1000000000000000+1)
	transport, err := newDockerTransport(TransportConfig{
		Name: name, ProfileHome: profileHome, Source: sources.head.Name(), Baseline: sources.baselinePath(),
		BaselineSHA: sampleBaselineSHA, HeadSHA: sampleHeadSHA,
		sourceHandle: sources.head, baselineHandle: sources.baseline,
		NativeImage: image, RepositoryImage: image, MaxCommands: 16,
	}, loggedTestDocker{t})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := transport.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	// Replace both paths after selection: extraction must use retained identities.
	for _, path := range []string{base, head} {
		if err := os.Rename(path, path+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "substituted.txt"), []byte("wrong snapshot\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := transport.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// The repository container shares this fixture's base image with the
	// native container, so an empty /profile mount point exists in both by
	// construction; the security property under test is that it carries no
	// credential material, i.e. the profile bind mount itself never reaches
	// this container.
	probe := `set -eu
for target in /workspace/probe /baseline/probe /workspace/../baseline/probe /proc/self/root/baseline/probe; do
  if (printf corrupted > "$target") 2>/dev/null; then exit 41; fi
done
if chmod 777 /baseline/changed.txt 2>/dev/null; then exit 42; fi
if rm /baseline/deleted.txt 2>/dev/null; then exit 43; fi
if ln /baseline/changed.txt /workspace/base-hard 2>/dev/null; then exit 44; fi
if ln -s /baseline /workspace/base-link 2>/dev/null; then exit 45; fi
if (printf corrupted > /workspace/changed.txt) 2>/dev/null; then exit 46; fi
test ! -e /snapshots && test ! -e /workspace/base && test ! -e /workspace/../base
test ! -e /workspace/.git
test -z "$(ls -A /profile 2>/dev/null)"
echo probe-ok`
	if result, err := transport.rawRun(ctx, nil, "exec", "-i", name+"-repo", "/bin/sh", "-c", probe); err != nil || result.exitCode != 0 || strings.TrimSpace(string(result.stdout)) != "probe-ok" {
		t.Fatalf("container boundary probe failed: exit=%d stdout=%s stderr=%s err=%v", result.exitCode, result.stdout, result.stderr, err)
	}

	type toolCall struct {
		name      string
		arguments map[string]any
	}
	calls := []toolCall{
		{"review_list", map[string]any{"snapshot": "workspace", "path": "", "offset": 0}},
		{"review_list", map[string]any{"snapshot": "workspace", "path": "src", "offset": 0}},
		{"review_read", map[string]any{"snapshot": "baseline", "path": "changed.txt", "start_line": 1, "line_count": 10}},
		{"review_search", map[string]any{"snapshot": "workspace", "path": "", "query": "head contents"}},
		{"review_diff", map[string]any{"path": ""}},
		{"review_diff", map[string]any{"path": "changed.txt"}},
		{"review_read", map[string]any{"snapshot": "baseline", "path": "../deleted.txt", "start_line": 1, "line_count": 10}},
		{"review_read", map[string]any{"snapshot": "workspace", "path": "deleted.txt", "start_line": 1, "line_count": 10}},
		// A profile-shaped path is absent from baseline (the profile canary was
		// never loaded into either snapshot map at all), while the SAME path
		// legitimately exists in workspace with unrelated content planted
		// there directly (see call 10 below) - proving the isolation is
		// structural, not merely "that path happens not to exist".
		{"review_read", map[string]any{"snapshot": "baseline", "path": ".codex/auth.json", "start_line": 1, "line_count": 10}},
		{"review_search", map[string]any{"snapshot": "workspace", "path": "", "query": canary}},
		{"review_read", map[string]any{"snapshot": "workspace", "path": ".codex/auth.json", "start_line": 1, "line_count": 10}},
	}
	var requests strings.Builder
	requests.WriteString("{\"jsonrpc\":\"2.0\",\"id\":0,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{},\"clientInfo\":{\"name\":\"review-regression\",\"version\":\"1\"}}}\n")
	for index, call := range calls {
		frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": index + 1, "method": "tools/call", "params": map[string]any{"name": call.name, "arguments": call.arguments}})
		if err != nil {
			t.Fatal(err)
		}
		requests.Write(frame)
		requests.WriteByte('\n')
	}
	result, err := transport.RunNative(ctx, []string{"/usr/local/bin/node", "/usr/local/lib/shipmunk/codex-mcp.mjs", "review"}, []byte(requests.String()))
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("real review MCP failed: result=%+v err=%v", result, err)
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != len(calls)+1 {
		t.Fatalf("unexpected MCP response: %s", result.Stdout)
	}
	outputs := make([]string, 0, len(calls))
	errored := make([]bool, 0, len(calls))
	for _, line := range lines[1:] {
		var response struct {
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
		}
		if err := json.Unmarshal([]byte(line), &response); err != nil || len(response.Result.Content) != 1 {
			t.Fatalf("MCP tool call failed: %s (%v)", line, err)
		}
		outputs = append(outputs, response.Result.Content[0].Text)
		errored = append(errored, response.Result.IsError)
	}

	if errored[0] {
		t.Fatalf("review_list root failed: %s", outputs[0])
	}
	for _, want := range []string{"file\tadded.txt", "file\tchanged.txt", "dir\tsrc"} {
		if !strings.Contains(outputs[0], want) {
			t.Fatalf("root listing missing %q: %s", want, outputs[0])
		}
	}
	if strings.Contains(outputs[0], "substituted.txt") {
		t.Fatalf("listing used a path instead of the pinned descriptor: %s", outputs[0])
	}
	// snapshot_sha must reach the model in the text content itself, not just
	// the mediator's own struct: this is what codex-mcp.mjs's reviewBridge
	// forwards as the actual MCP tool result.
	if !strings.HasPrefix(outputs[0], "snapshot_sha: "+sampleHeadSHA+"\n") {
		t.Fatalf("review_list workspace response missing real head snapshot_sha: %s", outputs[0])
	}

	if errored[1] || !strings.Contains(outputs[1], "app.go") {
		t.Fatalf("review_list src failed: %s", outputs[1])
	}

	if errored[2] || strings.TrimSpace(strings.TrimPrefix(outputs[2], "snapshot_sha: "+sampleBaselineSHA+"\n")) != "base contents" {
		t.Fatalf("review_read baseline/changed.txt unexpected: %s", outputs[2])
	}
	if !strings.HasPrefix(outputs[2], "snapshot_sha: "+sampleBaselineSHA+"\n") {
		t.Fatalf("review_read baseline response missing real baseline snapshot_sha: %s", outputs[2])
	}

	if errored[3] || !strings.Contains(outputs[3], "changed.txt:1: head contents") {
		t.Fatalf("review_search unexpected: %s", outputs[3])
	}

	if errored[4] {
		t.Fatalf("review_diff changed-file list failed: %s", outputs[4])
	}
	for _, want := range []string{"modified\tchanged.txt", "deleted\tdeleted.txt", "added\tadded.txt"} {
		if !strings.Contains(outputs[4], want) {
			t.Fatalf("changed-file list missing %q: %s", want, outputs[4])
		}
	}
	if strings.Contains(outputs[4], "unchanged.txt") {
		t.Fatalf("changed-file list included an unchanged file: %s", outputs[4])
	}

	if errored[5] || !strings.Contains(outputs[5], "-base contents") || !strings.Contains(outputs[5], "+head contents") {
		t.Fatalf("review_diff single-file diff unexpected: %s", outputs[5])
	}

	for index, description := range map[int]string{
		6: "traversal path",
		7: "cross-snapshot access",
		8: "profile-shaped path absent from baseline",
	} {
		if !errored[index] {
			t.Fatalf("%s was not rejected: %s", description, outputs[index])
		}
	}

	if errored[9] || outputs[9] != "no matches" {
		t.Fatalf("canary search unexpected: error=%t output=%s", errored[9], outputs[9])
	}

	// The same profile-shaped path legitimately exists in workspace (planted
	// directly in the fixture, not via /profile): it must return that planted
	// content, never the real profile canary, proving the tool surface has no
	// channel to /profile regardless of what the path string looks like.
	if errored[10] || strings.TrimSpace(strings.TrimPrefix(outputs[10], "snapshot_sha: "+sampleHeadSHA+"\n")) != "not a credential" {
		t.Fatalf("profile-shaped path in workspace returned unexpected content: error=%t output=%s", errored[10], outputs[10])
	}
	if !strings.HasPrefix(outputs[10], "snapshot_sha: "+sampleHeadSHA+"\n") {
		t.Fatalf("review_read workspace response missing real head snapshot_sha: %s", outputs[10])
	}

	for index, output := range outputs {
		if strings.Contains(output, canary) {
			t.Fatalf("canary credential leaked through tool response %d: %s", index, output)
		}
	}
	t.Logf("real review MCP outputs:\n%s", strings.Join(outputs, "\n---\n"))

	if err := transport.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{name, name + "-repo", name + "-diff"} {
		_, absent, err := transport.ownedResource(ctx, resource, false)
		if err != nil || !absent {
			t.Fatalf("container remains after cleanup: %s (%v)", resource, err)
		}
	}
	_, absent, err := transport.ownedResource(ctx, name+"-workspace", true)
	if err != nil || !absent {
		t.Fatalf("snapshot volume remains after cleanup: %v", err)
	}
}

type loggedTestDocker struct{ t *testing.T }

func (d loggedTestDocker) Run(ctx context.Context, timeout time.Duration, limit int, stdin io.Reader, args ...string) (transportResult, error) {
	result, err := (execDockerCommand{}).Run(ctx, timeout, limit, stdin, args...)
	if err != nil || result.exitCode != 0 && args[0] != "inspect" && args[0] != "volume" {
		d.t.Logf("Docker %v: stdout=%s stderr=%s err=%v", args, result.stdout, result.stderr, err)
	}
	return result, err
}
