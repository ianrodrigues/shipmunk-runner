package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	runnerID  = "01kkkkkkkkkkkkkkkkkkkkkkkk"
	profileID = "01mmmmmmmmmmmmmmmmmmmmmmmm"
)

func TestGuardPreservesStateAndProfilesDuringRenewal(t *testing.T) {
	root := installation(t)
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	config := configuration(identity, "new-image")
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

func TestGuardRejectsChangedIdentityAndRecoveryJournals(t *testing.T) {
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	for name, prepare := range map[string]func(*testing.T, string){
		"changed server": func(t *testing.T, root string) {
			mustWrite(t, filepath.Join(root, "config.json"), configuration(Identity{BaseURL: "https://other.example", RunnerID: runnerID, ProfileID: profileID}, "image"))
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
		"config.json":     configuration(identity, "old-image"),
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
	if err := guard.Activate(configuration(identity, "new-image"), []byte("new-profile-token"), []byte("new-execution-token")); err == nil {
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

func configuration(identity Identity, image string) []byte {
	return []byte("{\"image_id\":\"" + image + "\",\"base_url\":\"" + identity.BaseURL + "\",\"runner_id\":\"" + identity.RunnerID + "\",\"profile_id\":\"" + identity.ProfileID + "\"}")
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
