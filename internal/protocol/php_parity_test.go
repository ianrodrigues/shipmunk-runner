//go:build phpbaseline

package protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in test compares only valid synthetic requests with the temporary
// PHP baseline. Go's stricter prevalidation is covered by the normal suite.
func TestPHPBaselineWireParity(t *testing.T) {
	root := filepath.Join("..", "..")
	manifest := readContractFixture(t, "manifest")
	events := readContractFixture(t, "event-batch")
	result := readContractFixture(t, "result")
	claim := Claim{RunID: "01k4w000000000000000000001", AttemptID: "01k4w000000000000000000002", Fence: 1}
	artifact := []byte("synthetic artifact bytes")
	artifactHash := fmt.Sprintf("%x", sha256Sum(artifact))
	artifactID := "01k4w000000000000000000003"
	profileID := "01k4w000000000000000000003"
	operationID := "01k4w000000000000000000004"
	manifestEnvelope := append([]byte(`{"data":`), manifest...)
	manifestEnvelope = append(manifestEnvelope, '}')

	tests := []struct {
		name      string
		operation string
		suffix    string
		payload   map[string]any
		body      []byte
		status    int
		headers   http.Header
	}{
		{name: "claim", operation: "claim", body: manifestEnvelope, status: http.StatusOK},
		{name: "heartbeat", operation: "heartbeat", body: jsonBytes(t, map[string]any{"data": map[string]any{"protocol_version": Version, "attempt_id": claim.AttemptID, "fence": claim.Fence, "state": "running", "lease_expires_at": "2026-09-10T17:01:00Z", "stop_requested": true}}), status: http.StatusOK},
		{name: "stopped", operation: "stopped", status: http.StatusNoContent},
		{name: "events", operation: "events", payload: map[string]any{"events": mustDecode(t, events)}, status: http.StatusNoContent},
		{name: "upload", operation: "upload", payload: map[string]any{"kind": "native_output", "body": string(artifact), "sha256": artifactHash}, body: jsonBytes(t, map[string]any{"data": map[string]any{"id": artifactID}}), status: http.StatusCreated},
		{name: "download", operation: "download", payload: map[string]any{"artifact_id": artifactID, "sha256": artifactHash}, body: artifact, status: http.StatusOK, headers: http.Header{"X-Artifact-Sha256": []string{artifactHash}, "Content-Length": []string{fmt.Sprint(len(artifact))}}},
		{name: "completion", operation: "completion", payload: map[string]any{"result": mustDecode(t, result)}, status: http.StatusNoContent},
		{name: "profile operations", operation: "profile", suffix: "operations", payload: map[string]any{"profile_id": profileID, "suffix": "operations", "payload": map[string]any{"action": "inspect"}}, body: profileResponse(t, profileID), status: http.StatusOK},
		{name: "profile heartbeat", operation: "profile", suffix: "operations/" + operationID + "/heartbeat", payload: map[string]any{"profile_id": profileID, "suffix": "operations/" + operationID + "/heartbeat", "payload": map[string]any{"state": "running"}}, body: profileResponse(t, profileID), status: http.StatusOK},
		{name: "profile completion", operation: "profile", suffix: "operations/" + operationID + "/completion", payload: map[string]any{"profile_id": profileID, "suffix": "operations/" + operationID + "/completion", "payload": map[string]any{"outcome": "completed"}}, body: profileResponse(t, profileID), status: http.StatusCreated},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := capturedResponse{Status: test.status, Headers: flattenHeaders(test.headers), Body: string(test.body)}
			input := oracleInput{Operation: test.operation, Manifest: string(manifest), Payload: test.payload, Response: response}
			phpRequest := runPHPOracle(t, root, input)

			transport := &captureRoundTripper{response: test.headers, status: test.status, body: test.body}
			client, err := NewHTTPClient("https://control.example/base/", "synthetic-token", transport)
			if err != nil {
				t.Fatal(err)
			}
			invokeGoOperation(t, client, test.operation, test.suffix, test.payload, claim, artifact, artifactID, profileID, events, result, time.Date(2026, 9, 10, 17, 0, 0, 0, time.UTC))
			if transport.request == nil {
				t.Fatal("Go client did not issue a request")
			}
			goRequest := capturedRequest{Method: transport.request.Method, Path: transport.request.URL.Path, Headers: selectedHeaders(transport.request.Header), Body: transport.requestBody}
			if !equalCapturedRequests(goRequest, phpRequest) {
				t.Fatalf("wire request differs\nPHP: %#v\nGo:  %#v", phpRequest, goRequest)
			}
		})
	}
}

type oracleInput struct {
	Operation string           `json:"operation"`
	Manifest  string           `json:"manifest"`
	Payload   map[string]any   `json:"payload"`
	Response  capturedResponse `json:"response"`
}

type capturedResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type capturedRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func runPHPOracle(t *testing.T, root string, input oracleInput) capturedRequest {
	t.Helper()
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("php", filepath.Join(root, "tests", "GoProtocolOracle.php"))
	command.Stdin = bytes.NewReader(encoded)
	output, err := command.Output()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			t.Fatalf("PHP baseline oracle failed: %v: %s", err, exitError.Stderr)
		}
		t.Fatalf("run PHP baseline oracle: %v", err)
	}
	var request capturedRequest
	if err := json.Unmarshal(output, &request); err != nil {
		t.Fatalf("decode PHP baseline capture: %v: %s", err, output)
	}
	return request
}

type captureRoundTripper struct {
	request     *http.Request
	requestBody string
	response    http.Header
	status      int
	body        []byte
}

func (transport *captureRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.request = request
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	transport.requestBody = string(body)
	responseBody := append([]byte(nil), transport.body...)
	return &http.Response{StatusCode: transport.status, Header: transport.response.Clone(), Body: io.NopCloser(bytes.NewReader(responseBody)), Request: request}, nil
}

func invokeGoOperation(t *testing.T, client *HTTPClient, operation, suffix string, payload map[string]any, claim Claim, artifact []byte, artifactID, profileID string, events, result []byte, now time.Time) {
	t.Helper()
	switch operation {
	case "claim":
		if _, err := client.Claim(context.Background(), now); err != nil {
			t.Fatal(err)
		}
	case "heartbeat":
		if _, _, err := client.Heartbeat(context.Background(), claim); err != nil {
			t.Fatal(err)
		}
	case "stopped":
		if err := client.AcknowledgeStopped(context.Background(), claim); err != nil {
			t.Fatal(err)
		}
	case "events":
		if err := client.SendEvents(context.Background(), claim, events); err != nil {
			t.Fatal(err)
		}
	case "upload":
		if _, err := client.UploadArtifact(context.Background(), claim, payload["kind"].(string), artifact, payload["sha256"].(string)); err != nil {
			t.Fatal(err)
		}
	case "download":
		if _, err := client.DownloadArtifact(context.Background(), claim, artifactID, payload["sha256"].(string)); err != nil {
			t.Fatal(err)
		}
	case "completion":
		if err := client.Complete(context.Background(), claim, result); err != nil {
			t.Fatal(err)
		}
	case "profile":
		if _, err := client.ProfileRequest(context.Background(), profileID, suffix, payload["payload"].(map[string]any)); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown operation %q", operation)
	}
}

func equalCapturedRequests(left, right capturedRequest) bool {
	if left.Method != right.Method || left.Path != right.Path || !equalStringMaps(left.Headers, right.Headers) {
		return false
	}
	if left.Headers["content-type"] == "application/octet-stream" {
		return left.Body == right.Body
	}
	leftBody, leftErr := decodeCapturedJSON(left.Body)
	rightBody, rightErr := decodeCapturedJSON(right.Body)
	if leftErr != nil || rightErr != nil {
		return left.Body == right.Body
	}
	leftJSON, _ := json.Marshal(leftBody)
	rightJSON, _ := json.Marshal(rightBody)
	return bytes.Equal(leftJSON, rightJSON)
}

func decodeCapturedJSON(body string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	return value, err
}

func selectedHeaders(headers http.Header) map[string]string {
	selected := make(map[string]string)
	for _, name := range []string{"Accept", "Authorization", "Content-Type", "X-Shipmunk-Protocol", "X-Artifact-Kind", "X-Artifact-SHA256", "X-Attempt-Fence", "X-Protocol-Version"} {
		if value := headers.Get(name); value != "" {
			selected[strings.ToLower(name)] = value
		}
	}
	return selected
}

func flattenHeaders(headers http.Header) map[string]string {
	flat := make(map[string]string)
	for name, values := range headers {
		if len(values) > 0 {
			flat[strings.ToLower(name)] = values[0]
		}
	}
	return flat
}

func equalStringMaps(left, right map[string]string) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}

func jsonBytes(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mustDecode(t *testing.T, raw []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func profileResponse(t *testing.T, profileID string) []byte {
	t.Helper()
	return jsonBytes(t, map[string]any{"data": map[string]any{"profile_id": profileID, "status": "active"}})
}

func sha256Sum(value []byte) [32]byte {
	return sha256.Sum256(value)
}
