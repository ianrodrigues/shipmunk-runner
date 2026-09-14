package install

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	shipmunkrelease "github.com/ianrodrigues/shipmunk-runner/internal/release"
)

const maxManifestBytes = 128 << 10

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ParseManifest(raw []byte) (shipmunkrelease.Manifest, error) {
	value, err := protocol.Decode(raw, maxManifestBytes)
	root, ok := value.(map[string]any)
	if err != nil || !ok || !exactKeys(root, "schema_version", "version", "platforms", "native_image") {
		return shipmunkrelease.Manifest{}, errors.New("runner release manifest is invalid")
	}
	platforms, ok := root["platforms"].(map[string]any)
	if !ok {
		return shipmunkrelease.Manifest{}, errors.New("runner release manifest is invalid")
	}
	for _, rawPlatform := range platforms {
		platform, ok := rawPlatform.(map[string]any)
		if !ok || !exactKeys(platform, "os", "arch", "archive", "files") {
			return shipmunkrelease.Manifest{}, errors.New("runner release manifest is invalid")
		}
		artifact, ok := platform["archive"].(map[string]any)
		files, filesOK := platform["files"].([]any)
		if !ok || !filesOK || !exactKeys(artifact, "name", "url", "sha256", "size") {
			return shipmunkrelease.Manifest{}, errors.New("runner release manifest is invalid")
		}
		for _, rawFile := range files {
			file, ok := rawFile.(map[string]any)
			if !ok || !exactKeys(file, "path", "sha256", "size", "mode") {
				return shipmunkrelease.Manifest{}, errors.New("runner release manifest is invalid")
			}
		}
	}
	native, ok := root["native_image"].(map[string]any)
	if !ok || !exactKeys(native, "platforms") {
		return shipmunkrelease.Manifest{}, errors.New("runner release manifest is invalid")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return shipmunkrelease.Manifest{}, errors.New("runner release manifest is invalid")
	}
	var manifest shipmunkrelease.Manifest
	if err := json.Unmarshal(canonical, &manifest); err != nil {
		return shipmunkrelease.Manifest{}, errors.New("runner release manifest is invalid")
	}
	return manifest, nil
}

func exactKeys(value map[string]any, expected ...string) bool {
	if len(value) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, ok := value[key]; !ok {
			return false
		}
	}
	return true
}

func SelectPlatform(manifest shipmunkrelease.Manifest, goos, goarch string) (shipmunkrelease.PlatformRelease, error) {
	key := goos + "-" + goarch
	if key != "linux-amd64" && key != "linux-arm64" && key != "darwin-amd64" && key != "darwin-arm64" {
		return shipmunkrelease.PlatformRelease{}, fmt.Errorf("unsupported runner platform %s", key)
	}
	if manifest.SchemaVersion != 1 || !releaseVersionPattern.MatchString(manifest.Version) || len(manifest.Platforms) != 4 {
		return shipmunkrelease.PlatformRelease{}, errors.New("runner release manifest is invalid")
	}
	if len(manifest.NativeImage.Platforms) != 2 || manifest.NativeImage.Platforms[0] != "linux-amd64" || manifest.NativeImage.Platforms[1] != "linux-arm64" {
		return shipmunkrelease.PlatformRelease{}, errors.New("runner native image platforms are invalid")
	}
	for _, manifestKey := range []string{"darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"} {
		platform, ok := manifest.Platforms[manifestKey]
		if !ok || manifestKey != platform.OS+"-"+platform.Arch {
			return shipmunkrelease.PlatformRelease{}, errors.New("runner release platform binding is invalid")
		}
		if err := validatePlatform(platform); err != nil {
			return shipmunkrelease.PlatformRelease{}, err
		}
	}
	platform, ok := manifest.Platforms[key]
	if !ok {
		return shipmunkrelease.PlatformRelease{}, fmt.Errorf("runner release does not support platform %s", key)
	}
	return platform, nil
}

func validatePlatform(platform shipmunkrelease.PlatformRelease) error {
	artifact := platform.Archive
	if artifact.Name == "" || artifact.Size < 1024 || artifact.Size > shipmunkrelease.MaxArchiveBytes || !digestPattern.MatchString(artifact.SHA256) {
		return errors.New("runner release archive metadata is invalid")
	}
	if len(platform.Files) == 0 || len(platform.Files) > shipmunkrelease.MaxFilesPerArchive {
		return errors.New("runner release inventory exceeds its limit")
	}
	seen := make(map[string]bool, len(platform.Files))
	previous := ""
	for _, file := range platform.Files {
		if seen[file.Path] || (previous != "" && file.Path <= previous) || !safePath(file.Path) || file.Size < 0 || file.Size > shipmunkrelease.MaxFileBytes || !digestPattern.MatchString(file.SHA256) || (file.Mode != "0644" && file.Mode != "0755") {
			return errors.New("runner release inventory is invalid")
		}
		seen[file.Path] = true
		previous = file.Path
	}
	return nil
}

// Install verifies the complete archive before writing and atomically activates
// it under releases/<archive-sha256>. Existing releases are never repaired.
func Install(archive io.Reader, releases string, platform shipmunkrelease.PlatformRelease) (string, error) {
	return installRelease(archive, releases, platform, syncDirectory)
}

func installRelease(archive io.Reader, releases string, platform shipmunkrelease.PlatformRelease, syncDir func(string) error) (string, error) {
	if err := validatePlatform(platform); err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(archive, shipmunkrelease.MaxArchiveBytes+1))
	if err != nil || len(raw) > shipmunkrelease.MaxArchiveBytes || int64(len(raw)) != platform.Archive.Size {
		return "", errors.New("runner release archive size is invalid")
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != platform.Archive.SHA256 {
		return "", errors.New("runner release archive digest is invalid")
	}
	files, err := readArchive(raw, platform.Files)
	if err != nil {
		return "", err
	}
	if err := ensurePrivateDirectory(releases); err != nil {
		return "", err
	}
	destination := filepath.Join(releases, platform.Archive.SHA256)
	if _, err := os.Lstat(destination); err == nil {
		if err := verifyRelease(destination, platform.Files); err != nil {
			return "", errors.New("existing runner release was altered")
		}
		if err := syncDirectory(releases); err != nil {
			return "", err
		}
		return destination, nil
	} else if !os.IsNotExist(err) {
		return "", errors.New("cannot inspect runner release destination")
	}
	staging, err := os.MkdirTemp(releases, ".staging-")
	if err != nil {
		return "", errors.New("cannot create private runner staging")
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0700); err != nil {
		return "", err
	}
	directories := map[string]bool{staging: true}
	for index, file := range platform.Files {
		path := filepath.Join(staging, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", err
		}
		for directory := filepath.Dir(path); directory != staging; directory = filepath.Dir(directory) {
			directories[directory] = true
		}
		mode := os.FileMode(0644)
		if file.Mode == "0755" {
			mode = 0755
		}
		handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return "", err
		}
		if err := handle.Chmod(mode); err != nil {
			_ = handle.Close()
			return "", err
		}
		_, writeErr := handle.Write(files[index])
		syncErr := handle.Sync()
		closeErr := handle.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return "", err
		}
	}
	if err := verifyRelease(staging, platform.Files); err != nil {
		return "", err
	}
	ordered := make([]string, 0, len(directories))
	for directory := range directories {
		ordered = append(ordered, directory)
	}
	sort.Slice(ordered, func(left, right int) bool {
		return strings.Count(ordered[left], string(filepath.Separator)) > strings.Count(ordered[right], string(filepath.Separator))
	})
	for _, directory := range ordered {
		if err := syncDir(directory); err != nil {
			return "", err
		}
	}
	if err := os.Rename(staging, destination); err != nil {
		if _, statErr := os.Lstat(destination); statErr == nil && verifyRelease(destination, platform.Files) == nil {
			return destination, nil
		}
		return "", errors.New("cannot activate runner release")
	}
	if err := syncDir(releases); err != nil {
		return "", err
	}
	return destination, nil
}

func readArchive(raw []byte, inventory []shipmunkrelease.File) ([][]byte, error) {
	reader := tar.NewReader(bytes.NewReader(raw))
	contents := make([][]byte, 0, len(inventory))
	for index := 0; ; index++ {
		header, err := reader.Next()
		if err == io.EOF {
			if index != len(inventory) {
				return nil, errors.New("runner release archive is missing files")
			}
			break
		}
		if err != nil {
			return nil, errors.New("runner release archive is truncated or malformed")
		}
		if index >= len(inventory) {
			return nil, errors.New("runner release archive contains extra files")
		}
		expected := inventory[index]
		if header.Name != expected.Path || header.Typeflag != tar.TypeReg || header.Format != tar.FormatUSTAR || header.Mode != parseMode(expected.Mode) || header.Size != expected.Size || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || header.ModTime.Unix() != 0 || !header.AccessTime.IsZero() || !header.ChangeTime.IsZero() {
			return nil, errors.New("runner release archive header is noncanonical")
		}
		data, err := io.ReadAll(io.LimitReader(reader, shipmunkrelease.MaxFileBytes+1))
		if err != nil || int64(len(data)) != expected.Size {
			return nil, errors.New("runner release archive member size is invalid")
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != expected.SHA256 {
			return nil, errors.New("runner release archive member digest is invalid")
		}
		contents = append(contents, data)
	}
	canonical, err := canonicalArchive(inventory, contents)
	if err != nil || !bytes.Equal(canonical, raw) {
		return nil, errors.New("runner release archive framing is noncanonical")
	}
	return contents, nil
}

func canonicalArchive(inventory []shipmunkrelease.File, contents [][]byte) ([]byte, error) {
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for index, file := range inventory {
		header := &tar.Header{Name: file.Path, Mode: parseMode(file.Mode), Size: file.Size, ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
		if err := writer.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := writer.Write(contents[index]); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func verifyRelease(root string, inventory []shipmunkrelease.File) error {
	if err := protect(root, true, 0700); err != nil {
		return err
	}
	seen := make(map[string]bool, len(inventory))
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("runner release contains a link")
		}
		if entry.IsDir() {
			return protect(path, true, 0700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("runner release contains a special file")
		}
		relative = filepath.ToSlash(relative)
		index := sort.Search(len(inventory), func(i int) bool { return inventory[i].Path >= relative })
		if index == len(inventory) || inventory[index].Path != relative || seen[relative] {
			return errors.New("runner release contains an unexpected file")
		}
		expected := inventory[index]
		mode := os.FileMode(parseMode(expected.Mode))
		if err := protect(path, false, mode); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil || int64(len(data)) != expected.Size {
			return errors.New("runner release file size is invalid")
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != expected.SHA256 {
			return errors.New("runner release file digest is invalid")
		}
		seen[relative] = true
		return nil
	})
	if err != nil || len(seen) != len(inventory) {
		return errors.New("runner release inventory verification failed")
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("runner releases path must be canonical")
	}
	if err := protect(filepath.Dir(path), true, 0700); err != nil {
		return errors.New("runner releases parent is unsafe")
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return protect(path, true, 0700)
}

func protect(path string, directory bool, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) || info.Mode().Perm() != mode || fileUID(info) != os.Geteuid() || (!directory && fileNlink(info) != 1) {
		return errors.New("unsafe runner release ownership, type, mode or links")
	}
	return nil
}

func safePath(path string) bool {
	return path != "" && path != "." && len(path) <= 160 && filepath.ToSlash(filepath.Clean(path)) == path && !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "../")
}

func parseMode(mode string) int64 {
	if mode == "0755" {
		return 0755
	}
	return 0644
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
