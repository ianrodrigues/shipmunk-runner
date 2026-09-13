package attemptstate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreSaveReloadAndClearUsesPHPCompatibleState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "active.json")
	profileID := "profile-1"
	sandboxID := "sandbox-1"
	state := testState()
	state.ProfileID = &profileID
	state.SandboxID = &sandboxID

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"run_id"`, `"attempt_id"`, `"fence":7`, `"profile_id":"profile-1"`, `"sandbox_id":"sandbox-1"`, `"lease_expires_at":"2026-09-13T10:11:12+00:00"`} {
		if !strings.Contains(string(contents), expected) {
			t.Fatalf("state file does not contain %s: %s", expected, contents)
		}
	}
	if mode := fileMode(t, path); mode != 0600 {
		t.Fatalf("state mode = %#o, want 0600", mode)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || *loaded.ProfileID != profileID || *loaded.SandboxID != sandboxID || !loaded.LeaseExpiresAt.Equal(state.LeaseExpiresAt) || !loaded.Deadline.Equal(state.Deadline) {
		t.Fatalf("reloaded state = %#v, want %#v", loaded, state)
	}
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.Load()
	if err != nil || loaded != nil {
		t.Fatalf("Load after Clear = (%#v, %v), want (nil, nil)", loaded, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreSupportsNullableIDsAndPHPUTCVariants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.json")
	contents := `{"run_id":"run","attempt_id":"attempt","fence":1,"profile_id":null,"sandbox_id":null,"lease_expires_at":"2026-09-13T10:11:12Z","deadline":"2026-09-13T10:12:12+00:00","workspace":"/workspace"}`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.ProfileID != nil || state.SandboxID != nil {
		t.Fatalf("nullable identifiers = %#v, want nil values", state)
	}
}

func TestStoreRejectsUnsupportedStateWithoutMutation(t *testing.T) {
	for name, contents := range map[string]string{
		"malformed":   `{"run_id":`,
		"duplicate":   `{"run_id":"run","run_id":"other"}`,
		"unsupported": `{"run_id":"run","attempt_id":"attempt","fence":1,"lease_expires_at":"2026-09-13T10:11:12+00:00","deadline":"2026-09-13T10:12:12+00:00","workspace":"/workspace","future":true}`,
		"oversized":   strings.Repeat(" ", maxStateBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "active.json")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if _, err := store.Load(); err == nil {
				t.Fatal("Load accepted unsupported state")
			}
			if err := store.Clear(); err == nil {
				t.Fatal("Clear accepted unsupported state")
			}
			if err := store.Save(testState()); err == nil {
				t.Fatal("Save replaced unsupported state")
			}
			actual, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(actual) != contents {
				t.Fatalf("unsupported state changed: got %d bytes, want %d", len(actual), len(contents))
			}
		})
	}
}

func TestOpenRejectsSymlinkLeafAndNonDirectoryAncestor(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(root, "active.json")
	if err := os.Symlink(target, leaf); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(leaf); err == nil {
		t.Fatal("Open accepted symlink state leaf")
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != "preserve" {
		t.Fatalf("symlink target changed: %q, %v", actual, err)
	}
	if _, err := Open(filepath.Join(target, "child", "active.json")); err == nil {
		t.Fatal("Open accepted a file as a path ancestor")
	}
}

func TestStorePinsResolvedAncestorAgainstSymlinkSubstitution(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "original")
	replacement := filepath.Join(root, "replacement")
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(replacement, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "state-dir")
	if err := os.Symlink(original, link); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(link, "active.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, link); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testState()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(original, "active.json")); err != nil {
		t.Fatalf("state was not saved in the resolved directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(replacement, "active.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state followed replaced ancestor symlink: %v", err)
	}
}

func TestStoreRefusesReplacementOfResolvedDirectory(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "state")
	replacement := filepath.Join(root, "replacement")
	moved := filepath.Join(root, "moved-state")
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(replacement, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(original, "active.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, original); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testState()); err == nil {
		t.Fatal("Save followed a replacement of the resolved state directory")
	}
	if _, err := os.Stat(filepath.Join(replacement, "active.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement directory was modified: %v", err)
	}
}

func TestStoreLockIsExclusiveForItsLifetime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.json")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("second supervisor acquired the same state lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatalf("lock was not released by Close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreSyncFailureDoesNotReplacePreviousState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	previous := testState()
	if err := store.Save(previous); err != nil {
		t.Fatal(err)
	}
	oldBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store.fs.syncFile = func(*os.File) error { return errors.New("injected sync failure") }
	if err := store.Save(testStateWithRunID("replacement")); err == nil {
		t.Fatal("Save succeeded despite the injected file sync failure")
	}
	newBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(newBytes) != string(oldBytes) {
		t.Fatal("failed durable write replaced previous state")
	}
}

func TestStoreDirectorySyncFailureReportsPostRenameCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.fs.syncDir = func(string) error { return errors.New("injected directory sync failure") }
	if err := store.Save(testState()); err == nil {
		t.Fatal("Save succeeded despite the injected directory sync failure")
	}
	loaded, err := store.Load()
	if err != nil || loaded == nil {
		t.Fatalf("renamed state is not readable after directory sync failure: (%#v, %v)", loaded, err)
	}
}

func TestStoreUsesBoundedReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.json")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxStateBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Load(); err == nil {
		t.Fatal("Load accepted oversized state")
	}
}

func testState() State {
	return State{
		RunID:          "01k4w000000000000000000001",
		AttemptID:      "01k4w000000000000000000002",
		Fence:          7,
		LeaseExpiresAt: time.Date(2026, 9, 13, 10, 11, 12, 0, time.UTC),
		Deadline:       time.Date(2026, 9, 13, 10, 15, 0, 0, time.UTC),
		Workspace:      "/workspace/run",
	}
}

func testStateWithRunID(runID string) State {
	state := testState()
	state.RunID = runID
	return state
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
