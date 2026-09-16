package release

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestBuilderProducesDeterministicClosedPlatformRelease(t *testing.T) {
	root := fixtureRoot(t)
	goExecutable := fakeGo(t)
	first, second := t.TempDir(), t.TempDir()
	builder := Builder{GoExecutable: goExecutable, Repository: "example/release"}
	manifest, err := builder.Build(context.Background(), root, first, "v1.2.3-alpha.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Build(context.Background(), root, second, "v1.2.3-alpha.1"); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 1 || len(manifest.Platforms) != 4 || strings.Join(manifest.NativeImage.Platforms, ",") != "linux-amd64,linux-arm64" {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	entries, _ := os.ReadDir(first)
	if len(entries) != 6 {
		t.Fatalf("got %d release assets", len(entries))
	}
	for _, entry := range entries {
		one, _ := os.ReadFile(filepath.Join(first, entry.Name()))
		two, _ := os.ReadFile(filepath.Join(second, entry.Name()))
		if !bytes.Equal(one, two) {
			t.Errorf("asset %s is not deterministic", entry.Name())
		}
	}
	for key, platform := range manifest.Platforms {
		if key != platform.OS+"-"+platform.Arch || platform.Archive.Name != "shipmunk-runner-v1.2.3-alpha.1-"+key+".tar" || !strings.Contains(platform.Archive.URL, "/example/release/releases/download/v1.2.3-alpha.1/") {
			t.Errorf("invalid platform entry %s: %#v", key, platform)
		}
		raw, _ := os.ReadFile(filepath.Join(first, platform.Archive.Name))
		digest := sha256.Sum256(raw)
		if int64(len(raw)) != platform.Archive.Size || hex.EncodeToString(digest[:]) != platform.Archive.SHA256 || len(raw) > MaxArchiveBytes {
			t.Errorf("archive metadata mismatch for %s", key)
		}
		verifyArchive(t, raw, platform.Files)
	}
	rawManifest, _ := os.ReadFile(filepath.Join(first, ManifestName))
	var decoded Manifest
	if json.Unmarshal(rawManifest, &decoded) != nil || len(decoded.Platforms) != 4 {
		t.Fatal("manifest is not valid JSON")
	}
	checksums, _ := os.ReadFile(filepath.Join(first, ChecksumsName))
	lines := strings.Split(strings.TrimSpace(string(checksums)), "\n")
	if len(lines) != 5 || !sort.StringsAreSorted(lines) || strings.Contains(string(checksums), ChecksumsName) {
		t.Fatalf("invalid checksum inventory: %s", checksums)
	}
}

func TestArchiveRejectsUnsafeDuplicateOversizeAndModes(t *testing.T) {
	tests := map[string][]inputFile{
		"empty":        {},
		"duplicate":    {{"file", []byte("a"), 0644}, {"file", []byte("b"), 0644}},
		"traversal":    {{"../file", []byte("a"), 0644}},
		"absolute":     {{"/file", []byte("a"), 0644}},
		"noncanonical": {{"dir/../file", []byte("a"), 0644}},
		"mode":         {{"file", []byte("a"), 0600}},
		"member size":  {{"file", make([]byte, MaxFileBytes+1), 0644}},
	}
	for name, inputs := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := makeArchive(inputs); err == nil {
				t.Fatal("accepted invalid archive inputs")
			}
		})
	}
	tooMany := make([]inputFile, MaxFilesPerArchive+1)
	for index := range tooMany {
		tooMany[index] = inputFile{path: "file-" + string(rune('a'+index)), data: []byte("x"), mode: 0644}
	}
	if _, _, err := makeArchive(tooMany); err == nil {
		t.Fatal("accepted too many files")
	}
}

func TestBuilderRejectsInvalidVersionDuplicatePlatformAndExistingAsset(t *testing.T) {
	root, builder := fixtureRoot(t), Builder{GoExecutable: fakeGo(t)}
	if _, err := builder.Build(context.Background(), root, t.TempDir(), "1.2.3"); err == nil {
		t.Fatal("accepted invalid version")
	}
	if _, err := builder.Build(context.Background(), root, t.TempDir(), "v1.2.3-alpha.01"); err == nil {
		t.Fatal("accepted a leading-zero numeric prerelease identifier")
	}
	builder.Platforms = []Platform{{"linux", "amd64"}, {"linux", "amd64"}}
	partialOutput := t.TempDir()
	if _, err := builder.Build(context.Background(), root, partialOutput, "v1.2.3"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate platform error = %v", err)
	}
	if entries, err := os.ReadDir(partialOutput); err != nil || len(entries) != 0 {
		t.Fatalf("failed build retained %d partial assets: %v", len(entries), err)
	}
	builder.Platforms = []Platform{{"windows", "amd64"}}
	if _, err := builder.Build(context.Background(), root, t.TempDir(), "v1.2.3"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported platform error = %v", err)
	}
	builder.Platforms = nil
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(output, "shipmunk-runner-v1.2.3-darwin-amd64.tar"), []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Build(context.Background(), root, output, "v1.2.3"); err == nil {
		t.Fatal("overwrote an existing release asset")
	}
}

func TestVersionLinkerContract(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shipmunk-runner", "shipmunk-watchdog"} {
		binary := filepath.Join(t.TempDir(), name)
		command := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-X github.com/ianrodrigues/shipmunk-runner/internal/command.Version=v9.8.7", "-o", binary, "./cmd/"+name)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v: %s", name, err, output)
		}
		output, err := exec.Command(binary, "--version").CombinedOutput()
		if err != nil || string(output) != name+" v9.8.7\n" {
			t.Fatalf("%s version output = %q, %v", name, output, err)
		}
	}
}

func TestReleaseBuildEnvironmentPinsBuildAffectingOptions(t *testing.T) {
	for _, name := range []string{"GOFLAGS", "GOWORK", "GOENV", "GOEXPERIMENT", "GOAMD64", "GOARM64", "GOFIPS140", "GOTOOLCHAIN"} {
		t.Setenv(name, "hostile")
	}
	environment := releaseBuildEnvironment(Platform{"linux", "amd64"})
	want := map[string]string{"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "amd64", "GOFLAGS": "", "GOWORK": "off", "GOENV": "off", "GOEXPERIMENT": "", "GOAMD64": "v1", "GOARM64": "v8.0", "GOFIPS140": "off", "GOTOOLCHAIN": "local"}
	got := map[string]string{}
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		if _, ok := want[name]; ok {
			got[name] = value
		}
	}
	for name, expected := range want {
		if value, ok := got[name]; !ok || value != expected {
			t.Errorf("%s = %q (present %t), want %q", name, value, ok, expected)
		}
	}
}

func verifyArchive(t *testing.T, raw []byte, inventory []File) {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(raw))
	seen := make([]File, 0, len(inventory))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		seen = append(seen, File{header.Name, hex.EncodeToString(digest[:]), header.Size, formatMode(header.Mode)})
		if header.Typeflag != tar.TypeReg || header.Uid != 0 || header.Gid != 0 || !header.ModTime.Equal(header.ModTime.UTC()) || header.ModTime.Unix() != 0 {
			t.Errorf("noncanonical header for %s", header.Name)
		}
	}
	if len(seen) != len(inventory) {
		t.Fatalf("inventory length %d != %d", len(seen), len(inventory))
	}
	for index := range seen {
		if seen[index] != inventory[index] {
			t.Errorf("inventory[%d] = %#v, want %#v", index, seen[index], inventory[index])
		}
	}
}

func formatMode(mode int64) string {
	if mode == 0755 {
		return "0755"
	}
	return "0644"
}

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		"runner/LICENSE": "license\n", "runner/containers/Dockerfile": "FROM scratch\n", "runner/containers/Dockerfile.dockerignore": "**\n",
		"runner/containers/codex-mcp.mjs": "export {};\n", "runner/containers/codex-result.schema.json": "{}\n",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func fakeGo(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("release fixtures require a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "go")
	script := `#!/bin/sh
set -eu
destination=
previous=
target=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then destination=$argument; fi
  previous=$argument
  target=$argument
done
printf 'synthetic:%s:%s:%s\n' "$GOOS" "$GOARCH" "$target" > "$destination"
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
