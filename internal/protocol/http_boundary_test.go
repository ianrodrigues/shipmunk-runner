package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type testRoundTripper func(*http.Request) (*http.Response, error)

func (transport testRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func testClaim() Claim {
	return Claim{RunID: "01k4w000000000000000000001", AttemptID: "01k4w000000000000000000002", Fence: 1}
}

func TestOutboundFenceAndSizeFailuresNeverReachTransport(t *testing.T) {
	client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("invalid request reached transport")
		return nil, fmt.Errorf("unexpected request")
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	claim := testClaim()
	result := readContractFixture(t, "result")
	event := readContractFixture(t, "event-batch")
	wrongResult := strings.Replace(string(result), `"fence": 1`, `"fence": 2`, 1)
	wrongEvent := strings.Replace(string(event), `"fence": 1`, `"fence": 2`, 1)
	if err := client.Complete(ctx, claim, []byte(wrongResult)); err == nil {
		t.Error("accepted mismatched result fence")
	}
	if err := client.SendEvents(ctx, claim, []byte(wrongEvent)); err == nil {
		t.Error("accepted mismatched event fence")
	}
	oversizedEvent := strings.Replace(string(event), "Reading diff.", strings.Repeat("界", 24_000), 1)
	if err := client.SendEvents(ctx, claim, []byte(oversizedEvent)); err == nil {
		t.Error("accepted individual event exceeding 64 KiB")
	}
	body := make([]byte, ResultMaxBytes+1)
	hash := sha256.Sum256(body)
	if _, err := client.UploadArtifact(ctx, claim, "native_output", body, hex.EncodeToString(hash[:])); err == nil {
		t.Error("accepted oversized upload")
	}
	if _, err := client.UploadArtifact(ctx, claim, "unknown", nil, strings.Repeat("0", 64)); err == nil {
		t.Error("accepted unsupported upload kind")
	}
	badClaim := claim
	badClaim.AttemptID = "../other-attempt"
	if _, _, err := client.Heartbeat(ctx, badClaim); err == nil {
		t.Error("accepted unsafe attempt identifier")
	}
	if err := client.AcknowledgeStopped(ctx, badClaim); err == nil {
		t.Error("accepted unsafe stopped identifier")
	}
	if _, err := client.DownloadArtifact(ctx, badClaim, "01k4w000000000000000000003", strings.Repeat("0", 64)); err == nil {
		t.Error("accepted unsafe download identity")
	}
}

func TestHTTPStatusRetainsRecoveryInformationWithoutResponseText(t *testing.T) {
	for _, status := range []int{403, 409, 426, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("SYNTHETIC_PRIVATE_PROVIDER_TEXT"))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Claim(context.Background(), time.Now())
			var statusError *ControlPlaneError
			if !errors.As(err, &statusError) || statusError.StatusCode != status {
				t.Fatalf("status lost: %v", err)
			}
			if strings.Contains(err.Error(), "SYNTHETIC_PRIVATE") {
				t.Fatal("response body leaked")
			}
			if errors.Is(err, ErrProtocolIncompatible) != (status == 426) {
				t.Fatal("incorrect version classification")
			}
		})
	}
}

func TestHTTPRejectsRedirectsAndBoundsResponses(t *testing.T) {
	targetRequests := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetRequests++ }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client, err := NewHTTPClient(redirect.URL, "synthetic-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Claim(context.Background(), time.Now()); err == nil {
		t.Fatal("followed redirect")
	}
	if targetRequests != 0 {
		t.Fatal("credentials redirected to another host")
	}
	for _, body := range []string{strings.Repeat("x", ManifestMaxBytes+1), `{"data":{}} {}`, `{"data":[]}`, `{"data":{},"data":{}}`} {
		client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Claim(context.Background(), time.Now()); err == nil {
			t.Fatal("accepted malformed or oversized response")
		}
	}
}

func TestHeartbeatValidatesLeaseStopAndFence(t *testing.T) {
	claim := testClaim()
	for _, field := range []string{"valid", "fence", "attempt_id", "protocol_version", "lease_expires_at", "stop_requested", "unknown"} {
		t.Run(field, func(t *testing.T) {
			data := map[string]any{"protocol_version": Version, "attempt_id": claim.AttemptID, "fence": 1, "lease_expires_at": "2099-01-01T00:00:00Z", "stop_requested": false, "state": "running", "stop_reason": nil}
			switch field {
			case "fence":
				data[field] = 2
			case "attempt_id":
				data[field] = "01k4w000000000000000000003"
			case "protocol_version":
				data[field] = "2.0"
			case "lease_expires_at":
				data[field] = "2099-01-01T00:00:00+01:00"
			case "stop_requested":
				data[field] = "false"
			case "unknown":
				data[field] = true
			}
			raw, _ := json.Marshal(map[string]any{"data": data})
			client, _ := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			}))
			lease, stop, err := client.Heartbeat(context.Background(), claim)
			if field == "valid" {
				if err != nil || stop || lease.Year() != 2099 {
					t.Fatalf("valid heartbeat: %v %v %v", lease, stop, err)
				}
			} else if err == nil {
				t.Fatal("accepted invalid heartbeat")
			}
		})
	}
}

func TestHTTPHonorsCancellationBeforeClaim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("cancelled claim reached server") }))
	defer server.Close()
	client, _ := NewHTTPClient(server.URL, "synthetic-token", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Claim(ctx, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
