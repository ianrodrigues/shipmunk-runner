package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/supervisor"
)

func newTestCodexProfileRouter(profilesDir string) *codexProfileRouter {
	return &codexProfileRouter{
		profilesDir:      profilesDir,
		nativeImage:      "native:fixture",
		repositoryImage:  "repository:fixture",
		dockerExecutable: "docker",
		active:           make(map[string]supervisor.Executor),
	}
}

func claimForProfile(profileID any) protocol.Claim {
	return protocol.Claim{
		RunID: "01k4w000000000000000000001", AttemptID: "01k4w000000000000000000002", Fence: 1,
		Manifest: map[string]any{"profile_id": profileID},
	}
}

// The router rejects a malformed identity before it touches the profile directory.
// This order keeps unrecoverable state off the disk.
func TestCodexProfileRouterRejectsMalformedProfileIdentity(t *testing.T) {
	root := t.TempDir()
	for _, profileID := range []any{nil, "", "../invalid", 42, []any{}} {
		router := newTestCodexProfileRouter(root)
		if _, err := router.Execute(context.Background(), claimForProfile(profileID), map[string]any{}, t.TempDir()); err == nil {
			t.Fatalf("router accepted malformed profile identity %#v", profileID)
		}
		if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
			t.Fatalf("malformed profile identity %#v touched the profile store: %v %v", profileID, entries, err)
		}
		if len(router.active) != 0 {
			t.Fatalf("malformed profile identity %#v left an active executor", profileID)
		}
	}
}

func TestCodexProfileRouterExecuteFailsWhenProfileIsBusy(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	profilesDir := filepath.Join(root, "profiles")
	profileID := "01k4w000000000000000000003"
	holder, err := profile.Open(profilesDir, profileID)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	router := newTestCodexProfileRouter(profilesDir)
	claim := claimForProfile(profileID)
	if err := holder.WithExclusive(func(*profile.Store) error {
		_, execErr := router.Execute(context.Background(), claim, map[string]any{}, t.TempDir())
		if execErr == nil {
			t.Fatal("router executed while the profile was busy")
		}
		if !strings.Contains(execErr.Error(), "already in use") {
			t.Fatalf("unexpected busy-profile error: %v", execErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
