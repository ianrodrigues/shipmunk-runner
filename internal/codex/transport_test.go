package codex

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedDocker struct {
	mu          sync.Mutex
	calls       [][]string
	blockNative bool
}

func (d *recordedDocker) Run(ctx context.Context, _ time.Duration, _ int, stdin io.Reader, args ...string) (transportResult, error) {
	d.mu.Lock()
	d.calls = append(d.calls, append([]string(nil), args...))
	d.mu.Unlock()
	if len(args) >= 3 && args[0] == "image" {
		return transportResult{stdout: []byte("sha256:" + strings.Repeat("a", 64) + "\n")}, nil
	}
	if args[0] == "inspect" || len(args) > 1 && args[0] == "volume" && args[1] == "inspect" {
		return transportResult{stderr: []byte("No such object"), exitCode: 1}, nil
	}
	if len(args) > 3 && args[0] == "exec" && args[1] == "-i" && args[2] == testTransportName+"-repo" {
		if stdin != nil {
			_, _ = io.Copy(io.Discard, stdin)
		}
		return transportResult{stdout: []byte("repository output\n")}, nil
	}
	if len(args) > 2 && args[0] == "exec" && args[1] == "-i" && args[2] == testTransportName && d.blockNative {
		<-ctx.Done()
		return transportResult{}, ctx.Err()
	}
	return transportResult{}, nil
}

const testTransportName = "shipmunk-codex-01aaaaaaaaaaaaaaaaaaaaaaaa-1"

func transportFixture(t *testing.T, docker dockerCommand) *DockerTransport {
	t.Helper()
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	transport, err := newDockerTransport(TransportConfig{Name: testTransportName, ProfileHome: profile, Source: source, NativeImage: "native:pinned", RepositoryImage: "repo:pinned", MaxCommands: 2}, docker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Stop(context.Background()) })
	return transport
}

func TestDockerTransportCreatesSeparatedBoundaries(t *testing.T) {
	d := new(recordedDocker)
	transport := transportFixture(t, d)
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	calls := append([][]string(nil), d.calls...)
	d.mu.Unlock()
	joined := make([]string, len(calls))
	for i, c := range calls {
		joined[i] = strings.Join(c, " ")
	}
	all := strings.Join(joined, "\n")
	if !strings.Contains(all, "--name "+testTransportName+" --label shipmunk.codex=true") || !strings.Contains(all, "--network bridge") {
		t.Fatal("native provider boundary was not created with network access")
	}
	if !strings.Contains(all, "--name "+testTransportName+"-repo") || !strings.Contains(all, "--network none") || !strings.Contains(all, "type=volume,src="+testTransportName+"-workspace,dst=/workspace") {
		t.Fatal("repository boundary did not use an isolated memory volume")
	}
	for _, line := range joined {
		if strings.Contains(line, "--name "+testTransportName+"-repo") && strings.Contains(line, transport.cfg.ProfileHome) {
			t.Fatal("repository container received the credential home")
		}
	}
}

func TestDockerTransportServicesStrictFencedBridge(t *testing.T) {
	d := new(recordedDocker)
	transport := transportFixture(t, d)
	transport.started = true
	if err := os.WriteFile(filepath.Join(transport.bridge, "request.json"), []byte(`{"id":1,"command":"printf safe"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := transport.serviceBridge(context.Background()); err != nil {
		t.Fatal(err)
	}
	response, err := os.ReadFile(filepath.Join(transport.bridge, "response.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), `"id":1`) || !strings.Contains(string(response), `repository output\n`) {
		t.Fatalf("unexpected bridge response: %s", response)
	}
	if err := transport.serviceBridge(context.Background()); err != nil {
		t.Fatalf("published response caused replay: %v", err)
	}
}

func TestDockerTransportRejectsBridgeDuplicateKeys(t *testing.T) {
	transport := transportFixture(t, new(recordedDocker))
	transport.started = true
	if err := os.WriteFile(filepath.Join(transport.bridge, "request.json"), []byte(`{"id":1,"id":1,"command":"true"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := transport.serviceBridge(context.Background()); err == nil || err.Error() != "repository command request schema is invalid" {
		t.Fatalf("duplicate keys were not rejected: %v", err)
	}
}

func TestDockerTransportCancellationCleansEverySibling(t *testing.T) {
	d := &recordedDocker{blockNative: true}
	transport := transportFixture(t, d)
	transport.started = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := transport.RunNative(ctx, []string{"codex", "exec"}, nil)
	if err == nil {
		t.Fatal("cancellation was ignored")
	}
	d.mu.Lock()
	all := ""
	for _, call := range d.calls {
		all += strings.Join(call, " ") + "\n"
	}
	d.mu.Unlock()
	for _, name := range []string{testTransportName, testTransportName + "-repo", testTransportName + "-diff"} {
		if !strings.Contains(all, "rm --force "+name) {
			t.Fatalf("cleanup omitted %s", name)
		}
	}
}

func TestExtractSnapshotRejectsLinksAndTraversal(t *testing.T) {
	for _, fixture := range [][]byte{tarFixture(t, "../escape", 0), tarFixture(t, "link", 2)} {
		if err := extractSnapshot(strings.NewReader(string(fixture)), t.TempDir()); err == nil {
			t.Fatal("unsafe snapshot archive was accepted")
		}
	}
}

func tarFixture(t *testing.T, name string, kind byte) []byte {
	t.Helper()
	var b strings.Builder
	w := tar.NewWriter(&b)
	h := &tar.Header{Name: name, Mode: 0600, Size: 0, Typeflag: kind}
	if err := w.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(b.String())
}
