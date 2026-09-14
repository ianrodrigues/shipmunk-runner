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

// This fixture calls the shipped MCP server and host mediator against real
// Linux containers. It uses no provider account or predetermined review result.
func TestDockerReviewSnapshotsThroughRepositoryMCP(t *testing.T) {
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
	if err := os.Mkdir(head, 0700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(base, "changed.txt"): "base contents\n", filepath.Join(head, "changed.txt"): "head contents\n",
		filepath.Join(base, "deleted.txt"): "base-only file\n", filepath.Join(head, "added.txt"): "head-only file\n",
		filepath.Join(base, "unchanged.txt"): "same\n", filepath.Join(head, "unchanged.txt"): "same\n",
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
	name := fmt.Sprintf("shipmunk-codex-01k4w000000000000000000066-%d", time.Now().UnixNano()%1000000000000000+1)
	transport, err := newDockerTransport(TransportConfig{
		Name: name, ProfileHome: t.TempDir(), Source: sources.head.Name(), Baseline: sources.baselinePath(),
		sourceHandle: sources.head, baselineHandle: sources.baseline,
		NativeImage: image, RepositoryImage: image, MaxCommands: 3,
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
	commands := []string{
		`set -eu
cat /baseline/changed.txt /workspace/changed.txt /baseline/deleted.txt /workspace/added.txt
test ! -e /baseline/added.txt && test ! -e /workspace/deleted.txt
test ! -e /baseline/.git && test "$(git rev-list --count HEAD)" = 1
test -z "$(git status --porcelain)"
diff -qr --exclude=.git /baseline /workspace || test "$?" = 1`,
		`set -eu
for target in /baseline/changed.txt /proc/self/root/baseline/changed.txt /proc/1/root/baseline/changed.txt /workspace/../baseline/changed.txt; do
  if (printf corrupted > "$target") 2>/dev/null; then exit 41; fi
done
if chmod 777 /baseline/changed.txt 2>/dev/null; then exit 42; fi
if rm /baseline/deleted.txt 2>/dev/null; then exit 43; fi
if mv /baseline/changed.txt /workspace/stolen.txt 2>/dev/null; then exit 44; fi
if ln /baseline/changed.txt /workspace/base-hard 2>/dev/null; then exit 46; fi
ln -s /baseline /workspace/base-link
if (printf corrupted > /workspace/base-link/changed.txt) 2>/dev/null; then exit 45; fi
rm /workspace/base-link
test ! -e /snapshots && test ! -e /workspace/base && test ! -e /workspace/../base
printf 'mutable head\n' > /workspace/changed.txt
cat /baseline/changed.txt /workspace/changed.txt
printf 'baseline immutable\n'`,
	}
	var requests strings.Builder
	requests.WriteString("{\"jsonrpc\":\"2.0\",\"id\":0,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{},\"clientInfo\":{\"name\":\"snapshot-regression\",\"version\":\"1\"}}}\n")
	for index, command := range commands {
		frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": index + 1, "method": "tools/call", "params": map[string]any{"name": "repository_command", "arguments": map[string]any{"command": command}}})
		requests.Write(frame)
		requests.WriteByte('\n')
	}
	result, err := transport.RunNative(ctx, []string{"/usr/local/bin/node", "/usr/local/lib/shipmunk/codex-mcp.mjs"}, []byte(requests.String()))
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("real MCP failed: result=%+v err=%v", result, err)
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("unexpected MCP response: %s", result.Stdout)
	}
	outputs := make([]string, 0, 2)
	for _, line := range lines[1:] {
		var response struct {
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
		}
		if err := json.Unmarshal([]byte(line), &response); err != nil || response.Result.IsError || len(response.Result.Content) != 1 {
			t.Fatalf("MCP repository command failed: %s (%v)", line, err)
		}
		var command commandResponse
		if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &command); err != nil || command.ExitCode != 0 {
			t.Fatalf("repository command failed: %+v (%v)", command, err)
		}
		outputs = append(outputs, command.Stdout)
	}
	for _, want := range []string{"base contents\nhead contents\nbase-only file\nhead-only file\n", "Only in /workspace: added.txt", "Files /baseline/changed.txt and /workspace/changed.txt differ", "Only in /baseline: deleted.txt"} {
		if !strings.Contains(outputs[0], want) {
			t.Fatalf("snapshot comparison omitted %q: %s", want, outputs[0])
		}
	}
	if strings.Contains(outputs[0], "unchanged.txt") || strings.Contains(outputs[0], "substituted.txt") {
		t.Fatalf("incorrect changed-file set: %s", outputs[0])
	}
	if outputs[1] != "base contents\nmutable head\nbaseline immutable\n" {
		t.Fatalf("baseline isolation failed: %s", outputs[1])
	}
	t.Logf("real repository MCP comparison:\n%s%s", outputs[0], outputs[1])
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
