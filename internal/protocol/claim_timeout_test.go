package protocol

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// stalledTransport never answers: it waits for the request context, which
// carries the budget, so the client's own timeout is what ends the call.
func stalledTransport(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// deadlineExceededTransport answers at once with a transport-originated
// DeadlineExceeded, before any budget could have elapsed.
func deadlineExceededTransport(*http.Request) (*http.Response, error) {
	return nil, context.DeadlineExceeded
}

func shrinkBudgets(t *testing.T) {
	t.Helper()
	standard, claim := standardHTTPBudget, claimHTTPBudget
	standardHTTPBudget, claimHTTPBudget = 20*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { standardHTTPBudget, claimHTTPBudget = standard, claim })
}

func TestClaimTimeoutIsNamedAndUsesItsOwnBudget(t *testing.T) {
	shrinkBudgets(t)
	client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(stalledTransport))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Claim(context.Background(), time.Now())
	var timeout *HTTPTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("expected an HTTPTimeoutError, got %v", err)
	}
	if timeout.Endpoint != "/runner/v1/claims" || timeout.Budget != claimHTTPBudget {
		t.Fatalf("unexpected timeout error: %+v", timeout)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("HTTPTimeoutError must still satisfy errors.Is(err, context.DeadlineExceeded) for generic callers")
	}
}

func TestHeartbeatTimeoutNamesTheStandardBudgetNotTheClaimBudget(t *testing.T) {
	shrinkBudgets(t)
	client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(stalledTransport))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Heartbeat(context.Background(), testClaim())
	var timeout *HTTPTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("expected an HTTPTimeoutError, got %v", err)
	}
	if timeout.Budget != standardHTTPBudget {
		t.Fatalf("expected the standard budget, got %s", timeout.Budget)
	}
}

func TestAcknowledgeStoppedTimeoutUsesTheStandardBudget(t *testing.T) {
	shrinkBudgets(t)
	client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(stalledTransport))
	if err != nil {
		t.Fatal(err)
	}
	err = client.AcknowledgeStopped(context.Background(), testClaim())
	var timeout *HTTPTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("expected an HTTPTimeoutError, got %v", err)
	}
	if timeout.Budget != standardHTTPBudget {
		t.Fatalf("expected the standard budget, got %s", timeout.Budget)
	}
}

func TestCompleteTimeoutUsesTheStandardBudget(t *testing.T) {
	shrinkBudgets(t)
	client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(stalledTransport))
	if err != nil {
		t.Fatal(err)
	}
	err = client.Complete(context.Background(), testClaim(), readContractFixture(t, "result"))
	var timeout *HTTPTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("expected an HTTPTimeoutError, got %v", err)
	}
	if timeout.Endpoint != "/runner/v1/attempts/"+testClaim().AttemptID+"/completion" || timeout.Budget != standardHTTPBudget {
		t.Fatalf("unexpected timeout error: %+v", timeout)
	}
}

// A transport's own DeadlineExceeded arrives before any budget elapsed, so
// it must pass through untouched rather than claim a budget that never ran out.
func TestTransportDeadlineErrorIsNotReportedAsABudgetTimeout(t *testing.T) {
	client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(deadlineExceededTransport))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Heartbeat(context.Background(), testClaim())
	var timeout *HTTPTimeoutError
	if errors.As(err, &timeout) {
		t.Fatalf("a transport-originated deadline error must not be reported as the request's own budget: %+v", timeout)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the transport error to survive unchanged: %v", err)
	}
}

// An already-expired caller context ends the call before the budget context
// can; that must still read as the attempt passing its deadline.
func TestAlreadyExpiredContextIsNeverReportedAsAnHTTPTimeout(t *testing.T) {
	shrinkBudgets(t)
	client, err := NewHTTPClient("https://control.example", "synthetic-token", testRoundTripper(stalledTransport))
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
