package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedContractInventory(t *testing.T) {
	raw, err := os.ReadFile("contracts-source.json")
	if err != nil {
		t.Fatal(err)
	}
	var pin struct {
		Repository string `json:"repository"`
		Commit     string `json:"commit"`
		Files      map[string]struct {
			Source string `json:"source"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &pin); err != nil {
		t.Fatal(err)
	}
	if pin.Repository == "" || len(pin.Commit) != 40 || len(pin.Files) == 0 {
		t.Fatal("missing upstream contract provenance")
	}
	visited := make(map[string]bool)
	for _, root := range []string{"contracts/v1", "testdata/contracts/v1"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			path = filepath.ToSlash(path)
			pinned, found := pin.Files[path]
			if !found {
				t.Errorf("untracked contract asset %s", path)
				return nil
			}
			visited[path] = true
			if pinned.Source != strings.TrimPrefix(path, "testdata/") {
				t.Errorf("incorrect upstream path for %s", path)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(content)
			if hex.EncodeToString(sum[:]) != pinned.SHA256 {
				t.Errorf("contract asset differs from pinned upstream: %s", path)
			}
			if strings.HasSuffix(path, ".schema.json") {
				embedded, err := schemaBytes(strings.TrimSuffix(filepath.Base(path), ".schema.json"))
				if err != nil || !bytes.Equal(content, embedded) {
					t.Errorf("runtime schema differs from corpus: %s", path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for path := range pin.Files {
		if !visited[path] {
			t.Errorf("missing pinned contract asset %s", path)
		}
	}
}
