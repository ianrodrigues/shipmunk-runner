package protocol

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// deadlineExceededTransport simulates an HTTP call whose own timeout elapsed,
// without a real wait: net/http.Client.Do wraps a RoundTripper's returned
// error in *url.Error, which still satisfies errors.Is(err,
// context.DeadlineExceeded) through Unwrap, exactly as it would if the
// client's real Timeout had fired.
func deadlineExceededTransport(*http.Request) (*http.Response, error) {
	return nil, context.DeadlineExceeded
}

func TestClaimTimeoutIsNamedAndUsesItsOwnBudget(t *testing.T) {
	client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(deadlineExceededTransport))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Claim(context.Background(), time.Now())
	var timeout *HTTPTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("expected an HTTPTimeoutError, got %v", err)
	}
	if timeout.Endpoint != "/runner/v1/claims" || timeout.Budget != ClaimHTTPTimeoutSeconds*time.Second {
		t.Fatalf("unexpected timeout error: %+v", timeout)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("HTTPTimeoutError must still satisfy errors.Is(err, context.DeadlineExceeded) for generic callers")
	}
}

func TestHeartbeatTimeoutNamesTheStandardBudgetNotTheClaimBudget(t *testing.T) {
	client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(deadlineExceededTransport))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Heartbeat(context.Background(), testClaim())
	var timeout *HTTPTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("expected an HTTPTimeoutError, got %v", err)
	}
	if timeout.Budget != HTTPTimeoutSeconds*time.Second {
		t.Fatalf("expected the standard budget, got %s", timeout.Budget)
	}
}

func TestAcknowledgeStoppedTimeoutUsesTheStandardBudget(t *testing.T) {
	client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(deadlineExceededTransport))
	if err != nil {
		t.Fatal(err)
	}
	err = client.AcknowledgeStopped(context.Background(), testClaim())
	var timeout *HTTPTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("expected an HTTPTimeoutError, got %v", err)
	}
	if timeout.Budget != HTTPTimeoutSeconds*time.Second {
		t.Fatalf("expected the standard budget, got %s", timeout.Budget)
	}
}

func TestClaimAndStandardBudgetsAreDistinctAndOrdered(t *testing.T) {
	if ClaimHTTPTimeoutSeconds <= HTTPTimeoutSeconds {
		t.Fatalf("the claim budget (%ds) must be larger than the standard budget (%ds)", ClaimHTTPTimeoutSeconds, HTTPTimeoutSeconds)
	}
}
