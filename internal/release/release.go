package release

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	ManifestName       = "runner-release.json"
	ChecksumsName      = "SHA256SUMS"
	MaxArchiveBytes    = 128 << 20
	MaxFileBytes       = 32 << 20
	MaxFilesPerArchive = 16
)

var versionPattern = regexp.MustCompile(`^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

type Platform struct{ OS, Arch string }

var supportedPlatforms = []Platform{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}}

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode"`
}

type Artifact struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type PlatformRelease struct {
	OS      string   `json:"os"`
	Arch    string   `json:"arch"`
	Archive Artifact `json:"archive"`
	Files   []File   `json:"files"`
}

type Manifest struct {
	SchemaVersion int                        `json:"schema_version"`
	Version       string                     `json:"version"`
	Platforms     map[string]PlatformRelease `json:"platforms"`
	NativeImage   struct {
		Platforms []string `json:"platforms"`
	} `json:"native_image"`
}

type Builder struct {
	GoExecutable string
	Repository   string
	Platforms    []Platform
}

func (b Builder) Build(ctx context.Context, root, output, version string) (manifest Manifest, returnedErr error) {
	created := make([]string, 0, len(supportedPlatforms)+2)
	defer func() {
		if returnedErr != nil {
			for _, path := range created {
				_ = os.Remove(path)
			}
		}
	}()
	if !versionPattern.MatchString(version) {
		return Manifest{}, errors.New("release version must be a semantic version tag beginning with v")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Manifest{}, errors.New("release root is invalid")
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr != nil || resolved != root {
		return Manifest{}, errors.New("release root must be canonical")
	}
	if err := os.MkdirAll(output, 0755); err != nil {
		return Manifest{}, fmt.Errorf("create release output: %w", err)
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return Manifest{}, errors.New("release output is invalid")
	}
	outputInfo, statErr := os.Lstat(output)
	if statErr != nil || !outputInfo.IsDir() || outputInfo.Mode()&os.ModeSymlink != 0 {
		return Manifest{}, errors.New("release output must be a regular directory")
	}
	goBin := b.GoExecutable
	if goBin == "" {
		goBin = "go"
	}
	repository := b.Repository
	if repository == "" {
		repository = "ianrodrigues/shipmunk-runner"
	}
	manifest = Manifest{SchemaVersion: 1, Version: version, Platforms: make(map[string]PlatformRelease)}
	manifest.NativeImage.Platforms = []string{"linux-amd64", "linux-arm64"}
	platforms := b.Platforms
	if platforms == nil {
		platforms = supportedPlatforms
	}
	if len(platforms) == 0 {
		return Manifest{}, errors.New("release platform list is empty")
	}
	temporary, err := os.MkdirTemp("", "shipmunk-release-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(temporary)
	for _, platform := range platforms {
		key := platform.OS + "-" + platform.Arch
		if !supportedPlatform(platform) {
			return Manifest{}, fmt.Errorf("unsupported release platform %s", key)
		}
		if _, exists := manifest.Platforms[key]; exists {
			return Manifest{}, errors.New("duplicate release platform")
		}
		files, buildErr := b.buildPlatform(ctx, goBin, root, temporary, version, platform)
		if buildErr != nil {
			return Manifest{}, buildErr
		}
		archive, archiveFiles, archiveErr := makeArchive(files)
		if archiveErr != nil {
			return Manifest{}, archiveErr
		}
		name := "shipmunk-runner-" + version + "-" + key + ".tar"
		assetPath := filepath.Join(output, name)
		if err := writeExclusive(assetPath, archive, 0644); err != nil {
			return Manifest{}, err
		}
		created = append(created, assetPath)
		digest := sha256.Sum256(archive)
		manifest.Platforms[key] = PlatformRelease{OS: platform.OS, Arch: platform.Arch, Archive: Artifact{Name: name, URL: "https://github.com/" + repository + "/releases/download/" + version + "/" + name, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(archive))}, Files: archiveFiles}
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	raw = append(raw, '\n')
	manifestPath := filepath.Join(output, ManifestName)
	if err := writeExclusive(manifestPath, raw, 0644); err != nil {
		return Manifest{}, err
	}
	created = append(created, manifestPath)
	checksums := make([]string, 0, len(manifest.Platforms)+1)
	for _, entry := range manifest.Platforms {
		checksums = append(checksums, entry.Archive.SHA256+"  "+entry.Archive.Name)
	}
	digest := sha256.Sum256(raw)
	checksums = append(checksums, hex.EncodeToString(digest[:])+"  "+ManifestName)
	sort.Strings(checksums)
	checksumsPath := filepath.Join(output, ChecksumsName)
	if err := writeExclusive(checksumsPath, []byte(strings.Join(checksums, "\n")+"\n"), 0644); err != nil {
		return Manifest{}, err
	}
	created = append(created, checksumsPath)
	return manifest, nil
}

func supportedPlatform(candidate Platform) bool {
	for _, platform := range supportedPlatforms {
		if candidate == platform {
			return true
		}
	}
	return false
}

type inputFile struct {
	path string
	data []byte
	mode int64
}

func (b Builder) buildPlatform(ctx context.Context, goBin, root, temporary, version string, platform Platform) ([]inputFile, error) {
	commands := []string{"shipmunk-runner", "shipmunk-watchdog"}
	files := make([]inputFile, 0, len(commands)+5)
	for _, name := range commands {
		destination := filepath.Join(temporary, platform.OS+"-"+platform.Arch+"-"+name)
		command := exec.CommandContext(ctx, goBin, "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w -X github.com/ianrodrigues/shipmunk-runner/internal/command.Version="+version, "-o", destination, "./cmd/"+name)
		command.Dir = root
		command.Env = releaseBuildEnvironment(platform)
		if output, err := command.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("build %s for %s/%s: %w: %s", name, platform.OS, platform.Arch, err, strings.TrimSpace(string(output)))
		}
		data, err := os.ReadFile(destination)
		if err != nil {
			return nil, err
		}
		files = append(files, inputFile{"bin/" + name, data, 0755})
	}
	for _, path := range []string{"runner/LICENSE", "runner/containers/Dockerfile", "runner/containers/Dockerfile.dockerignore", "runner/containers/codex-mcp.mjs", "runner/containers/codex-result.schema.json"} {
		source := filepath.Join(root, path)
		resolved, resolveErr := filepath.EvalSymlinks(source)
		info, err := os.Lstat(source)
		if resolveErr != nil || resolved != source || err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || sysNlink(info) != 1 {
			return nil, fmt.Errorf("release input %s is not a regular unlinked file", path)
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return nil, err
		}
		files = append(files, inputFile{strings.TrimPrefix(path, "runner/"), data, 0644})
	}
	return files, nil
}

func releaseBuildEnvironment(platform Platform) []string {
	controlled := map[string]bool{
		"CGO_ENABLED": true, "GOOS": true, "GOARCH": true, "GOFLAGS": true,
		"GOWORK": true, "GOENV": true, "GOEXPERIMENT": true, "GOAMD64": true, "GOARM64": true,
		"GOFIPS140": true, "GOTOOLCHAIN": true,
	}
	environment := make([]string, 0, len(os.Environ())+9)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !controlled[name] {
			environment = append(environment, entry)
		}
	}
	return append(environment,
		"CGO_ENABLED=0", "GOOS="+platform.OS, "GOARCH="+platform.Arch,
		"GOFLAGS=", "GOWORK=off", "GOENV=off", "GOEXPERIMENT=", "GOAMD64=v1", "GOARM64=v8.0",
		"GOFIPS140=off", "GOTOOLCHAIN=local",
	)
}

func makeArchive(inputs []inputFile) ([]byte, []File, error) {
	if len(inputs) == 0 || len(inputs) > MaxFilesPerArchive {
		return nil, nil, errors.New("release archive file count exceeds its limit")
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].path < inputs[j].path })
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	inventory := make([]File, 0, len(inputs))
	seen := map[string]bool{}
	for _, input := range inputs {
		clean := filepath.ToSlash(filepath.Clean(input.path))
		if seen[input.path] || clean != input.path || input.path == "." || strings.HasPrefix(input.path, "/") || strings.HasPrefix(input.path, "../") || len(input.path) > 160 {
			return nil, nil, errors.New("release archive contains an unsafe or duplicate path")
		}
		if input.mode != 0644 && input.mode != 0755 {
			return nil, nil, errors.New("release archive contains an invalid mode")
		}
		if len(input.data) > MaxFileBytes {
			return nil, nil, errors.New("release archive member exceeds its limit")
		}
		seen[input.path] = true
		header := &tar.Header{Name: input.path, Mode: input.mode, Size: int64(len(input.data)), ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
		if err := writer.WriteHeader(header); err != nil {
			return nil, nil, err
		}
		if _, err := writer.Write(input.data); err != nil {
			return nil, nil, err
		}
		digest := sha256.Sum256(input.data)
		inventory = append(inventory, File{input.path, hex.EncodeToString(digest[:]), int64(len(input.data)), fmt.Sprintf("%04o", input.mode)})
	}
	if err := writer.Close(); err != nil {
		return nil, nil, err
	}
	if buffer.Len() > MaxArchiveBytes {
		return nil, nil, errors.New("release archive exceeds its limit")
	}
	return buffer.Bytes(), inventory, nil
}

func writeExclusive(path string, contents []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create release asset %s: %w", filepath.Base(path), err)
	}
	_, writeErr := io.Copy(file, bytes.NewReader(contents))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
