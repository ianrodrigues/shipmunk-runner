package protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrProtocolIncompatible identifies an HTTP 426 response through errors.Is.
var ErrProtocolIncompatible = errors.New("runner protocol major is incompatible")

// ControlPlaneError preserves the status for fenced recovery and never includes the untrusted response body.
type ControlPlaneError struct {
	StatusCode int
}

// Error reports the status without exposing the server response body.
func (err *ControlPlaneError) Error() string {
	if err.StatusCode == http.StatusUpgradeRequired {
		return ErrProtocolIncompatible.Error()
	}
	return fmt.Sprintf("control-plane request returned HTTP %d", err.StatusCode)
}

// Unwrap makes protocol incompatibility recognizable without string matching.
func (err *ControlPlaneError) Unwrap() error {
	if err.StatusCode == http.StatusUpgradeRequired {
		return ErrProtocolIncompatible
	}
	return nil
}

// HTTPClient performs bounded, authenticated protocol requests without retries; callers own recovery.
type HTTPClient struct {
	baseURL *url.URL
	token   string
	client  *http.Client
}

// NewHTTPClient requires HTTPS outside loopback development, never follows redirects, and gives every exchange the PHP-compatible five-second timeout.
func NewHTTPClient(baseURL, token string, transport http.RoundTripper) (*HTTPClient, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("control-plane URL is invalid")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHost(parsed.Hostname())) {
		return nil, fmt.Errorf("the control plane must use HTTPS outside local development")
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, fmt.Errorf("runner token is invalid")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &HTTPClient{baseURL: parsed, token: token, client: &http.Client{
		Transport: transport,
		Timeout:   HTTPTimeoutSeconds * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

// Claim requests work and returns (nil, nil) when the queue is idle; the caller must persist the lease.
func (client *HTTPClient) Claim(ctx context.Context, now time.Time) (*Claim, error) {
	response, err := client.json(ctx, http.MethodPost, "/runner/v1/claims", map[string]any{"protocol_version": Version}, ManifestMaxBytes)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	data, err := responseData(response, http.StatusOK, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	claim, err := ClaimFromManifest(data, now)
	if err != nil {
		return nil, err
	}
	return &claim, nil
}

// Heartbeat renews the fenced attempt and returns the lease expiry and stop flag; callers must enforce the deadline.
func (client *HTTPClient) Heartbeat(ctx context.Context, claim Claim) (time.Time, bool, error) {
	if err := validateFence(claim); err != nil {
		return time.Time{}, false, err
	}
	response, err := client.json(ctx, http.MethodPost, "/runner/v1/attempts/"+claim.AttemptID+"/heartbeat", client.fence(claim), ManifestMaxBytes)
	if err != nil {
		return time.Time{}, false, err
	}
	data, err := responseData(response, http.StatusOK)
	if err != nil {
		return time.Time{}, false, err
	}
	for key := range data {
		switch key {
		case "protocol_version", "attempt_id", "fence", "state", "lease_expires_at", "stop_requested", "stop_reason":
		default:
			return time.Time{}, false, fmt.Errorf("heartbeat response contained an unsupported field")
		}
	}
	version, _ := data["protocol_version"].(string)
	attemptID, _ := data["attempt_id"].(string)
	fence, fenceErr := positiveInteger(data["fence"])
	if version != Version || attemptID != claim.AttemptID || fenceErr != nil || fence != claim.Fence {
		return time.Time{}, false, fmt.Errorf("heartbeat response did not match the active fence")
	}
	leaseText, err := stringField(data, "lease_expires_at")
	if err != nil {
		return time.Time{}, false, err
	}
	lease, err := utcTime(leaseText)
	if err != nil {
		return time.Time{}, false, err
	}
	stop, ok := data["stop_requested"].(bool)
	if !ok {
		return time.Time{}, false, fmt.Errorf("heartbeat stop_requested is invalid")
	}
	return lease, stop, nil
}

// AcknowledgeStopped reports confirmed cleanup; callers must wait until containers and workspaces are confirmed.
func (client *HTTPClient) AcknowledgeStopped(ctx context.Context, claim Claim) error {
	if err := validateFence(claim); err != nil {
		return err
	}
	payload := client.fence(claim)
	payload["stopped"] = true
	response, err := client.json(ctx, http.MethodPost, "/runner/v1/attempts/"+claim.AttemptID+"/heartbeat", payload, ManifestMaxBytes)
	if err != nil {
		return err
	}
	return expect(response, http.StatusOK, http.StatusNoContent)
}

// SendEvents validates the batch and the attempt fence before sending; it never assigns or retries sequences.
func (client *HTTPClient) SendEvents(ctx context.Context, claim Claim, raw []byte) error {
	if err := validateFence(claim); err != nil {
		return err
	}
	if err := Validate("event-batch", raw); err != nil {
		return fmt.Errorf("invalid event batch: %w", err)
	}
	var events []json.RawMessage
	if err := json.Unmarshal(raw, &events); err != nil {
		return err
	}
	for _, event := range events {
		if len(event) > WorkerEventMaxBytes {
			return fmt.Errorf("event exceeded its byte limit")
		}
		value, err := Decode(event, WorkerEventMaxBytes)
		if err != nil {
			return err
		}
		if err := matchesFence(value.(map[string]any), claim, false); err != nil {
			return err
		}
	}
	response, err := client.request(ctx, http.MethodPost, "/runner/v1/attempts/"+claim.AttemptID+"/events", string(raw), "application/json", ManifestMaxBytes)
	if err != nil {
		return err
	}
	return expect(response, http.StatusOK, http.StatusNoContent)
}

// UploadArtifact accepts a bounded body only when its SHA-256 matches expectedSHA256.
func (client *HTTPClient) UploadArtifact(ctx context.Context, claim Claim, kind string, body []byte, expectedSHA256 string) (string, error) {
	if err := validateFence(claim); err != nil {
		return "", err
	}
	if (kind != "patch" && kind != "native_output") || !sha256Pattern.MatchString(expectedSHA256) {
		return "", fmt.Errorf("artifact kind or hash is invalid")
	}
	if len(body) > ResultMaxBytes {
		return "", fmt.Errorf("artifact exceeded its byte limit")
	}
	actual := sha256.Sum256(body)
	if hex.EncodeToString(actual[:]) != expectedSHA256 {
		return "", fmt.Errorf("upload artifact hash is invalid")
	}
	response, err := client.artifactRequest(ctx, "/runner/v1/attempts/"+claim.AttemptID+"/artifacts", kind, expectedSHA256, claim.Fence, body)
	if err != nil {
		return "", err
	}
	data, err := responseData(response, http.StatusOK, http.StatusCreated)
	if err != nil {
		return "", err
	}
	id, err := stringField(data, "id")
	if err != nil || !ulidPattern.MatchString(id) {
		return "", fmt.Errorf("artifact response identifier is invalid")
	}
	return id, nil
}

// Complete submits a result; accepted completion does not replace cleanup or the stopped acknowledgement.
func (client *HTTPClient) Complete(ctx context.Context, claim Claim, raw []byte) error {
	if err := validateFence(claim); err != nil {
		return err
	}
	if err := Validate("result", raw); err != nil {
		return fmt.Errorf("invalid completion result: %w", err)
	}
	value, err := Decode(raw, ResultMaxBytes)
	if err != nil {
		return err
	}
	result, err := object(value)
	if err != nil {
		return err
	}
	if err := matchesFence(result, claim, true); err != nil {
		return err
	}
	response, err := client.json(ctx, http.MethodPost, "/runner/v1/attempts/"+claim.AttemptID+"/completion", result, ResultMaxBytes)
	if err != nil {
		return err
	}
	return expect(response, http.StatusOK, http.StatusNoContent)
}

// DownloadArtifact returns attempt-scoped input only after checking its byte bound and hashes.
func (client *HTTPClient) DownloadArtifact(ctx context.Context, claim Claim, artifactID, expectedSHA256 string) ([]byte, error) {
	if err := validateFence(claim); err != nil {
		return nil, err
	}
	if !ulidPattern.MatchString(artifactID) || !sha256Pattern.MatchString(expectedSHA256) {
		return nil, fmt.Errorf("artifact identifier or hash is invalid")
	}
	response, err := client.request(ctx, http.MethodGet, "/runner/v1/attempts/"+claim.AttemptID+"/input-artifacts/"+artifactID, "", "application/octet-stream", InputArtifactMaxBytes)
	if err != nil {
		return nil, err
	}
	if err := expect(response, http.StatusOK); err != nil {
		return nil, err
	}
	body := response.Body
	if contentLength := response.Header.Get("Content-Length"); contentLength != "" && contentLength != fmt.Sprint(len(body)) {
		return nil, fmt.Errorf("artifact content length did not match body")
	}
	actual := sha256.Sum256(body)
	if !strings.EqualFold(response.Header.Get("X-Artifact-SHA256"), expectedSHA256) || hex.EncodeToString(actual[:]) != expectedSHA256 {
		return nil, fmt.Errorf("downloaded artifact hash did not match claim")
	}
	return body, nil
}

type response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func (client *HTTPClient) json(ctx context.Context, method, path string, payload any, maxBytes int) (response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return response{}, err
	}
	if len(body) > maxBytes {
		return response{}, fmt.Errorf("control-plane request exceeded its byte limit")
	}
	return client.request(ctx, method, path, string(body), "application/json", maxBytes)
}

func (client *HTTPClient) request(ctx context.Context, method, path, body, accept string, maxBytes int) (response, error) {
	endpoint := client.baseURL.JoinPath(path)
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewBufferString(body))
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Accept", accept)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+client.token)
	req.Header.Set("X-Shipmunk-Protocol", Version)
	res, err := client.client.Do(req)
	if err != nil {
		return response{}, fmt.Errorf("control-plane request failed: %w", err)
	}
	defer res.Body.Close()
	bytes, err := io.ReadAll(io.LimitReader(res.Body, int64(maxBytes)+1))
	if err != nil {
		return response{}, fmt.Errorf("read control-plane response: %w", err)
	}
	if len(bytes) > maxBytes {
		return response{}, fmt.Errorf("control-plane response exceeded its byte limit")
	}
	return response{StatusCode: res.StatusCode, Header: res.Header.Clone(), Body: bytes}, nil
}

func (client *HTTPClient) artifactRequest(ctx context.Context, path, kind, hash string, fence int64, body []byte) (response, error) {
	endpoint := client.baseURL.JoinPath(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+client.token)
	req.Header.Set("X-Shipmunk-Protocol", Version)
	req.Header.Set("X-Artifact-Kind", kind)
	req.Header.Set("X-Artifact-SHA256", hash)
	req.Header.Set("X-Attempt-Fence", fmt.Sprint(fence))
	req.Header.Set("X-Protocol-Version", Version)
	res, err := client.client.Do(req)
	if err != nil {
		return response{}, fmt.Errorf("control-plane request failed: %w", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, int64(ManifestMaxBytes)+1))
	if err != nil {
		return response{}, fmt.Errorf("read control-plane response: %w", err)
	}
	if len(data) > ManifestMaxBytes {
		return response{}, fmt.Errorf("control-plane response exceeded its byte limit")
	}
	return response{StatusCode: res.StatusCode, Header: res.Header.Clone(), Body: data}, nil
}

func (client *HTTPClient) fence(claim Claim) map[string]any {
	return map[string]any{"protocol_version": Version, "attempt_id": claim.AttemptID, "fence": claim.Fence}
}

func validateFence(claim Claim) error {
	if !ulidPattern.MatchString(claim.RunID) || !ulidPattern.MatchString(claim.AttemptID) || claim.Fence < 1 || claim.Fence > MaxSafeInteger {
		return fmt.Errorf("invalid active claim identity")
	}
	return nil
}

func matchesFence(data map[string]any, claim Claim, requireRun bool) error {
	fence, err := positiveInteger(data["fence"])
	if err != nil || fence != claim.Fence || data["attempt_id"] != claim.AttemptID || data["protocol_version"] != Version {
		return fmt.Errorf("document did not match the active attempt fence")
	}
	if requireRun && data["run_id"] != claim.RunID {
		return fmt.Errorf("document did not match the active run")
	}
	return nil
}

func responseData(response response, statuses ...int) (map[string]any, error) {
	if err := expect(response, statuses...); err != nil {
		return nil, err
	}
	value, err := Decode(response.Body, ManifestMaxBytes)
	if err != nil {
		return nil, err
	}
	envelope, err := object(value)
	if err != nil {
		return nil, fmt.Errorf("control-plane response data is missing")
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("control-plane response data is missing")
	}
	return data, nil
}

func expect(response response, statuses ...int) error {
	for _, status := range statuses {
		if response.StatusCode == status {
			return nil
		}
	}
	return &ControlPlaneError{StatusCode: response.StatusCode}
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
