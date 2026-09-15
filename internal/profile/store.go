// Package profile stores native subscription credentials in a protected per-profile directory; it excludes containers and control-plane operations.
package profile

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const maxJournalBytes = 16 * 1024

var journalNames = map[string]struct{}{
	"active": {}, "pending": {}, "completed": {}, "execution": {},
}

// Store addresses the protected directory for one native profile; callers hold WithExclusive for a full lifecycle operation or native execution.
type Store struct {
	root      string
	directory string
	profileID string
	rootLock  *os.File
	lock      *os.File
	mu        sync.Mutex
	locked    bool
	closed    bool
}

// Open creates or validates root/profileID and does not itself acquire a lock.
func Open(root, profileID string) (*Store, error) {
	if !profileIdentifierPattern.MatchString(profileID) {
		return nil, errors.New("invalid profile identifier")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.Contains(root, "..") || strings.Contains(root, ",") {
		return nil, errors.New("profile root must be an absolute canonical path")
	}
	if err := prepareProfileRoot(root); err != nil {
		return nil, fmt.Errorf("prepare profile root: %w", err)
	}
	rootLock, err := openProfileRootLock(filepath.Join(root, ".lock"), false)
	if err != nil {
		return nil, fmt.Errorf("profile root is in use or unsafe: %w", err)
	}
	directory := filepath.Join(root, profileID)
	if err := ensureProfileDirectory(directory); err != nil {
		_ = unlockProfileLock(rootLock)
		return nil, fmt.Errorf("prepare profile directory: %w", err)
	}
	store := &Store{root: root, directory: directory, profileID: profileID, rootLock: rootLock}
	return store, nil
}

// ProfileID returns the profile identifier associated with this store.
func (store *Store) ProfileID() string { return store.profileID }

// WithExclusive holds a nonblocking OS-level exclusive lock for the callback.
func (store *Store) WithExclusive(operation func(*Store) error) error {
	if operation == nil {
		return errors.New("profile operation is required")
	}
	store.mu.Lock()
	if store.closed {
		store.mu.Unlock()
		return errors.New("profile store is closed")
	}
	if store.locked {
		store.mu.Unlock()
		return errors.New("profile is already in use")
	}
	store.mu.Unlock()

	if err := store.verifyDirectories(); err != nil {
		return err
	}
	lock, err := openProfileLock(filepath.Join(store.directory, "lock"))
	if err != nil {
		return fmt.Errorf("profile is already in use or unsafe: %w", err)
	}
	store.mu.Lock()
	if store.closed || store.locked {
		store.mu.Unlock()
		_ = unlockProfileLock(lock)
		return errors.New("profile store is closed or already in use")
	}
	store.locked = true
	store.lock = lock
	store.mu.Unlock()
	defer func() {
		store.mu.Lock()
		store.locked = false
		store.lock = nil
		store.mu.Unlock()
		_ = unlockProfileLock(lock)
	}()
	if err := store.verifyDirectories(); err != nil {
		return err
	}
	return operation(store)
}

// Close prevents future operations and is safe to call more than once.
func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	if store.locked {
		return errors.New("cannot close a profile store while its lock is held")
	}
	store.closed = true
	return unlockProfileLock(store.rootLock)
}

func WithRootExclusive(root string, operation func() error) error {
	if operation == nil {
		return errors.New("profile root operation is required")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.Contains(root, "..") || strings.Contains(root, ",") {
		return errors.New("profile root must be an absolute canonical path")
	}
	if err := prepareProfileRoot(root); err != nil {
		return fmt.Errorf("prepare profile root: %w", err)
	}
	lock, err := openProfileRootLock(filepath.Join(root, ".lock"), true)
	if err != nil {
		return fmt.Errorf("profile root is in use or unsafe: %w", err)
	}
	defer unlockProfileLock(lock)
	if err := prepareProfileRoot(root); err != nil {
		return fmt.Errorf("profile root changed while locked: %w", err)
	}
	if err := validateHeldProfileLock(lock, filepath.Join(root, ".lock")); err != nil {
		return err
	}
	return operation()
}

// Home returns the private native home path for this profile.
func (store *Store) Home() string { return filepath.Join(store.directory, "home") }

// CreateHome creates the native home with exact 0700 permissions.
func (store *Store) CreateHome() (string, error) {
	if err := store.ensureOpen(); err != nil {
		return "", err
	}
	if err := store.assertLocked(); err != nil {
		return "", err
	}
	if err := ensureProfileDirectory(store.Home()); err != nil {
		return "", fmt.Errorf("create protected profile home: %w", err)
	}
	return store.Home(), nil
}

// Read reads one fixed-name journal, rejecting unsafe files and inputs above the store's 16 KiB bound; callers preserve unknown object fields.
func (store *Store) Read(name string) (map[string]any, error) {
	if err := store.ensureOpen(); err != nil {
		return nil, err
	}
	path, err := store.journalPath(name)
	if err != nil {
		return nil, err
	}
	if err := store.verifyDirectories(); err != nil {
		return nil, err
	}
	file, err := openProfileRead(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("unsafe profile journal: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxJournalBytes+1))
	if err != nil || len(raw) > maxJournalBytes {
		return nil, errors.New("invalid profile journal")
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil || data == nil {
		return nil, errors.New("invalid profile journal")
	}
	return data, nil
}

// Write atomically replaces one journal after validating any existing value.
func (store *Store) Write(name string, data map[string]any) error {
	if err := store.ensureOpen(); err != nil {
		return err
	}
	if data == nil {
		return errors.New("profile journal must be a JSON object")
	}
	path, err := store.journalPath(name)
	if err != nil {
		return err
	}
	if err := store.verifyDirectories(); err != nil {
		return err
	}
	if _, err := store.Read(name); err != nil {
		return fmt.Errorf("refuse to replace invalid profile journal: %w", err)
	}
	contents, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode profile journal: %w", err)
	}
	if len(contents) > maxJournalBytes {
		return errors.New("profile journal exceeded its byte limit")
	}
	temporary, file, err := createProfileTemporary(path)
	if err != nil {
		return fmt.Errorf("create profile journal temporary: %w", err)
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	if err := writeProfileFile(file, contents); err != nil {
		return fmt.Errorf("persist profile journal: %w", err)
	}
	if err := store.verifyDirectories(); err != nil {
		return err
	}
	if err := validateProfileLeaf(path); err != nil {
		return fmt.Errorf("unsafe profile journal target: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("activate profile journal: %w", err)
	}
	committed = true
	if err := syncProfileDirectory(store.directory); err != nil {
		return fmt.Errorf("sync profile journal directory: %w", err)
	}
	return nil
}

// Forget removes one journal without following a replaced or linked leaf.
func (store *Store) Forget(name string) error {
	if err := store.ensureOpen(); err != nil {
		return err
	}
	path, err := store.journalPath(name)
	if err != nil {
		return err
	}
	if err := store.verifyDirectories(); err != nil {
		return err
	}
	if err := validateProfileLeaf(path); err != nil {
		return fmt.Errorf("unsafe profile journal: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove profile journal: %w", err)
	} else if err == nil {
		return syncProfileDirectory(store.directory)
	}
	return nil
}

func (store *Store) journalPath(name string) (string, error) {
	if _, ok := journalNames[name]; !ok {
		return "", errors.New("invalid profile journal name")
	}
	return filepath.Join(store.directory, name+".json"), nil
}

func (store *Store) ensureOpen() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return errors.New("profile store is closed")
	}
	return nil
}

func (store *Store) assertLocked() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || !store.locked || store.lock == nil {
		return errors.New("profile operation requires the exclusive profile lock")
	}
	return nil
}

func profileIDFromClaim(claim protocol.Claim, expected string) (map[string]any, error) {
	if protocol.ValidateProfileID(expected) != nil || claim.Manifest["profile_id"] != expected ||
		protocol.ValidateOperationID(claim.RunID) != nil || protocol.ValidateOperationID(claim.AttemptID) != nil ||
		claim.Fence < 1 || claim.Fence > protocol.MaxSafeInteger {
		return nil, errors.New("profile execution identity mismatch")
	}
	sandboxID := fmt.Sprintf("shipmunk-codex-%s-%d", claim.AttemptID, claim.Fence)
	return map[string]any{
		"profile_id": expected,
		"run_id":     claim.RunID,
		"attempt_id": claim.AttemptID,
		"fence":      claim.Fence,
		"sandbox_id": sandboxID,
	}, nil
}

// ReserveExecution journals the fenced run while the caller holds the lock.
func (store *Store) ReserveExecution(claim protocol.Claim) error {
	if err := store.assertLocked(); err != nil {
		return err
	}
	identity, err := profileIDFromClaim(claim, store.profileID)
	if err != nil {
		return err
	}
	pending, err := store.Read("pending")
	if err != nil {
		return err
	}
	if pending != nil {
		return errors.New("profile lifecycle recovery requires reconciliation")
	}
	current, err := store.Read("execution")
	if err != nil {
		return err
	}
	if current != nil {
		return errors.New("profile execution requires stopped recovery")
	}
	return store.Write("execution", identity)
}

// AssertExecutionMatches reports whether claim owns the current reservation.
func (store *Store) AssertExecutionMatches(claim protocol.Claim) (bool, error) {
	if err := store.assertLocked(); err != nil {
		return false, err
	}
	want, err := profileIDFromClaim(claim, store.profileID)
	if err != nil {
		return false, err
	}
	actual, err := store.Read("execution")
	if err != nil || actual == nil {
		return false, err
	}
	if !equalJournalIdentity(actual, want) {
		return false, errors.New("profile execution belongs to another attempt")
	}
	return true, nil
}

// ReleaseExecution clears a matching reservation after its process tree stops.
func (store *Store) ReleaseExecution(claim protocol.Claim) error {
	if err := store.assertLocked(); err != nil {
		return err
	}
	matched, err := store.AssertExecutionMatches(claim)
	if err != nil || !matched {
		return err
	}
	if err := store.NormalizeNativeHome(); err != nil {
		return err
	}
	return store.Forget("execution")
}

func equalJournalIdentity(left, right map[string]any) bool {
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftRaw) == string(rightRaw)
}

// ValidateHome verifies every home entry uses the required protected mode.
func (store *Store) ValidateHome() error {
	if err := store.ensureOpen(); err != nil {
		return err
	}
	if err := store.verifyDirectories(); err != nil {
		return err
	}
	return validateProfileTree(store.Home())
}

// NormalizeNativeHome validates the complete native tree before changing any permissions, and must be called under WithExclusive after native processes stop.
func (store *Store) NormalizeNativeHome() error {
	if err := store.assertLocked(); err != nil {
		return err
	}
	if err := store.verifyDirectories(); err != nil {
		return err
	}
	return normalizeNativeProfileTree(store.Home(), nativeTreeHooks{})
}

// Invalidate forgets the active identity and removes home entries without following symlinks; pending/execution recovery journals remain untouched.
func (store *Store) Invalidate() error {
	if err := store.ensureOpen(); err != nil {
		return err
	}
	if err := store.assertLocked(); err != nil {
		return err
	}
	if err := store.verifyDirectories(); err != nil {
		return err
	}
	if err := store.Forget("active"); err != nil {
		return err
	}
	if err := removeProfileTree(store.Home(), nativeTreeHooks{}); err != nil {
		return err
	}
	return syncProfileDirectory(store.directory)
}

func createProfileTemporary(path string) (string, *os.File, error) {
	for attempt := 0; attempt < 5; attempt++ {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		temporary := path + "." + hex.EncodeToString(random[:])
		file, err := createProfileFile(temporary)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return temporary, file, nil
	}
	return "", nil, errors.New("cannot create unique profile temporary file")
}
