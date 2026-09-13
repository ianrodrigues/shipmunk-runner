package command

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/supervisor"
)

type commandFixtureExecutor struct {
	executeError error
	cleanupError error
	executes     int
	cleanups     int
}

func (e *commandFixtureExecutor) Execute(context.Context, protocol.Claim, map[string]any, string) (supervisor.Execution, error) {
	e.executes++
	return supervisor.Execution{}, e.executeError
}

func (e *commandFixtureExecutor) Cleanup(context.Context, protocol.Claim) error {
	e.cleanups++
	return e.cleanupError
}

func TestProfileExecutorReservesCleansAndReleases(t *testing.T) {
	store, claim := preparedExecutionProfile(t)
	delegate := &commandFixtureExecutor{executeError: errors.New("synthetic execution failure")}
	executor, err := newProfileExecutor(store, delegate)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), claim, map[string]any{}, t.TempDir())
	if !errors.Is(err, delegate.executeError) || delegate.executes != 1 || delegate.cleanups != 1 {
		t.Fatalf("profile execution lifecycle = executes %d cleanups %d err %v", delegate.executes, delegate.cleanups, err)
	}
	if err := store.WithExclusive(func(locked *profile.Store) error {
		reservation, readErr := locked.Read("execution")
		if readErr != nil {
			return readErr
		}
		if reservation != nil {
			t.Fatal("profile capacity remained reserved after confirmed cleanup")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProfileExecutorRetainsReservationUntilCleanupSucceeds(t *testing.T) {
	store, claim := preparedExecutionProfile(t)
	cleanupFailure := errors.New("synthetic cleanup failure")
	delegate := &commandFixtureExecutor{cleanupError: cleanupFailure}
	executor, _ := newProfileExecutor(store, delegate)
	if _, err := executor.Execute(context.Background(), claim, map[string]any{}, t.TempDir()); !errors.Is(err, cleanupFailure) {
		t.Fatalf("cleanup failure was lost: %v", err)
	}
	if err := store.WithExclusive(func(locked *profile.Store) error {
		matched, matchErr := locked.AssertExecutionMatches(claim)
		if matchErr != nil || !matched {
			t.Fatalf("profile reservation was lost: matched=%t err=%v", matched, matchErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	delegate.cleanupError = nil
	if err := executor.Cleanup(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
}

func TestProfileExecutorRejectsMismatchedCredentialBeforeSideEffects(t *testing.T) {
	store, claim := preparedExecutionProfile(t)
	claim.Manifest["supervisor"].(map[string]any)["credential_reference"] = "credential:other"
	delegate := &commandFixtureExecutor{}
	executor, _ := newProfileExecutor(store, delegate)
	if _, err := executor.Execute(context.Background(), claim, map[string]any{}, t.TempDir()); err == nil || delegate.executes != 0 || delegate.cleanups != 0 {
		t.Fatalf("mismatched profile reached transport: executes=%d cleanups=%d err=%v", delegate.executes, delegate.cleanups, err)
	}
}

func preparedExecutionProfile(t *testing.T) (*profile.Store, protocol.Claim) {
	t.Helper()
	claim := fixtureClaimForCommand(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := profile.Open(filepath.Join(root, "profiles"), claim.Manifest["profile_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	err = store.WithExclusive(func(locked *profile.Store) error {
		if _, err := locked.CreateHome(); err != nil {
			return err
		}
		return locked.Write("active", map[string]any{
			"profile_id": claim.Manifest["profile_id"], "credential_reference": "credential:fixture",
			"agent": "codex", "auth_mode": "subscription", "runtime_version": profile.CodexVersion,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, claim
}

func fixtureClaimForCommand(t *testing.T) protocol.Claim {
	t.Helper()
	claim := protocol.Claim{
		RunID: "01k4w000000000000000000001", AttemptID: "01k4w000000000000000000002", Fence: 7,
		Manifest: map[string]any{
			"profile_id": "01k4w000000000000000000003", "agent": "codex", "runtime_version": profile.CodexVersion,
			"supervisor": map[string]any{"credential_reference": "credential:fixture"},
		},
	}
	return claim
}
