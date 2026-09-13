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

func TestDecodeExecutionDiscardsPatchArtifactsForFailedExit(t *testing.T) {
	claim := fixtureClaim(t)
	execution, err := DecodeExecution(claim, 1, outputWithPatch(t, claim, "changes_proposed"))
	if err != nil {
		t.Fatalf("failed native execution with a partial patch was not normalized: %v", err)
	}
	if execution.Result["outcome"] != "incomplete" || execution.Result["patch_artifact"] != nil {
		t.Fatalf("failed native execution was not normalized without a patch: %#v", execution.Result)
	}
	if len(execution.Artifacts) != 0 {
		t.Fatalf("failed native execution retained patch artifacts: %#v", execution.Artifacts)
	}
}

func TestDecodeExecutionKeepsUnuploadedPatchReferenceNull(t *testing.T) {
	claim := fixtureClaim(t)
	execution, err := DecodeExecution(claim, 0, outputWithPatch(t, claim, "changes_proposed"))
	if err != nil {
		t.Fatal(err)
	}
	if execution.Result["outcome"] != "changes_proposed" || execution.Result["patch_artifact"] != nil {
		t.Fatal("pre-upload validation invented a published artifact reference")
	}
	if len(execution.Artifacts) != 1 || execution.Artifacts[0].Kind != "patch" {
		t.Fatal("pre-upload validation lost the local patch")
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

	t.Run("validates final completion with the server-assigned ID", func(t *testing.T) {
		s, client, _, _, _, _ := fixtureSupervisor(t)
		claim := *client.claim
		process := s.Sandbox.(*fixtureSandbox).process
		process.output = outputWithPatch(t, claim, "changes_proposed")
		client.uploadID = "not-a-valid-ulid"
		_, err := s.RunOnce(t.Context())
		if err == nil {
			t.Fatal("completed with an invalid server-assigned artifact ID")
		}
		if client.uploads != 1 || client.completions != 0 {
			t.Fatalf("invalid final reference was published: uploads=%d completions=%d", client.uploads, client.completions)
		}
	})

	t.Run("failed exit publishes incomplete without uploading patch", func(t *testing.T) {
		s, client, _, _, _, _ := fixtureSupervisor(t)
		claim := *client.claim
		process := s.Sandbox.(*fixtureSandbox).process
		process.output = outputWithPatch(t, claim, "changes_proposed")
		process.exit = 1
		outcome, err := s.RunOnce(t.Context())
		if err != nil || outcome.Result != "incomplete" {
			t.Fatalf("failed native execution did not publish incomplete: %+v %v", outcome, err)
		}
		if client.uploads != 0 || client.completions != 1 || client.completed["outcome"] != "incomplete" || client.completed["patch_artifact"] != nil {
			t.Fatalf("failed patch was uploaded or published: uploads=%d completions=%d result=%#v", client.uploads, client.completions, client.completed)
		}
	})
}
