package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

// Artifact is bounded normalized output, not a filesystem path supplied by an agent.
type Artifact struct {
	Kind   string
	Bytes  []byte
	SHA256 string
}

// Execution is detached from native output and validated before publication begins.
type Execution struct {
	Events    []json.RawMessage
	Artifacts []Artifact
	Result    map[string]any
}

// DecodeExecution decodes and validates normalized fixture output for claim. A
// nonzero exit code normalizes the result to incomplete and discards patch
// artifacts so partial changes cannot be published.
func DecodeExecution(claim protocol.Claim, exitCode int, output []byte) (Execution, error) {
	value, err := protocol.Decode(output, protocol.ResultMaxBytes)
	if err != nil {
		return Execution{}, errors.New("native output is invalid")
	}
	envelope, ok := value.(map[string]any)
	if !ok {
		return Execution{}, errors.New("native output must be an object")
	}
	for key := range envelope {
		if key != "result" && key != "events" && key != "artifacts" {
			return Execution{}, errors.New("native output has an unsupported field")
		}
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok || !sameIdentity(result, claim, true) {
		return Execution{}, errors.New("native result does not match active attempt")
	}
	failedExit := exitCode != 0
	if failedExit {
		result["outcome"] = "incomplete"
		result["summary"] = "Native executable exited unsuccessfully."
		result["findings"] = []any{}
		result["patch_artifact"] = nil
		result["tests"] = []any{}
		result["usage"] = nil
		// incomplete forbids charter_version/questions/verification_state and
		// only optionally carries coverage; a native result overwritten here
		// may still carry the fields its original (pre-failure) outcome required.
		delete(result, "charter_version")
		delete(result, "coverage")
		delete(result, "verification_state")
		delete(result, "questions")
	}
	execution := Execution{Result: result}
	if raw, exists := envelope["events"]; exists && !failedExit {
		events, ok := raw.([]any)
		if !ok {
			return Execution{}, errors.New("native events must be an array")
		}
		for index, event := range events {
			object, ok := event.(map[string]any)
			if !ok || !sameIdentity(object, claim, false) {
				return Execution{}, errors.New("native event does not match active attempt")
			}
			if err := validateJSON("worker-event", object); err != nil {
				return Execution{}, err
			}
			sequence, _ := object["sequence"].(json.Number).Int64()
			expected := int64(index + 1)
			if sequence != expected {
				return Execution{}, errors.New("native event sequences must be contiguous")
			}
			encoded, _ := json.Marshal(object)
			execution.Events = append(execution.Events, encoded)
		}
	}
	if raw, exists := envelope["artifacts"]; exists && !failedExit {
		artifacts, ok := raw.([]any)
		if !ok {
			return Execution{}, errors.New("native artifacts must be an array")
		}
		for _, item := range artifacts {
			object, ok := item.(map[string]any)
			if !ok || len(object) != 3 {
				return Execution{}, errors.New("native artifact is invalid")
			}
			kind, kindOK := object["kind"].(string)
			body, bodyOK := object["bytes"].(string)
			hash, hashOK := object["sha256"].(string)
			actual := sha256.Sum256([]byte(body))
			if !kindOK || !bodyOK || !hashOK || (kind != "patch" && kind != "native_output") || hex.EncodeToString(actual[:]) != hash || len(body) > protocol.ResultMaxBytes {
				return Execution{}, errors.New("native artifact kind, size or hash is invalid")
			}
			execution.Artifacts = append(execution.Artifacts, Artifact{Kind: kind, Bytes: []byte(body), SHA256: hash})
		}
	}
	patches := 0
	for _, artifact := range execution.Artifacts {
		if artifact.Kind == "patch" {
			patches++
		}
	}
	if patches > 1 || result["patch_artifact"] != nil {
		return Execution{}, errors.New("native patch reference has no unique uploaded artifact")
	}
	if patches != 0 && result["outcome"] != "changes_proposed" {
		return Execution{}, errors.New("native patch is incompatible with result outcome")
	}
	if err := validatePreUploadResult(result, execution.Artifacts); err != nil {
		return Execution{}, err
	}
	return execution, nil
}

// validatePreUploadResult validates the normalized result before publication.
// A changes_proposed envelope may legitimately carry a null patch reference
// because the control plane assigns the artifact ID only after upload. Validate
// a detached copy using a contract-valid provisional reference; runClaim will
// validate the final result again after replacing it with the real upload ID.
func validatePreUploadResult(result map[string]any, artifacts []Artifact) error {
	validationResult := result
	if result["outcome"] == "changes_proposed" && result["patch_artifact"] == nil {
		for _, artifact := range artifacts {
			if artifact.Kind != "patch" {
				continue
			}
			validationResult = make(map[string]any, len(result))
			for key, value := range result {
				validationResult[key] = value
			}
			validationResult["patch_artifact"] = map[string]any{
				"artifact_id": "01k4w000000000000000000099",
				"sha256":      artifact.SHA256,
			}
			break
		}
	}
	return validateJSON("result", validationResult)
}

func sameIdentity(object map[string]any, claim protocol.Claim, run bool) bool {
	fence, ok := object["fence"].(json.Number)
	if !ok {
		return false
	}
	number, err := fence.Int64()
	return err == nil && number == claim.Fence && object["attempt_id"] == claim.AttemptID && (!run || object["run_id"] == claim.RunID) && object["protocol_version"] == protocol.Version
}

func validateJSON(contract string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return errors.New("native document cannot be encoded")
	}
	if err := protocol.Validate(contract, raw); err != nil {
		return fmt.Errorf("invalid native %s: %w", contract, err)
	}
	return nil
}
