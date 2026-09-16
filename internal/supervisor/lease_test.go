package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

// Records the context each call received.
// Confirms the guard passes its own attempt context through unwrapped.
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

// Reproduces the budget-context derivation in http.go's httpCallError, without a real transport.
// Waits out its own budget against the caller's context.
// Reports HTTPTimeoutError only if the caller's context was still live when the budget elapsed.
type budgetRaceClient struct {
	*fixtureClient
	budget  time.Duration
	entered chan struct{}
	onEntry func()
}

func (c *budgetRaceClient) raceBudget(ctx context.Context, endpoint string) error {
	if c.entered != nil {
		close(c.entered)
	}
	if c.onEntry != nil {
		c.onEntry()
	}
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

// Looks live until expired, then reports context.DeadlineExceeded.
// Lets a test fire the attempt deadline from inside a call, deterministically.
type deadlineOnEntryContext struct {
	done chan struct{}
}

func newDeadlineOnEntryContext() (*deadlineOnEntryContext, func()) {
	ctx := &deadlineOnEntryContext{done: make(chan struct{})}
	return ctx, func() { close(ctx.done) }
}

func (c *deadlineOnEntryContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *deadlineOnEntryContext) Done() <-chan struct{}       { return c.done }
func (c *deadlineOnEntryContext) Value(any) any               { return nil }
func (c *deadlineOnEntryContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
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
	// The attempt context is live at the guard's pre-check, then the client
	// itself fires it once entered, before waiting out its own budget. No
	// wall-clock race: the client can only fire it after being entered.
	ctx, expire := newDeadlineOnEntryContext()
	entered := make(chan struct{})
	client := &budgetRaceClient{fixtureClient: &fixtureClient{claim: &claim}, budget: 50 * time.Millisecond, entered: entered, onEntry: expire}
	g := &leaseGuard{ctx: ctx, cancel: func() {}, supervisor: &Supervisor{Client: client}, claim: claim, state: attemptstate.State{}, done: make(chan struct{})}

	err := g.renew()

	assertClientWasEntered(t, entered)
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
	// Same as the heartbeat case above: the client fires the attempt context
	// itself once entered, so there is no wall-clock race to expire early.
	ctx, expire := newDeadlineOnEntryContext()
	entered := make(chan struct{})
	client := &budgetRaceClient{fixtureClient: &fixtureClient{claim: &claim}, budget: 50 * time.Millisecond, entered: entered, onEntry: expire}
	g := &leaseGuard{ctx: ctx, cancel: func() {}, supervisor: &Supervisor{Client: client}, claim: claim, state: attemptstate.State{}, done: make(chan struct{})}

	err := g.complete([]byte("{}"))

	assertClientWasEntered(t, entered)
	var timeout *protocol.HTTPTimeoutError
	if errors.As(err, &timeout) {
		t.Fatalf("an already-expired attempt context must not be reported as the request's own timeout: %+v", timeout)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the attempt to still read as passing its deadline, got %v", err)
	}
}

// Fails the test outright rather than let the race silently degrade to
// the guard's own pre-check when the call is never reached.
func assertClientWasEntered(t *testing.T, entered chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	default:
		t.Fatal("client call was never entered; the guard's pre-check short-circuited before the race could happen")
	}
}
