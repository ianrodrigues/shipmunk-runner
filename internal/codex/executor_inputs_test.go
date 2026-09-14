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
	claim.Manifest["kind"] = "review"
	claim.Manifest["source_artifacts"] = sourceReferences(2)
	if err := os.Mkdir(filepath.Join(workspace, "sources", "1"), 0700); err != nil {
		t.Fatal(err)
	}
	sources, trusted, err := executionInputs(claim, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer sources.close()
	if sources.head.Name() != filepath.Join(workspace, "sources", "1") || sources.baselinePath() != filepath.Join(workspace, "sources", "0") || trusted != "Approved agents." {
		t.Fatalf("inputs = %q %q", sources.head.Name(), trusted)
	}
}

func TestClaimCommandBudgetUsesValidatedMaxTurns(t *testing.T) {
	claim := protocol.Claim{Manifest: map[string]any{"effective_config": map[string]any{"max_turns": json.Number("3")}}}
	if budget, err := claimCommandBudget(claim); err != nil || budget != 3 {
		t.Fatalf("budget = %d, %v", budget, err)
	}
	for _, value := range []any{nil, 3, json.Number("0"), json.Number("1001"), json.Number("1.5")} {
		claim.Manifest["effective_config"].(map[string]any)["max_turns"] = value
		if _, err := claimCommandBudget(claim); err == nil {
			t.Fatalf("accepted invalid max_turns %#v", value)
		}
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
		"kind": "implement", "source_artifacts": sourceReferences(1),
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

func sourceReferences(count int) []any {
	refs := []any{map[string]any{"artifact_id": "01k4w000000000000000000005", "sha256": strings.Repeat("c", 64)}}
	if count == 2 {
		refs = append(refs, map[string]any{"artifact_id": "01k4w000000000000000000006", "sha256": strings.Repeat("d", 64)})
	}
	return refs
}

func TestExecutionInputsRequireClaimedSnapshots(t *testing.T) {
	for _, test := range []struct {
		name, kind              string
		references, directories int
		valid                   bool
	}{
		{"review", "review", 2, 2, true},
		{"review missing head", "review", 2, 1, false},
		{"review missing reference", "review", 1, 1, false},
		{"implement", "implement", 1, 1, true},
		{"fix", "fix", 1, 1, true},
		{"implement extra source", "implement", 1, 2, false},
		{"implement extra reference", "implement", 2, 2, false},
		{"unknown kind", "unknown", 1, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, claim := trustedInputFixture(t)
			claim.Manifest["kind"] = test.kind
			claim.Manifest["source_artifacts"] = sourceReferences(test.references)
			if test.directories == 2 {
				if err := os.Mkdir(filepath.Join(workspace, "sources", "1"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			sources, _, err := executionInputs(claim, workspace)
			if sources != nil {
				defer sources.close()
			}
			if (err == nil) != test.valid {
				t.Fatalf("sources=%v err=%v", sources, err)
			}
			if test.valid && test.kind != "review" && sources.baseline != nil {
				t.Fatal("single-source run received baseline")
			}
		})
	}
}

func TestExecutionInputsRejectInvalidArchiveIdentitiesAndEitherSnapshot(t *testing.T) {
	for _, test := range []string{"base identity", "head identity", "base symlink", "head symlink", "missing base", "missing head"} {
		t.Run(test, func(t *testing.T) {
			workspace, claim := trustedInputFixture(t)
			claim.Manifest["kind"] = "review"
			claim.Manifest["source_artifacts"] = sourceReferences(2)
			if err := os.Mkdir(filepath.Join(workspace, "sources", "1"), 0700); err != nil {
				t.Fatal(err)
			}
			index := 0
			if strings.Contains(test, "head") {
				index = 1
			}
			if strings.Contains(test, "identity") {
				claim.Manifest["source_artifacts"].([]any)[index].(map[string]any)["sha256"] = "invalid"
			} else {
				path := filepath.Join(workspace, "sources", string(rune('0'+index)))
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(test, "symlink") {
					if err := os.Symlink(t.TempDir(), path); err != nil {
						t.Fatal(err)
					}
				}
			}
			if sources, _, err := executionInputs(claim, workspace); err == nil {
				sources.close()
				t.Fatal("accepted invalid source inputs")
			}
		})
	}
}
