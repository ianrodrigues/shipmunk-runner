package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

func TestExecutionInputsSelectHeadAndReadPrivateTrustedBundle(t *testing.T) {
	workspace, claim := trustedInputFixture(t)
	if err := os.Mkdir(filepath.Join(workspace, "sources", "1"), 0700); err != nil {
		t.Fatal(err)
	}
	source, trusted, err := executionInputs(claim, workspace)
	if err != nil || source != filepath.Join(workspace, "sources", "1") || trusted != "Approved agents." {
		t.Fatalf("inputs = %q %q %v", source, trusted, err)
	}
}

func TestExecutionInputsRejectUnsafeSourceAndWorkspaceRoots(t *testing.T) {
	workspace, claim := trustedInputFixture(t)
	target := t.TempDir()
	if err := os.Remove(filepath.Join(workspace, "sources", "0")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(workspace, "sources", "0")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executionInputs(claim, workspace); err == nil {
		t.Fatal("accepted a symbolic-link repository source")
	}
	parent := t.TempDir()
	link := filepath.Join(parent, "workspace")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executionInputs(claim, link); err == nil {
		t.Fatal("accepted a symbolic-link workspace root")
	}
}

func TestTrustedBundleRejectsSymlinkFIFOHardlinkAndOpenMode(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{"symlink", func(t *testing.T, path string) {
			replaceWith(t, path, func() error { return os.Symlink("target", path) })
		}},
		{"fifo", func(t *testing.T, path string) {
			replaceWith(t, path, func() error { return syscall.Mkfifo(path, 0600) })
		}},
		{"hardlink", func(t *testing.T, path string) {
			other := filepath.Join(filepath.Dir(filepath.Dir(path)), "other")
			if err := os.Link(path, other); err != nil {
				t.Fatal(err)
			}
		}},
		{"open mode", func(t *testing.T, path string) {
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, claim := trustedInputFixture(t)
			test.setup(t, filepath.Join(workspace, "instructions", "0.json"))
			if _, err := trustedInstructions(claim, workspace); err == nil {
				t.Fatal("accepted unsafe trusted instruction file")
			}
		})
	}
}

func TestTrustedBundleRejectsReplacementAfterInspection(t *testing.T) {
	workspace, _ := trustedInputFixture(t)
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	path := filepath.Join(workspace, "instructions", "0.json")
	if _, err := readTrustedBundle(root, func() {
		if err := os.Rename(path, path+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
	}); err == nil {
		t.Fatal("accepted a trusted bundle replaced after inspection")
	}
}

func trustedInputFixture(t *testing.T) (string, protocol.Claim) {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "sources", "0"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "instructions"), 0700); err != nil {
		t.Fatal(err)
	}
	instructions, agents := "Approved instructions.", "Approved agents."
	configHash, revision := strings.Repeat("a", 64), strings.Repeat("b", 40)
	claim := protocol.Claim{Manifest: map[string]any{
		"instruction_artifacts": []any{map[string]any{"artifact_id": "fixture"}},
		"effective_config":      map[string]any{"instructions": instructions, "instructions_sha256": digestText(instructions), "effective_configuration_sha256": configHash, "trusted_revision": revision},
	}}
	bundle := map[string]any{
		"version": 1, "trusted_revision": revision, "effective_configuration_sha256": configHash,
		"effective_instructions": map[string]any{"contents": instructions, "sha256": digestText(instructions)},
		"trusted_files":          map[string]any{"AGENTS.md": map[string]any{"path": "AGENTS.md", "contents": agents, "sha256": digestText(agents)}},
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "instructions", "0.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return workspace, claim
}

func replaceWith(t *testing.T, path string, replacement func() error) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := replacement(); err != nil {
		t.Fatal(err)
	}
}

func digestText(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
