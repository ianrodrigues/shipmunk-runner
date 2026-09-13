package codex

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedDocker struct {
	mu               sync.Mutex
	calls            [][]string
	blockNative      bool
	failCreate       bool
	inspectUncertain bool
	lastInput        []byte
}

func (d *recordedDocker) Run(ctx context.Context, _ time.Duration, _ int, stdin io.Reader, args ...string) (transportResult, error) {
	d.mu.Lock()
	d.calls = append(d.calls, append([]string(nil), args...))
	d.mu.Unlock()
	if d.failCreate && len(args) > 0 && args[0] == "create" {
		return transportResult{exitCode: 1}, nil
	}
	if len(args) >= 3 && args[0] == "image" {
		return transportResult{stdout: []byte("sha256:" + strings.Repeat("a", 64) + "\n")}, nil
	}
	if args[0] == "inspect" || len(args) > 1 && args[0] == "volume" && args[1] == "inspect" {
		if d.inspectUncertain {
			return transportResult{}, context.DeadlineExceeded
		}
		return transportResult{stderr: []byte("No such object"), exitCode: 1}, nil
	}
	if len(args) > 3 && args[0] == "exec" && args[1] == "-i" && args[2] == testTransportName+"-repo" {
		if stdin != nil {
			raw, _ := io.ReadAll(stdin)
			d.mu.Lock()
			d.lastInput = raw
			d.mu.Unlock()
		}
		return transportResult{stdout: []byte("repository output\n")}, nil
	}
	if len(args) > 2 && args[0] == "exec" && args[1] == "-i" && args[2] == testTransportName && d.blockNative {
		<-ctx.Done()
		return transportResult{}, ctx.Err()
	}
	return transportResult{}, nil
}

func TestDockerTransportPinsSourceAndProfileDirectories(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fd-backed Docker bind paths are used on Linux")
	}
	d := new(recordedDocker)
	transport := transportFixture(t, d)
	original := transport.sourceHandle.Name()
	moved := original + "-moved"
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, "attacker.txt"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	input := append([]byte(nil), d.lastInput...)
	calls := d.calls
	d.mu.Unlock()
	if !strings.Contains(string(input), "original") || strings.Contains(string(input), "replacement") {
		t.Fatal("source path replacement changed archived bytes")
	}
	all := ""
	for _, call := range calls {
		all += strings.Join(call, " ") + "\n"
	}
	if !strings.Contains(all, "src=/proc/") {
		t.Fatal("profile bind did not use a pinned descriptor path")
	}
}

func TestDockerTransportJoinsSetupAndCleanupFailures(t *testing.T) {
	d := &recordedDocker{failCreate: true, inspectUncertain: true}
	transport := transportFixture(t, d)
	err := transport.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot create native container") || !strings.Contains(err.Error(), "cleanup was not confirmed") {
		t.Fatalf("setup/cleanup failures were not both retained: %v", err)
	}
}

func TestDockerTransportJoinsCollectionAndCleanupFailures(t *testing.T) {
	d := &recordedDocker{inspectUncertain: true}
	transport := transportFixture(t, d)
	transport.started = true
	d.failCreate = true
	_, err := transport.CollectPatch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot create patch collector") || !strings.Contains(err.Error(), "cleanup was not confirmed") {
		t.Fatalf("collection/cleanup failures were not both retained: %v", err)
	}
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
	for _, resource := range []string{testTransportName, testTransportName + "-repo"} {
		if !strings.Contains(all, "--name "+resource) || !strings.Contains(all, "shipmunk.codex-owner="+testTransportName) {
			t.Fatalf("Codex boundary %s omitted its watchdog ownership label", resource)
		}
	}
	if !strings.Contains(all, "volume create --label shipmunk.codex=true --label shipmunk.codex-owner="+testTransportName) {
		t.Fatal("Codex workspace volume omitted its watchdog ownership label")
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
