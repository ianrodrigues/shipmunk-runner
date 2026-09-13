package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestCollectPatchDerivesSortedMetadataAndHashes(t *testing.T) {
	before, after := t.TempDir(), t.TempDir()
	writeFile(t, before, "changed.txt", "old\n", 0644)
	writeFile(t, before, "deleted.sh", "gone\n", 0755)
	writeFile(t, before, "same.txt", "same\n", 0644)
	writeFile(t, after, "changed.txt", "new\n", 0644)
	writeFile(t, after, "new.sh", "added\n", 0755)
	writeFile(t, after, "same.txt", "same\n", 0644)

	result, err := CollectPatch(before, after, trustedPatch(t, before, after))
	if err != nil {
		t.Fatal(err)
	}
	if result.SHA256 == "" || len(result.ChangedFiles) != 3 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.ChangedFiles[0].Path != "changed.txt" || result.ChangedFiles[1].Path != "deleted.sh" || result.ChangedFiles[2].Path != "new.sh" {
		t.Fatalf("changes not sorted: %#v", result.ChangedFiles)
	}
	if result.ChangedFiles[0].BeforeSHA256 == nil || result.ChangedFiles[0].AfterSHA256 == nil || *result.ChangedFiles[0].BeforeSHA256 == *result.ChangedFiles[0].AfterSHA256 {
		t.Fatal("changed hashes not independently derived")
	}
	if result.ChangedFiles[1].AfterMode != nil || result.ChangedFiles[2].BeforeMode != nil || *result.ChangedFiles[2].AfterMode != "100755" {
		t.Fatal("incorrect add/delete metadata")
	}
}

func TestCollectSnapshotsMatchesIndependentPatchVerification(t *testing.T) {
	before, after := t.TempDir(), t.TempDir()
	writeFile(t, before, "file.txt", "before\n", 0644)
	writeFile(t, after, "file.txt", "after\n", 0644)
	generated, err := CollectSnapshots(before, after)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := CollectPatch(before, after, generated.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if generated.SHA256 != verified.SHA256 || !bytes.Equal(generated.Bytes, verified.Bytes) || !reflect.DeepEqual(generated.ChangedFiles, verified.ChangedFiles) {
		t.Fatal("single-pass collection differs from independent supplied-patch verification")
	}
}

func TestCollectPatchRejectsUnsafeEntries(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, string){
		"symbolic link": func(t *testing.T, root string) {
			if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
		},
		"hard link": func(t *testing.T, root string) {
			writeFile(t, root, "one", "x", 0644)
			if err := os.Link(filepath.Join(root, "one"), filepath.Join(root, "two")); err != nil {
				t.Fatal(err)
			}
		},
		"git metadata":   func(t *testing.T, root string) { writeFile(t, root, ".git/config", "x", 0644) },
		"forbidden mode": func(t *testing.T, root string) { writeFile(t, root, "open", "x", 0666) },
	} {
		t.Run(name, func(t *testing.T) {
			before, after := t.TempDir(), t.TempDir()
			setup(t, after)
			if _, err := CollectPatch(before, after, []byte("diff")); err == nil {
				t.Fatal("accepted unsafe entry")
			}
		})
	}
	if safeRepositoryPath(strings.Repeat("a", MaxPathBytes+1)) {
		t.Fatal("accepted oversized repository path")
	}
}

func TestCollectPatchEnforcesPatchAndChangeBounds(t *testing.T) {
	before, after := t.TempDir(), t.TempDir()
	writeFile(t, after, "changed", "x", 0644)
	if _, err := CollectPatch(before, after, nil); err == nil {
		t.Fatal("accepted changes without patch")
	}
	if _, err := CollectPatch(before, before, []byte("diff")); err == nil {
		t.Fatal("accepted patch without changes")
	}
	if _, err := CollectPatch(before, after, make([]byte, MaxPatchBytes+1)); err == nil {
		t.Fatal("accepted oversized patch")
	}
	for index := 0; index <= MaxChangedFiles; index++ {
		writeFile(t, after, filepath.Join("many", fmt.Sprintf("file-%03d", index)), fmt.Sprintf("%d", index), 0644)
	}
	if _, err := CollectPatch(before, after, []byte("diff")); err == nil {
		t.Fatal("accepted too many changed files")
	}
}

func TestCollectPatchRejectsIncompleteOrMismatchedPatch(t *testing.T) {
	before, after := t.TempDir(), t.TempDir()
	writeFile(t, before, "one.txt", "old one\n", 0644)
	writeFile(t, before, "two.txt", "old two\n", 0644)
	writeFile(t, after, "one.txt", "new one\n", 0644)
	writeFile(t, after, "two.txt", "new two\n", 0644)
	complete := trustedPatch(t, before, after)
	if _, err := CollectPatch(before, after, complete[:len(complete)/2]); err == nil {
		t.Fatal("accepted truncated patch")
	}
	partialAfter := t.TempDir()
	writeFile(t, partialAfter, "one.txt", "new one\n", 0644)
	writeFile(t, partialAfter, "two.txt", "old two\n", 0644)
	if _, err := CollectPatch(before, after, trustedPatch(t, before, partialAfter)); err == nil {
		t.Fatal("accepted patch omitting a verified change")
	}
}

func TestGeneratePatchIgnoresUntrustedAttributesAndPreservesCRLF(t *testing.T) {
	before, after := t.TempDir(), t.TempDir()
	attributes := "*.txt text eol=lf\n*.txt filter=attacker diff=attacker\n"
	writeFile(t, before, ".gitattributes", attributes, 0644)
	writeFile(t, after, ".gitattributes", attributes, 0644)
	writeFile(t, before, "message.txt", "before\r\n", 0644)
	writeFile(t, after, "message.txt", "after\r\n", 0644)
	patch := trustedPatch(t, before, after)
	if !bytes.Contains(patch, []byte("+after\r\n")) || bytes.Contains(patch, []byte("+after\n")) {
		t.Fatalf("patch altered CRLF snapshot bytes:\n%s", patch)
	}
	if _, err := CollectPatch(before, after, patch); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratePatchIncludesChangedIgnoredFile(t *testing.T) {
	before, after := t.TempDir(), t.TempDir()
	writeFile(t, before, ".gitignore", "ignored.txt\n", 0644)
	writeFile(t, after, ".gitignore", "ignored.txt\n", 0644)
	writeFile(t, before, "ignored.txt", "before\n", 0644)
	writeFile(t, after, "ignored.txt", "after\n", 0644)
	patch := trustedPatch(t, before, after)
	if !bytes.Contains(patch, []byte("diff --git a/ignored.txt b/ignored.txt")) || !bytes.Contains(patch, []byte("+after\n")) {
		t.Fatalf("patch omitted changed ignored file:\n%s", patch)
	}
	if _, err := CollectPatch(before, after, patch); err != nil {
		t.Fatal(err)
	}
}

func TestCollectPatchRejectsSymlinkRoots(t *testing.T) {
	realBefore, realAfter := t.TempDir(), t.TempDir()
	writeFile(t, realAfter, "changed", "after", 0644)
	for name, beforeLink := range map[string]bool{"before root": true, "after root": false} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			link := filepath.Join(parent, "root")
			target := realAfter
			before, after := realBefore, link
			if beforeLink {
				target, before, after = realBefore, link, realAfter
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if _, err := CollectPatch(before, after, []byte("diff")); err == nil {
				t.Fatal("accepted symlink root")
			}
		})
	}
}

func TestCollectPatchNoChangesReturnsNil(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "same", "x", 0600)
	result, err := CollectPatch(root, root, nil)
	if err != nil || result != nil {
		t.Fatalf("got %#v, %v", result, err)
	}
}

func TestSnapshotPinsRootAcrossParentReplacement(t *testing.T) {
	parent := t.TempDir()
	original := filepath.Join(parent, "snapshot")
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, original, "value", "original", 0644)
	root, err := os.OpenRoot(original)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(original, filepath.Join(parent, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, original, "value", "attacker", 0644)
	states, err := snapshotRoot(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256Hex("original")
	if states["value"].hash != want {
		t.Fatal("snapshot followed replacement parent path")
	}
}

func TestSnapshotFinalOpenRejectsFIFOReplacementWithoutBlocking(t *testing.T) {
	rootPath := t.TempDir()
	writeFile(t, rootPath, "value", "safe", 0644)
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	_, err = snapshotRoot(root, func(path string) {
		if path != "value" {
			return
		}
		if err := os.Remove(filepath.Join(rootPath, path)); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(rootPath, path), 0600); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatal("accepted FIFO swapped after metadata inspection")
	}
}

func TestTrustedGitHonorsLocalContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runGit(ctx, t.TempDir(), t.TempDir(), nil, "version"); err == nil {
		t.Fatal("trusted Git ignored canceled context")
	}
}

func writeFile(t *testing.T, root, relative, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func trustedPatch(t *testing.T, beforeRoot, afterRoot string) []byte {
	t.Helper()
	before, err := snapshot(beforeRoot)
	if err != nil {
		t.Fatal(err)
	}
	after, err := snapshot(afterRoot)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := generatePatch(before, after)
	if err != nil {
		t.Fatal(err)
	}
	return patch
}

func sha256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
