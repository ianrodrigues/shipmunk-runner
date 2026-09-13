package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

func marshalEnvelope(t *testing.T, envelope map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func outputWithoutTests(t *testing.T, claim protocol.Claim) []byte {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(normalizedOutput(t, claim, "no_findings"), &envelope); err != nil {
		t.Fatal(err)
	}
	delete(envelope["result"].(map[string]any), "tests")
	return marshalEnvelope(t, envelope)
}

func outputWithPatch(t *testing.T, claim protocol.Claim, outcome string) []byte {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(normalizedOutput(t, claim, outcome), &envelope); err != nil {
		t.Fatal(err)
	}
	body := "diff --git a/example b/example\n"
	hash := sha256.Sum256([]byte(body))
	hashText := hex.EncodeToString(hash[:])
	result := envelope["result"].(map[string]any)
	result["patch_artifact"] = map[string]any{
		"artifact_id": "01k4w000000000000000000099",
		"sha256":      hashText,
	}
	envelope["artifacts"] = []any{map[string]any{
		"kind":   "patch",
		"bytes":  body,
		"sha256": hashText,
	}}
	return marshalEnvelope(t, envelope)
}

func TestDecodeExecutionRequiresContiguousEventSequences(t *testing.T) {
	claim := fixtureClaim(t)
	for _, test := range []struct {
		name      string
		sequences []int64
		wantError bool
	}{
		{name: "contiguous", sequences: []int64{1, 2}},
		{name: "gap", sequences: []int64{1, 3}, wantError: true},
		{name: "starts after one", sequences: []int64{2}, wantError: true},
		{name: "duplicate", sequences: []int64{1, 1}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := normalizedOutputWithEvents(t, claim, "no_findings", test.sequences...)
			_, err := DecodeExecution(claim, 0, raw)
			if (err != nil) != test.wantError {
				t.Fatalf("DecodeExecution error = %v, wantError %t", err, test.wantError)
			}
		})
	}
}

func TestDecodeExecutionNormalizesMissingTestsForFailedExit(t *testing.T) {
	claim := fixtureClaim(t)
	raw := outputWithoutTests(t, claim)
	execution, err := DecodeExecution(claim, 1, raw)
	if err != nil {
		t.Fatalf("failed native execution was not normalized: %v", err)
	}
	if execution.Result["outcome"] != "incomplete" {
		t.Fatalf("failed native execution outcome = %v", execution.Result["outcome"])
	}
	if tests, ok := execution.Result["tests"].([]any); !ok || len(tests) != 0 {
		t.Fatalf("failed native execution tests = %#v, want empty array", execution.Result["tests"])
	}
}

func TestPatchPublicationRequiresProposedChangesAndUsesUploadedID(t *testing.T) {
	t.Run("rejects incompatible outcome before upload", func(t *testing.T) {
		s, client, _, _, _, _ := fixtureSupervisor(t)
		claim := *client.claim
		process := s.Sandbox.(*fixtureSandbox).process
		process.output = outputWithPatch(t, claim, "no_findings")
		_, err := s.RunOnce(t.Context())
		if err == nil {
			t.Fatal("published a patch for a no-findings result")
		}
		if client.uploads != 0 || client.completions != 0 {
			t.Fatalf("incompatible patch had side effects: uploads=%d completions=%d", client.uploads, client.completions)
		}
	})

	t.Run("binds completion to uploaded artifact", func(t *testing.T) {
		s, client, _, _, _, _ := fixtureSupervisor(t)
		claim := *client.claim
		process := s.Sandbox.(*fixtureSandbox).process
		process.output = outputWithPatch(t, claim, "changes_proposed")
		client.uploadID = "01k4w000000000000000000099"
		outcome, err := s.RunOnce(t.Context())
		if err != nil || outcome.Result != "changes_proposed" {
			t.Fatalf("valid patch completion failed: %+v %v", outcome, err)
		}
		patch, ok := client.completed["patch_artifact"].(map[string]any)
		if !ok || patch["artifact_id"] != client.uploadID {
			t.Fatalf("completion did not use server-assigned artifact id: %#v", client.completed["patch_artifact"])
		}
		if client.uploads != 1 || client.completions != 1 {
			t.Fatalf("patch publication sequence mismatch: uploads=%d completions=%d", client.uploads, client.completions)
		}
	})
}
