package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	MaxChangedFiles  = 200
	MaxSnapshotFiles = 20_000
	MaxSnapshotBytes = 128 * 1024 * 1024
	MaxPatchBytes    = 1024 * 1024
	MaxPathBytes     = 1024
)

type FileChange struct {
	Path         string  `json:"path"`
	BeforeSHA256 *string `json:"before_sha256"`
	AfterSHA256  *string `json:"after_sha256"`
	BeforeMode   *string `json:"before_mode"`
	AfterMode    *string `json:"after_mode"`
}

type Patch struct {
	Bytes        []byte
	SHA256       string
	ChangedFiles []FileChange
}

type fileState struct{ hash, mode string }

// CollectPatch verifies independently collected patch bytes against protected
// original and frozen snapshot trees. Generating the diff remains a collector-
// container responsibility; repository Git state is never trusted.
func CollectPatch(beforeRoot, afterRoot string, patch []byte) (*Patch, error) {
	if len(patch) > MaxPatchBytes {
		return nil, errors.New("repository patch exceeds its byte limit")
	}
	before, err := snapshot(beforeRoot)
	if err != nil {
		return nil, fmt.Errorf("original snapshot: %w", err)
	}
	after, err := snapshot(afterRoot)
	if err != nil {
		return nil, fmt.Errorf("changed snapshot: %w", err)
	}

	paths := make([]string, 0, len(before)+len(after))
	seen := make(map[string]bool)
	for path := range before {
		seen[path] = true
		paths = append(paths, path)
	}
	for path := range after {
		if !seen[path] {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	changes := make([]FileChange, 0)
	for _, path := range paths {
		old, oldOK := before[path]
		current, currentOK := after[path]
		if oldOK && currentOK && old == current {
			continue
		}
		change := FileChange{Path: path}
		if oldOK {
			change.BeforeSHA256, change.BeforeMode = pointer(old.hash), pointer(old.mode)
		}
		if currentOK {
			change.AfterSHA256, change.AfterMode = pointer(current.hash), pointer(current.mode)
		}
		changes = append(changes, change)
		if len(changes) > MaxChangedFiles {
			return nil, errors.New("repository changed-file limit exceeded")
		}
	}
	if (len(changes) == 0) != (len(patch) == 0) {
		return nil, errors.New("repository patch does not represent verified changes")
	}
	if len(patch) == 0 {
		return nil, nil
	}
	digest := sha256.Sum256(patch)
	return &Patch{Bytes: append([]byte(nil), patch...), SHA256: hex.EncodeToString(digest[:]), ChangedFiles: changes}, nil
}

func snapshot(root string) (map[string]fileState, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("root must be a real directory")
	}
	states := make(map[string]fileState)
	var total int64
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("cannot inspect snapshot")
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || !safeRepositoryPath(relative) {
			return errors.New("repository snapshot path is invalid")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return errors.New("cannot inspect snapshot")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("repository snapshot links are forbidden")
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("repository snapshot special files are forbidden")
		}
		permissions := info.Mode().Perm()
		if permissions != 0600 && permissions != 0700 && permissions != 0644 && permissions != 0755 {
			return errors.New("repository snapshot file mode is forbidden")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink != 1 {
			return errors.New("repository snapshot hard links are forbidden")
		}
		if len(states) >= MaxSnapshotFiles || info.Size() < 0 || info.Size() > MaxSnapshotBytes-total {
			return errors.New("repository snapshot limits exceeded")
		}
		file, err := os.Open(path)
		if err != nil {
			return errors.New("cannot read snapshot file")
		}
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, opened) {
			file.Close()
			return errors.New("repository snapshot changed while opening")
		}
		hash := sha256.New()
		read, copyErr := io.Copy(hash, io.LimitReader(file, info.Size()+1))
		finished, finishErr := file.Stat()
		closeErr := file.Close()
		if copyErr != nil || finishErr != nil || closeErr != nil || read != info.Size() ||
			!os.SameFile(info, finished) || finished.Size() != info.Size() || !finished.ModTime().Equal(info.ModTime()) {
			return errors.New("cannot hash snapshot file")
		}
		total += read
		mode := "100644"
		if permissions&0100 != 0 {
			mode = "100755"
		}
		states[filepath.ToSlash(relative)] = fileState{hash: hex.EncodeToString(hash.Sum(nil)), mode: mode}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return states, nil
}

func safeRepositoryPath(path string) bool {
	if path == "" || len(path) > MaxPathBytes || !utf8.ValidString(path) || filepath.IsAbs(path) || strings.ContainsAny(path, "\\\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == "" || part == "." || part == ".." || part == ".git" {
			return false
		}
		for _, character := range part {
			if character < 0x20 || character == 0x7f {
				return false
			}
		}
	}
	return true
}

func pointer(value string) *string { return &value }
