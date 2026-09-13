package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	result, err := CollectPatch(before, after, []byte("diff --git a/changed.txt b/changed.txt\n"))
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

func TestCollectPatchNoChangesReturnsNil(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "same", "x", 0600)
	result, err := CollectPatch(root, root, nil)
	if err != nil || result != nil {
		t.Fatalf("got %#v, %v", result, err)
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
