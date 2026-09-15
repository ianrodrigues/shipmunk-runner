package workspace

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

func TestPrepareGitSourceArchivesAndTrustedInstructions(t *testing.T) {
	archives := readGitArchives(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	manager, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureWritable(); err != nil {
		t.Fatal(err)
	}
	client := &fakeDownloader{artifacts: map[string][]byte{
		"01k4w000000000000000000003": archives["short"],
		"01k4w000000000000000000004": archives["long"],
		"01k4w000000000000000000005": []byte(`{"instructions":"Keep the review focused."}`),
	}}
	claim := fixtureClaim(map[string]any{
		"source_artifacts": []any{
			artifactReferenceMap("01k4w000000000000000000003", archives["short"]),
			artifactReferenceMap("01k4w000000000000000000004", archives["long"]),
		},
		"instruction_artifacts": []any{artifactReferenceMap("01k4w000000000000000000005", client.artifacts["01k4w000000000000000000005"])},
		"target_sha":            strings.Repeat("b", 40),
		"diff_base_sha":         "f2e2ca336a3c496a4643919b8ecccc367bda857b",
		"head_sha":              "637e6ee9813bac7ee840f57417193897d930776a",
	})
	path, err := manager.Prepare(context.Background(), claim, client)
	if err != nil {
		t.Fatal(err)
	}
	if path != manager.Path(claim) {
		t.Fatalf("prepared path %q does not match the fenced path", path)
	}
	assertFileContents(t, filepath.Join(path, "sources", "0", "src", "index.txt"), "fixture\n")
	assertFileContents(t, filepath.Join(path, "sources", "1", strings.Repeat("x", 120)), "long path\n")
	assertFileContents(t, filepath.Join(path, "instructions", "0.json"), `{"instructions":"Keep the review focused."}`)
	assertMode(t, filepath.Join(path, "sources", "0", "run.sh"), 0700)
	assertMode(t, filepath.Join(path, "sources", "0", "src", "index.txt"), 0600)
	if got := client.calls; got != 3 {
		t.Fatalf("expected three downloads, got %d", got)
	}
	if err := manager.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("attempt workspace still exists: %v", err)
	}
}

func TestExtractGitArchiveVariants(t *testing.T) {
	archives := readGitArchives(t)
	for _, test := range []struct {
		name     string
		revision string
		relative string
		want     string
		wantMode os.FileMode
	}{
		{name: "full", revision: "f2e2ca336a3c496a4643919b8ecccc367bda857b", relative: "run.sh", want: "#!/bin/sh\nprintf fixture\\n\n", wantMode: 0700},
		{name: "short", revision: "f2e2ca336a3c496a4643919b8ecccc367bda857b", relative: "src/index.txt", want: "fixture\n", wantMode: 0600},
		{name: "executable", revision: "f2e2ca336a3c496a4643919b8ecccc367bda857b", relative: "run.sh", want: "#!/bin/sh\nprintf fixture\\n\n", wantMode: 0700},
		{name: "subtree", revision: "f2e2ca336a3c496a4643919b8ecccc367bda857b", relative: "src/index.txt", want: "fixture\n", wantMode: 0600},
		{name: "long", revision: "637e6ee9813bac7ee840f57417193897d930776a", relative: strings.Repeat("x", 120), want: "long path\n", wantMode: 0600},
	} {
		t.Run(test.name, func(t *testing.T) {
			destination := t.TempDir()
			if err := (TarExtractor{}).Extract(context.Background(), archives[test.name], destination); err != nil {
				t.Fatal(err)
			}
			if err := unwrapGitHubSnapshot(context.Background(), destination, test.revision); err != nil {
				t.Fatal(err)
			}
			assertFileContents(t, filepath.Join(destination, test.relative), test.want)
			assertMode(t, filepath.Join(destination, test.relative), test.wantMode)
		})
	}
}

func TestSafeTarRejectsHostileEntriesAndLimits(t *testing.T) {
	for _, test := range []struct {
		name    string
		archive []byte
		limits  TarExtractor
	}{
		{name: "PAX traversal", archive: paxEntry("x", "path", "../outside")},
		{name: "absolute PAX path", archive: paxEntry("x", "path", "/outside")},
		{name: "backslash path", archive: paxEntry("x", "path", `safe\\outside`)},
		{name: "unsupported global path", archive: paxEntry("g", "path", "outside")},
		{name: "unsupported link metadata", archive: paxEntry("x", "linkpath", "outside")},
		{name: "duplicate PAX path", archive: joinTar(paxHeader("x", paxRecord("path", "first")), paxHeader("x", paxRecord("path", "second")), tarEntry("placeholder", "unsafe", '0', 0644), tarEnd())},
		{name: "link type", archive: joinTar(tarEntry("link", "", '2', 0644), tarEnd())},
		{name: "file with trailing slash", archive: joinTar(tarEntry("file/", "unsafe", '0', 0644), tarEnd())},
		{name: "directory with empty segment", archive: joinTar(tarEntry("dir//", "", '5', 0755), tarEnd())},
		{name: "too many entries", archive: joinTar(tarEntry("one", "1", '0', 0644), tarEntry("two", "2", '0', 0644), tarEnd()), limits: TarExtractor{MaxFiles: 1}},
		{name: "too many bytes", archive: joinTar(tarEntry("large", "1234", '0', 0644), tarEnd()), limits: TarExtractor{MaxBytes: 3}},
		{name: "bad checksum", archive: corruptChecksum(joinTar(tarEntry("file", "x", '0', 0644), tarEnd()))},
		{name: "truncated file", archive: tarEntry("file", "x", '0', 0644)[:512]},
		{name: "missing end marker", archive: tarEntry("file", "x", '0', 0644)},
		{name: "bad PAX length", archive: paxRawEntry("x", []byte("0 path=safe\n"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			destination := t.TempDir()
			err := test.limits.Extract(context.Background(), test.archive, destination)
			if err == nil {
				t.Fatal("accepted an unsafe archive")
			}
			entries, readErr := os.ReadDir(destination)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if entry.Name() == "outside" || entry.Type()&os.ModeSymlink != 0 {
					t.Fatalf("rejected archive escaped the destination: %#v", entries)
				}
			}
		})
	}
}

func TestSafeTarBoundsGzipExpansionAndHonorsCancellation(t *testing.T) {
	archive := gzipBytes(t, joinTar(tarEntry("large", strings.Repeat("x", 2048), '0', 0644), tarEnd()))
	destination := t.TempDir()
	if err := (TarExtractor{MaxBytes: 1024}).Extract(context.Background(), archive, destination); err == nil {
		t.Fatal("accepted a gzip archive beyond the decoded byte limit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (TarExtractor{}).Extract(ctx, joinTar(tarEntry("file", "data", '0', 0644), tarEnd()), destination); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestPrepareCancellationAndInvalidInstructionsCleanWorkspace(t *testing.T) {
	archive := joinTar(tarEntry("file", "data", '0', 0644), tarEnd())
	for _, test := range []struct {
		name         string
		instructions []byte
		cancel       bool
	}{
		{name: "cancel", instructions: []byte(`{}`), cancel: true},
		{name: "invalid instructions", instructions: []byte(`[]`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			client := &fakeDownloader{artifacts: map[string][]byte{
				"01k4w000000000000000000003": archive,
				"01k4w000000000000000000004": test.instructions,
			}}
			claim := fixtureClaim(map[string]any{
				"source_artifacts":      []any{artifactReferenceMap("01k4w000000000000000000003", archive)},
				"instruction_artifacts": []any{artifactReferenceMap("01k4w000000000000000000004", test.instructions)},
			})
			ctx := context.Background()
			if test.cancel {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			if _, err := manager.Prepare(ctx, claim, client); err == nil {
				t.Fatal("preparation unexpectedly succeeded")
			}
			if _, err := os.Lstat(manager.Path(claim)); !os.IsNotExist(err) {
				t.Fatalf("failed preparation left workspace behind: %v", err)
			}
		})
	}
}

func TestRemoveRequiresDirectAttemptPathAndRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	manager, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	claim := fixtureClaim(map[string]any{})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "preserve.txt"), []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := manager.Remove(outside); err == nil {
		t.Fatal("removed a directory outside the configured root")
	}
	linkPath := manager.Path(claim)
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := manager.Remove(linkPath); err == nil {
		t.Fatal("removed a symlinked attempt workspace")
	}
	if err := os.Remove(linkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(linkPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "preserve.txt"), filepath.Join(linkPath, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := manager.Remove(linkPath); err == nil {
		t.Fatal("removed an attempt workspace containing a symlink")
	}
	assertFileContents(t, filepath.Join(outside, "preserve.txt"), "preserved")
	if err := manager.Remove(filepath.Join(root, "unrelated")); err == nil {
		t.Fatal("accepted a non-attempt workspace name")
	}
}

func TestSanitizeRecursivelyRemovesForbiddenKeysWithoutMutatingInput(t *testing.T) {
	input := map[string]any{
		"task_context": "safe",
		"SUPERvisor":   map[string]any{"Authorization": "secret", "visible": "kept"},
		"nested": []any{
			map[string]any{"callback": "private", "kept": true},
			map[string]any{"child": map[string]any{"GitHub_Token": "secret", "database_url": "secret"}},
		},
	}
	clean := Sanitize(input)
	if _, exists := clean["SUPERvisor"]; exists {
		t.Fatal("retained a forbidden top-level key")
	}
	nested := clean["nested"].([]any)
	if _, exists := nested[0].(map[string]any)["callback"]; exists {
		t.Fatal("retained a forbidden nested key")
	}
	child := nested[1].(map[string]any)["child"].(map[string]any)
	if len(child) != 0 {
		t.Fatalf("retained nested credentials: %#v", child)
	}
	if _, exists := input["SUPERvisor"]; !exists {
		t.Fatal("mutated the input manifest")
	}
}

func TestConcurrentPrepareAllowsOnlyOneAttemptOwner(t *testing.T) {
	root := t.TempDir()
	manager, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	archive := joinTar(tarEntry("file", "data", '0', 0644), tarEnd())
	claim := fixtureClaim(map[string]any{
		"source_artifacts":      []any{artifactReferenceMap("01k4w000000000000000000003", archive)},
		"instruction_artifacts": []any{artifactReferenceMap("01k4w000000000000000000004", []byte(`{}`))},
	})
	client := &fakeDownloader{artifacts: map[string][]byte{
		"01k4w000000000000000000003": archive,
		"01k4w000000000000000000004": []byte(`{}`),
	}}
	var group sync.WaitGroup
	group.Add(2)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			defer group.Done()
			_, err := manager.Prepare(context.Background(), claim, client)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected one successful preparation, got %d", succeeded)
	}
}

type fakeDownloader struct {
	artifacts map[string][]byte
	calls     int
}

func (client *fakeDownloader) DownloadArtifact(ctx context.Context, _ protocol.Claim, artifactID, _ string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client.calls++
	data, ok := client.artifacts[artifactID]
	if !ok {
		return nil, fmt.Errorf("unknown fixture artifact")
	}
	return data, nil
}

func fixtureClaim(manifest map[string]any) protocol.Claim {
	return protocol.Claim{
		RunID:          "01kkkkkkkkkkkkkkkkkkkkkkkk",
		AttemptID:      "01nnnnnnnnnnnnnnnnnnnnnnnn",
		Fence:          1,
		LeaseExpiresAt: time.Now().Add(45 * time.Second),
		Deadline:       time.Now().Add(10 * time.Minute),
		Manifest:       manifest,
	}
}

func artifactReferenceMap(id string, data []byte) map[string]any {
	sum := sha256.Sum256(data)
	return map[string]any{"artifact_id": id, "sha256": hex.EncodeToString(sum[:])}
}

func readGitArchives(t *testing.T) map[string][]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "runner", "tests", "fixtures", "source-archive", "git-archives.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]string
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	archives := make(map[string][]byte)
	for _, name := range []string{"short", "full", "executable", "subtree", "long"} {
		archive, err := base64.StdEncoding.DecodeString(fixture[name])
		if err != nil {
			t.Fatal(err)
		}
		archives[name] = archive
	}
	return archives
}

func tarEntry(name, contents string, typeFlag byte, mode int) []byte {
	content := []byte(contents)
	header := make([]byte, 512)
	copy(header[0:100], []byte(name))
	copy(header[100:108], []byte(fmt.Sprintf("%07o\x00", mode)))
	copy(header[108:116], []byte("0017774\x00"))
	copy(header[116:124], []byte("0017774\x00"))
	copy(header[124:136], []byte(fmt.Sprintf("%011o\x00", len(content))))
	copy(header[136:148], []byte("00000000000\x00"))
	for index := 148; index < 156; index++ {
		header[index] = ' '
	}
	header[156] = typeFlag
	copy(header[257:263], []byte("ustar\x00"))
	copy(header[263:265], []byte("00"))
	var checksum int
	for _, value := range header {
		checksum += int(value)
	}
	copy(header[148:156], []byte(fmt.Sprintf("%06o\x00 ", checksum)))
	entry := append(header, content...)
	if padding := (512 - len(content)%512) % 512; padding > 0 {
		entry = append(entry, make([]byte, padding)...)
	}
	return entry
}

func paxRecord(key, value string) []byte {
	body := " " + key + "=" + value + "\n"
	length := len(body) + 1
	for {
		updated := len(strconv.Itoa(length)) + len(body)
		if updated == length {
			break
		}
		length = updated
	}
	return []byte(strconv.Itoa(length) + body)
}

func paxEntry(typeFlag string, key, value string) []byte {
	return paxRawEntry(typeFlag, paxRecord(key, value))
}

func paxRawEntry(typeFlag string, payload []byte) []byte {
	entry := tarEntry("extended", string(payload), typeFlag[0], 0644)
	return append(entry, tarEntry("placeholder", "unsafe", '0', 0644)...)
}

func paxHeader(typeFlag string, payload []byte) []byte {
	return tarEntry("extended", string(payload), typeFlag[0], 0644)
}

func joinTar(entries ...[]byte) []byte {
	var archive []byte
	for _, entry := range entries {
		archive = append(archive, entry...)
	}
	return archive
}

func tarEnd() []byte {
	return make([]byte, 1024)
}

func corruptChecksum(archive []byte) []byte {
	copyArchive := append([]byte(nil), archive...)
	copyArchive[0] ^= 1
	return copyArchive
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != expected {
		t.Fatalf("unexpected contents in %s: %q", path, actual)
	}
}

func assertMode(t *testing.T, path string, expected os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != expected {
		t.Fatalf("permissions for %s = %04o, want %04o", path, info.Mode().Perm(), expected)
	}
}
