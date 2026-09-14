package install

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	shipmunkrelease "github.com/ianrodrigues/shipmunk-runner/internal/release"
)

func TestSelectPlatformStrictlyBindsSupportedTuple(t *testing.T) {
	platform, _ := archiveFixture(t)
	manifest := manifestFixture(platform)
	selected, err := SelectPlatform(manifest, "linux", "amd64")
	if err != nil || selected.OS != "linux" || selected.Arch != "amd64" {
		t.Fatalf("selection = %#v, %v", selected, err)
	}
	if _, err := SelectPlatform(manifest, "windows", "amd64"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported error = %v", err)
	}
	delete(manifest.Platforms, "linux-amd64")
	if _, err := SelectPlatform(manifest, "linux", "amd64"); err == nil {
		t.Fatal("accepted incomplete platform manifest")
	}
	manifest = manifestFixture(platform)
	entry := manifest.Platforms["linux-amd64"]
	entry.Arch = "arm64"
	manifest.Platforms["linux-amd64"] = entry
	if _, err := SelectPlatform(manifest, "linux", "amd64"); err == nil {
		t.Fatal("accepted incorrectly bound platform")
	}
	manifest = manifestFixture(platform)
	invalid := manifest.Platforms["darwin-arm64"]
	invalid.Archive.SHA256 = "invalid"
	manifest.Platforms["darwin-arm64"] = invalid
	if _, err := SelectPlatform(manifest, "linux", "amd64"); err == nil {
		t.Fatal("accepted invalid unselected platform")
	}
	manifest = manifestFixture(platform)
	for _, version := range []string{"1.2.3", "v1.2.3-01"} {
		manifest.Version = version
		if _, err := SelectPlatform(manifest, "linux", "amd64"); err == nil {
			t.Fatalf("accepted noncanonical release version %q", version)
		}
	}
	manifest = manifestFixture(platform)
	manifest.Platforms["windows-amd64"] = manifest.Platforms["darwin-amd64"]
	delete(manifest.Platforms, "darwin-amd64")
	if _, err := SelectPlatform(manifest, "linux", "amd64"); err == nil {
		t.Fatal("accepted substituted platform tuple")
	}
}

func TestParseManifestRejectsDuplicateAndUnknownFields(t *testing.T) {
	platform, _ := archiveFixture(t)
	raw, _ := json.Marshal(manifestFixture(platform))
	if _, err := ParseManifest(raw); err != nil {
		t.Fatal(err)
	}
	duplicate := bytes.Replace(raw, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1)
	if _, err := ParseManifest(duplicate); err == nil {
		t.Fatal("accepted duplicate manifest field")
	}
	unknown := bytes.Replace(raw, []byte(`"schema_version":1`), []byte(`"schema_version":1,"unknown":true`), 1)
	if _, err := ParseManifest(unknown); err == nil {
		t.Fatal("accepted unknown manifest field")
	}
}

func TestInstallActivatesPrivateContentAddressedRelease(t *testing.T) {
	platform, archive := archiveFixture(t)
	releases := canonicalTemp(t)
	destination, err := Install(bytes.NewReader(archive), releases, platform)
	if err != nil {
		t.Fatal(err)
	}
	if destination != filepath.Join(releases, platform.Archive.SHA256) {
		t.Fatalf("destination = %s", destination)
	}
	for _, file := range platform.Files {
		info, err := os.Stat(filepath.Join(destination, filepath.FromSlash(file.Path)))
		if err != nil || info.Mode().Perm() != os.FileMode(parseMode(file.Mode)) {
			t.Errorf("installed %s with mode %v: %v", file.Path, info.Mode(), err)
		}
	}
	if again, err := Install(bytes.NewReader(archive), releases, platform); err != nil || again != destination {
		t.Fatalf("idempotent install = %q, %v", again, err)
	}
	if err := os.WriteFile(filepath.Join(destination, platform.Files[0].Path), []byte("altered"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(bytes.NewReader(archive), releases, platform); err == nil || !strings.Contains(err.Error(), "altered") {
		t.Fatalf("altered release error = %v", err)
	}
}

func TestInstallRejectsArchiveDigestAndSizeBeforeCreatingReleaseRoot(t *testing.T) {
	platform, archive := archiveFixture(t)
	parent := canonicalTemp(t)
	releases := filepath.Join(parent, "releases")
	tampered := append([]byte(nil), archive...)
	tampered[513] ^= 1
	if _, err := Install(bytes.NewReader(tampered), releases, platform); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("digest error = %v", err)
	}
	if _, err := os.Stat(releases); !os.IsNotExist(err) {
		t.Fatal("created release root before archive verification")
	}
	platform.Archive.Size++
	if _, err := Install(bytes.NewReader(archive), releases, platform); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("size error = %v", err)
	}
}

func TestInstallRejectsMalformedClosedInventoryCases(t *testing.T) {
	platform, archive := archiveFixture(t)
	tests := map[string]func([]byte, shipmunkrelease.PlatformRelease) ([]byte, shipmunkrelease.PlatformRelease){
		"truncated": func(raw []byte, p shipmunkrelease.PlatformRelease) ([]byte, shipmunkrelease.PlatformRelease) {
			return raw[:len(raw)-1], p
		},
		"trailing": func(raw []byte, p shipmunkrelease.PlatformRelease) ([]byte, shipmunkrelease.PlatformRelease) {
			return append(raw, 0), p
		},
		"tampered member": func(raw []byte, p shipmunkrelease.PlatformRelease) ([]byte, shipmunkrelease.PlatformRelease) {
			raw[512] ^= 1
			return raw, p
		},
		"missing inventory": func(raw []byte, p shipmunkrelease.PlatformRelease) ([]byte, shipmunkrelease.PlatformRelease) {
			p.Files = p.Files[:1]
			return raw, p
		},
		"extra inventory": func(raw []byte, p shipmunkrelease.PlatformRelease) ([]byte, shipmunkrelease.PlatformRelease) {
			p.Files = append(p.Files, shipmunkrelease.File{Path: "extra", SHA256: strings.Repeat("a", 64), Mode: "0644"})
			return raw, p
		},
		"wrong mode": func(raw []byte, p shipmunkrelease.PlatformRelease) ([]byte, shipmunkrelease.PlatformRelease) {
			p.Files[0].Mode = "0644"
			return raw, p
		},
		"traversal": func(raw []byte, p shipmunkrelease.PlatformRelease) ([]byte, shipmunkrelease.PlatformRelease) {
			p.Files[0].Path = "../escape"
			return raw, p
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			raw := append([]byte(nil), archive...)
			p := platform
			p.Files = append([]shipmunkrelease.File(nil), platform.Files...)
			raw, p = mutate(raw, p)
			bindArchive(&p, raw)
			if _, err := Install(bytes.NewReader(raw), filepath.Join(canonicalTemp(t), "releases"), p); err == nil {
				t.Fatal("accepted malformed release")
			}
		})
	}
}

func TestArchiveRejectsLinksSpecialDuplicatesAndNoncanonicalHeaders(t *testing.T) {
	for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeDir} {
		t.Run(string([]byte{kind}), func(t *testing.T) {
			platform, raw := customArchive(t, []archiveEntry{{"bin/tool", []byte("one"), 0755, kind}, {"share/data", []byte("two"), 0644, tar.TypeReg}})
			if _, err := Install(bytes.NewReader(raw), filepath.Join(canonicalTemp(t), "releases"), platform); err == nil {
				t.Fatal("accepted non-regular member")
			}
		})
	}
	platform, raw := customArchive(t, []archiveEntry{{"bin/tool", []byte("one"), 0755, tar.TypeReg}, {"bin/tool", []byte("one"), 0755, tar.TypeReg}})
	if _, err := Install(bytes.NewReader(raw), filepath.Join(canonicalTemp(t), "releases"), platform); err == nil {
		t.Fatal("accepted duplicate member")
	}
}

type archiveEntry struct {
	path string
	data []byte
	mode int64
	kind byte
}

func archiveFixture(t *testing.T) (shipmunkrelease.PlatformRelease, []byte) {
	return customArchive(t, []archiveEntry{{"bin/tool", []byte("executable\n"), 0755, tar.TypeReg}, {"share/data", []byte("data\n"), 0644, tar.TypeReg}})
}

func customArchive(t *testing.T, entries []archiveEntry) (shipmunkrelease.PlatformRelease, []byte) {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	files := make([]shipmunkrelease.File, 0, len(entries))
	for _, entry := range entries {
		header := &tar.Header{Name: entry.path, Mode: entry.mode, Size: int64(len(entry.data)), ModTime: time.Unix(0, 0).UTC(), Typeflag: entry.kind, Format: tar.FormatUSTAR}
		if entry.kind != tar.TypeReg {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := writer.Write(entry.data); err != nil {
				t.Fatal(err)
			}
		}
		digest := sha256.Sum256(entry.data)
		files = append(files, shipmunkrelease.File{Path: entry.path, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(entry.data)), Mode: formatMode(entry.mode)})
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	platform := shipmunkrelease.PlatformRelease{OS: "linux", Arch: "amd64", Files: files, Archive: shipmunkrelease.Artifact{Name: "fixture.tar"}}
	bindArchive(&platform, output.Bytes())
	return platform, output.Bytes()
}

func bindArchive(platform *shipmunkrelease.PlatformRelease, raw []byte) {
	digest := sha256.Sum256(raw)
	platform.Archive.SHA256 = hex.EncodeToString(digest[:])
	platform.Archive.Size = int64(len(raw))
}
func formatMode(mode int64) string {
	if mode == 0755 {
		return "0755"
	}
	return "0644"
}

func manifestFixture(platform shipmunkrelease.PlatformRelease) shipmunkrelease.Manifest {
	manifest := shipmunkrelease.Manifest{SchemaVersion: 1, Version: "v1.2.3", Platforms: map[string]shipmunkrelease.PlatformRelease{}}
	for _, tuple := range []struct{ os, arch string }{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		copy := platform
		copy.OS = tuple.os
		copy.Arch = tuple.arch
		manifest.Platforms[tuple.os+"-"+tuple.arch] = copy
	}
	manifest.NativeImage.Platforms = []string{"linux-amd64", "linux-arm64"}
	return manifest
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
