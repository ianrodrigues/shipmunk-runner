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

// TestAlreadyExpiredContextIsNeverReportedAsAnHTTPTimeout guards the case the
// issue was filed against in the other direction: when the caller's own
// context (for example leaseGuard's attempt-deadline context) has already
// expired before the HTTP round trip even starts, net/http.Client.Do still
// returns a context.DeadlineExceeded-satisfying error, but it must not be
// renamed to HTTPTimeoutError, since the request's own fixed budget never
// elapsed — the attempt's deadline did.
func TestAlreadyExpiredContextIsNeverReportedAsAnHTTPTimeout(t *testing.T) {
	client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(deadlineExceededTransport))
	if err != nil {
		t.Fatal(err)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
	defer cancel()
	_, _, err = client.Heartbeat(expired, testClaim())
	var timeout *HTTPTimeoutError
	if errors.As(err, &timeout) {
		t.Fatalf("an already-expired caller context must not be reported as the request's own timeout: %+v", timeout)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the bare context.DeadlineExceeded to survive so the attempt-deadline message still applies: %v", err)
	}
}

func TestClaimAndStandardBudgetsAreDistinctAndOrdered(t *testing.T) {
	if ClaimHTTPTimeoutSeconds <= HTTPTimeoutSeconds {
		t.Fatalf("the claim budget (%ds) must be larger than the standard budget (%ds)", ClaimHTTPTimeoutSeconds, HTTPTimeoutSeconds)
	}
}
