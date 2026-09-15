package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	t.Run("other profile", func(t *testing.T) {
		root := installation(t)
		other := "01nnnnnnnnnnnnnnnnnnnnnnnn"
		store, err := profile.Open(filepath.Join(root, "profiles"), other)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		err = store.WithExclusive(func(*profile.Store) error {
			if err := (Guard{Root: root, Identity: identity}).Activate(configuration(root, identity, "v1.2.3"), []byte("profile"), []byte("execution")); err == nil {
				t.Fatal("activation ignored another profile lock")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestPreflightRefusalDoesNotChangeExistingLayout(t *testing.T) {
	root := installation(t)
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	mustWrite(t, filepath.Join(root, "config.json"), configuration(root, Identity{BaseURL: "https://other.example", RunnerID: runnerID, ProfileID: profileID}, "v1.2.3"))
	before := treeSnapshot(t, root)
	if err := (Guard{Root: root, Identity: identity}).Preflight(); err == nil {
		t.Fatal("preflight accepted changed identity")
	}
	after := treeSnapshot(t, root)
	if before != after {
		t.Fatalf("preflight changed layout\nbefore: %s\nafter: %s", before, after)
	}
}

// installedArchiveDigest is the archive digest every configuration() fixture
// records as its release_path basename.
var installedArchiveDigest = strings.Repeat("a", 64)

func TestPreflightRefusesOlderReleaseAndAllowsSameOrNewer(t *testing.T) {
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	differentDigest := strings.Repeat("c", 64)
	for _, test := range []struct {
		name                string
		installed, incoming string
		digest              string
		refused             bool
	}{
		{"older patch refused", "v1.2.3", "v1.2.2", installedArchiveDigest, true},
		{"same version same digest allowed", "v1.2.3", "v1.2.3", installedArchiveDigest, false},
		{"same version different digest refused", "v1.2.3", "v1.2.3", differentDigest, true},
		{"newer patch allowed", "v1.2.3", "v1.2.4", installedArchiveDigest, false},
		{"older prerelease refused", "v0.1.0-alpha.10", "v0.1.0-alpha.9", installedArchiveDigest, true},
		{"newer prerelease allowed", "v0.1.0-alpha.9", "v0.1.0-alpha.10", installedArchiveDigest, false},
		{"release supersedes prerelease", "v0.1.0-alpha.10", "v0.1.0", installedArchiveDigest, false},
		{"prerelease after release refused", "v0.1.0", "v0.1.0-alpha.10", installedArchiveDigest, true},
		{"same version with build metadata and same digest allowed", "v1.2.3", "v1.2.3+build.5", installedArchiveDigest, false},
		{"same version with build metadata but different digest refused", "v1.2.3", "v1.2.3+build.5", differentDigest, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := installation(t)
			mustWrite(t, filepath.Join(root, "config.json"), configuration(root, identity, test.installed))
			guard := Guard{Root: root, Identity: identity, IncomingReleaseVersion: test.incoming, IncomingArchiveDigest: test.digest}
			err := guard.Preflight()
			if test.refused && err == nil {
				t.Fatalf("release %s (digest %s) over installed %s was accepted", test.incoming, test.digest, test.installed)
			}
			if !test.refused && err != nil {
				t.Fatalf("release %s (digest %s) over installed %s was refused: %v", test.incoming, test.digest, test.installed, err)
			}
		})
	}
}

func TestValidateReleaseOrderFailsClosedOnMalformedInstalledVersion(t *testing.T) {
	guard := Guard{IncomingReleaseVersion: "v1.0.0", IncomingArchiveDigest: installedArchiveDigest}
	if err := guard.validateReleaseOrder("not-a-version", "releases/"+installedArchiveDigest); err == nil {
		t.Fatal("malformed installed release version was accepted")
	}
}

func TestValidateReleaseOrderFailsClosedOnMissingArchiveDigest(t *testing.T) {
	guard := Guard{IncomingReleaseVersion: "v1.0.0"}
	if err := guard.validateReleaseOrder("v1.0.0", "releases/"+installedArchiveDigest); err == nil {
		t.Fatal("same-version renewal without an incoming archive digest was accepted")
	}
}

func TestCompareReleaseVersionsOrdering(t *testing.T) {
	for _, test := range []struct {
		name string
		a, b string
		want int
	}{
		{"patch older", "v1.2.3", "v1.2.4", -1},
		{"equal", "v1.2.3", "v1.2.3", 0},
		{"major newer", "v2.0.0", "v1.9.9", 1},
		{"prerelease numeric ordering", "v0.1.0-alpha.9", "v0.1.0-alpha.10", -1},
		{"prerelease below release", "v0.1.0-alpha.10", "v0.1.0", -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmp, err := compareReleaseVersions(test.a, test.b)
			if err != nil {
				t.Fatal(err)
			}
			got := 0
			if cmp < 0 {
				got = -1
			} else if cmp > 0 {
				got = 1
			}
			if got != test.want {
				t.Fatalf("compareReleaseVersions(%q, %q) = %d, want sign %d", test.a, test.b, cmp, test.want)
			}
		})
	}
}

func TestCompareReleaseVersionsFailsClosedOnMalformedInput(t *testing.T) {
	for _, test := range []struct{ a, b string }{
		{"not-a-version", "v1.0.0"},
		{"v1.0.0", "not-a-version"},
		{"v1.0", "v1.0.0"},
		{"", "v1.0.0"},
	} {
		if _, err := compareReleaseVersions(test.a, test.b); err == nil {
			t.Fatalf("compareReleaseVersions(%q, %q) accepted malformed input", test.a, test.b)
		}
	}
}

func TestNewProfileCannotStartDuringActivation(t *testing.T) {
	root := installation(t)
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	started := make(chan struct{})
	resume := make(chan struct{})
	result := make(chan error, 1)
	guard := Guard{Root: root, Identity: identity, Rename: func(old, new string) error {
		select {
		case <-started:
		default:
			close(started)
			<-resume
		}
		return os.Rename(old, new)
	}}
	go func() {
		result <- guard.Activate(configuration(root, identity, "v1.2.3"), []byte("profile"), []byte("execution"))
	}()
	<-started
	if store, err := profile.Open(filepath.Join(root, "profiles"), "01nnnnnnnnnnnnnnnnnnnnnnnn"); err == nil {
		store.Close()
		t.Fatal("new profile opened during activation")
	}
	close(resume)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestActivationSyncFailureHasTruthfulOutcome(t *testing.T) {
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	for _, test := range []struct {
		name    string
		failAt  int
		outcome ActivationOutcome
	}{
		{"prepared files", 1, ActivationPreserved},
		{"live files", 3, ActivationIndeterminate},
		{"postcommit cleanup", 5, ActivationCommitted},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := installation(t)
			calls := 0
			guard := Guard{Root: root, Identity: identity, Sync: func(string) error {
				calls++
				if calls == test.failAt {
					return errors.New("injected sync failure")
				}
				return nil
			}}
			err := guard.Activate(configuration(root, identity, "v1.2.3"), []byte("profile"), []byte("execution"))
			var activationErr *ActivationError
			if !errors.As(err, &activationErr) || activationErr.Outcome != test.outcome {
				t.Fatalf("activation error = %#v, want outcome %v", err, test.outcome)
			}
		})
	}
}

func TestJournalWriteFailureRemovesAndSyncsMarkerBeforeCleanup(t *testing.T) {
	root := installation(t)
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	guard := Guard{Root: root, Identity: identity}
	guard.Write = func(path string, raw []byte) error {
		mustWrite(t, path, raw)
		return errors.New("injected journal close failure")
	}
	syncCalls := 0
	guard.Sync = func(string) error {
		syncCalls++
		if syncCalls == 2 {
			return errors.New("injected marker directory sync failure")
		}
		return nil
	}
	err := guard.Activate(configuration(root, identity, "v1.2.3"), []byte("profile"), []byte("execution"))
	var activationErr *ActivationError
	if !errors.As(err, &activationErr) || activationErr.Outcome != ActivationIndeterminate {
		t.Fatalf("activation error = %#v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, activationJournalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed journal marker still blocks runtime")
	}
	if _, err := os.Lstat(guard.pending("config.json")); err != nil {
		t.Fatal("backup assets were removed before marker removal became durable")
	}
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

// The supervised run recovers these journals itself; renewal and profile work must still refuse them.
func TestRuntimeConfigurationAcceptsOnlyRecoverableJournals(t *testing.T) {
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	attempt := `{"run_id":"01kkkkkkkkkkkkkkkkkkkkkkkk","attempt_id":"01mmmmmmmmmmmmmmmmmmmmmmmm","fence":1,"profile_id":"` + profileID +
		`","sandbox_id":null,"lease_expires_at":"2026-09-15T16:29:17+00:00","deadline":"2026-09-15T16:43:14+00:00","workspace":"%s","refused_stopped_count":0}`
	for name, test := range map[string]struct {
		prepare     func(*testing.T, string)
		recoverable bool
	}{
		"interrupted attempt": {recoverable: true, prepare: func(t *testing.T, root string) {
			mustMkdir(t, filepath.Join(root, "state"))
			workspace := filepath.Join(root, "state", "workspaces", "01mmmmmmmmmmmmmmmmmmmmmmmm-1")
			mustWrite(t, filepath.Join(root, "state", "active-attempt.json"), fmt.Appendf(nil, attempt, workspace))
		}},
		"profile execution": {recoverable: true, prepare: func(t *testing.T, root string) {
			mustMkdir(t, filepath.Join(root, "profiles", profileID))
			mustWrite(t, filepath.Join(root, "profiles", profileID, "execution.json"), []byte("{}"))
		}},
		"unreadable attempt": {prepare: func(t *testing.T, root string) {
			mustMkdir(t, filepath.Join(root, "state"))
			mustWrite(t, filepath.Join(root, "state", "active-attempt.json"), []byte("{}"))
		}},
		"profile operation": {prepare: func(t *testing.T, root string) {
			mustMkdir(t, filepath.Join(root, "profiles", profileID))
			mustWrite(t, filepath.Join(root, "profiles", profileID, "pending.json"), []byte("{}"))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			root := installation(t)
			mustWrite(t, filepath.Join(root, "config.json"), configuration(root, identity, "v1.2.3"))
			test.prepare(t, root)
			guard := Guard{Root: root, Identity: identity}
			if _, err := guard.RuntimeConfiguration(); (err == nil) != test.recoverable {
				t.Fatalf("supervised run recovery = %v", err)
			}
			if _, err := guard.Configuration(); err == nil {
				t.Fatal("operational guard accepted a pending journal")
			}
			if err := guard.Validate(); err == nil || guard.Preflight() == nil {
				t.Fatal("renewal accepted a pending journal")
			}
		})
	}
}

func TestGuardRejectsUnknownInstallationShape(t *testing.T) {
	root := installation(t)
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	unknown := `{"image_id":"sha256:` + strings.Repeat("a", 64) + `","base_url":"https://shipmunk.example","runner_id":"` + runnerID + `","profile_id":"` + profileID + `","expires_at":"2099-01-01T00:00:00+00:00"}`
	mustWrite(t, filepath.Join(root, "config.json"), []byte(unknown))

	if err := (Guard{Root: root, Identity: identity}).Preflight(); err == nil {
		t.Fatal("unknown installation shape was accepted")
	}
}

func TestOperationalConfigurationRejectsSevenFieldSchema(t *testing.T) {
	root := installation(t)
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	current := string(configuration(root, identity, "v1.2.3"))
	sevenField := strings.Replace(current, `,"image_id":"sha256:`+strings.Repeat("b", 64)+`"`, "", 1)
	mustWrite(t, filepath.Join(root, "config.json"), []byte(sevenField))

	if _, err := LoadConfiguration(root); err == nil {
		t.Fatal("operational load accepted configuration without an immutable image")
	}
	if _, err := (Guard{Root: root, Identity: identity}).Configuration(); err == nil {
		t.Fatal("operational guard accepted configuration without an immutable image")
	}
	if err := (Guard{Root: root, Identity: identity}).Preflight(); err == nil {
		t.Fatal("preflight accepted a seven-field configuration for renewal")
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

func TestActivateLaunchersPreservesModesAndRollsBackEveryRename(t *testing.T) {
	identity := Identity{BaseURL: "https://shipmunk.example", RunnerID: runnerID, ProfileID: profileID}
	for failure := 1; failure <= 5; failure++ {
		t.Run(fmt.Sprintf("rename-%d", failure), func(t *testing.T) {
			root := installation(t)
			previous := map[string][]byte{
				"config.json": configuration(root, identity, "v1.2.3-old"), "profile.token": []byte("old-profile"),
				"execution.token": []byte("old-execution"), "run": []byte("#!/bin/sh\nold-run\n"), "connect": []byte("#!/bin/sh\nold-connect\n"),
			}
			for name, raw := range previous {
				if name == "run" || name == "connect" {
					if err := os.WriteFile(filepath.Join(root, name), raw, 0700); err != nil {
						t.Fatal(err)
					}
				} else {
					mustWrite(t, filepath.Join(root, name), raw)
				}
			}
			calls := 0
			guard := Guard{Root: root, Identity: identity, Rename: func(old, new string) error {
				calls++
				if calls == failure {
					return errors.New("injected rename failure")
				}
				return os.Rename(old, new)
			}}
			err := guard.ActivateLaunchers(configuration(root, identity, "v1.2.3-new"), []byte("new-profile"), []byte("new-execution"), map[string][]byte{
				"run": []byte("#!/bin/sh\nnew-run\n"), "connect": []byte("#!/bin/sh\nnew-connect\n"),
			})
			if err == nil {
				t.Fatal("injected failure was ignored")
			}
			for name, expected := range previous {
				raw, readErr := os.ReadFile(filepath.Join(root, name))
				if readErr != nil || !bytes.Equal(raw, expected) {
					t.Fatalf("%s = %q, %v", name, raw, readErr)
				}
				mode := os.FileMode(0600)
				if name == "run" || name == "connect" {
					mode = 0700
				}
				if info, statErr := os.Stat(filepath.Join(root, name)); statErr != nil || info.Mode().Perm() != mode {
					t.Fatalf("%s mode = %#o, %v", name, info.Mode().Perm(), statErr)
				}
			}
		})
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
	return []byte("{\"base_url\":\"" + identity.BaseURL + "\",\"runner_id\":\"" + identity.RunnerID + "\",\"profile_id\":\"" + identity.ProfileID + "\",\"expires_at\":\"2099-01-01T00:00:00Z\",\"release_path\":\"" + release + "\",\"release_version\":\"" + version + "\",\"platform\":\"linux-amd64\",\"image_id\":\"sha256:" + strings.Repeat("b", 64) + "\"}")
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

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func treeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var snapshot strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf(&snapshot, "%s:%s:%o\n", strings.TrimPrefix(path, root), info.Mode().Type(), info.Mode().Perm())
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&snapshot, "%x\n", raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.String()
}
