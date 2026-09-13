package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	MaxChangedFiles   = 200
	MaxSnapshotFiles  = 20_000
	MaxSnapshotBytes  = 128 * 1024 * 1024
	MaxPatchBytes     = 1024 * 1024
	MaxPathBytes      = 1024
	trustedGitTimeout = 30 * time.Second
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

type fileState struct {
	hash, mode string
	data       []byte
}

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
		if oldOK && currentOK && old.hash == current.hash && old.mode == current.mode {
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
	generated, err := generatePatch(before, after)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(patch, generated) {
		return nil, errors.New("repository patch does not completely represent verified changes")
	}
	digest := sha256.Sum256(generated)
	return &Patch{Bytes: generated, SHA256: hex.EncodeToString(digest[:]), ChangedFiles: changes}, nil
}

func snapshot(root string) (map[string]fileState, error) {
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, errors.New("root must be a real directory")
	}
	defer rootHandle.Close()
	return snapshotRoot(rootHandle, nil)
}

func snapshotRoot(rootHandle *os.Root, beforeOpen func(string)) (map[string]fileState, error) {
	rootInfo, err := rootHandle.Lstat(".")
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("root must be a real directory")
	}
	states := make(map[string]fileState)
	var total int64
	err = fs.WalkDir(rootHandle.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("cannot inspect snapshot")
		}
		if path == "." {
			return nil
		}
		if !safeRepositoryPath(path) {
			return errors.New("repository snapshot path is invalid")
		}
		info, err := rootHandle.Lstat(path)
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
		if beforeOpen != nil {
			beforeOpen(path)
		}
		file, err := rootHandle.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			return errors.New("cannot read snapshot file")
		}
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, opened) {
			file.Close()
			return errors.New("repository snapshot changed while opening")
		}
		contents, readErr := io.ReadAll(io.LimitReader(file, info.Size()+1))
		finished, finishErr := file.Stat()
		closeErr := file.Close()
		if readErr != nil || finishErr != nil || closeErr != nil || int64(len(contents)) != info.Size() ||
			!os.SameFile(info, finished) || finished.Size() != info.Size() || !finished.ModTime().Equal(info.ModTime()) {
			return errors.New("cannot hash snapshot file")
		}
		total += int64(len(contents))
		mode := "100644"
		if permissions&0100 != 0 {
			mode = "100755"
		}
		digest := sha256.Sum256(contents)
		states[filepath.ToSlash(path)] = fileState{hash: hex.EncodeToString(digest[:]), mode: mode, data: contents}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return states, nil
}

func generatePatch(before, after map[string]fileState) (patch []byte, returnedErr error) {
	temporary, err := os.MkdirTemp("", "shipmunk-patch-")
	if err != nil {
		return nil, errors.New("cannot create trusted patch workspace")
	}
	defer func() {
		if err := os.RemoveAll(temporary); err != nil && returnedErr == nil {
			patch = nil
			returnedErr = errors.New("cannot remove trusted patch workspace")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), trustedGitTimeout)
	defer cancel()
	work := filepath.Join(temporary, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		return nil, errors.New("cannot create trusted patch workspace")
	}
	if err := materialize(work, before); err != nil {
		return nil, err
	}
	if err := runGit(ctx, temporary, work, nil, "init", "-q"); err != nil {
		return nil, err
	}
	// Repository attributes are untrusted input. Info attributes have higher
	// precedence than worktree .gitattributes files and keep index/diff bytes
	// identical to the independently read snapshots.
	attributes := []byte("** -text -filter -ident -working-tree-encoding !eol !diff\n")
	if err := os.WriteFile(filepath.Join(work, ".git", "info", "attributes"), attributes, 0600); err != nil {
		return nil, errors.New("cannot protect trusted patch attributes")
	}
	commands := [][]string{
		{"-c", "core.autocrlf=false", "-c", "core.hooksPath=/dev/null", "add", "--all"},
		{"-c", "core.autocrlf=false", "-c", "core.hooksPath=/dev/null", "-c", "user.name=Shipmunk", "-c", "user.email=runner@shipmunk.local", "commit", "-qm", "baseline", "--allow-empty"},
	}
	for _, arguments := range commands {
		if err := runGit(ctx, temporary, work, nil, arguments...); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		return nil, errors.New("cannot reset trusted patch workspace")
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(work, entry.Name())); err != nil {
			return nil, errors.New("cannot reset trusted patch workspace")
		}
	}
	if err := materialize(work, after); err != nil {
		return nil, err
	}
	if err := runGit(ctx, temporary, work, nil, "-c", "core.autocrlf=false", "-c", "core.hooksPath=/dev/null", "add", "-N", "--all"); err != nil {
		return nil, err
	}
	output := &boundedWriter{remaining: MaxPatchBytes + 1}
	if err := runGit(ctx, temporary, work, output, "-c", "core.autocrlf=false", "-c", "core.hooksPath=/dev/null", "-c", "diff.external=", "diff", "--no-ext-diff", "--no-textconv", "--binary", "HEAD", "--", "."); err != nil {
		return nil, err
	}
	if output.buffer.Len() > MaxPatchBytes {
		return nil, errors.New("repository patch exceeds its byte limit")
	}
	return append([]byte(nil), output.buffer.Bytes()...), nil
}

func materialize(root string, states map[string]fileState) error {
	for path, state := range states {
		target := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return errors.New("cannot materialize trusted snapshot")
		}
		mode := os.FileMode(0600)
		if state.mode == "100755" {
			mode = 0700
		}
		if err := os.WriteFile(target, state.data, mode); err != nil {
			return errors.New("cannot materialize trusted snapshot")
		}
	}
	return nil
}

type boundedWriter struct {
	buffer    bytes.Buffer
	remaining int
}

func (writer *boundedWriter) Write(data []byte) (int, error) {
	if len(data) > writer.remaining {
		return 0, errors.New("output limit exceeded")
	}
	writer.remaining -= len(data)
	return writer.buffer.Write(data)
}

func runGit(ctx context.Context, home, work string, stdout *boundedWriter, arguments ...string) error {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = work
	command.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LANG=C"}
	if stdout != nil {
		command.Stdout = stdout
	}
	stderr := &boundedWriter{remaining: 4096}
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return errors.New("trusted patch generation failed")
	}
	return nil
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
