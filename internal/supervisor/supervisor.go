package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
	"github.com/ianrodrigues/shipmunk-runner/internal/workspace"
)

const HeartbeatInterval = 10 * time.Second
const LeaseDuration = 45 * time.Second
const cleanupTimeout = 90 * time.Second

// refusedStoppedSettleThreshold bounds the recovery retry that a permanently
// superseded attempt would otherwise repeat forever. HeartbeatRunAttempt
// refuses the stopped path with 409 only for a fence mismatch or a
// non-current attempt, both permanent, so a consecutive run of refusals on
// the same journal converges without waiting on the server to change its
// mind. That guarantee depends on two things staying true server-side:
// RunAttemptHeartbeatController maps every DomainException from the whole
// action to 409, not just the fence check, and Run::transitionTo still
// allows Preparing/Running into NeedsAttention or Cancelled on the stopped
// path rather than raising its own conflict there.
const refusedStoppedSettleThreshold = 3

var ErrStopped = errors.New("control plane requested stop")
var ErrLeaseExpired = errors.New("attempt lease expired")
var ErrCleanupUnconfirmed = errors.New("cleanup unconfirmed; durable attempt retained")

// RefusedStoppedError reports how many consecutive stopped-acknowledgement
// refusals the journal has recorded below refusedStoppedSettleThreshold, so
// callers can name the condition instead of a generic cleanup failure.
type RefusedStoppedError struct {
	Count, Threshold int64
}

func (err *RefusedStoppedError) Error() string {
	return fmt.Sprintf("control plane refused the stopped acknowledgement (%d of %d)", err.Count, err.Threshold)
}

// Client operations must honor cancellation.
type Client interface {
	workspace.Downloader
	Claim(context.Context, time.Time) (*protocol.Claim, error)
	Heartbeat(context.Context, protocol.Claim) (time.Time, bool, error)
	AcknowledgeStopped(context.Context, protocol.Claim) error
	SendEvents(context.Context, protocol.Claim, []byte) error
	UploadArtifact(context.Context, protocol.Claim, string, []byte, string) (string, error)
	Complete(context.Context, protocol.Claim, []byte) error
}

// StateStore must hold an exclusive process-lifetime lock until supervision ends.
type StateStore interface {
	Load() (*attemptstate.State, error)
	Save(attemptstate.State) error
	Clear() error
}

type Workspaces interface {
	EnsureWritable() error
	Path(protocol.Claim) string
	Prepare(context.Context, protocol.Claim, workspace.Downloader) (string, error)
	Remove(string) error
}

type Process interface {
	ContainerID() string
	Start(context.Context) error
	Wait(context.Context) (int, []byte, error)
}

// Sandbox reserves a stable name before Create, whose immutable container ID CreateFinished must acknowledge.
type Sandbox interface {
	Name(protocol.Claim) (string, error)
	Create(context.Context, protocol.Claim, map[string]any, string) (Process, error)
	Reconcile(context.Context, string) error
}

type WatchdogLease interface {
	Renew(time.Time) error
	CreateStarted() error
	CreateFinished(string) error
	Disarm() error
}

// Watchdog must be independent of the supervisor process and monitor parent death.
type Watchdog interface {
	Arm(string, time.Time, time.Time) (WatchdogLease, error)
}

// Executor owns a composite execution boundary: Cleanup must reconcile every resource a partial Execute may have created.
type Executor interface {
	Execute(context.Context, protocol.Claim, map[string]any, string) (Execution, error)
	Cleanup(context.Context, protocol.Claim) error
}

// ExecutorLease is implemented by composite executors whose cleanup watchdog tracks lease renewal.
type ExecutorLease interface {
	Renew(protocol.Claim, time.Time) error
}

// EventLogger records one structured event for the runner log. Implementations
// must be safe for concurrent use and must never receive secrets, tokens, or
// repository content in fields.
type EventLogger interface {
	Event(event string, fields map[string]any)
}

type Supervisor struct {
	Client            Client
	State             StateStore
	Workspaces        Workspaces
	Sandbox           Sandbox
	Watchdog          Watchdog
	Executor          Executor
	Log               EventLogger
	mu                sync.Mutex
	blocked           bool
	heartbeatInterval time.Duration
}

// log is a nil-safe convenience wrapper so call sites never check s.Log themselves.
func (s *Supervisor) log(event string, fields map[string]any) {
	if s.Log != nil {
		s.Log.Event(event, fields)
	}
}

// Outcome is observable only after accepted completion and confirmed cleanup.
type Outcome struct {
	Worked    bool
	RunID     string
	AttemptID string
	Result    string
}

// RunOnce reconciles old state, then claims and supervises at most one attempt; the journal's lifetime lock excludes other processes.
func (s *Supervisor) RunOnce(ctx context.Context) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blocked {
		return Outcome{}, errors.New("supervisor requires restart after journal failure")
	}
	if s.Client == nil || s.State == nil || s.Workspaces == nil ||
		(s.Executor == nil && (s.Sandbox == nil || s.Watchdog == nil)) {
		return Outcome{}, errors.New("supervisor dependencies are incomplete")
	}
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	if err := s.reconcile(ctx); err != nil {
		return Outcome{}, err
	}
	if err := s.Workspaces.EnsureWritable(); err != nil {
		return Outcome{}, err
	}
	claim, err := s.Client.Claim(ctx, time.Now())
	if err != nil {
		s.log("claim_failed", map[string]any{"error": err.Error()})
		return Outcome{}, err
	}
	if claim == nil {
		s.log("claim_empty", nil)
		return Outcome{}, nil
	}
	// Validate and detach even injected clients; never mutate the server's manifest.
	validated, err := protocol.ClaimFromManifest(claim.Manifest, time.Now())
	if err != nil || validated.RunID != claim.RunID || validated.AttemptID != claim.AttemptID || validated.Fence != claim.Fence {
		s.log("claim_invalid", map[string]any{"run_id": claim.RunID, "attempt_id": claim.AttemptID, "fence": claim.Fence})
		s.blocked = true
		return Outcome{}, errors.New("claimed identity is invalid")
	}
	validated.LeaseExpiresAt = claim.LeaseExpiresAt
	claim = &validated
	s.log("claim", map[string]any{"run_id": claim.RunID, "attempt_id": claim.AttemptID, "fence": claim.Fence, "deadline": claim.Deadline, "lease_expires_at": claim.LeaseExpiresAt})
	path := s.Workspaces.Path(*claim)
	state := attemptstate.State{RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, LeaseExpiresAt: claim.LeaseExpiresAt, Deadline: claim.Deadline, Workspace: path}
	if profile, ok := claim.Manifest["profile_id"].(string); ok {
		state.ProfileID = &profile
	}
	if err := s.State.Save(state); err != nil {
		s.blocked = true
		s.log("claim_persist_failed", map[string]any{"run_id": claim.RunID, "attempt_id": claim.AttemptID, "error": err.Error()})
		ackCtx, cancel := context.WithTimeout(context.Background(), protocol.HTTPTimeoutSeconds*time.Second)
		ackErr := s.Client.AcknowledgeStopped(ackCtx, *claim)
		cancel()
		persistErr := fmt.Errorf("unable to persist claim; restart required: %w", err)
		if ackErr != nil {
			return Outcome{}, errors.Join(persistErr, fmt.Errorf("stopped acknowledgement failed: %w", ackErr))
		}
		return Outcome{}, persistErr
	}
	guard, err := newLeaseGuard(ctx, s, *claim, state)
	if err != nil {
		s.log("lease_guard_failed", map[string]any{"run_id": claim.RunID, "attempt_id": claim.AttemptID, "error": err.Error()})
		if cleanupErr := s.cleanup(*claim, state, nil); cleanupErr != nil {
			return Outcome{}, cleanupErr
		}
		return Outcome{}, err
	}
	result, executionErr := s.runClaim(guard, *claim)
	guard.stop()
	state, watchdog, creationPending := guard.snapshot()
	if creationPending {
		if executionErr == nil {
			executionErr = errors.New("sandbox creation outcome is not confirmed")
		}
		s.log("cleanup_unconfirmed", map[string]any{"run_id": claim.RunID, "attempt_id": claim.AttemptID, "error": executionErr.Error()})
		return Outcome{}, fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, executionErr)
	}
	if err := s.cleanup(*claim, state, watchdog); err != nil {
		return Outcome{}, err
	}
	if executionErr != nil {
		s.log("attempt_failed", map[string]any{"run_id": claim.RunID, "attempt_id": claim.AttemptID, "error": executionErr.Error()})
		return Outcome{}, executionErr
	}
	return Outcome{Worked: true, RunID: claim.RunID, AttemptID: claim.AttemptID, Result: result}, nil
}

// reconcile cleans up a journaled attempt before new work is claimed. It
// retains the journal when cleanup cannot be confirmed, except that the third
// stopped-acknowledgement conflict settles a permanently superseded attempt.
func (s *Supervisor) reconcile(parent context.Context) error {
	state, err := s.State.Load()
	if err != nil || state == nil {
		return err
	}
	// Recovery must end in bounded time; an unresponsive Docker engine or control plane must not hold the next claim.
	ctx, cancel := context.WithTimeout(parent, cleanupTimeout)
	defer cancel()
	claim := protocol.Claim{RunID: state.RunID, AttemptID: state.AttemptID, Fence: state.Fence, LeaseExpiresAt: state.LeaseExpiresAt, Deadline: state.Deadline}
	if state.ProfileID != nil {
		claim.Manifest = map[string]any{"profile_id": *state.ProfileID}
	}
	if state.Workspace != s.Workspaces.Path(claim) {
		return errors.New("recovery workspace does not match active attempt")
	}
	if s.Executor != nil {
		if state.SandboxID != nil {
			return fmt.Errorf("%w: legacy sandbox requires its Docker reconciler", ErrCleanupUnconfirmed)
		}
		if err := s.Executor.Cleanup(ctx, claim); err != nil {
			return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
		}
	} else if state.SandboxID != nil {
		name, err := s.Sandbox.Name(claim)
		if err != nil {
			return err
		}
		// A reserved name cannot distinguish a delayed create from confirmed absence.
		// Keep it durable and leave resolution to the original watchdog/operator.
		if *state.SandboxID == name {
			return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, sandbox.ErrCreateUncertain)
		}
		// Legacy PHP journals and promoted Go journals store full Docker hex IDs.
		if !legacyContainerID(*state.SandboxID) {
			return errors.New("recovery container identity does not match active attempt")
		}
		if err := s.Sandbox.Reconcile(ctx, *state.SandboxID); err != nil {
			return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
		}
	}
	if err := s.Workspaces.Remove(state.Workspace); err != nil {
		return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
	}
	if err := s.reportInterrupted(ctx, claim); err != nil {
		if !refusedStoppedAcknowledgement(err) {
			return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
		}
		state.RefusedStoppedCount++
		if state.RefusedStoppedCount < refusedStoppedSettleThreshold {
			if saveErr := s.State.Save(*state); saveErr != nil {
				return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, errors.Join(err, saveErr))
			}
			refused := &RefusedStoppedError{Count: state.RefusedStoppedCount, Threshold: refusedStoppedSettleThreshold}
			return fmt.Errorf("%w: %w: %w", ErrCleanupUnconfirmed, refused, err)
		}
		// A third consecutive refusal on the same fenced attempt is not a
		// transient condition: the server has permanently superseded it, so
		// recovery converges here instead of retrying forever.
	}
	return s.State.Clear()
}

// refusedStoppedAcknowledgement reports whether the control plane refused a
// stopped acknowledgement because the attempt is permanently superseded, the
// only condition under which the stopped path returns 409.
func refusedStoppedAcknowledgement(err error) bool {
	var controlPlaneErr *protocol.ControlPlaneError
	return errors.As(err, &controlPlaneErr) && controlPlaneErr.StatusCode == http.StatusConflict
}

// reportInterrupted completes the recovered attempt as failed while the
// journal's lease is live, then acknowledges the stopped sandbox. The control
// plane accepts that acknowledgement even for an expired lease.
func (s *Supervisor) reportInterrupted(ctx context.Context, claim protocol.Claim) error {
	if time.Now().Before(claim.LeaseExpiresAt) {
		raw, err := json.Marshal(interruptedResult(claim))
		if err != nil {
			return err
		}
		// The result is best effort.
		// The control plane refuses it once the attempt is reconciled or its lease has lapsed.
		// A refused retry must not hold the journal.
		_ = s.Client.Complete(ctx, claim, raw)
	}
	// Only the acknowledgement releases the attempt, so its refusal keeps the journal for the next run.
	return s.Client.AcknowledgeStopped(ctx, claim)
}

// interruptedResult carries the incomplete shape contracts/v1/result.schema.json
// requires: no findings, no patch artifact and none of the charter fields.
func interruptedResult(claim protocol.Claim) map[string]any {
	return map[string]any{
		"protocol_version": protocol.Version, "run_id": claim.RunID, "attempt_id": claim.AttemptID,
		"fence": claim.Fence, "outcome": "incomplete", "findings": []any{}, "tests": []any{},
		"patch_artifact": nil, "usage": nil,
		"summary": "The runner stopped before this attempt reported a result. Stage: recovery. The sandbox was removed and the attempt was released.",
	}
}

func legacyContainerID(id string) bool {
	if len(id) < 12 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (s *Supervisor) runClaim(g *leaseGuard, claim protocol.Claim) (string, error) {
	s.log("workspace_prepare_start", map[string]any{"attempt_id": claim.AttemptID})
	path, err := s.Workspaces.Prepare(g.ctx, claim, s.Client)
	if err != nil {
		s.log("workspace_prepare_failed", map[string]any{"attempt_id": claim.AttemptID, "error": err.Error()})
		return "", err
	}
	if path != s.Workspaces.Path(claim) {
		return "", errors.New("prepared workspace does not match attempt")
	}
	s.log("workspace_prepare_done", map[string]any{"attempt_id": claim.AttemptID})
	if s.Executor != nil {
		execution, err := s.Executor.Execute(g.ctx, claim, workspace.Sanitize(claim.Manifest), path)
		if err != nil {
			s.log("executor_failed", map[string]any{"attempt_id": claim.AttemptID, "error": err.Error()})
			return "", err
		}
		s.log("executor_exit", map[string]any{"attempt_id": claim.AttemptID})
		if err := g.ctx.Err(); err != nil {
			return "", err
		}
		return s.publishExecution(g, claim, execution)
	}
	name, err := s.Sandbox.Name(claim)
	if err != nil {
		return "", err
	}
	if err := g.reserve(name); err != nil {
		s.log("sandbox_reserve_failed", map[string]any{"attempt_id": claim.AttemptID, "error": err.Error()})
		return "", g.finishWithoutCreate(err)
	}
	if err := g.createStarted(); err != nil {
		s.log("sandbox_create_start_failed", map[string]any{"attempt_id": claim.AttemptID, "error": err.Error()})
		return "", g.finishWithoutCreate(err)
	}
	s.log("sandbox_create_started", map[string]any{"attempt_id": claim.AttemptID, "name": name})
	process, err := s.Sandbox.Create(g.ctx, claim, workspace.Sanitize(claim.Manifest), path)
	if err != nil {
		s.log("sandbox_create_failed", map[string]any{"attempt_id": claim.AttemptID, "error": err.Error()})
		if errors.Is(err, sandbox.ErrCreateUncertain) {
			return "", err
		}
		return "", g.finishWithoutCreate(err)
	}
	if process == nil {
		return "", errors.New("sandbox creation returned no process")
	}
	if err := g.created(process.ContainerID()); err != nil {
		return "", err
	}
	s.log("sandbox_created", map[string]any{"attempt_id": claim.AttemptID, "container_id": process.ContainerID()})
	if err := g.ctx.Err(); err != nil {
		return "", err
	}
	if err := process.Start(g.ctx); err != nil {
		s.log("sandbox_start_failed", map[string]any{"attempt_id": claim.AttemptID, "error": err.Error()})
		return "", err
	}
	exitCode, output, err := process.Wait(g.ctx)
	if err != nil {
		s.log("sandbox_wait_failed", map[string]any{"attempt_id": claim.AttemptID, "error": err.Error()})
		return "", err
	}
	s.log("sandbox_exit", map[string]any{"attempt_id": claim.AttemptID, "exit_code": exitCode})
	if err := g.ctx.Err(); err != nil {
		return "", err
	}
	execution, err := DecodeExecution(claim, exitCode, output)
	if err != nil {
		s.log("execution_decode_failed", map[string]any{"attempt_id": claim.AttemptID, "error": err.Error()})
		return "", err
	}
	return s.publishExecution(g, claim, execution)
}

func (s *Supervisor) publishExecution(g *leaseGuard, claim protocol.Claim, execution Execution) (string, error) {
	if execution.Result == nil {
		return "", errors.New("native execution returned no result")
	}
	if execution.FailureReason != "" {
		s.log("classified_failure", map[string]any{"attempt_id": claim.AttemptID, "reason": execution.FailureReason})
	}
	for start := 0; start < len(execution.Events); start += 100 {
		end := min(start+100, len(execution.Events))
		raw, err := json.Marshal(execution.Events[start:end])
		if err != nil {
			return "", err
		}
		if err := s.Client.SendEvents(g.ctx, claim, raw); err != nil {
			return "", err
		}
	}
	var patch any
	for _, artifact := range execution.Artifacts {
		id, err := s.Client.UploadArtifact(g.ctx, claim, artifact.Kind, artifact.Bytes, artifact.SHA256)
		if err != nil {
			return "", err
		}
		if artifact.Kind == "patch" {
			patch = map[string]any{"artifact_id": id, "sha256": artifact.SHA256}
		}
	}
	execution.Result["patch_artifact"] = patch
	if err := validateJSON("result", execution.Result); err != nil {
		return "", fmt.Errorf("uploaded execution result is invalid: %w", err)
	}
	raw, err := json.Marshal(execution.Result)
	if err != nil {
		return "", err
	}
	if err := g.complete(raw); err != nil {
		return "", err
	}
	outcome := execution.Result["outcome"].(string)
	s.log("result_delivered", map[string]any{"attempt_id": claim.AttemptID, "outcome": outcome})
	return outcome, nil
}

func (s *Supervisor) cleanup(claim protocol.Claim, state attemptstate.State, watchdog WatchdogLease) error {
	s.log("cleanup_start", map[string]any{"attempt_id": claim.AttemptID})
	// Revoked execution authority cannot cancel mandatory cleanup.
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if s.Executor != nil {
		if err := s.Executor.Cleanup(ctx, claim); err != nil {
			s.log("cleanup_failed", map[string]any{"attempt_id": claim.AttemptID, "step": "executor", "error": err.Error()})
			return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
		}
	} else if state.SandboxID != nil {
		if err := s.Sandbox.Reconcile(ctx, *state.SandboxID); err != nil {
			s.log("cleanup_failed", map[string]any{"attempt_id": claim.AttemptID, "step": "sandbox", "error": err.Error()})
			return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
		}
	}
	if watchdog != nil {
		if err := watchdog.Disarm(); err != nil {
			s.log("cleanup_failed", map[string]any{"attempt_id": claim.AttemptID, "step": "watchdog", "error": err.Error()})
			return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
		}
	}
	if err := s.Workspaces.Remove(state.Workspace); err != nil {
		s.log("cleanup_failed", map[string]any{"attempt_id": claim.AttemptID, "step": "workspace", "error": err.Error()})
		return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
	}
	if err := s.Client.AcknowledgeStopped(ctx, claim); err != nil {
		s.log("cleanup_failed", map[string]any{"attempt_id": claim.AttemptID, "step": "acknowledge", "error": err.Error()})
		return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
	}
	if err := s.State.Clear(); err != nil {
		s.log("cleanup_failed", map[string]any{"attempt_id": claim.AttemptID, "step": "state_clear", "error": err.Error()})
		return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
	}
	s.log("cleanup_done", map[string]any{"attempt_id": claim.AttemptID})
	return nil
}
