// Package attemptstate reads and writes the runner's unversioned PHP-compatible
// active-attempt journal.
package attemptstate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const maxStateBytes = 16 * 1024

const phpDateAtom = "2006-01-02T15:04:05-07:00"

var stateFields = map[string]struct{}{
	"run_id": {}, "attempt_id": {}, "fence": {}, "profile_id": {},
	"sandbox_id": {}, "lease_expires_at": {}, "deadline": {}, "workspace": {},
	"refused_stopped_count": {},
}

// maxRefusedStoppedCount bounds RefusedStoppedCount, which only a 409 refusal
// advances and no other outcome resets; supervisor.reconcile treats it as
// settled well before this ceiling, so any larger value is corrupt.
const maxRefusedStoppedCount = 1000

var stateULIDPattern = regexp.MustCompile(`^[0-7][0-9a-hjkmnp-tv-z]{25}$`)
var safeSandboxIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// State is the durable identity and recovery context for one fenced attempt.
type State struct {
	RunID          string
	AttemptID      string
	Fence          int64
	ProfileID      *string
	SandboxID      *string
	LeaseExpiresAt time.Time
	Deadline       time.Time
	Workspace      string
	// RefusedStoppedCount counts stopped acknowledgements the control plane
	// refused with a fence mismatch or non-current attempt; only that refusal
	// advances it, and no other outcome (success or an unrelated failure)
	// resets it.
	RefusedStoppedCount int64
}

// Store holds the exclusive process-lifetime supervisor lock for one state directory.
type Store struct {
	mu        sync.Mutex
	path      string
	directory *os.File
	lockFile  *os.File
	closed    bool
	fs        fileOps
}

type fileOps struct {
	syncFile func(*os.File) error
	syncDir  func(string) error
}

// Open locks the state directory until Close; a second supervisor errors instead of sharing state.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("attempt state path is empty")
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve attempt state path: %w", err)
	}
	parent, err := prepareDirectory(filepath.Dir(absPath))
	if err != nil {
		return nil, fmt.Errorf("prepare attempt state directory: %w", err)
	}
	statePath := filepath.Join(parent, filepath.Base(absPath))
	if err := validateLeaf(statePath); err != nil {
		return nil, fmt.Errorf("unsafe attempt state path: %w", err)
	}
	directory, err := os.Open(parent)
	if err != nil {
		return nil, fmt.Errorf("open attempt state directory: %w", err)
	}
	if err := verifyPrivateDirectory(directory, parent); err != nil {
		_ = directory.Close()
		return nil, err
	}
	if err := lockDirectory(directory); err != nil {
		_ = directory.Close()
		return nil, fmt.Errorf("lock attempt state directory: %w", err)
	}
	lock, err := openAndLock(statePath + ".lock")
	if err != nil {
		_ = unlockDirectory(directory)
		_ = directory.Close()
		return nil, fmt.Errorf("lock attempt state: %w", err)
	}
	store := &Store{
		path:      statePath,
		directory: directory,
		lockFile:  lock,
		fs: fileOps{
			syncFile: func(file *os.File) error { return file.Sync() },
			syncDir:  syncDirectory,
		},
	}
	if err := store.verifyDirectory(); err != nil {
		_ = unlockAndClose(lock)
		_ = unlockDirectory(directory)
		_ = directory.Close()
		return nil, err
	}
	return store, nil
}

// Load returns nil when no journal exists, and rejects invalid state without changing the file.
func (store *Store) Load() (*State, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.ensureOpen(); err != nil {
		return nil, err
	}
	if err := store.verifyDirectory(); err != nil {
		return nil, err
	}
	return store.loadLocked()
}

// Save replaces the state file only after reading existing state, so an unsupported journal is never destroyed.
func (store *Store) Save(state State) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.ensureOpen(); err != nil {
		return err
	}
	if err := store.verifyDirectory(); err != nil {
		return err
	}
	if _, err := store.loadLocked(); err != nil {
		return fmt.Errorf("refuse to replace unsupported attempt state: %w", err)
	}
	if err := validateState(state); err != nil {
		return err
	}

	contents, err := marshalState(state)
	if err != nil {
		return err
	}
	if len(contents) > maxStateBytes {
		return errors.New("attempt state exceeded its byte limit")
	}
	temporary, err := store.createTemporary(contents)
	if err != nil {
		return err
	}
	defer func() {
		if temporary != "" {
			_ = os.Remove(temporary)
		}
	}()

	if err := validateLeaf(store.path); err != nil {
		return fmt.Errorf("unsafe attempt state path: %w", err)
	}
	if err := store.verifyDirectory(); err != nil {
		return err
	}
	if err := os.Rename(temporary, store.path); err != nil {
		return fmt.Errorf("commit attempt state: %w", err)
	}
	temporary = ""
	if err := store.fs.syncDir(filepath.Dir(store.path)); err != nil {
		return fmt.Errorf("sync attempt state directory: %w", err)
	}
	return nil
}

// Clear removes a valid journal, but retains invalid or unsupported state for operator recovery.
func (store *Store) Clear() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.ensureOpen(); err != nil {
		return err
	}
	if err := store.verifyDirectory(); err != nil {
		return err
	}
	if _, err := store.loadLocked(); err != nil {
		return fmt.Errorf("refuse to clear unsupported attempt state: %w", err)
	}
	if err := validateLeaf(store.path); err != nil {
		return fmt.Errorf("unsafe attempt state path: %w", err)
	}
	if err := os.Remove(store.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear attempt state: %w", err)
	} else if err == nil {
		if err := store.fs.syncDir(filepath.Dir(store.path)); err != nil {
			return fmt.Errorf("sync cleared attempt state directory: %w", err)
		}
	}
	return nil
}

// Close releases the supervisor lock and may be called more than once safely.
func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	lockErr := unlockAndClose(store.lockFile)
	directoryLockErr := unlockDirectory(store.directory)
	directoryErr := store.directory.Close()
	if lockErr != nil {
		return fmt.Errorf("release attempt state lock: %w", lockErr)
	}
	if directoryLockErr != nil {
		return fmt.Errorf("release attempt state directory lock: %w", directoryLockErr)
	}
	if directoryErr != nil {
		return fmt.Errorf("close attempt state directory: %w", directoryErr)
	}
	return nil
}

func (store *Store) ensureOpen() error {
	if store.closed {
		return errors.New("attempt state store is closed")
	}
	return nil
}

func (store *Store) verifyDirectory() error {
	return verifyPrivateDirectory(store.directory, filepath.Dir(store.path))
}

func (store *Store) loadLocked() (*State, error) {
	if err := validateLeaf(store.path); err != nil {
		return nil, fmt.Errorf("unsafe attempt state path: %w", err)
	}
	file, err := openReadNoFollow(store.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open attempt state: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect attempt state: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("attempt state is not a regular file")
	}
	if err := validatePrivateOwnedFile(info); err != nil {
		return nil, err
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read attempt state: %w", err)
	}
	if len(contents) > maxStateBytes {
		return nil, errors.New("attempt state exceeded its byte limit")
	}
	decoded, err := protocol.Decode(contents, maxStateBytes)
	if err != nil {
		return nil, fmt.Errorf("decode attempt state: %w", err)
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, errors.New("attempt state must be a JSON object")
	}
	for key := range object {
		if _, ok := stateFields[key]; !ok {
			return nil, fmt.Errorf("attempt state contains unsupported field %q", key)
		}
	}
	state, err := stateFromObject(object)
	if err != nil {
		return nil, err
	}
	if err := validateState(state); err != nil {
		return nil, err
	}
	return &state, nil
}

// stateFromObject decodes a parsed journal object and rejects missing or
// incorrectly typed required fields. validateState checks the decoded values.
func stateFromObject(object map[string]any) (State, error) {
	var state State
	var ok bool
	if state.RunID, ok = object["run_id"].(string); !ok {
		return State{}, errors.New("attempt state run_id is invalid")
	}
	if state.AttemptID, ok = object["attempt_id"].(string); !ok {
		return State{}, errors.New("attempt state attempt_id is invalid")
	}
	fenceNumber, ok := object["fence"].(json.Number)
	if !ok {
		return State{}, errors.New("attempt state fence is invalid")
	}
	fence, err := fenceNumber.Int64()
	if err != nil {
		return State{}, errors.New("attempt state fence is invalid")
	}
	state.Fence = fence
	if value, exists := object["profile_id"]; exists && value != nil {
		profileID, ok := value.(string)
		if !ok {
			return State{}, errors.New("attempt state profile_id is invalid")
		}
		state.ProfileID = &profileID
	}
	if value, exists := object["sandbox_id"]; exists && value != nil {
		sandboxID, ok := value.(string)
		if !ok {
			return State{}, errors.New("attempt state sandbox_id is invalid")
		}
		state.SandboxID = &sandboxID
	}
	leaseText, ok := object["lease_expires_at"].(string)
	if !ok {
		return State{}, errors.New("attempt state lease_expires_at is invalid")
	}
	leaseExpiresAt, err := parseDateAtom(leaseText)
	if err != nil {
		return State{}, errors.New("attempt state lease_expires_at is invalid")
	}
	state.LeaseExpiresAt = leaseExpiresAt
	deadlineText, ok := object["deadline"].(string)
	if !ok {
		return State{}, errors.New("attempt state deadline is invalid")
	}
	deadline, err := parseDateAtom(deadlineText)
	if err != nil {
		return State{}, errors.New("attempt state deadline is invalid")
	}
	state.Deadline = deadline
	if state.Workspace, ok = object["workspace"].(string); !ok {
		return State{}, errors.New("attempt state workspace is invalid")
	}
	countNumber, ok := object["refused_stopped_count"].(json.Number)
	if !ok {
		return State{}, errors.New("attempt state refused_stopped_count is invalid")
	}
	count, err := countNumber.Int64()
	if err != nil {
		return State{}, errors.New("attempt state refused_stopped_count is invalid")
	}
	state.RefusedStoppedCount = count
	return state, nil
}

// validateState rejects state that cannot safely identify and recover one
// attempt, including an out-of-range stopped-refusal count.
func validateState(state State) error {
	if !stateULIDPattern.MatchString(state.RunID) || !stateULIDPattern.MatchString(state.AttemptID) {
		return errors.New("attempt state identity is invalid")
	}
	if state.Fence < 1 || state.Fence > protocol.MaxSafeInteger {
		return errors.New("attempt state fence is invalid")
	}
	if state.ProfileID != nil && protocol.ValidateProfileID(*state.ProfileID) != nil {
		return errors.New("attempt state profile_id is invalid")
	}
	if state.SandboxID != nil && !safeSandboxIDPattern.MatchString(*state.SandboxID) {
		return errors.New("attempt state sandbox_id is invalid")
	}
	workspaceName := state.AttemptID + "-" + strconv.FormatInt(state.Fence, 10)
	validWorkspace := filepath.IsAbs(state.Workspace) && filepath.Clean(state.Workspace) == state.Workspace && filepath.Base(state.Workspace) == workspaceName
	if state.LeaseExpiresAt.IsZero() || state.Deadline.IsZero() || !validWorkspace {
		return errors.New("attempt state recovery context is invalid")
	}
	if state.RefusedStoppedCount < 0 || state.RefusedStoppedCount > maxRefusedStoppedCount {
		return errors.New("attempt state refused_stopped_count is invalid")
	}
	return nil
}

func verifyPrivateDirectory(directory *os.File, path string) error {
	opened, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened attempt state directory: %w", err)
	}
	if err := validatePrivateDirectory(opened); err != nil {
		return err
	}
	current, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect attempt state directory: %w", err)
	}
	if err := validatePrivateDirectory(current); err != nil {
		return err
	}
	if !os.SameFile(opened, current) {
		return errors.New("attempt state directory changed during store lifetime")
	}
	return nil
}

func parseDateAtom(value string) (time.Time, error) {
	var parsed time.Time
	var err error
	if strings.HasSuffix(value, "Z") {
		parsed, err = time.Parse(time.RFC3339, value)
	} else {
		parsed, err = time.Parse(phpDateAtom, value)
	}
	if err != nil {
		return time.Time{}, err
	}
	canonical := parsed.Format(phpDateAtom)
	if strings.HasSuffix(value, "Z") {
		canonical = parsed.UTC().Format(time.RFC3339)
	}
	if canonical != value {
		return time.Time{}, errors.New("non-canonical DATE_ATOM timestamp")
	}
	_, offset := parsed.Zone()
	if offset <= -24*60*60 || offset >= 24*60*60 {
		return time.Time{}, errors.New("invalid DATE_ATOM offset")
	}
	return parsed.UTC(), nil
}

// marshalState encodes every journal field using UTC PHP DATE_ATOM timestamps.
// Callers must validate the state separately.
func marshalState(state State) ([]byte, error) {
	object := struct {
		RunID               string  `json:"run_id"`
		AttemptID           string  `json:"attempt_id"`
		Fence               int64   `json:"fence"`
		ProfileID           *string `json:"profile_id"`
		SandboxID           *string `json:"sandbox_id"`
		LeaseExpiresAt      string  `json:"lease_expires_at"`
		Deadline            string  `json:"deadline"`
		Workspace           string  `json:"workspace"`
		RefusedStoppedCount int64   `json:"refused_stopped_count"`
	}{
		RunID:               state.RunID,
		AttemptID:           state.AttemptID,
		Fence:               state.Fence,
		ProfileID:           state.ProfileID,
		SandboxID:           state.SandboxID,
		LeaseExpiresAt:      state.LeaseExpiresAt.UTC().Format(phpDateAtom),
		Deadline:            state.Deadline.UTC().Format(phpDateAtom),
		Workspace:           state.Workspace,
		RefusedStoppedCount: state.RefusedStoppedCount,
	}
	contents, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode attempt state: %w", err)
	}
	return contents, nil
}

func (store *Store) createTemporary(contents []byte) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		var suffix [12]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("generate attempt state temporary name: %w", err)
		}
		temporary := store.path + ".tmp-" + hex.EncodeToString(suffix[:])
		file, err := createPrivateFile(temporary)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create attempt state temporary file: %w", err)
		}
		createdInfo, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			_ = os.Remove(temporary)
			return "", fmt.Errorf("inspect attempt state temporary file: %w", statErr)
		}
		writeErr := writeAll(file, contents)
		if writeErr == nil {
			writeErr = file.Chmod(0600)
		}
		if writeErr == nil {
			writeErr = store.fs.syncFile(file)
		}
		closeErr := file.Close()
		if writeErr != nil {
			_ = os.Remove(temporary)
			return "", fmt.Errorf("write attempt state durably: %w", writeErr)
		}
		if closeErr != nil {
			_ = os.Remove(temporary)
			return "", fmt.Errorf("close attempt state temporary file: %w", closeErr)
		}
		currentInfo, statErr := os.Lstat(temporary)
		if statErr != nil || !os.SameFile(createdInfo, currentInfo) || !currentInfo.Mode().IsRegular() {
			_ = os.Remove(temporary)
			if statErr != nil {
				return "", fmt.Errorf("inspect attempt state temporary path: %w", statErr)
			}
			return "", errors.New("attempt state temporary path changed during write")
		}
		if err := validatePrivateOwnedFile(currentInfo); err != nil {
			_ = os.Remove(temporary)
			return "", fmt.Errorf("validate attempt state temporary file: %w", err)
		}
		return temporary, nil
	}
	return "", errors.New("unable to allocate attempt state temporary file")
}

func writeAll(file *os.File, contents []byte) error {
	for len(contents) > 0 {
		written, err := file.Write(contents)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		contents = contents[written:]
	}
	return nil
}
