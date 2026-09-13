package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeExecutionRejectsCrossAttemptResultIdentity(t *testing.T) {
	claim := fixtureClaim(t)
	for name, mutate := range map[string]func(map[string]any){
		"run":      func(result map[string]any) { result["run_id"] = "01k4w000000000000000000099" },
		"attempt":  func(result map[string]any) { result["attempt_id"] = "01k4w000000000000000000099" },
		"fence":    func(result map[string]any) { result["fence"] = claim.Fence + 1 },
		"protocol": func(result map[string]any) { result["protocol_version"] = "shipmunk.runner.v999" },
	} {
		t.Run(name, func(t *testing.T) {
			var envelope map[string]any
			if err := json.Unmarshal(normalizedOutput(t, claim, "no_findings"), &envelope); err != nil {
				t.Fatal(err)
			}
			mutate(envelope["result"].(map[string]any))
			if _, err := DecodeExecution(claim, 0, marshalEnvelope(t, envelope)); err == nil {
				t.Fatal("accepted a result bound to a different execution authority")
			}
		})
	}
}

func TestDecodeExecutionRejectsCrossAttemptEventIdentity(t *testing.T) {
	claim := fixtureClaim(t)
	for name, mutate := range map[string]func(map[string]any){
		"attempt":  func(event map[string]any) { event["attempt_id"] = "01k4w000000000000000000099" },
		"fence":    func(event map[string]any) { event["fence"] = claim.Fence + 1 },
		"protocol": func(event map[string]any) { event["protocol_version"] = "shipmunk.runner.v999" },
	} {
		t.Run(name, func(t *testing.T) {
			var envelope map[string]any
			if err := json.Unmarshal(normalizedOutputWithEvents(t, claim, "no_findings", 1), &envelope); err != nil {
				t.Fatal(err)
			}
			mutate(envelope["events"].([]any)[0].(map[string]any))
			if _, err := DecodeExecution(claim, 0, marshalEnvelope(t, envelope)); err == nil {
				t.Fatal("accepted an event bound to a different execution authority")
			}
		})
	}
}

func TestDecodeExecutionRejectsArtifactSubstitutionAndAmbiguity(t *testing.T) {
	claim := fixtureClaim(t)
	patchBody := "diff --git a/example b/example\n"
	patchHash := sha256.Sum256([]byte(patchBody))
	artifact := func() map[string]any {
		return map[string]any{"kind": "patch", "bytes": patchBody, "sha256": hex.EncodeToString(patchHash[:])}
	}
	for name, mutate := range map[string]func(map[string]any){
		"hash substitution": func(envelope map[string]any) {
			envelope["artifacts"] = []any{artifact()}
			envelope["artifacts"].([]any)[0].(map[string]any)["sha256"] = strings.Repeat("0", 64)
		},
		"unsupported metadata": func(envelope map[string]any) {
			value := artifact()
			value["path"] = "../repository"
			envelope["artifacts"] = []any{value}
		},
		"ambiguous patches": func(envelope map[string]any) {
			envelope["artifacts"] = []any{artifact(), artifact()}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var envelope map[string]any
			if err := json.Unmarshal(normalizedOutput(t, claim, "changes_proposed"), &envelope); err != nil {
				t.Fatal(err)
			}
			mutate(envelope)
			if _, err := DecodeExecution(claim, 0, marshalEnvelope(t, envelope)); err == nil {
				t.Fatal("accepted substituted or ambiguous native artifacts")
			}
		})
	}
}

func TestDecodeExecutionRejectsPatchlessChangeResult(t *testing.T) {
	claim := fixtureClaim(t)
	if _, err := DecodeExecution(claim, 0, normalizedOutput(t, claim, "changes_proposed")); err == nil {
		t.Fatal("accepted changes_proposed without exactly one verified patch")
	}
}

func TestDecodeExecutionDiscardsUntrustedDiagnosticsAfterFailedExit(t *testing.T) {
	claim := fixtureClaim(t)
	var envelope map[string]any
	if err := json.Unmarshal(normalizedOutputWithEvents(t, claim, "no_findings", 1), &envelope); err != nil {
		t.Fatal(err)
	}
	result := envelope["result"].(map[string]any)
	result["tests"] = []any{map[string]any{"command": "SYNTHETIC_PRIVATE_PROVIDER_TEXT", "status": "passed", "summary": "private"}}
	body := "SYNTHETIC_PRIVATE_PROVIDER_TEXT"
	hash := sha256.Sum256([]byte(body))
	envelope["artifacts"] = []any{map[string]any{"kind": "native_output", "bytes": body, "sha256": hex.EncodeToString(hash[:])}}

	execution, err := DecodeExecution(claim, 1, marshalEnvelope(t, envelope))
	if err != nil {
		t.Fatal(err)
	}
	if len(execution.Events) != 0 || len(execution.Artifacts) != 0 {
		t.Fatalf("failed native execution retained diagnostics: %#v %#v", execution.Events, execution.Artifacts)
	}
	if tests, ok := execution.Result["tests"].([]any); !ok || len(tests) != 0 {
		t.Fatalf("failed native execution retained tests: %#v", execution.Result["tests"])
	}
}

func TestDecodeExecutionRejectsNativePatchReferenceBeforeUpload(t *testing.T) {
	claim := fixtureClaim(t)
	var envelope map[string]any
	if err := json.Unmarshal(outputWithPatch(t, claim, "changes_proposed"), &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["result"].(map[string]any)["patch_artifact"] = map[string]any{
		"artifact_id": "01k4w000000000000000000099",
		"sha256":      envelope["artifacts"].([]any)[0].(map[string]any)["sha256"],
	}
	if _, err := DecodeExecution(claim, 0, marshalEnvelope(t, envelope)); err == nil {
		t.Fatal("accepted a native-selected artifact identity")
	}
}
