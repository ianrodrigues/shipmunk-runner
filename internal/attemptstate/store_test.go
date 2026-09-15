package attemptstate

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStoreSaveReloadAndClearUsesPHPCompatibleState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "active.json")
	profileID := "01k4w000000000000000000009"
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
	for _, expected := range []string{`"run_id"`, `"attempt_id"`, `"fence":7`, `"profile_id":"01k4w000000000000000000009"`, `"sandbox_id":"sandbox-1"`, `"lease_expires_at":"2026-09-13T10:11:12+00:00"`} {
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
	path := filepath.Join(privateTempDir(t), "active.json")
	contents := `{"run_id":"01k4w000000000000000000001","attempt_id":"01k4w000000000000000000002","fence":1,"profile_id":null,"sandbox_id":null,"lease_expires_at":"2026-09-13T10:11:12Z","deadline":"2026-09-13T10:12:12+00:00","workspace":"/workspace/01k4w000000000000000000002-1","refused_stopped_count":0}`
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

func TestStoreNormalizesCanonicalPHPTimezoneOffsetsWithoutChangingInstants(t *testing.T) {
	for _, timestamp := range []string{"2026-09-13T11:11:12+01:00", "2026-09-13T06:11:12-04:00", "2026-09-13T15:56:12+05:45"} {
		t.Run(timestamp, func(t *testing.T) {
			path := filepath.Join(privateTempDir(t), "active.json")
			raw, err := marshalState(testState())
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.ReplaceAll(string(raw), "2026-09-13T10:11:12+00:00", timestamp))
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			state, err := store.Load()
			if err != nil || state == nil || !state.LeaseExpiresAt.Equal(testState().LeaseExpiresAt) || state.LeaseExpiresAt.Location() != time.UTC {
				t.Fatalf("PHP offset changed the lease instant: %+v %v", state, err)
			}
			unchanged, err := os.ReadFile(path)
			if err != nil || string(unchanged) != string(raw) {
				t.Fatal("loading a PHP journal rewrote it")
			}
			if err := store.Save(*state); err != nil {
				t.Fatal(err)
			}
			normalized, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(normalized), `"lease_expires_at":"2026-09-13T10:11:12+00:00"`) {
				t.Fatalf("Go save did not normalize to UTC: %s %v", normalized, err)
			}
		})
	}
}

func TestDateAtomRejectsNoncanonicalAndInvalidOffsets(t *testing.T) {
	for _, value := range []string{"2026-09-13T10:11:12+24:00", "2026-09-13T10:11:12+01:60", "2026-09-13T10:11:12-00:00", "2026-09-13T10:11:12+0100", "2026-09-13T10:11:12.5+01:00"} {
		if _, err := parseDateAtom(value); err == nil {
			t.Errorf("accepted invalid or noncanonical DATE_ATOM: %s", value)
		}
	}
}

func TestStoreRejectsUnsupportedStateWithoutMutation(t *testing.T) {
	for name, contents := range map[string]string{
		"malformed":            `{"run_id":`,
		"duplicate":            `{"run_id":"run","run_id":"other"}`,
		"unsupported":          `{"run_id":"run","attempt_id":"attempt","fence":1,"lease_expires_at":"2026-09-13T10:11:12+00:00","deadline":"2026-09-13T10:12:12+00:00","workspace":"/workspace","future":true}`,
		"oversized":            strings.Repeat(" ", maxStateBytes+1),
		"invalid IDs":          `{"run_id":"run","attempt_id":"attempt","fence":1,"lease_expires_at":"2026-09-13T10:11:12+00:00","deadline":"2026-09-13T10:12:12+00:00","workspace":"/workspace/attempt-1"}`,
		"relative workspace":   `{"run_id":"01k4w000000000000000000001","attempt_id":"01k4w000000000000000000002","fence":1,"lease_expires_at":"2026-09-13T10:11:12+00:00","deadline":"2026-09-13T10:12:12+00:00","workspace":"workspaces/01k4w000000000000000000002-1"}`,
		"mismatched workspace": `{"run_id":"01k4w000000000000000000001","attempt_id":"01k4w000000000000000000002","fence":1,"lease_expires_at":"2026-09-13T10:11:12+00:00","deadline":"2026-09-13T10:12:12+00:00","workspace":"/workspace/another-attempt-1"}`,
		"unsafe sandbox id":    `{"run_id":"01k4w000000000000000000001","attempt_id":"01k4w000000000000000000002","fence":1,"sandbox_id":"../other","lease_expires_at":"2026-09-13T10:11:12+00:00","deadline":"2026-09-13T10:12:12+00:00","workspace":"/workspace/01k4w000000000000000000002-1"}`,
		"invalid profile id":   `{"run_id":"01k4w000000000000000000001","attempt_id":"01k4w000000000000000000002","fence":1,"profile_id":"profile","lease_expires_at":"2026-09-13T10:11:12+00:00","deadline":"2026-09-13T10:12:12+00:00","workspace":"/workspace/01k4w000000000000000000002-1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(privateTempDir(t), "active.json")
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

func TestStoreRoundTripsRefusedStoppedCount(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "active.json")
	state := testState()
	state.RefusedStoppedCount = 2

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
	if !strings.Contains(string(contents), `"refused_stopped_count":2`) {
		t.Fatalf("state file does not journal refused_stopped_count: %s", contents)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, err := store.Load()
	if err != nil || loaded == nil || loaded.RefusedStoppedCount != 2 {
		t.Fatalf("reloaded refused_stopped_count = %+v, %v", loaded, err)
	}
}

func TestStoreRejectsInvalidRefusedStoppedCount(t *testing.T) {
	for name, value := range map[string]string{
		"negative":  `-1`,
		"over max":  strconv.Itoa(maxRefusedStoppedCount + 1),
		"non-int":   `1.5`,
		"non-numer": `"2"`,
		"missing":   ``,
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := marshalState(testState())
			if err != nil {
				t.Fatal(err)
			}
			contents := string(raw)
			if value == "" {
				contents = strings.Replace(contents, `,"refused_stopped_count":0`, "", 1)
			} else {
				contents = strings.Replace(contents, `"refused_stopped_count":0`, `"refused_stopped_count":`+value, 1)
			}
			if contents == string(raw) {
				t.Fatal("fixture refused_stopped_count field was not found")
			}
			path := filepath.Join(privateTempDir(t), "active.json")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if _, err := store.Load(); err == nil {
				t.Fatal("Load accepted an invalid refused_stopped_count")
			}
		})
	}
}

func TestOpenRejectsSymlinkLeafAndNonDirectoryAncestor(t *testing.T) {
	root := privateTempDir(t)
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
	path := filepath.Join(privateTempDir(t), "active.json")
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

func TestStoreLockCannotBeReplacedWhileHeld(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "active.json")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("second supervisor acquired a replacement lock inode")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatalf("lock was not released by Close after unlink: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreLockDescriptorsAreCloseOnExec(t *testing.T) {
	if os.Getenv("ATTEMPT_STATE_CLOEXEC_HELPER") == "1" {
		path := os.Getenv("ATTEMPT_STATE_CLOEXEC_PATH")
		ready := os.Getenv("ATTEMPT_STATE_CLOEXEC_READY")
		release := os.Getenv("ATTEMPT_STATE_CLOEXEC_RELEASE")
		if err := os.WriteFile(ready, []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(release); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for parent to release the lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
		store, err := Open(path)
		if err != nil {
			t.Fatalf("inherited descriptors kept the attempt lock held: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}

	root := privateTempDir(t)
	path := filepath.Join(root, "active.json")
	ready := filepath.Join(root, "ready")
	release := filepath.Join(root, "release")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStoreLockDescriptorsAreCloseOnExec$")
	command.Env = append(os.Environ(),
		"ATTEMPT_STATE_CLOEXEC_HELPER=1",
		"ATTEMPT_STATE_CLOEXEC_PATH="+path,
		"ATTEMPT_STATE_CLOEXEC_READY="+ready,
		"ATTEMPT_STATE_CLOEXEC_RELEASE="+release,
	)
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for subprocess readiness")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Closing descriptors without issuing LOCK_UN simulates abrupt supervisor death.
	if err := first.lockFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.directory.Close(); err != nil {
		t.Fatal(err)
	}
	first.closed = true
	if err := os.WriteFile(release, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	finished = true
	if err != nil {
		t.Fatalf("subprocess could not acquire released lock: %v\n%s", err, output.String())
	}
}

func TestOpenRejectsInsecureDirectoryAndLeavesItsModeUntouched(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(root, "active.json")); err == nil {
		t.Fatal("Open accepted a state directory accessible to group or other users")
	}
	if mode := fileMode(t, root); mode != 0755 {
		t.Fatalf("Open changed pre-existing directory mode to %#o", mode)
	}
}

func TestOpenRejectsInsecureExistingLeavesWithoutChangingThem(t *testing.T) {
	for _, name := range []string{"active.json", "active.json.lock"} {
		t.Run(name, func(t *testing.T) {
			root := privateTempDir(t)
			path := filepath.Join(root, "active.json")
			leaf := path
			if name == "active.json.lock" {
				leaf += ".lock"
			}
			if err := os.WriteFile(leaf, []byte("pre-existing"), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil {
				t.Fatal("Open accepted a pre-existing public state leaf")
			}
			if mode := fileMode(t, leaf); mode != 0644 {
				t.Fatalf("Open changed pre-existing leaf mode to %#o", mode)
			}
		})
	}
}

func TestOpenRejectsHardLinkedLockWithoutChangingTargetMode(t *testing.T) {
	root := privateTempDir(t)
	path := filepath.Join(root, "active.json")
	target := filepath.Join(root, "unrelated.txt")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, path+".lock"); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a hard-linked lock file")
	} else if !strings.Contains(err.Error(), "multiple hard links") {
		t.Fatalf("hard-linked lock failed for an unexpected reason: %v", err)
	}
	if mode := fileMode(t, target); mode != 0600 {
		t.Fatalf("Open changed unrelated hard-link target mode to %#o", mode)
	}
}

func TestStoreSyncFailureDoesNotReplacePreviousState(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "active.json")
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
	if err := store.Save(testStateWithRunID("01k4w000000000000000000003")); err == nil {
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
	path := filepath.Join(privateTempDir(t), "active.json")
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
	path := filepath.Join(privateTempDir(t), "active.json")
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
		Workspace:      filepath.Join("/workspace", "01k4w000000000000000000002-7"),
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

func privateTempDir(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return directory
}
