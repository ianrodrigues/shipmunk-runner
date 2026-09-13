package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

type leaseGuard struct {
	ctx                context.Context
	cancel             context.CancelFunc
	mu                 sync.Mutex
	supervisor         *Supervisor
	claim              protocol.Claim
	state              attemptstate.State
	watchdog           WatchdogLease
	timer              *time.Timer
	done               chan struct{}
	finished           bool
	creationPending    bool
	reservationPending bool
}

func newLeaseGuard(parent context.Context, s *Supervisor, claim protocol.Claim, state attemptstate.State) (*leaseGuard, error) {
	ctx, cancel := context.WithDeadline(parent, claim.Deadline)
	g := &leaseGuard{ctx: ctx, cancel: cancel, supervisor: s, claim: claim, state: state, done: make(chan struct{})}
	remaining := time.Until(claim.LeaseExpiresAt)
	if remaining <= 0 {
		cancel()
		return nil, ErrLeaseExpired
	}
	// The monotonic timer remains live while a heartbeat or transfer is blocked.
	g.timer = time.AfterFunc(min(remaining, LeaseDuration), cancel)
	if err := g.renew(); err != nil {
		g.timer.Stop()
		cancel()
		return nil, err
	}
	go g.loop()
	return g, nil
}

func (g *leaseGuard) loop() {
	defer close(g.done)
	interval := g.supervisor.heartbeatInterval
	if interval <= 0 || interval > HeartbeatInterval {
		interval = HeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
			if err := g.renew(); err != nil {
				g.cancel()
				return
			}
		}
	}
}

func (g *leaseGuard) renew() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.finished {
		return nil
	}
	if err := g.ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(g.ctx, 5*time.Second)
	defer cancel()
	lease, stop, err := g.supervisor.Client.Heartbeat(ctx, g.claim)
	if err != nil {
		return err
	}
	if stop {
		return ErrStopped
	}
	if err := g.ctx.Err(); err != nil {
		return err
	}
	remaining := min(time.Until(lease), LeaseDuration)
	if remaining <= 0 {
		return ErrLeaseExpired
	}
	next := g.state
	next.LeaseExpiresAt = lease
	if err := g.supervisor.State.Save(next); err != nil {
		return err
	}
	if g.watchdog != nil {
		if err := g.watchdog.Renew(lease); err != nil {
			return err
		}
	}
	if executor, ok := g.supervisor.Executor.(ExecutorLease); ok {
		if err := executor.Renew(g.claim, lease); err != nil {
			return err
		}
	}
	if !g.timer.Stop() {
		return ErrLeaseExpired
	}
	if err := g.ctx.Err(); err != nil {
		return err
	}
	g.state = next
	g.timer.Reset(remaining)
	return nil
}

func (g *leaseGuard) reserve(name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ctx.Err(); err != nil {
		return err
	}
	next := g.state
	next.SandboxID = &name
	g.reservationPending = true
	if err := g.supervisor.State.Save(next); err != nil {
		return err
	}
	g.state = next
	watchdog, err := g.supervisor.Watchdog.Arm(name, next.LeaseExpiresAt, next.Deadline)
	if err != nil {
		return err
	}
	g.watchdog = watchdog
	return g.ctx.Err()
}

func (g *leaseGuard) createStarted() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ctx.Err(); err != nil {
		return err
	}
	if g.watchdog == nil || g.state.SandboxID == nil {
		return errors.New("sandbox reservation and watchdog are required before creation")
	}
	// Mark before sending: a failed/lost ACK is ambiguous about whether the
	// watchdog accepted the phase transition, so the caller must finish-empty.
	g.creationPending = true
	if err := g.watchdog.CreateStarted(); err != nil {
		return err
	}
	// The ACK can arrive just as the lease/deadline is revoked. Never begin a
	// Docker side effect unless execution authority is still live after it.
	return g.ctx.Err()
}

func (g *leaseGuard) created(containerID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.creationPending || !fullContainerID(containerID) {
		return errors.New("created sandbox did not provide a valid container ID")
	}
	if err := g.watchdog.CreateFinished(containerID); err != nil {
		return fmt.Errorf("watchdog did not confirm created container ID: %w", err)
	}
	next := g.state
	next.SandboxID = &containerID
	if err := g.supervisor.State.Save(next); err != nil {
		return fmt.Errorf("persist created sandbox container ID: %w", err)
	}
	g.state = next
	g.creationPending = false
	g.reservationPending = false
	return nil
}

func fullContainerID(id string) bool {
	return len(id) == 64 && legacyContainerID(id)
}

// finishWithoutCreate clears a reserved name only when the sandbox did not
// cross its create boundary or Create returned a definitive failure.
func (g *leaseGuard) finishWithoutCreate(cause error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.SandboxID != nil || g.reservationPending {
		if g.watchdog != nil && g.creationPending {
			if err := g.watchdog.CreateFinished(""); err != nil {
				g.creationPending = true
				return errors.Join(cause, fmt.Errorf("watchdog did not confirm that creation was skipped: %w", err))
			}
		}
		next := g.state
		next.SandboxID = nil
		if err := g.supervisor.State.Save(next); err != nil {
			g.creationPending = true
			return errors.Join(cause, fmt.Errorf("clear sandbox reservation: %w", err))
		}
		g.state = next
		g.creationPending = false
		g.reservationPending = false
	}
	return cause
}

func (g *leaseGuard) complete(raw []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(g.ctx, 5*time.Second)
	defer cancel()
	if err := g.supervisor.Client.Complete(ctx, g.claim, raw); err != nil {
		return err
	}
	// Do not send another active heartbeat after accepted completion.
	g.finished = true
	return nil
}

func (g *leaseGuard) stop() {
	g.cancel()
	<-g.done
	g.mu.Lock()
	defer g.mu.Unlock()
	g.timer.Stop()
}

func (g *leaseGuard) snapshot() (attemptstate.State, WatchdogLease, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state, g.watchdog, g.creationPending || g.reservationPending
}
