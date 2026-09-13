package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
			envelope["artifacts"].([]any)[0].(map[string]any)["sha256"] = string(make([]byte, 64))
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
