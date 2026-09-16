package command

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
)

func TestRunnerLocksAttemptStateBeforeReadingActivationGuardedToken(t *testing.T) {
	original := runnerStartupAfterAttemptLock
	t.Cleanup(func() { runnerStartupAfterAttemptLock = original })

	for _, driver := range []string{"fixture", "codex"} {
		t.Run(driver, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			token := filepath.Join(root, "execution.token")
			if err := os.WriteFile(token, []byte("1|synthetic-token"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".activation.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			locked, release := make(chan struct{}), make(chan struct{})
			runnerStartupAfterAttemptLock = func() {
				close(locked)
				<-release
			}
			options := RunnerOptions{BaseURL: "https://runner.example", TokenFile: token, StateDir: filepath.Join(root, "state"), Image: "image", Driver: driver, ProfilesDir: filepath.Join(root, "profiles"), RepositoryImage: "image"}
			result := make(chan error, 1)
			go func() {
				var prepareErr error
				if driver == "fixture" {
					_, _, prepareErr = prepareFixtureRunner(context.Background(), options, nil)
				} else {
					_, _, prepareErr = prepareCodexRunner(context.Background(), options, nil)
				}
				result <- prepareErr
			}()
			<-locked
			contender, contenderErr := attemptstate.Open(filepath.Join(options.StateDir, "active-attempt.json"))
			if contender != nil {
				_ = contender.Close()
			}
			if contenderErr == nil {
				t.Fatal("installer could acquire attempt lock after runner startup passed its lock point")
			}
			close(release)
			if err := <-result; err == nil {
				t.Fatal("runner accepted token during incomplete activation")
			}
		})
	}
}

func TestProfileLocksLifetimeStoreBeforeReadingActivationGuardedToken(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("native profile startup refuses root")
	}
	original := profileStartupAfterLock
	t.Cleanup(func() { profileStartupAfterLock = original })
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(root, "profile.token")
	if err := os.WriteFile(token, []byte("1|synthetic-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".activation.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles := filepath.Join(root, "profiles")
	locked, release := make(chan struct{}), make(chan struct{})
	profileStartupAfterLock = func() {
		close(locked)
		<-release
	}
	options := ProfileOptions{BaseURL: "https://runner.example", TokenFile: token, ProfilesDir: profiles, Image: "image", Profile: testProfileID, Operation: "probe", OperationID: testOperation}
	result := make(chan error, 1)
	go func() {
		_, err := executeNativeProfile(options, &bytes.Buffer{})
		result <- err
	}()
	<-locked
	contender, err := profile.Open(profiles, testProfileID)
	if err != nil {
		t.Fatal(err)
	}
	err = contender.WithExclusive(func(*profile.Store) error { return nil })
	_ = contender.Close()
	if err == nil {
		t.Fatal("installer could acquire profile lock after profile startup passed its lock point")
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("profile accepted token during incomplete activation")
	}
}
