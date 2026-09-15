package command

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

func discardAttemptOptions(t *testing.T, root, baseURL string) RunnerOptions {
	t.Helper()
	token := filepath.Join(root, "execution.token")
	if err := os.WriteFile(token, []byte("123|synthetic-token"), 0600); err != nil {
		t.Fatal(err)
	}
	return RunnerOptions{
		BaseURL:   baseURL,
		TokenFile: token,
		StateDir:  filepath.Join(root, "state"),
		Image:     "sha256:" + strings.Repeat("b", 64),
		Driver:    "codex",
	}
}

func TestDiscardAttemptWithoutJournalIsIdempotent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("discard-attempt refuses root")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	original := setupRuntime
	t.Cleanup(func() { setupRuntime = original })
	setupRuntime.effectiveUID = os.Geteuid

	options := discardAttemptOptions(t, root, "https://runner.example")
	var stdout, stderr bytes.Buffer
	if code := runDiscardAttempt(options, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "No interrupted attempt journal") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestDiscardAttemptRequiresConfirmationUnlessYes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("discard-attempt refuses root")
	}
	for name, test := range map[string]struct {
		confirmed  bool
		stdin      string
		wantKept   bool
		wantAcks   int64
		wantRemove bool
	}{
		"declined":       {stdin: "n\n", wantKept: true},
		"typed yes":      {stdin: "y\n", wantAcks: 1, wantRemove: true},
		"--yes skips it": {confirmed: true, stdin: "", wantAcks: 1, wantRemove: true},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeInterruptedAttempt(t, root)
			workspaceDir := filepath.Join(root, "state", "workspaces", testOperation+"-1")
			if err := os.MkdirAll(workspaceDir, 0700); err != nil {
				t.Fatal(err)
			}
			var acks atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/heartbeat") {
					acks.Add(1)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			original := setupRuntime
			t.Cleanup(func() { setupRuntime = original })
			setupRuntime.effectiveUID = os.Geteuid
			setupRuntime.stdin = strings.NewReader(test.stdin)
			setupRuntime.isTerminal = func(any) bool { return true }

			options := discardAttemptOptions(t, root, server.URL)
			options.Confirmed = test.confirmed
			var stdout, stderr bytes.Buffer
			if code := runDiscardAttempt(options, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}

			store, err := attemptstate.Open(filepath.Join(root, "state", "active-attempt.json"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			saved, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if (saved != nil) != test.wantKept {
				t.Fatalf("journal retained = %t, want %t", saved != nil, test.wantKept)
			}
			if acks.Load() != test.wantAcks {
				t.Fatalf("acknowledgements = %d, want %d", acks.Load(), test.wantAcks)
			}
			_, statErr := os.Stat(workspaceDir)
			if removed := os.IsNotExist(statErr); removed != test.wantRemove {
				t.Fatalf("workspace removed = %t, want %t", removed, test.wantRemove)
			}
		})
	}
}

func TestDiscardAttemptDiscardsJournalEvenWhenAcknowledgementFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("discard-attempt refuses root")
	}
	root := t.TempDir()
	writeInterruptedAttempt(t, root)

	original := setupRuntime
	t.Cleanup(func() { setupRuntime = original })
	setupRuntime.effectiveUID = os.Geteuid
	setupRuntime.stdin = strings.NewReader("")

	// The server is closed before use so the acknowledgement fails fast
	// with a loopback connection refusal instead of a real DNS timeout.
	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable.Close()

	options := discardAttemptOptions(t, root, unreachable.URL)
	options.Confirmed = true
	var stdout, stderr bytes.Buffer
	if code := runDiscardAttempt(options, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "did not confirm the stopped acknowledgement") {
		t.Fatalf("unreachable control plane did not report the unconfirmed acknowledgement: %q", stdout.String())
	}
	store, err := attemptstate.Open(filepath.Join(root, "state", "active-attempt.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if saved, err := store.Load(); err != nil || saved != nil {
		t.Fatalf("journal retained after an unreachable control plane: %+v %v", saved, err)
	}
}

func TestDiscardAttemptRequiresYesOutsideATerminal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("discard-attempt refuses root")
	}
	root := t.TempDir()
	writeInterruptedAttempt(t, root)

	original := setupRuntime
	t.Cleanup(func() { setupRuntime = original })
	setupRuntime.effectiveUID = os.Geteuid
	setupRuntime.stdin = strings.NewReader("y\n")
	setupRuntime.isTerminal = func(any) bool { return false }

	options := discardAttemptOptions(t, root, "https://runner.example")
	var stdout, stderr bytes.Buffer
	if code := runDiscardAttempt(options, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "--yes") {
		t.Fatalf("non-terminal discard without --yes = %d, stderr=%q", code, stderr.String())
	}

	store, err := attemptstate.Open(filepath.Join(root, "state", "active-attempt.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if saved, err := store.Load(); err != nil || saved == nil {
		t.Fatalf("journal discarded without confirmation: %+v %v", saved, err)
	}
}

func TestDiscardAttemptRejectsMismatchedWorkspace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("discard-attempt refuses root")
	}
	root := t.TempDir()
	writeInterruptedAttempt(t, root)
	journalPath := filepath.Join(root, "state", "active-attempt.json")
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	wrongRoot := filepath.Join(t.TempDir(), testOperation+"-1")
	tampered := strings.Replace(string(raw), filepath.Join(root, "state", "workspaces", testOperation+"-1"), wrongRoot, 1)
	if tampered == string(raw) {
		t.Fatal("fixture workspace path was not found")
	}
	if err := os.WriteFile(journalPath, []byte(tampered), 0600); err != nil {
		t.Fatal(err)
	}

	original := setupRuntime
	t.Cleanup(func() { setupRuntime = original })
	setupRuntime.effectiveUID = os.Geteuid

	options := discardAttemptOptions(t, root, "https://runner.example")
	options.Confirmed = true
	var stdout, stderr bytes.Buffer
	if code := runDiscardAttempt(options, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "identity") {
		t.Fatalf("mismatched workspace discard = %d, stderr=%q", code, stderr.String())
	}
	store, err := attemptstate.Open(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if saved, err := store.Load(); err != nil || saved == nil {
		t.Fatalf("journal discarded despite a workspace identity mismatch: %+v %v", saved, err)
	}
}

func TestDiscardAttemptRefusesRootBeforeReadingJournal(t *testing.T) {
	root := t.TempDir()
	writeInterruptedAttempt(t, root)

	original := setupRuntime
	t.Cleanup(func() { setupRuntime = original })
	setupRuntime.effectiveUID = func() int { return 0 }

	options := discardAttemptOptions(t, root, "https://runner.example")
	options.Confirmed = true
	var stdout, stderr bytes.Buffer
	if code := runDiscardAttempt(options, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "non-root") {
		t.Fatalf("root discard = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "state", "active-attempt.json")); err != nil {
		t.Fatalf("root refusal changed the journal: %v", err)
	}
}

func TestDiscardAttemptLeavesUnreadableJournalUntouched(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(stateDir, "active-attempt.json")
	contents := []byte(`{"run_id":`)
	if err := os.WriteFile(journal, contents, 0600); err != nil {
		t.Fatal(err)
	}

	original := setupRuntime
	t.Cleanup(func() { setupRuntime = original })
	setupRuntime.effectiveUID = os.Geteuid

	options := discardAttemptOptions(t, root, "https://runner.example")
	options.Confirmed = true
	var stdout, stderr bytes.Buffer
	if code := runDiscardAttempt(options, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "unreadable") {
		t.Fatalf("unreadable journal discard = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	actual, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(actual, contents) {
		t.Fatalf("unreadable journal changed: %q, %v", actual, err)
	}
}

func TestSandboxIdentityUsesDurableLegacyIDOrCompositeName(t *testing.T) {
	legacyID := strings.Repeat("a", 64)
	for name, test := range map[string]struct {
		state attemptstate.State
		want  string
	}{
		"legacy Docker journal": {
			state: attemptstate.State{AttemptID: testOperation, Fence: 7, SandboxID: &legacyID},
			want:  legacyID,
		},
		"composite driver journal": {
			state: attemptstate.State{AttemptID: testOperation, Fence: 7},
			want:  "shipmunk-codex-" + testOperation + "-7",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sandboxIdentity(test.state); got != test.want {
				t.Fatalf("sandboxIdentity() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDiscardAttemptAcknowledgementUsesJournalFence(t *testing.T) {
	root := t.TempDir()
	type acknowledgement struct {
		ProtocolVersion string `json:"protocol_version"`
		AttemptID       string `json:"attempt_id"`
		Fence           int64  `json:"fence"`
		Stopped         bool   `json:"stopped"`
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/runner/v1/attempts/"+testOperation+"/heartbeat" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer 123|synthetic-token" {
			t.Errorf("Authorization = %q", got)
		}
		var body acknowledgement
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode acknowledgement: %v", err)
		}
		want := acknowledgement{ProtocolVersion: protocol.Version, AttemptID: testOperation, Fence: 7, Stopped: true}
		if body != want {
			t.Errorf("acknowledgement = %+v, want %+v", body, want)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	options := discardAttemptOptions(t, root, server.URL)
	state := attemptstate.State{RunID: testRunnerID, AttemptID: testOperation, Fence: 7}
	if !attemptAcknowledgeStopped(options, state) || requests.Load() != 1 {
		t.Fatalf("acknowledgement success = false or requests = %d", requests.Load())
	}
}
