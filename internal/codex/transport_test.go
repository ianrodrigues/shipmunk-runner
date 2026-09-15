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
	"syscall"
	"testing"
	"time"
)

type recordedDocker struct {
	mu               sync.Mutex
	calls            [][]string
	blockNative      bool
	failCreate       bool
	failSnapshot     bool
	inspectUncertain bool
	lastInput        []byte
	owned            bool
	removed          map[string]bool
	labelOwner       string
	collectorOutput  []byte
	onNativeStart    func()
	profilePath      string
	nativeMountProof []byte
}

func (d *recordedDocker) Run(ctx context.Context, _ time.Duration, _ int, stdin io.Reader, args ...string) (transportResult, error) {
	var onNativeStart func()
	d.mu.Lock()
	if d.removed == nil {
		d.removed = make(map[string]bool)
	}
	d.calls = append(d.calls, append([]string(nil), args...))
	if len(args) > 2 && args[0] == "rm" {
		d.removed[args[len(args)-1]] = true
	}
	if len(args) > 2 && args[0] == "volume" && args[1] == "rm" {
		d.removed[args[len(args)-1]] = true
	}
	if len(args) == 2 && args[0] == "start" && args[1] == testTransportName {
		onNativeStart = d.onNativeStart
	}
	d.mu.Unlock()
	if onNativeStart != nil {
		onNativeStart()
	}
	if d.failSnapshot && len(args) > 2 && args[0] == "exec" && args[1] == "-i" && args[2] == testTransportName+"-diff" {
		return transportResult{exitCode: 1}, nil
	}
	if d.failCreate && len(args) > 0 && args[0] == "create" {
		return transportResult{exitCode: 1}, nil
	}
	if args[0] == "version" {
		return transportResult{stdout: []byte("26.0.0\n26.0.0\nlinux\n")}, nil
	}
	if len(args) >= 3 && args[0] == "image" {
		return transportResult{stdout: []byte("sha256:" + strings.Repeat("a", 64) + "\n")}, nil
	}
	if args[0] == "inspect" || len(args) > 1 && args[0] == "volume" && args[1] == "inspect" {
		if d.inspectUncertain {
			return transportResult{}, context.DeadlineExceeded
		}
		name := args[len(args)-1]
		d.mu.Lock()
		owned := d.owned && !d.removed[name]
		owner := d.labelOwner
		d.mu.Unlock()
		if owned {
			if owner == "" {
				owner = testTransportName
			}
			return transportResult{stdout: []byte("true\n" + owner + "\n")}, nil
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
	if len(args) > 1 && args[0] == "exec" && args[1] == testTransportName+"-diff" {
		return transportResult{stdout: d.collectorOutput}, nil
	}
	if len(args) == 4 && args[0] == "exec" && args[1] == testTransportName && args[2] == "/bin/cat" {
		d.mu.Lock()
		var proof []byte
		if d.nativeMountProof != nil {
			proof = make([]byte, len(d.nativeMountProof))
			copy(proof, d.nativeMountProof)
		}
		profilePath := d.profilePath
		d.mu.Unlock()
		if proof != nil {
			return transportResult{stdout: proof}, nil
		}
		proof, err := os.ReadFile(filepath.Join(profilePath, filepath.Base(args[3])))
		if err != nil {
			return transportResult{exitCode: 1}, nil
		}
		return transportResult{stdout: proof}, nil
	}
	if len(args) > 2 && args[0] == "exec" && args[1] == "-i" && args[2] == testTransportName && d.blockNative {
		<-ctx.Done()
		return transportResult{}, ctx.Err()
	}
	return transportResult{}, nil
}

func TestDockerTransportPinsSourceAndProfileDirectories(t *testing.T) {
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
	if runtime.GOOS == "linux" && !strings.HasPrefix(transport.cfg.Source, "/proc/") {
		t.Fatal("source path did not use a pinned descriptor path")
	}
	if strings.Contains(all, "src=/proc/") && strings.Contains(all, "dst=/profile") {
		t.Fatal("profile bind used a client descriptor path")
	}
	if !strings.Contains(all, "src="+transport.cfg.ProfileHome+",dst=/profile") {
		t.Fatal("profile bind did not use the canonical profile path")
	}
}

func TestDockerTransportRejectsReplacedProfileHomeBeforeNativeContainerCreation(t *testing.T) {
	d := new(recordedDocker)
	transport := transportFixture(t, d)
	profile := transport.cfg.ProfileHome
	if err := os.Rename(profile, profile+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}

	err := transport.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Codex profile directory changed before container setup") {
		t.Fatalf("replaced profile home was accepted: %v", err)
	}

	d.mu.Lock()
	calls := append([][]string(nil), d.calls...)
	d.mu.Unlock()
	for _, call := range calls {
		if len(call) == 0 || call[0] != "create" {
			continue
		}
		for index, argument := range call[:len(call)-1] {
			if argument == "--name" && call[index+1] == testTransportName {
				t.Fatal("native container was created after the profile home changed")
			}
		}
	}
}

func TestDockerTransportRejectsProfileHomeReplacedDuringNativeContainerStart(t *testing.T) {
	d := new(recordedDocker)
	transport := transportFixture(t, d)
	profile := transport.cfg.ProfileHome
	d.onNativeStart = func() {
		if err := os.Rename(profile, profile+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(profile, 0700); err != nil {
			t.Fatal(err)
		}
	}

	err := transport.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Codex profile directory changed before container setup") {
		t.Fatalf("profile home replaced during native start was accepted: %v", err)
	}

	d.mu.Lock()
	calls := append([][]string(nil), d.calls...)
	d.mu.Unlock()
	for _, call := range calls {
		if len(call) == 2 && call[0] == "start" && call[1] == testTransportName+"-repo" {
			t.Fatal("repository container started after the profile home changed")
		}
	}
}

func TestDockerTransportRejectsProfileHomeReplacedAndRestoredDuringNativeContainerStart(t *testing.T) {
	d := new(recordedDocker)
	transport := transportFixture(t, d)
	profile := transport.cfg.ProfileHome
	d.nativeMountProof = []byte{}
	d.onNativeStart = func() {
		moved := profile + "-moved"
		if err := os.Rename(profile, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(profile, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(profile); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(moved, profile); err != nil {
			t.Fatal(err)
		}
	}

	err := transport.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Codex profile mount does not match retained directory") {
		t.Fatalf("profile home replaced and restored during native start was accepted: %v", err)
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

func TestDockerTransportCollectsPatchFromPinnedSource(t *testing.T) {
	d := &recordedDocker{collectorOutput: tarFileFixture(t, "file.txt", "changed\n")}
	transport := transportFixture(t, d)
	transport.started = true
	original := transport.sourceHandle.Name()
	if err := os.Rename(original, original+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, "file.txt"), []byte("attacker\n"), 0600); err != nil {
		t.Fatal(err)
	}

	patch, err := transport.CollectPatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if patch == nil || len(patch.ChangedFiles) != 1 || patch.ChangedFiles[0].BeforeSHA256 == nil || *patch.ChangedFiles[0].BeforeSHA256 != sha256Hex("original\n") {
		t.Fatalf("transport did not collect from its pinned source: %#v", patch)
	}
}

const testTransportName = "shipmunk-codex-01aaaaaaaaaaaaaaaaaaaaaaaa-1"

func transportFixture(t *testing.T, docker dockerCommand, review ...bool) *DockerTransport {
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
	baseline, baselineSHA, headSHA := "", "", ""
	if len(review) > 0 && review[0] {
		baseline = filepath.Join(root, "baseline")
		if err := os.Mkdir(baseline, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(baseline, "base.txt"), []byte("base\n"), 0600); err != nil {
			t.Fatal(err)
		}
		baselineSHA, headSHA = strings.Repeat("a", 40), strings.Repeat("b", 40)
	}
	transport, err := newDockerTransport(TransportConfig{Name: testTransportName, ProfileHome: profile, Source: source, Baseline: baseline, BaselineSHA: baselineSHA, HeadSHA: headSHA, NativeImage: "native:pinned", RepositoryImage: "repo:pinned", MaxCommands: 2}, docker)
	if err != nil {
		t.Fatal(err)
	}
	if recorded, ok := docker.(*recordedDocker); ok {
		recorded.profilePath = profile
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
	if !strings.Contains(all, "o=size=256m,") {
		t.Fatal("single-source volume capacity changed")
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

func TestBridgeRequestMetadataRejectsNilAfterStatFailure(t *testing.T) {
	if validBridgeRequestInfo(nil) {
		t.Fatal("nil metadata from a failed stat was accepted")
	}
}

func TestDockerTransportRejectsStaleResponseTemporary(t *testing.T) {
	transport := transportFixture(t, new(recordedDocker))
	transport.started = true
	if err := os.WriteFile(filepath.Join(transport.bridge, "request.json"), []byte(`{"id":1,"command":"true"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transport.bridge, "response.tmp"), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := transport.serviceBridge(context.Background()); err == nil {
		t.Fatal("stale response temporary was overwritten")
	}
}

func TestDockerTransportRejectsSymlinkResponseTemporary(t *testing.T) {
	transport := transportFixture(t, new(recordedDocker))
	transport.started = true
	if err := os.WriteFile(filepath.Join(transport.bridge, "request.json"), []byte(`{"id":1,"command":"true"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(transport.bridge, "request.json"), filepath.Join(transport.bridge, "response.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := transport.serviceBridge(context.Background()); err == nil {
		t.Fatal("symlink response temporary was followed")
	}
}

func TestDockerTransportRefusesForeignOwnedCleanup(t *testing.T) {
	d := &recordedDocker{owned: true, labelOwner: "shipmunk-codex-01bbbbbbbbbbbbbbbbbbbbbbbb-1"}
	transport := transportFixture(t, d)
	if err := transport.Stop(context.Background()); err == nil {
		t.Fatal("foreign resource ownership was accepted")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, call := range d.calls {
		if len(call) > 0 && (call[0] == "rm" || call[0] == "stop") {
			t.Fatalf("foreign resource was mutated: %v", call)
		}
	}
}

func TestDockerTransportCancellationCleansEverySibling(t *testing.T) {
	d := &recordedDocker{blockNative: true, owned: true}
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

// select can pick either ready case in RunNative.
// This test forces both cases ready at once.
// Repeated runs then hit each branch.
func TestDockerTransportCancellationRacesSelectAgainstDone(t *testing.T) {
	d := &recordedDocker{blockNative: true, owned: true}
	transport := transportFixture(t, d)
	transport.started = true

	previousHook := runNativeRaceHook
	runNativeRaceHook = func(done <-chan struct{}) { <-done }
	t.Cleanup(func() { runNativeRaceHook = previousHook })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := transport.RunNative(ctx, []string{"codex", "exec"}, nil); err == nil {
		t.Fatal("cancellation was ignored")
	}

	d.mu.Lock()
	// This inspect shape runs on every cleanup call, even when already removed.
	// A different inspect shape runs only once.
	// Counting it would hide a repeat.
	stops := 0
	for _, call := range d.calls {
		if len(call) == 4 && call[0] == "inspect" && call[1] == "--format" && call[3] == testTransportName {
			stops++
		}
	}
	removed := make(map[string]bool, len(d.removed))
	for name, gone := range d.removed {
		removed[name] = gone
	}
	d.mu.Unlock()

	if stops != 1 {
		t.Fatalf("Stop ran %d times, want exactly 1", stops)
	}
	for _, name := range []string{testTransportName, testTransportName + "-repo", testTransportName + "-diff", testTransportName + "-workspace"} {
		if !removed[name] {
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

func tarFileFixture(t *testing.T, name, contents string) []byte {
	t.Helper()
	var b strings.Builder
	w := tar.NewWriter(&b)
	h := &tar.Header{Name: name, Mode: 0600, Size: int64(len(contents)), Typeflag: tar.TypeReg}
	if err := w.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(contents)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(b.String())
}

func TestDockerReviewHasNoWritableBaselineAlias(t *testing.T) {
	d := new(recordedDocker)
	transport := transportFixture(t, d, true)
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var repoStart, writerRemoved, repository int
	var boundedVolume bool
	for index, args := range d.calls {
		line := strings.Join(args, " ")
		if strings.HasPrefix(line, "volume create ") && strings.Contains(line, "o=size=512m,") {
			boundedVolume = true
		}
		if strings.HasPrefix(line, "create --name "+testTransportName+"-repo ") {
			repository++
			for _, required := range []string{",dst=/baseline,volume-subpath=base,readonly", ",dst=/workspace,volume-subpath=head,readonly", "--read-only", "--network none", "--cap-drop ALL"} {
				if !strings.Contains(line, required) {
					t.Fatalf("repository omitted %q: %s", required, line)
				}
			}
			if strings.Contains(line, "dst=/snapshots") {
				t.Fatal("repository received writable snapshot parent")
			}
		}
		if line == "start "+testTransportName+"-repo" {
			repoStart = index
		}
		if line == "rm --force "+testTransportName+"-diff" {
			writerRemoved = index
		}
	}
	if !boundedVolume {
		t.Fatal("review snapshot volume omitted its aggregate capacity bound")
	}
	if repository != 1 || repoStart == 0 || writerRemoved <= repoStart {
		t.Fatal("tmpfs snapshot writer lifetime is invalid")
	}
	if transport.workspaceMount("/snapshot-source", true) != "type=volume,src="+testTransportName+"-workspace,dst=/snapshot-source,volume-subpath=head,readonly" {
		t.Fatal("collector mount includes baseline or permits writing")
	}
}

func TestDockerReviewPopulationFailureCleansEveryResource(t *testing.T) {
	d := &recordedDocker{owned: true, failSnapshot: true}
	transport := transportFixture(t, d, true)
	source, baseline := transport.sourceHandle, transport.baselineHandle
	if err := transport.Start(context.Background()); err == nil {
		t.Fatal("snapshot population failure was ignored")
	}
	for _, resource := range []string{testTransportName, testTransportName + "-repo", testTransportName + "-diff", testTransportName + "-workspace"} {
		if !d.removed[resource] {
			t.Fatalf("cleanup omitted %s", resource)
		}
	}
	for _, handle := range []*os.File{source, baseline} {
		if _, err := handle.Stat(); err == nil {
			t.Fatal("source descriptor leaked after failure")
		}
	}
}

func TestDockerReviewCancellationClosesBothSnapshots(t *testing.T) {
	d := &recordedDocker{owned: true, blockNative: true}
	transport := transportFixture(t, d, true)
	source, baseline := transport.sourceHandle, transport.baselineHandle
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := transport.RunNative(ctx, []string{"codex", "exec"}, nil); err == nil {
		t.Fatal("cancellation was ignored")
	}
	for _, resource := range []string{testTransportName, testTransportName + "-repo", testTransportName + "-diff", testTransportName + "-workspace"} {
		if !d.removed[resource] {
			t.Fatalf("cleanup omitted %s", resource)
		}
	}
	for _, handle := range []*os.File{source, baseline} {
		if _, err := handle.Stat(); err == nil {
			t.Fatal("source descriptor leaked on cancellation")
		}
	}
}

func TestDockerReviewRejectsUnsafeBaselineContents(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "git"} {
		t.Run(kind, func(t *testing.T) {
			d := new(recordedDocker)
			transport := transportFixture(t, d, true)
			baseline := transport.baselineHandle.Name()
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(t.TempDir(), filepath.Join(baseline, "unsafe"))
			case "fifo":
				err = syscall.Mkfifo(filepath.Join(baseline, "unsafe"), 0600)
			case "git":
				err = os.Mkdir(filepath.Join(baseline, ".git"), 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := transport.Start(context.Background()); err == nil {
				t.Fatal("accepted unsafe baseline contents")
			}
		})
	}
}
