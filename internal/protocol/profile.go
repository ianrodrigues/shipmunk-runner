package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const ProfileControlPlaneMaxBytes = 32 * 1024

// ProfileRequest sends one profile-scoped lifecycle request using the same
// endpoint, headers, timeout, and response checks as the PHP control plane.
// suffix is one of operations, operations/{operation-id}/heartbeat, or
// operations/{operation-id}/completion.
func (client *HTTPClient) ProfileRequest(
	ctx context.Context,
	profileID string,
	suffix string,
	payload map[string]any,
) (map[string]any, error) {
	if err := ValidateProfileID(profileID); err != nil {
		return nil, err
	}
	if !validProfileSuffix(suffix) {
		return nil, fmt.Errorf("invalid profile operation path")
	}
	if _, exists := payload["protocol_version"]; exists {
		return nil, fmt.Errorf("profile payload cannot override protocol_version")
	}

	body := make(map[string]any, len(payload)+1)
	body["protocol_version"] = Version
	for key, value := range payload {
		body[key] = value
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode profile request: %w", err)
	}
	if len(encoded) > ProfileControlPlaneMaxBytes {
		return nil, fmt.Errorf("profile request exceeded its byte limit")
	}
	response, err := client.request(
		ctx,
		http.MethodPost,
		"/runner/v1/profiles/"+profileID+"/"+suffix,
		string(encoded),
		"application/json",
		ProfileControlPlaneMaxBytes,
	)
	if err != nil {
		return nil, err
	}
	if err := expect(response, http.StatusOK, http.StatusCreated); err != nil {
		return nil, err
	}

	value, err := Decode(response.Body, ProfileControlPlaneMaxBytes)
	if err != nil {
		return nil, err
	}
	envelope, err := object(value)
	if err != nil {
		return nil, fmt.Errorf("profile control-plane response data is missing")
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("profile control-plane response data is missing")
	}
	responseProfileID, err := stringField(data, "profile_id")
	if err != nil || responseProfileID != profileID {
		return nil, fmt.Errorf("invalid profile control-plane response")
	}
	return data, nil
}

func validProfileSuffix(suffix string) bool {
	if suffix == "operations" {
		return true
	}
	parts := strings.Split(suffix, "/")
	if len(parts) != 3 || parts[0] != "operations" || (parts[2] != "heartbeat" && parts[2] != "completion") {
		return false
	}
	return ValidateOperationID(parts[1]) == nil
}

// ValidateProfileID accepts only canonical lowercase ULIDs for profile IDs.
func ValidateProfileID(profileID string) error {
	if !ulidPattern.MatchString(profileID) {
		return fmt.Errorf("profile identifier must be a lowercase ULID")
	}
	return nil
}

// ValidateOperationID accepts only canonical lowercase ULIDs for profile
// operation IDs.
func ValidateOperationID(operationID string) error {
	if !ulidPattern.MatchString(operationID) {
		return fmt.Errorf("profile operation identifier must be a lowercase ULID")
	}
	return nil
}
