// Package codexsession stores explicitly selected Codex conversation sessions.
// Records are trusted host state and are never discovered from provider history.
package codexsession

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const maxRecordBytes = 4 * 1024

var (
	runPattern     = regexp.MustCompile(`^[0-7][0-9a-hjkmnp-tv-z]{25}$`)
	sessionPattern = regexp.MustCompile(`^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)
	digestPattern  = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Mode makes fresh and resume behavior an explicit caller decision.
type Mode uint8

const (
	Fresh Mode = iota + 1
	Resume
)

// Session is a provider session plus its immutable execution-context binding.
type Session struct {
	ID      string `json:"id"`
	Binding string `json:"binding"`
}

// BindingFromClaim hashes all context that may affect a resumed conversation.
// It deliberately defines a new Go-native format rather than preserving PHP bytes.
func BindingFromClaim(claim protocol.Claim) (string, error) {
	if !runPattern.MatchString(claim.RunID) || claim.Manifest == nil {
		return "", errors.New("cannot bind an invalid claim")
	}
	bound := map[string]any{
		"run_id":                claim.RunID,
		"repository_id":         claim.Manifest["repository_id"],
		"base_sha":              claim.Manifest["base_sha"],
		"head_sha":              claim.Manifest["head_sha"],
		"profile_id":            claim.Manifest["profile_id"],
		"agent":                 claim.Manifest["agent"],
		"runtime_version":       claim.Manifest["runtime_version"],
		"effective_config":      claim.Manifest["effective_config"],
		"task_context":          claim.Manifest["task_context"],
		"instruction_artifacts": claim.Manifest["instruction_artifacts"],
		"source_artifacts":      claim.Manifest["source_artifacts"],
	}
	if supervisor, ok := claim.Manifest["supervisor"].(map[string]any); ok {
		bound["credential_reference"] = supervisor["credential_reference"]
	}
	raw, err := json.Marshal(bound)
	if err != nil {
		return "", fmt.Errorf("encode session binding: %w", err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// New validates a provider-issued session identifier and associates it with binding.
func New(id, binding string) (Session, error) {
	session := Session{ID: id, Binding: binding}
	if err := validateSession(session); err != nil {
		return Session{}, err
	}
	return session, nil
}

// Store maintains one bounded, private record per run.
type Store struct {
	root     string
	rootInfo os.FileInfo
	writeMu  sync.Mutex
}

// Open creates a private canonical root and rejects every symlink component.
func Open(root string) (*Store, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.Contains(root, ",") {
		return nil, errors.New("session root must be an absolute canonical path")
	}
	if err := preparePrivateRoot(root); err != nil {
		return nil, fmt.Errorf("prepare session root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect session root: %w", err)
	}
	return &Store{root: root, rootInfo: info}, nil
}

// Select returns nil for Fresh without consulting an existing record. Resume
// requires an existing, valid record with exactly the requested binding.
func (store *Store) Select(mode Mode, claim protocol.Claim) (*Session, error) {
	binding, err := BindingFromClaim(claim)
	if err != nil {
		return nil, err
	}
	switch mode {
	case Fresh:
		return nil, nil
	case Resume:
		session, err := store.Read(claim.RunID)
		if err != nil {
			return nil, err
		}
		if session == nil {
			return nil, errors.New("requested session does not exist")
		}
		if session.Binding != binding {
			return nil, errors.New("requested session belongs to different execution context")
		}
		return session, nil
	default:
		return nil, errors.New("session mode must be explicitly fresh or resume")
	}
}

// Persist records the provider session only when it belongs to claim's exact context.
func (store *Store) Persist(claim protocol.Claim, session Session) error {
	binding, err := BindingFromClaim(claim)
	if err != nil {
		return err
	}
	return store.write(claim.RunID, binding, session)
}

// Read loads only the named run's record. Malformed and unsafe state fails closed.
func (store *Store) Read(runID string) (*Session, error) {
	path, err := store.path(runID)
	if err != nil {
		return nil, err
	}
	file, err := openNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open session record: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || validatePrivateFile(info) != nil {
		return nil, errors.New("unsafe session record")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	if err != nil || len(raw) > maxRecordBytes {
		return nil, errors.New("invalid session record")
	}
	value, err := protocol.Decode(raw, maxRecordBytes)
	if err != nil {
		return nil, errors.New("invalid session record")
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) != 2 {
		return nil, errors.New("invalid session record")
	}
	id, idOK := object["id"].(string)
	binding, bindingOK := object["binding"].(string)
	if !idOK || !bindingOK {
		return nil, errors.New("invalid session record")
	}
	session, err := New(id, binding)
	if err != nil {
		return nil, errors.New("invalid session record")
	}
	return &session, nil
}

// Write atomically and durably replaces the named run's compatible record.
func (store *Store) write(runID, binding string, session Session) error {
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	rootLock, err := lockSessionRoot(store.root, store.rootInfo)
	if err != nil {
		return fmt.Errorf("lock session root: %w", err)
	}
	defer unlockSessionRoot(rootLock)

	path, err := store.path(runID)
	if err != nil {
		return err
	}
	if err := validateSession(session); err != nil {
		return err
	}
	if session.Binding != binding || !digestPattern.MatchString(binding) {
		return errors.New("cannot persist a session with a different binding")
	}
	if existing, err := store.Read(runID); err != nil {
		return fmt.Errorf("refuse to replace invalid session record: %w", err)
	} else if existing != nil && existing.Binding != binding {
		return errors.New("refuse to replace a session from a different execution context")
	}
	raw, err := json.Marshal(session)
	if err != nil || len(raw) > maxRecordBytes {
		return errors.New("invalid session record")
	}
	temporary := ""
	allocated := false
	for attempt := 0; attempt < 5; attempt++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return fmt.Errorf("create session temporary: %w", err)
		}
		temporary = path + ".tmp-" + hex.EncodeToString(random[:])
		file, err := createExclusive(temporary)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("create session temporary: %w", err)
		}
		writeErr := writeAndSync(file, raw)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			_ = os.Remove(temporary)
			return errors.New("persist session record")
		}
		allocated = true
		break
	}
	if !allocated {
		return errors.New("allocate session temporary")
	}
	defer os.Remove(temporary)
	if err := store.verifyRoot(); err != nil {
		return err
	}
	if err := validateTarget(path); err != nil {
		return fmt.Errorf("unsafe session target: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("commit session record: %w", err)
	}
	return syncDirectory(store.root)
}

func (store *Store) path(runID string) (string, error) {
	if !runPattern.MatchString(runID) {
		return "", errors.New("invalid run identifier")
	}
	if err := store.verifyRoot(); err != nil {
		return "", err
	}
	return filepath.Join(store.root, runID+".json"), nil
}

func (store *Store) verifyRoot() error {
	info, err := os.Lstat(store.root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe session root")
	}
	if !os.SameFile(store.rootInfo, info) {
		return errors.New("session root changed during store lifetime")
	}
	return validatePrivateDirectory(info)
}

func validateSession(session Session) error {
	if !sessionPattern.MatchString(session.ID) || !digestPattern.MatchString(session.Binding) {
		return errors.New("invalid native session")
	}
	return nil
}

func writeAndSync(file *os.File, raw []byte) error {
	for len(raw) > 0 {
		n, err := file.Write(raw)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		raw = raw[n:]
	}
	if err := file.Chmod(0600); err != nil {
		return err
	}
	return file.Sync()
}
