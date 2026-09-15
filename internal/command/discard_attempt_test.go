package command

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
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
	store, err := attemptstate.Open(filepath.Join(root, "state", "active-attempt.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if saved, err := store.Load(); err != nil || saved != nil {
		t.Fatalf("journal retained after an unreachable control plane: %+v %v", saved, err)
	}
}
