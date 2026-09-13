package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

var ErrStopped = errors.New("control plane requested stop")
var ErrLeaseExpired = errors.New("attempt lease expired")
var ErrCleanupUnconfirmed = errors.New("cleanup unconfirmed; durable attempt retained")

// Client operations must honor cancellation. The HTTP implementation also bounds
// each request to five seconds; lease renewal runs independently of transfers.
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

// Name reserves a stable identity without creating anything. The supervisor
// journals this name before signaling CreateStarted and calling Create. A
// successful Create returns an immutable full container ID; CreateFinished must
// acknowledge that ID before it is promoted into the journal. ErrCreateUncertain
// leaves the watchdog phase and name reservation pending. A fresh supervisor
// cannot prove a name-only reservation is absent and blocks for manual recovery.
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

// Watchdog must be independent of the supervisor process and monitor parent
// death before Create can cause a Docker side effect.
type Watchdog interface {
	Arm(string, time.Time, time.Time) (WatchdogLease, error)
}

// Executor owns a composite native execution boundary. Execute must return only
// bounded, normalized output. Cleanup must reconcile every resource it may have
// created, including resources left by a partially completed Execute call, and
// release reserved capacity only after that reconciliation succeeds.
type Executor interface {
	Execute(context.Context, protocol.Claim, map[string]any, string) (Execution, error)
	Cleanup(context.Context, protocol.Claim) error
}

// ExecutorLease is implemented by composite executors whose independent
// cleanup watchdog must track control-plane lease renewal.
type ExecutorLease interface {
	Renew(protocol.Claim, time.Time) error
}

type Supervisor struct {
	Client            Client
	State             StateStore
	Workspaces        Workspaces
	Sandbox           Sandbox
	Watchdog          Watchdog
	Executor          Executor
	mu                sync.Mutex
	blocked           bool
	heartbeatInterval time.Duration
}

// Outcome is observable only after accepted completion AND confirmed cleanup.
type Outcome struct {
	Worked    bool
	RunID     string
	AttemptID string
	Result    string
}

// RunOnce reconciles old state, then claims and supervises at most one attempt.
// It returns an empty Outcome when the queue is idle and a worked Outcome only
// after accepted completion and confirmed cleanup. Calls are serialized in this
// process; the journal's lifetime lock additionally excludes other processes.
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
	if err != nil || claim == nil {
		return Outcome{}, err
	}
	// Validate and detach even injected clients; never mutate the server's manifest.
	validated, err := protocol.ClaimFromManifest(claim.Manifest, time.Now())
	if err != nil || validated.RunID != claim.RunID || validated.AttemptID != claim.AttemptID || validated.Fence != claim.Fence {
		s.blocked = true
		return Outcome{}, errors.New("claimed identity is invalid")
	}
	validated.LeaseExpiresAt = claim.LeaseExpiresAt
	claim = &validated
	path := s.Workspaces.Path(*claim)
	state := attemptstate.State{RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, LeaseExpiresAt: claim.LeaseExpiresAt, Deadline: claim.Deadline, Workspace: path}
	if profile, ok := claim.Manifest["profile_id"].(string); ok {
		state.ProfileID = &profile
	}
	if err := s.State.Save(state); err != nil {
		s.blocked = true
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
		return Outcome{}, fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, executionErr)
	}
	if err := s.cleanup(*claim, state, watchdog); err != nil {
		return Outcome{}, err
	}
	if executionErr != nil {
		return Outcome{}, executionErr
	}
	return Outcome{Worked: true, RunID: claim.RunID, AttemptID: claim.AttemptID, Result: result}, nil
}

func (s *Supervisor) reconcile(ctx context.Context) error {
	state, err := s.State.Load()
	if err != nil || state == nil {
		return err
	}
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
	if err := s.Client.AcknowledgeStopped(ctx, claim); err != nil {
		return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
	}
	return s.State.Clear()
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
	path, err := s.Workspaces.Prepare(g.ctx, claim, s.Client)
	if err != nil {
		return "", err
	}
	if path != s.Workspaces.Path(claim) {
		return "", errors.New("prepared workspace does not match attempt")
	}
	if s.Executor != nil {
		execution, err := s.Executor.Execute(g.ctx, claim, workspace.Sanitize(claim.Manifest), path)
		if err != nil {
			return "", err
		}
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
		return "", g.finishWithoutCreate(err)
	}
	if err := g.createStarted(); err != nil {
		return "", g.finishWithoutCreate(err)
	}
	process, err := s.Sandbox.Create(g.ctx, claim, workspace.Sanitize(claim.Manifest), path)
	if err != nil {
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
	if err := g.ctx.Err(); err != nil {
		return "", err
	}
	if err := process.Start(g.ctx); err != nil {
		return "", err
	}
	exitCode, output, err := process.Wait(g.ctx)
	if err != nil {
		return "", err
	}
	if err := g.ctx.Err(); err != nil {
		return "", err
	}
	execution, err := DecodeExecution(claim, exitCode, output)
	if err != nil {
		return "", err
	}
	return s.publishExecution(g, claim, execution)
}

func (s *Supervisor) publishExecution(g *leaseGuard, claim protocol.Claim, execution Execution) (string, error) {
	if execution.Result == nil {
		return "", errors.New("native execution returned no result")
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
	return execution.Result["outcome"].(string), nil
}

func (s *Supervisor) cleanup(claim protocol.Claim, state attemptstate.State, watchdog WatchdogLease) error {
	// Revoked execution authority cannot cancel mandatory cleanup.
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if s.Executor != nil {
		if err := s.Executor.Cleanup(ctx, claim); err != nil {
			return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
		}
	} else if state.SandboxID != nil {
		if err := s.Sandbox.Reconcile(ctx, *state.SandboxID); err != nil {
			return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
		}
	}
	if watchdog != nil {
		if err := watchdog.Disarm(); err != nil {
			return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
		}
	}
	if err := s.Workspaces.Remove(state.Workspace); err != nil {
		return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
	}
	if err := s.Client.AcknowledgeStopped(ctx, claim); err != nil {
		return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
	}
	if err := s.State.Clear(); err != nil {
		return fmt.Errorf("%w: %w", ErrCleanupUnconfirmed, err)
	}
	return nil
}
