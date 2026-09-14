package install

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
)

const (
	runnerID  = "01kkkkkkkkkkkkkkkkkkkkkkkk"
	profileID = "01mmmmmmmmmmmmmmmmmmmmmmmm"
)

func TestGuardPreservesStateAndProfilesDuringRenewal(t *testing.T) {
	root := installation(t)
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	config := configuration(root, identity, "v1.2.3-new")
	profileHome := filepath.Join(root, "profiles", profileID, "home")
	mustMkdir(t, profileHome)
	mustWrite(t, filepath.Join(profileHome, "auth.json"), []byte("preserve"))
	mustWrite(t, filepath.Join(root, "profiles", profileID, "active.json"), []byte("{}"))

	guard := Guard{Root: root, Identity: identity}
	if err := guard.Activate(config, []byte("new-profile-token"), []byte("new-execution-token")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"config.json": string(config), "profile.token": "new-profile-token", "execution.token": "new-execution-token",
		filepath.Join("profiles", profileID, "home", "auth.json"): "preserve",
		filepath.Join("profiles", profileID, "active.json"):       "{}",
	} {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(raw) != want {
			t.Fatalf("%s = %q, %v", path, raw, err)
		}
	}
}

func TestGuardUsesRuntimeAttemptAndProfileLocks(t *testing.T) {
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	t.Run("attempt", func(t *testing.T) {
		root := installation(t)
		config := configuration(root, identity, "v1.2.3")
		state, err := attemptstate.Open(filepath.Join(root, "state", "active-attempt.json"))
		if err != nil {
			t.Fatal(err)
		}
		defer state.Close()
		if err := (Guard{Root: root, Identity: identity}).Activate(config, []byte("profile"), []byte("execution")); err == nil {
			t.Fatal("activation ignored active runner lock")
		}
		if _, err := os.Stat(filepath.Join(root, "config.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("activation mutated configuration under contention")
		}
	})
	t.Run("profile", func(t *testing.T) {
		root := installation(t)
		config := configuration(root, identity, "v1.2.3")
		store, err := profile.Open(filepath.Join(root, "profiles"), profileID)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		err = store.WithExclusive(func(*profile.Store) error {
			if err := (Guard{Root: root, Identity: identity}).Activate(config, []byte("profile"), []byte("execution")); err == nil {
				t.Fatal("activation ignored active profile lock")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestGuardRecoversDurableIncompleteActivation(t *testing.T) {
	root := installation(t)
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	old := map[string][]byte{"config.json": configuration(root, identity, "v1.2.3-old"), "profile.token": []byte("old-profile"), "execution.token": []byte("old-execution")}
	guard := Guard{Root: root, Identity: identity}
	journal := activationJournal{Version: 1, Files: map[string]bool{}}
	for _, name := range activationOrder {
		mustWrite(t, filepath.Join(root, name), old[name])
		mustWrite(t, guard.backup(name), old[name])
		journal.Files[name] = true
	}
	mustWrite(t, filepath.Join(root, "profile.token"), []byte("new-profile"))
	raw, _ := json.Marshal(journal)
	mustWrite(t, filepath.Join(root, activationJournalName), raw)
	if err := guard.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, expected := range old {
		actual, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(actual) != string(expected) {
			t.Fatalf("%s was not recovered: %q, %v", name, actual, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, activationJournalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recovery journal remained")
	}
}

func TestGuardRejectsDuplicateAndUnknownConfiguration(t *testing.T) {
	root := installation(t)
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	valid := string(configuration(root, identity, "v1.2.3"))
	for _, raw := range []string{
		strings.Replace(valid, `"base_url":`, `"base_url":"https://other.example","base_url":`, 1),
		strings.TrimSuffix(valid, "}") + `,"unknown":true}`,
		strings.Replace(valid, `"expires_at":"2099-01-01T00:00:00Z"`, `"expires_at":"not-a-time"`, 1),
		strings.Replace(valid, `"release_version":"v1.2.3"`, `"release_version":"development"`, 1),
		strings.Replace(valid, `"platform":"linux-amd64"`, `"platform":"windows-amd64"`, 1),
		strings.Replace(valid, filepath.Join(filepath.Dir(filepath.Dir(root)), "releases"), filepath.Join(root, "other-releases"), 1),
	} {
		if err := (Guard{Root: root, Identity: identity}).Activate([]byte(raw), []byte("profile"), []byte("execution")); err == nil {
			t.Fatal("accepted non-strict configuration")
		}
	}
	legacy := `{"image_id":"sha256:old","base_url":"https://shipmunk.example","runner_id":"` + runnerID + `","profile_id":"` + profileID + `"}`
	if _, err := decodeConfiguration([]byte(legacy)); err == nil {
		t.Fatal("accepted legacy runner configuration")
	}
}

func TestPrivateReadRejectsLinksAndSpecialFilesWithoutBlocking(t *testing.T) {
	root := canonicalPrivateTemp(t)
	regular := filepath.Join(root, "regular")
	mustWrite(t, regular, []byte("private"))
	for name, create := range map[string]func(string) error{
		"symlink":  func(path string) error { return os.Symlink(regular, path) },
		"hardlink": func(path string) error { return os.Link(regular, path) },
		"fifo":     func(path string) error { return syscall.Mkfifo(path, 0600) },
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name)
			if err := create(path); err != nil {
				t.Fatal(err)
			}
			if _, err := readPrivateFile(path, 100); err == nil {
				t.Fatal("accepted unsafe private file")
			}
		})
	}
}

func TestGuardRejectsChangedIdentityAndRecoveryJournals(t *testing.T) {
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	for name, prepare := range map[string]func(*testing.T, string){
		"changed server": func(t *testing.T, root string) {
			mustWrite(t, filepath.Join(root, "config.json"), configuration(root, Identity{BaseURL: "https://other.example", RunnerID: runnerID, ProfileID: profileID}, "v1.2.3"))
		},
		"active attempt": func(t *testing.T, root string) {
			mustMkdir(t, filepath.Join(root, "state"))
			mustWrite(t, filepath.Join(root, "state", "active-attempt.json"), []byte("{}"))
		},
		"pending profile": func(t *testing.T, root string) {
			mustMkdir(t, filepath.Join(root, "profiles", profileID))
			mustWrite(t, filepath.Join(root, "profiles", profileID, "pending.json"), []byte("{}"))
		},
		"executing profile": func(t *testing.T, root string) {
			mustMkdir(t, filepath.Join(root, "profiles", profileID))
			mustWrite(t, filepath.Join(root, "profiles", profileID, "execution.json"), []byte("{}"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := installation(t)
			prepare(t, root)
			if err := (Guard{Root: root, Identity: identity}).Validate(); err == nil {
				t.Fatal("unsafe renewal was accepted")
			}
		})
	}
}

func TestActivateRollsBackEveryPriorFileAfterInjectedFailure(t *testing.T) {
	root := installation(t)
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	previous := map[string][]byte{
		"config.json":     configuration(root, identity, "v1.2.3-old"),
		"profile.token":   []byte("old-profile-token"),
		"execution.token": []byte("old-execution-token"),
	}
	for name, raw := range previous {
		mustWrite(t, filepath.Join(root, name), raw)
	}
	calls := 0
	guard := Guard{Root: root, Identity: identity, Rename: func(old, new string) error {
		calls++
		if calls == 2 {
			return errors.New("injected activation failure")
		}
		return os.Rename(old, new)
	}}
	if err := guard.Activate(configuration(root, identity, "v1.2.3-new"), []byte("new-profile-token"), []byte("new-execution-token")); err == nil {
		t.Fatal("injected failure was ignored")
	}
	for name, want := range previous {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(raw) != string(want) {
			t.Fatalf("%s was not rolled back: %q, %v", name, raw, err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".activate") || strings.HasPrefix(entry.Name(), ".rollback") {
			t.Fatalf("temporary activation file remained: %s", entry.Name())
		}
	}
}

func installation(t *testing.T) string {
	t.Helper()
	temporary, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(temporary, runnerID)
	mustMkdir(t, root)
	return root
}

func canonicalPrivateTemp(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func configuration(root string, identity Identity, version string) []byte {
	release := filepath.Join(filepath.Dir(filepath.Dir(root)), "releases", strings.Repeat("a", 64))
	return []byte("{\"base_url\":\"" + identity.BaseURL + "\",\"runner_id\":\"" + identity.RunnerID + "\",\"profile_id\":\"" + identity.ProfileID + "\",\"expires_at\":\"2099-01-01T00:00:00Z\",\"release_path\":\"" + release + "\",\"release_version\":\"" + version + "\",\"platform\":\"linux-amd64\"}")
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
}
