// Package workspace prepares isolated, attempt-scoped input directories.
package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

var attemptDirectoryPattern = regexp.MustCompile(`^[0-7][0-9a-hjkmnp-tv-z]{25}-[1-9][0-9]*$`)

// Downloader is the input-artifact portion of the control-plane client.
type Downloader interface {
	DownloadArtifact(context.Context, protocol.Claim, string, string) ([]byte, error)
}

// Workspace owns a configured root for isolated attempt directories.
type Workspace struct {
	root      string
	extractor TarExtractor
	mu        sync.Mutex
}

// New creates a workspace manager for root without touching the filesystem.
func New(root string) (*Workspace, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("workspace root is invalid")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace root is invalid")
	}
	return &Workspace{root: filepath.Clean(absRoot), extractor: TarExtractor{}}, nil
}

// Path returns the stable attempt-fenced directory for claim.
func (workspace *Workspace) Path(claim protocol.Claim) string {
	return filepath.Join(workspace.root, claim.AttemptID+"-"+strconv.FormatInt(claim.Fence, 10))
}

// EnsureWritable creates the root if needed and verifies that a private probe
// file can be created and removed before the supervisor claims work.
func (workspace *Workspace) EnsureWritable() error {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()

	if err := os.MkdirAll(workspace.root, 0700); err != nil {
		return fmt.Errorf("unable to create workspace root: %w", err)
	}
	info, err := os.Lstat(workspace.root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("workspace root must be a real directory")
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("unable to create workspace write probe: %w", err)
	}
	probe := filepath.Join(workspace.root, ".write-probe-"+hex.EncodeToString(random[:]))
	file, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("workspace root is not writable; refusing to claim work: %w", err)
	}
	closeErr := file.Close()
	removeErr := os.Remove(probe)
	if closeErr != nil || removeErr != nil {
		return fmt.Errorf("workspace root is not writable; refusing to claim work")
	}
	return nil
}

// Prepare downloads and safely extracts source and trusted instruction inputs.
// It returns the attempt-fenced directory and removes that directory if
// preparation fails. The caller's context controls cancellation and is passed
// to every download.
func (workspace *Workspace) Prepare(ctx context.Context, claim protocol.Claim, client Downloader) (string, error) {
	if client == nil {
		return "", fmt.Errorf("artifact downloader is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path := workspace.Path(claim)
	if !validAttemptDirectoryName(filepath.Base(path)) || filepath.Dir(path) != workspace.root {
		return "", fmt.Errorf("attempt workspace identity is invalid")
	}

	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("attempt workspace already exists")
		}
		return "", fmt.Errorf("unable to create attempt workspace: %w", err)
	}
	cleanup := func(cause error) (string, error) {
		if removeErr := workspace.removeLocked(path); removeErr != nil {
			return "", fmt.Errorf("%w (workspace cleanup failed: %v)", cause, removeErr)
		}
		return "", cause
	}

	sourceRefs, err := artifactReferences(claim.Manifest, "source_artifacts")
	if err != nil {
		return cleanup(err)
	}
	for index, reference := range sourceRefs {
		if err := ctx.Err(); err != nil {
			return cleanup(err)
		}
		destination := filepath.Join(path, "sources", strconv.Itoa(index))
		if err := os.MkdirAll(destination, 0700); err != nil {
			return cleanup(fmt.Errorf("unable to create artifact workspace: %w", err))
		}
		archive, err := client.DownloadArtifact(ctx, claim, reference.id, reference.sha256)
		if err != nil {
			return cleanup(err)
		}
		if err := ctx.Err(); err != nil {
			return cleanup(err)
		}
		if err := workspace.extractor.Extract(ctx, archive, destination); err != nil {
			return cleanup(err)
		}
		var revision any
		if len(sourceRefs) == 2 && index == 0 {
			revision = claim.Manifest["base_sha"]
		} else if index == len(sourceRefs)-1 {
			revision = claim.Manifest["head_sha"]
		}
		if err := unwrapGitHubSnapshot(ctx, destination, revision); err != nil {
			return cleanup(err)
		}
	}

	instructionRefs, err := artifactReferences(claim.Manifest, "instruction_artifacts")
	if err != nil {
		return cleanup(err)
	}
	instructionDir := filepath.Join(path, "instructions")
	if err := os.Mkdir(instructionDir, 0700); err != nil {
		return cleanup(fmt.Errorf("claim instruction_artifacts is invalid: %w", err))
	}
	for index, reference := range instructionRefs {
		if err := ctx.Err(); err != nil {
			return cleanup(err)
		}
		data, err := client.DownloadArtifact(ctx, claim, reference.id, reference.sha256)
		if err != nil {
			return cleanup(err)
		}
		if err := ctx.Err(); err != nil {
			return cleanup(err)
		}
		value, err := protocol.Decode(data, protocol.InputArtifactMaxBytes)
		if err != nil {
			return cleanup(fmt.Errorf("trusted instruction artifact is not valid JSON: %w", err))
		}
		if _, ok := value.(map[string]any); !ok {
			return cleanup(fmt.Errorf("trusted instruction artifact must be a JSON object"))
		}
		filePath := filepath.Join(instructionDir, strconv.Itoa(index)+".json")
		if err := os.WriteFile(filePath, data, 0600); err != nil {
			return cleanup(fmt.Errorf("unable to store trusted instruction artifact: %w", err))
		}
	}
	return path, nil
}

type artifactReference struct {
	id     string
	sha256 string
}

func artifactReferences(manifest map[string]any, key string) ([]artifactReference, error) {
	values, ok := manifest[key].([]any)
	if !ok || len(values) == 0 {
		return nil, fmt.Errorf("claim %s is invalid", key)
	}
	references := make([]artifactReference, 0, len(values))
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("claim artifact reference is invalid")
		}
		id, idOK := item["artifact_id"].(string)
		hash, hashOK := item["sha256"].(string)
		if !idOK || !hashOK || id == "" || hash == "" {
			return nil, fmt.Errorf("claim artifact reference is invalid")
		}
		references = append(references, artifactReference{id: id, sha256: hash})
	}
	return references, nil
}

func validAttemptDirectoryName(name string) bool {
	if !attemptDirectoryPattern.MatchString(name) {
		return false
	}
	separator := strings.LastIndexByte(name, '-')
	fence, err := strconv.ParseInt(name[separator+1:], 10, 64)
	return err == nil && fence > 0 && fence <= protocol.MaxSafeInteger
}

// Remove removes only a direct child named for a valid attempt and positive
// fence. It refuses symlinks anywhere in the tree rather than following them.
func (workspace *Workspace) Remove(path string) error {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.removeLocked(path)
}

func (workspace *Workspace) removeLocked(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("attempt workspace path is invalid")
	}
	absPath = filepath.Clean(absPath)
	if filepath.Dir(absPath) != workspace.root || !validAttemptDirectoryName(filepath.Base(absPath)) {
		return fmt.Errorf("refusing to remove a path outside the attempt workspace root")
	}
	rootInfo, err := os.Lstat(workspace.root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("workspace root must be a real directory")
	}
	info, err := os.Lstat(absPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("attempt workspace must be a real directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(workspace.root)
	if err != nil {
		return fmt.Errorf("unable to resolve workspace root")
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return fmt.Errorf("unable to resolve attempt workspace")
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return fmt.Errorf("refusing to remove a path outside the attempt workspace root")
	}

	var paths []string
	err = filepath.WalkDir(absPath, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current != absPath && entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("attempt workspace contains a symbolic link")
		}
		paths = append(paths, current)
		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to inspect attempt workspace: %w", err)
	}
	sort.Slice(paths, func(left, right int) bool { return len(paths[left]) > len(paths[right]) })
	for _, current := range paths {
		entryInfo, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("unable to inspect attempt workspace entry: %w", err)
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("attempt workspace contains a symbolic link")
		}
		if err := os.Remove(current); err != nil {
			return fmt.Errorf("unable to remove attempt workspace contents: %w", err)
		}
	}
	return nil
}

// Sanitize returns a recursive copy of manifest with supervisor-only
// credentials and callback fields removed, leaving manifest unchanged.
func Sanitize(manifest map[string]any) map[string]any {
	return filterMap(manifest)
}

var forbiddenInputKeys = map[string]struct{}{
	"supervisor": {}, "authorization": {}, "callback": {}, "credential_reference": {},
	"runner_token": {}, "github_token": {}, "app_key": {}, "database_url": {},
}

func filterMap(input map[string]any) map[string]any {
	clean := make(map[string]any, len(input))
	for key, value := range input {
		if _, forbidden := forbiddenInputKeys[strings.ToLower(key)]; forbidden {
			continue
		}
		clean[key] = filterValue(value)
	}
	return clean
}

func filterValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return filterMap(typed)
	case []any:
		clean := make([]any, len(typed))
		for index, item := range typed {
			clean[index] = filterValue(item)
		}
		return clean
	default:
		return value
	}
}
