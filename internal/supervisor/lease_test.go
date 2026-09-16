package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

// contextCapturingClient records the exact context each call received, so
// tests can assert the lease guard hands the client its own attempt
// context unwrapped rather than a shorter per-call deadline of its own.
type contextCapturingClient struct {
	*fixtureClient
	heartbeatCtx, completeCtx context.Context
}

func (c *contextCapturingClient) Heartbeat(ctx context.Context, claim protocol.Claim) (time.Time, bool, error) {
	c.heartbeatCtx = ctx
	return time.Now().Add(LeaseDuration), false, nil
}

func (c *contextCapturingClient) Complete(ctx context.Context, _ protocol.Claim, _ []byte) error {
	c.completeCtx = ctx
	return nil
}

// budgetRaceClient reproduces protocol.HTTPClient's own budget-context
// derivation (see internal/protocol/http.go's httpCallError) without a real
// transport: it waits out its own budget against the caller's context and
// only reports HTTPTimeoutError when the caller's context was still live
// when the budget elapsed.
type budgetRaceClient struct {
	*fixtureClient
	budget time.Duration
}

func (c *budgetRaceClient) raceBudget(ctx context.Context, endpoint string) error {
	budgetCtx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	<-budgetCtx.Done()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &protocol.HTTPTimeoutError{Endpoint: endpoint, Budget: c.budget}
}

func (c *budgetRaceClient) Heartbeat(ctx context.Context, claim protocol.Claim) (time.Time, bool, error) {
	if err := c.raceBudget(ctx, "/runner/v1/attempts/"+claim.AttemptID+"/heartbeat"); err != nil {
		return time.Time{}, false, err
	}
	return time.Now().Add(LeaseDuration), false, nil
}

func (c *budgetRaceClient) Complete(ctx context.Context, claim protocol.Claim, _ []byte) error {
	return c.raceBudget(ctx, "/runner/v1/attempts/"+claim.AttemptID+"/completion")
}

func newTestLeaseGuard(claim protocol.Claim, client Client, deadline time.Time) (*leaseGuard, context.CancelFunc) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	g := &leaseGuard{ctx: ctx, cancel: cancel, supervisor: &Supervisor{Client: client}, claim: claim, state: attemptstate.State{}, done: make(chan struct{})}
	return g, cancel
}

func TestLeaseHeartbeatUsesTheAttemptContextWithoutAnAdditionalTimeout(t *testing.T) {
	claim := fixtureClaim(t)
	client := &contextCapturingClient{fixtureClient: &fixtureClient{claim: &claim}}
	g, cancel := newTestLeaseGuard(claim, client, claim.Deadline)
	defer cancel()
	g.supervisor.State = &memoryState{}
	g.timer = time.NewTimer(time.Hour)
	defer g.timer.Stop()

	if err := g.renew(); err != nil {
		t.Fatalf("unexpected renew failure: %v", err)
	}
	if client.heartbeatCtx != g.ctx {
		t.Fatalf("heartbeat must run directly under the attempt context, not a derived one with its own deadline")
	}
}

func TestLeaseCompleteUsesTheAttemptContextWithoutAnAdditionalTimeout(t *testing.T) {
	claim := fixtureClaim(t)
	client := &contextCapturingClient{fixtureClient: &fixtureClient{claim: &claim}}
	g, cancel := newTestLeaseGuard(claim, client, claim.Deadline)
	defer cancel()

	if err := g.complete([]byte("{}")); err != nil {
		t.Fatalf("unexpected complete failure: %v", err)
	}
	if client.completeCtx != g.ctx {
		t.Fatalf("completion must run directly under the attempt context, not a derived one with its own deadline")
	}
}

func TestLeaseHeartbeatTimeoutClassifiesAsHTTPBudgetTimeout(t *testing.T) {
	claim := fixtureClaim(t)
	client := &budgetRaceClient{fixtureClient: &fixtureClient{claim: &claim}, budget: 20 * time.Millisecond}
	g, cancel := newTestLeaseGuard(claim, client, claim.Deadline)
	defer cancel()

	err := g.renew()

	var timeout *protocol.HTTPTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("expected an HTTPTimeoutError from the heartbeat call site, got %v", err)
	}
	if timeout.Budget != client.budget {
		t.Fatalf("unexpected budget: %s", timeout.Budget)
	}
}

func TestLeaseHeartbeatWithExpiredAttemptContextReadsAsAttemptDeadline(t *testing.T) {
	claim := fixtureClaim(t)
	client := &budgetRaceClient{fixtureClient: &fixtureClient{claim: &claim}, budget: 20 * time.Millisecond}
	g, cancel := newTestLeaseGuard(claim, client, time.Now().Add(-time.Minute))
	defer cancel()

	err := g.renew()

	var timeout *protocol.HTTPTimeoutError
	if errors.As(err, &timeout) {
		t.Fatalf("an already-expired attempt context must not be reported as the request's own timeout: %+v", timeout)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the attempt to still read as passing its deadline, got %v", err)
	}
}

func TestLeaseCompleteTimeoutClassifiesAsHTTPBudgetTimeout(t *testing.T) {
	claim := fixtureClaim(t)
	client := &budgetRaceClient{fixtureClient: &fixtureClient{claim: &claim}, budget: 20 * time.Millisecond}
	g, cancel := newTestLeaseGuard(claim, client, claim.Deadline)
	defer cancel()

	err := g.complete([]byte("{}"))

	var timeout *protocol.HTTPTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("expected an HTTPTimeoutError from the completion call site, got %v", err)
	}
	if timeout.Budget != client.budget {
		t.Fatalf("unexpected budget: %s", timeout.Budget)
	}
}

func TestLeaseCompleteWithExpiredAttemptContextReadsAsAttemptDeadline(t *testing.T) {
	claim := fixtureClaim(t)
	client := &budgetRaceClient{fixtureClient: &fixtureClient{claim: &claim}, budget: 20 * time.Millisecond}
	g, cancel := newTestLeaseGuard(claim, client, time.Now().Add(-time.Minute))
	defer cancel()

	err := g.complete([]byte("{}"))

	var timeout *protocol.HTTPTimeoutError
	if errors.As(err, &timeout) {
		t.Fatalf("an already-expired attempt context must not be reported as the request's own timeout: %+v", timeout)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the attempt to still read as passing its deadline, got %v", err)
	}
}
