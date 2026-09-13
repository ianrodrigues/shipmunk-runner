package profile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const testProfileID = "01k4w000000000000000000001"

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	temporaryRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(temporaryRoot, "profiles")
	store, err := Open(root, testProfileID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, root
}

func TestStorePHPJournalFormatsAndAtomicReplacement(t *testing.T) {
	store, _ := openTestStore(t)
	fixtures := map[string]map[string]any{
		"active": {
			"profile_id": testProfileID, "credential_reference": "credential:fixture",
			"agent": "codex", "auth_mode": "subscription", "runtime_version": "0.154.0",
		},
		"pending": {
			"profile_id": testProfileID, "operation_id": "01k4w000000000000000000002",
			"operation": "login", "sandbox": "shipmunk-profile-01k4w000000000000000000002",
			"binding": map[string]any{"runtime_version": "0.154.0"},
			"outcome": map[string]any{"health": "ready", "reason": nil}, "future": true,
		},
		"completed": {"operation_id": "01k4w000000000000000000002"},
		"execution": {
			"profile_id": testProfileID, "run_id": "01k4w000000000000000000003",
			"attempt_id": "01k4w000000000000000000004", "fence": int64(5),
			"sandbox_id": "shipmunk-codex-01k4w000000000000000000004-5",
		},
	}
	for name, fixture := range fixtures {
		if err := store.Write(name, fixture); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
		actual, err := store.Read(name)
		if err != nil || actual == nil {
			t.Fatalf("Read(%s) = (%v, %v)", name, actual, err)
		}
		if name == "pending" && actual["future"] != true {
			t.Fatal("unknown PHP journal fields were not preserved")
		}
		info, err := os.Lstat(filepath.Join(store.directory, name+".json"))
		if err != nil || info.Mode().Perm() != 0600 || !info.Mode().IsRegular() {
			t.Fatalf("journal permissions/type = %v, %v", info, err)
		}
	}
	if err := store.Forget("pending"); err != nil {
		t.Fatal(err)
	}
	if value, err := store.Read("pending"); err != nil || value != nil {
		t.Fatalf("Read after Forget = (%v, %v)", value, err)
	}
	if err := store.Write("unknown", map[string]any{}); err == nil {
		t.Fatal("accepted an unknown journal name")
	}
}

func TestStoreEnforcesPHPJournalReadBoundWithoutMutation(t *testing.T) {
	store, _ := openTestStore(t)
	path := filepath.Join(store.directory, "active.json")
	for _, size := range []int{maxJournalBytes, maxJournalBytes + 1} {
		t.Run(string(rune('a'+size-maxJournalBytes)), func(t *testing.T) {
			contents := []byte(`{"pad":"` + strings.Repeat("x", size-10) + `"}`)
			if len(contents) != size {
				t.Fatalf("fixture length = %d, want %d", len(contents), size)
			}
			if err := os.WriteFile(path, contents, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := store.Read("active")
			if size == maxJournalBytes && err != nil {
				t.Fatalf("exactly 16 KiB rejected: %v", err)
			}
			if size > maxJournalBytes && err == nil {
				t.Fatal("accepted a journal larger than 16 KiB")
			}
			if size > maxJournalBytes {
				actual, readErr := os.ReadFile(path)
				if readErr != nil || string(actual) != string(contents) {
					t.Fatal("oversized journal was modified")
				}
			}
		})
	}
}

func TestStoreRejectsMalformedOrUnsafeJournalWithoutReplacement(t *testing.T) {
	store, _ := openTestStore(t)
	path := filepath.Join(store.directory, "active.json")
	for _, contents := range []string{`{`, `null`, `[]`} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Read("active"); err == nil {
			t.Fatalf("Read accepted %q", contents)
		}
		if err := store.Write("active", map[string]any{"profile_id": testProfileID}); err == nil {
			t.Fatalf("Write replaced unsupported state %q", contents)
		}
		actual, err := os.ReadFile(path)
		if err != nil || string(actual) != contents {
			t.Fatalf("unsupported journal changed: %q, %v", actual, err)
		}
	}
}

func TestOpenRejectsUnsafeProfileRootAndExistingDirectories(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"relative", parent + "/../escape", filepath.Join(parent, "comma,root")} {
		if _, err := Open(root, testProfileID); err == nil {
			t.Errorf("Open accepted unsafe root %q", root)
		}
	}
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Open(filepath.Join(link, "profiles"), testProfileID); err == nil {
		t.Fatal("Open accepted a symlinked root ancestor")
	}
	root := filepath.Join(parent, "broad")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, testProfileID); err == nil {
		t.Fatal("Open accepted a root with broad permissions")
	}
}

func TestStoreRejectsJournalSymlinkAndHardlink(t *testing.T) {
	store, _ := openTestStore(t)
	target := filepath.Join(filepath.Dir(store.directory), "outside")
	if err := os.WriteFile(target, []byte(`{"safe":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.directory, "active.json")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := store.Read("active"); err == nil {
		t.Fatal("Read followed a journal symlink")
	}
	if err := store.Write("active", map[string]any{"replacement": true}); err == nil {
		t.Fatal("Write replaced an unsafe journal")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, path); err != nil {
		t.Skipf("hardlink unavailable: %v", err)
	}
	if _, err := store.Read("active"); err == nil {
		t.Fatal("Read accepted a hard-linked journal")
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != `{"safe":true}` {
		t.Fatalf("unsafe journal operation changed outside target: %s %v", actual, err)
	}
}

func TestStoreRequiresExactLockPermissionsAndRejectsLockLinks(t *testing.T) {
	store, root := openTestStore(t)
	if err := store.WithExclusive(func(*Store) error { return nil }); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(store.directory, "lock")
	if mode := fileMode(t, lockPath); mode != 0600 {
		t.Fatalf("lock mode %#o, want 0600", mode)
	}
	if err := os.Chmod(lockPath, 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.WithExclusive(func(*Store) error { return nil }); err == nil {
		t.Fatal("accepted a lock file with broad permissions")
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside-lock")
	if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, lockPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := store.WithExclusive(func(*Store) error { return nil }); err == nil {
		t.Fatal("accepted a symlinked lock file")
	}
	actual, err := os.ReadFile(outside)
	if err != nil || string(actual) != "preserve" {
		t.Fatalf("lock operation changed symlink target: %q %v", actual, err)
	}
}

func TestExclusiveLockSpansCallbackAndIsPerProfile(t *testing.T) {
	store, root := openTestStore(t)
	other, err := Open(root, testProfileID)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherProfile, err := Open(root, "01k4w000000000000000000002")
	if err != nil {
		t.Fatal(err)
	}
	defer otherProfile.Close()
	started := make(chan struct{})
	finish := make(chan struct{})
	locked := make(chan error, 1)
	go func() {
		locked <- store.WithExclusive(func(*Store) error {
			close(started)
			<-finish
			return nil
		})
	}()
	<-started
	if err := other.WithExclusive(func(*Store) error { return nil }); err == nil {
		t.Fatal("second store acquired a held profile lock")
	}
	if err := otherProfile.WithExclusive(func(*Store) error { return nil }); err != nil {
		t.Fatalf("different profile lock failed: %v", err)
	}
	close(finish)
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
	if err := other.WithExclusive(func(*Store) error { return nil }); err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
}

func TestHomeValidationNormalizationAndSafeInvalidation(t *testing.T) {
	store, root := openTestStore(t)
	home, err := store.CreateHome()
	if err != nil {
		t.Fatal(err)
	}
	nativeDir := filepath.Join(home, "native")
	if err := os.Mkdir(nativeDir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(nativeDir, "metadata")
	if err := os.WriteFile(file, []byte("native"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateHome(); err == nil {
		t.Fatal("ValidateHome accepted native broad permissions")
	}
	if err := store.NormalizeNativeHome(); err == nil {
		t.Fatal("NormalizeNativeHome succeeded without profile lock")
	}
	if err := store.WithExclusive(func(locked *Store) error { return locked.NormalizeNativeHome() }); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateHome(); err != nil {
		t.Fatal(err)
	}
	if mode := fileMode(t, file); mode != 0600 {
		t.Fatalf("normalized file mode %#o, want 0600", mode)
	}
	if mode := fileMode(t, nativeDir); mode != 0700 {
		t.Fatalf("normalized directory mode %#o, want 0700", mode)
	}
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := store.ValidateHome(); err == nil {
		t.Fatal("ValidateHome accepted a symlink")
	}
	if err := store.Invalidate(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(outside)
	if err != nil || string(contents) != "preserve" {
		t.Fatalf("invalidation changed symlink target: %q, %v", contents, err)
	}
}

func TestNativeNormalizationValidatesWholeTreeBeforeChangingModes(t *testing.T) {
	store, _ := openTestStore(t)
	home, err := store.CreateHome()
	if err != nil {
		t.Fatal(err)
	}
	ordinary := filepath.Join(home, "ordinary")
	unsafe := filepath.Join(home, "unsafe")
	if err := os.WriteFile(ordinary, []byte("first"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ordinary, unsafe); err != nil {
		t.Fatal(err)
	}
	if err := store.WithExclusive(func(locked *Store) error {
		return locked.NormalizeNativeHome()
	}); err == nil {
		t.Fatal("normalizer accepted a directory where a regular native file was expected")
	}
	if mode := fileMode(t, ordinary); mode != 0644 {
		t.Fatalf("normalizer changed an earlier entry before finding unsafe state: %#o", mode)
	}
}

func TestNativeNormalizationRejectsUnsafeEntriesBeforeChangingAnyModes(t *testing.T) {
	for _, unsafeKind := range []string{"symlink", "hardlink", "fifo", "special_mode"} {
		t.Run(unsafeKind, func(t *testing.T) {
			store, root := openTestStore(t)
			home, err := store.CreateHome()
			if err != nil {
				t.Fatal(err)
			}
			ordinary := filepath.Join(home, "ordinary")
			if err := os.WriteFile(ordinary, []byte("native"), 0644); err != nil {
				t.Fatal(err)
			}
			unsafe := filepath.Join(home, "unsafe")
			outside := filepath.Join(root, "outside")
			if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
				t.Fatal(err)
			}
			switch unsafeKind {
			case "symlink":
				err = os.Symlink(outside, unsafe)
			case "hardlink":
				err = os.Link(outside, unsafe)
			case "fifo":
				err = syscall.Mkfifo(unsafe, 0600)
			case "special_mode":
				err = os.WriteFile(unsafe, []byte("special"), 0600)
				if err == nil {
					err = os.Chmod(unsafe, os.ModeSetuid|0600)
					if err == nil {
						info, statErr := os.Lstat(unsafe)
						if statErr != nil || info.Mode()&os.ModeSetuid == 0 {
							err = errors.New("filesystem did not preserve special mode fixture")
						}
					}
				}
			}
			if err != nil {
				t.Skipf("cannot create unsafe fixture: %v", err)
			}
			if err := store.WithExclusive(func(locked *Store) error { return locked.NormalizeNativeHome() }); err == nil {
				t.Fatal("normalizer accepted an unsafe native entry")
			}
			if mode := fileMode(t, ordinary); mode != 0644 {
				t.Fatalf("normalizer changed an ordinary entry before rejecting %s: %#o", unsafeKind, mode)
			}
			if unsafeKind == "hardlink" {
				if contents, err := os.ReadFile(outside); err != nil || string(contents) != "outside" {
					t.Fatalf("hardlink validation changed outside file: %q %v", contents, err)
				}
			}
		})
	}
}

func TestExecutionJournalUsesPHPIdentityAndRequiresLock(t *testing.T) {
	store, _ := openTestStore(t)
	home, err := store.CreateHome()
	if err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(home, "credential")
	if err := os.WriteFile(credential, []byte("synthetic"), 0644); err != nil {
		t.Fatal(err)
	}
	claim := protocol.Claim{
		RunID: "01k4w000000000000000000003", AttemptID: "01k4w000000000000000000004", Fence: 5,
		LeaseExpiresAt: time.Now().Add(45 * time.Second), Deadline: time.Now().Add(15 * time.Minute),
		Manifest: map[string]any{"profile_id": testProfileID},
	}
	if err := store.ReserveExecution(claim); err == nil {
		t.Fatal("ReserveExecution succeeded without lock")
	}
	if err := store.WithExclusive(func(locked *Store) error {
		if err := locked.ReserveExecution(claim); err != nil {
			return err
		}
		matched, err := locked.AssertExecutionMatches(claim)
		if err != nil || !matched {
			return errors.New("execution identity did not match its reservation")
		}
		wrong := claim
		wrong.Fence++
		if _, err := locked.AssertExecutionMatches(wrong); err == nil {
			return errors.New("execution identity accepted a different fence")
		}
		if err := locked.ReserveExecution(claim); err == nil {
			return errors.New("duplicate execution reservation succeeded")
		}
		return locked.ReleaseExecution(claim)
	}); err != nil {
		t.Fatal(err)
	}
	if value, err := store.Read("execution"); err != nil || value != nil {
		t.Fatalf("execution reservation remained after release: %v %v", value, err)
	}
	if mode := fileMode(t, credential); mode != 0600 {
		t.Fatalf("execution release did not normalize the home: %#o", mode)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
