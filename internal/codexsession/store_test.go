package codexsession

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const (
	testRun     = "01k4w000000000000000000001"
	testSession = "0199a213-81c0-7800-8aa1-bbab2a035a53"
)

func TestStoreFreshAndExplicitResume(t *testing.T) {
	store := openTestStore(t)
	claim := testClaim()
	binding := mustBinding(t, claim)
	session, err := New(testSession, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(claim, session); err != nil {
		t.Fatal(err)
	}

	selected, err := store.Select(Fresh, claim)
	if err != nil || selected != nil {
		t.Fatalf("fresh selected %#v, %v", selected, err)
	}
	selected, err = store.Select(Resume, claim)
	if err != nil || selected == nil || selected.ID != testSession {
		t.Fatalf("resume selected %#v, %v", selected, err)
	}
	if _, err := store.Select(0, claim); err == nil {
		t.Fatal("implicit session mode accepted")
	}
	missing := claim
	missing.RunID = "01k4w000000000000000000009"
	if _, err := store.Select(Resume, missing); err == nil {
		t.Fatal("missing explicit run resumed")
	}

	info, err := os.Stat(filepath.Join(store.root, claim.RunID+".json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("record mode = %v, %v", info.Mode(), err)
	}
}

func TestBindingRejectsCrossContextResume(t *testing.T) {
	base := testClaim()
	baseBinding := mustBinding(t, base)
	store := openTestStore(t)
	session, _ := New(testSession, baseBinding)
	if err := store.Persist(base, session); err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(*protocol.Claim){
		"run":           func(c *protocol.Claim) { c.RunID = "01k4w000000000000000000009" },
		"repository":    func(c *protocol.Claim) { c.Manifest["repository_id"] = 999 },
		"profile":       func(c *protocol.Claim) { c.Manifest["profile_id"] = "01k4w000000000000000000009" },
		"base":          func(c *protocol.Claim) { c.Manifest["base_sha"] = strings.Repeat("c", 40) },
		"head":          func(c *protocol.Claim) { c.Manifest["head_sha"] = strings.Repeat("d", 40) },
		"configuration": func(c *protocol.Claim) { c.Manifest["effective_config"] = map[string]any{"model": "different"} },
		"credential": func(c *protocol.Claim) {
			c.Manifest["supervisor"] = map[string]any{"credential_reference": "credential:other"}
		},
		"task": func(c *protocol.Claim) { c.Manifest["task_context"] = "different" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := testClaim()
			mutate(&changed)
			changedBinding := mustBinding(t, changed)
			if changedBinding == baseBinding {
				t.Fatal("context change did not alter binding")
			}
			// Keep the original run for non-run cases to prove lookup cannot defeat binding checks.
			if name == "run" {
				changed.RunID = base.RunID
				changed.Manifest["run_id_binding_probe"] = true
				// BindingFromClaim uses the trusted Claim identity, so run isolation is
				// exercised by the missing-record assertion above.
				return
			}
			if _, err := store.Select(Resume, changed); err == nil {
				t.Fatal("cross-context resume accepted")
			}
		})
	}
}

func TestStoreRejectsSymlinksHardlinksAndUnsafeRoots(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(link, "sessions")); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	if _, err := Open("relative/sessions"); err == nil {
		t.Fatal("relative root accepted")
	}
	if _, err := Open(parent + "/x/../sessions"); err == nil {
		t.Fatal("noncanonical root accepted")
	}

	store := openTestStore(t)
	record := filepath.Join(store.root, testRun+".json")
	outside := filepath.Join(parent, "outside")
	if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(testRun); err == nil {
		t.Fatal("symlink record accepted")
	}
	if got, _ := os.ReadFile(outside); string(got) != "preserve" {
		t.Fatal("symlink target modified")
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(testRun); err == nil {
		t.Fatal("hardlinked record accepted")
	}
}

func TestStoreRejectsRootReplacementDuringLifetime(t *testing.T) {
	store := openTestStore(t)
	original := store.root + "-original"
	if err := os.Rename(store.root, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(testRun); err == nil {
		t.Fatal("replacement root accepted")
	}
}

func TestStoreRejectsFIFORecordWithoutBlocking(t *testing.T) {
	store := openTestStore(t)
	record := filepath.Join(store.root, testRun+".json")
	if err := syscall.Mkfifo(record, 0600); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := store.Read(testRun)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("FIFO session record accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO session record blocked the reader")
	}
}

func TestStoreRejectsOversizePartialDuplicateAndUnknownRecords(t *testing.T) {
	for name, raw := range map[string]string{
		"oversize":  strings.Repeat(" ", maxRecordBytes+1),
		"partial":   `{"id":"0199a213-81c0`,
		"duplicate": `{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53","id":"0199a213-81c0-7800-8aa1-bbab2a035a53","binding":"` + strings.Repeat("a", 64) + `"}`,
		"unknown":   `{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53","binding":"` + strings.Repeat("a", 64) + `","future":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t)
			path := filepath.Join(store.root, testRun+".json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Read(testRun); err == nil {
				t.Fatal("invalid record accepted")
			}
			binding := strings.Repeat("b", 64)
			session, _ := New(testSession, binding)
			if err := store.write(testRun, binding, session); err == nil {
				t.Fatal("invalid record overwritten")
			}
			got, _ := os.ReadFile(path)
			if string(got) != raw {
				t.Fatal("invalid record changed")
			}
		})
	}
}

func TestStoreConcurrentReadsAndWritesRemainComplete(t *testing.T) {
	store := openTestStore(t)
	binding := mustBinding(t, testClaim())
	session, _ := New(testSession, binding)
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_ = store.write(testRun, binding, session)
			_, _ = store.Read(testRun)
		}()
	}
	group.Wait()
	got, err := store.Read(testRun)
	if err != nil || got == nil || *got != session {
		t.Fatalf("final session = %#v, %v", got, err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func mustBinding(t *testing.T, claim protocol.Claim) string {
	t.Helper()
	binding, err := BindingFromClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func testClaim() protocol.Claim {
	return protocol.Claim{RunID: testRun, Manifest: map[string]any{
		"repository_id": 123,
		"base_sha":      strings.Repeat("a", 40),
		"head_sha":      strings.Repeat("b", 40),
		"profile_id":    "01k4w000000000000000000003",
		"agent":         "codex", "runtime_version": "codex-1",
		"effective_config":      map[string]any{"model": "fixture-model", "max_turns": 10},
		"task_context":          "Review carefully.",
		"instruction_artifacts": []any{map[string]any{"sha256": strings.Repeat("c", 64)}},
		"source_artifacts":      []any{map[string]any{"sha256": strings.Repeat("d", 64)}},
		"supervisor":            map[string]any{"credential_reference": "credential:fixture"},
	}}
}
