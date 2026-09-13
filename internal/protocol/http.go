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

var ErrProtocolIncompatible = errors.New("runner protocol major is incompatible")

type HTTPClient struct {
	baseURL *url.URL
	token   string
	client  *http.Client
}

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

func (client *HTTPClient) Heartbeat(ctx context.Context, claim Claim) (time.Time, bool, error) {
	response, err := client.json(ctx, http.MethodPost, "/runner/v1/attempts/"+claim.AttemptID+"/heartbeat", client.fence(claim), ManifestMaxBytes)
	if err != nil {
		return time.Time{}, false, err
	}
	data, err := responseData(response, http.StatusOK)
	if err != nil {
		return time.Time{}, false, err
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

func (client *HTTPClient) AcknowledgeStopped(ctx context.Context, claim Claim) error {
	payload := client.fence(claim)
	payload["stopped"] = true
	response, err := client.json(ctx, http.MethodPost, "/runner/v1/attempts/"+claim.AttemptID+"/heartbeat", payload, ManifestMaxBytes)
	if err != nil {
		return err
	}
	return expect(response, http.StatusOK, http.StatusNoContent)
}

func (client *HTTPClient) SendEvents(ctx context.Context, claim Claim, raw []byte) error {
	if err := ValidateFixture("event-batch", pinnedSchemaDirectory(), raw); err != nil {
		return fmt.Errorf("invalid event batch: %w", err)
	}
	response, err := client.request(ctx, http.MethodPost, "/runner/v1/attempts/"+claim.AttemptID+"/events", string(raw), "application/json", EventBatchMaxBytes)
	if err != nil {
		return err
	}
	return expect(response, http.StatusOK, http.StatusNoContent)
}

func (client *HTTPClient) UploadArtifact(ctx context.Context, claim Claim, kind string, body []byte, expectedSHA256 string) (string, error) {
	if kind == "" || !sha256Pattern.MatchString(expectedSHA256) {
		return "", fmt.Errorf("artifact kind or hash is invalid")
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

func (client *HTTPClient) Complete(ctx context.Context, claim Claim, raw []byte) error {
	if err := ValidateFixture("result", pinnedSchemaDirectory(), raw); err != nil {
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
	result["protocol_version"] = Version
	result["attempt_id"] = claim.AttemptID
	result["fence"] = json.Number(fmt.Sprint(claim.Fence))
	response, err := client.json(ctx, http.MethodPost, "/runner/v1/attempts/"+claim.AttemptID+"/completion", result, ResultMaxBytes)
	if err != nil {
		return err
	}
	return expect(response, http.StatusOK, http.StatusNoContent)
}

func (client *HTTPClient) DownloadArtifact(ctx context.Context, claim Claim, artifactID, expectedSHA256 string) ([]byte, error) {
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
	if response.StatusCode == http.StatusUpgradeRequired {
		return ErrProtocolIncompatible
	}
	return fmt.Errorf("control-plane request returned HTTP %d", response.StatusCode)
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
