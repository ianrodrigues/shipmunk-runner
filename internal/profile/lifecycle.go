package profile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
)

const (
	operationHeartbeatInterval = 10 * time.Second
	operationDuration          = 15 * time.Minute
	profileCleanupTimeout      = 90 * time.Second
)

// ControlPlane is the profile-scoped subset implemented by protocol.HTTPClient.
type ControlPlane interface {
	ProfileRequest(context.Context, string, string, map[string]any) (map[string]any, error)
}

// WatchdogLease remains alive independently of the lifecycle process and owns
// cleanup when its control channel closes or its renewed lease expires.
type WatchdogLease interface {
	CreatePhase
	Renew(time.Time) error
	Disarm() error
}

type Watchdog interface {
	Arm(string, time.Time, time.Time) (WatchdogLease, error)
}

// SandboxWatchdog adapts the repository watchdog to profile ownership mode.
type SandboxWatchdog struct {
	Watchdog *sandbox.Watchdog
}

func (watchdog SandboxWatchdog) Arm(name string, lease, deadline time.Time) (WatchdogLease, error) {
	if watchdog.Watchdog == nil {
		return nil, errors.New("profile watchdog is unavailable")
	}
	return watchdog.Watchdog.ArmProfile(name, lease, deadline)
}

// Lifecycle runs serialized profile operations and recovers their durable
// pending journal before starting any new operation.
type Lifecycle struct {
	ControlPlane ControlPlane
	Runtime      Runtime
	Watchdog     Watchdog
	Diagnostic   func(string)

	heartbeatInterval time.Duration
	operationDuration time.Duration
	cleanupTimeout    time.Duration
}

func NewLifecycle(controlPlane ControlPlane, runtime Runtime, watchdog Watchdog) *Lifecycle {
	return &Lifecycle{
		ControlPlane:      controlPlane,
		Runtime:           runtime,
		Watchdog:          watchdog,
		heartbeatInterval: operationHeartbeatInterval,
		operationDuration: operationDuration,
		cleanupTimeout:    profileCleanupTimeout,
	}
}

// Operate performs login, probe, or disconnect under the profile's OS lock.
// Native and transport detail is reduced to a health reason before it can be
// persisted or sent to the application.
func (lifecycle *Lifecycle) Operate(
	ctx context.Context,
	store *Store,
	profileID string,
	operation string,
	operationID string,
) (Health, error) {
	if ctx == nil || store == nil || lifecycle.ControlPlane == nil || lifecycle.Runtime == nil || lifecycle.Watchdog == nil {
		return Health{}, errors.New("profile lifecycle dependencies are incomplete")
	}
	if err := protocol.ValidateProfileID(profileID); err != nil || store.ProfileID() != profileID {
		return Health{}, errors.New("profile store identity mismatch")
	}
	if err := protocol.ValidateOperationID(operationID); err != nil {
		return Health{}, err
	}
	if operation != "login" && operation != "probe" && operation != "disconnect" {
		return Health{}, errors.New("unsupported profile operation")
	}

	var outcome Health
	err := store.WithExclusive(func(locked *Store) error {
		execution, err := locked.Read("execution")
		if err != nil {
			return err
		}
		if execution != nil {
			return errors.New("profile execution requires stopped recovery")
		}
		if err := lifecycle.recover(ctx, locked, profileID); err != nil {
			return err
		}
		completed, err := locked.Read("completed")
		if err != nil {
			return err
		}
		if completedID, ok := completed["operation_id"].(string); ok && completedID == operationID {
			return errors.New("a completed profile operation cannot be replayed")
		}

		pending := map[string]any{
			"profile_id":   profileID,
			"operation_id": operationID,
			"operation":    operation,
			"sandbox":      "shipmunk-profile-" + operationID,
		}
		// Journal before begin so a lost response never hides a server reservation.
		if err := locked.Write("pending", pending); err != nil {
			return err
		}
		binding, err := lifecycle.begin(ctx, profileID, pending)
		if err != nil {
			var responseError *protocol.ControlPlaneError
			if errors.As(err, &responseError) && definitiveBeginStatus(responseError.StatusCode) {
				if forgetErr := locked.Forget("pending"); forgetErr != nil {
					return errors.Join(err, forgetErr)
				}
			}
			return err
		}
		lease, validationErr := validateBinding(binding, profileID, operationID, time.Now())
		if validationErr != nil {
			if rejectErr := lifecycle.rejectBegin(ctx, locked, profileID, pending, binding); rejectErr != nil {
				return errors.Join(validationErr, rejectErr)
			}
			return validationErr
		}
		pending["binding"] = binding
		if err := locked.Write("pending", pending); err != nil {
			return err
		}

		outcome = Health{Health: HealthError, Reason: "operation_failed"}
		stage := "credential_invalidation"
		var watchdogLease WatchdogLease
		if operation == "disconnect" {
			outcome = Health{Health: "disconnected", Reason: "disconnected"}
		} else {
			operationDeadline := time.Now().Add(lifecycle.operationDuration)
			operationContext, cancel := context.WithDeadline(ctx, operationDeadline)
			outcome, stage, watchdogLease, err = lifecycle.authenticate(operationContext, locked, pending, binding, lease, operationDeadline, stage)
			cancel()
			if err != nil {
				lifecycle.reportFailure(stage)
				outcome = Health{Health: HealthError, Reason: "operation_failed"}
			}
		}

		if err := lifecycle.stopPending(locked, pending, watchdogLease); err != nil {
			return err
		}
		if outcome.Health != HealthReady {
			if err := locked.Invalidate(); err != nil {
				return err
			}
		} else if err := locked.NormalizeNativeHome(); err != nil {
			lifecycle.reportFailure("post_auth_home_validation")
			outcome = Health{Health: HealthError, Reason: "operation_failed"}
			if invalidateErr := locked.Invalidate(); invalidateErr != nil {
				return errors.Join(err, invalidateErr)
			}
		}
		pending["outcome"] = healthJournal(outcome)
		if err := locked.Write("pending", pending); err != nil {
			return err
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), lifecycle.cleanupTimeout)
		response, err := lifecycle.complete(cleanupCtx, profileID, pending, outcome)
		cleanupCancel()
		if err != nil {
			return err
		}
		if err := validateCompletionResponse(response, pending, outcome); err != nil {
			return err
		}
		if outcome.Health == HealthReady && isTrue(response["active"]) {
			if err := locked.Write("active", identityJournal(binding)); err != nil {
				return err
			}
		} else {
			if err := locked.Invalidate(); err != nil {
				return err
			}
			if outcome.Health == HealthReady {
				outcome = Health{Health: HealthError, Reason: "operation_stopped"}
			}
		}
		if err := locked.Write("completed", map[string]any{"operation_id": operationID}); err != nil {
			return err
		}
		return locked.Forget("pending")
	})
	return outcome, err
}

func (lifecycle *Lifecycle) authenticate(
	ctx context.Context,
	store *Store,
	pending map[string]any,
	binding map[string]any,
	leaseExpiry time.Time,
	deadline time.Time,
	stage string,
) (Health, string, WatchdogLease, error) {
	operation, _ := pending["operation"].(string)
	agent, _ := binding["agent"].(string)
	authMode, _ := binding["auth_mode"].(string)
	runtimeVersion, _ := binding["runtime_version"].(string)
	if authMode != AuthSubscription {
		return Health{Health: HealthUnsupported, Reason: ReasonNativeModeMismatch}, stage, nil, nil
	}
	pinned, supported := PinnedVersion(agent)
	if !supported || runtimeVersion != pinned {
		return Health{Health: HealthUnsupported, Reason: ReasonRuntimeMismatch}, stage, nil, nil
	}
	stage = "credential_reference"
	credentialReference, _ := binding["credential_reference"].(string)
	if !credentialReferencePattern.MatchString(credentialReference) {
		return Health{}, stage, nil, errors.New("credential reference is invalid")
	}
	stage = "credential_home_preparation"
	if operation == "login" {
		if err := store.Invalidate(); err != nil {
			return Health{}, stage, nil, err
		}
		home, err := store.CreateHome()
		if err != nil {
			return Health{}, stage, nil, err
		}
		if err := initializeNativeHome(home); err != nil {
			return Health{}, stage, nil, err
		}
	} else {
		active, err := store.Read("active")
		if err != nil {
			return Health{}, stage, nil, err
		}
		if !reflect.DeepEqual(identityJournalFromMap(active), identityJournal(binding)) {
			return Health{Health: HealthExpired, Reason: ReasonNativeLoginRequired}, stage, nil, nil
		}
	}
	if err := store.Forget("active"); err != nil {
		return Health{}, stage, nil, err
	}
	stage = "credential_home_validation"
	if err := store.ValidateHome(); err != nil {
		return Health{}, stage, nil, err
	}

	sandboxName, ok := pending["sandbox"].(string)
	if !ok {
		return Health{}, stage, nil, errors.New("profile sandbox identity is invalid")
	}
	_, ok = ctx.Deadline()
	if !ok {
		return Health{}, stage, nil, errors.New("profile operation deadline is missing")
	}
	stage = "sandbox_watchdog_arm"
	watchdogLease, err := lifecycle.Watchdog.Arm(sandboxName, leaseExpiry, deadline)
	if err != nil {
		return Health{}, stage, nil, err
	}
	stage = "sandbox_start"
	checkpoint := newCheckpoint(lifecycle, ctx, pending, binding, leaseExpiry, deadline, watchdogLease)
	checkpoint.setStage(stage)
	// Persist the create intent immediately before Start so a crash or lost
	// Docker response cannot make recovery treat an invisible container as gone.
	pending["create_attempted"] = true
	if err := store.Write("pending", pending); err != nil {
		_ = watchdogLease.Disarm()
		return Health{}, checkpoint.stage(), nil, err
	}
	if err := lifecycle.Runtime.Start(ctx, sandboxName, store.Home(), checkpoint.callback, watchdogLease); err != nil {
		if !errors.Is(err, ErrCreateUncertain) {
			pending["create_attempted"] = false
			if persistErr := store.Write("pending", pending); persistErr != nil {
				return Health{}, checkpoint.stage(), watchdogLease, errors.Join(err, persistErr)
			}
		}
		return Health{}, checkpoint.stage(), watchdogLease, err
	}
	pending["create_attempted"] = false
	if err := store.Write("pending", pending); err != nil {
		return Health{}, checkpoint.stage(), watchdogLease, err
	}
	checkpoint.setLease(leaseExpiry)
	if err := checkpoint.run(false); err != nil {
		return Health{}, checkpoint.stage(), watchdogLease, err
	}
	stage = "native_version"
	checkpoint.setStage(stage)
	versionResult, err := lifecycle.Runtime.Run(ctx, sandboxName, agent, "version", checkpoint.callback)
	if err != nil {
		return Health{}, checkpoint.stage(), watchdogLease, err
	}
	if !VersionMatches(agent, runtimeVersion, versionResult) {
		return Health{Health: HealthUnsupported, Reason: ReasonRuntimeMismatch}, stage, watchdogLease, nil
	}
	if operation == "login" {
		stage = "native_login"
		checkpoint.setStage(stage)
		loginResult, err := lifecycle.Runtime.Run(ctx, sandboxName, agent, "login", checkpoint.callback)
		if err != nil {
			return Health{}, checkpoint.stage(), watchdogLease, err
		}
		if loginResult.ExitCode != 0 {
			return Health{Health: HealthExpired, Reason: ReasonNativeLoginRequired}, stage, watchdogLease, nil
		}
	}
	stage = "native_probe"
	checkpoint.setStage(stage)
	probeResult, err := lifecycle.Runtime.Run(ctx, sandboxName, agent, "probe", checkpoint.callback)
	if err != nil {
		return Health{}, checkpoint.stage(), watchdogLease, err
	}
	health := StatusHealth(agent, probeResult)
	if health.Health == HealthReady {
		stage = "native_preflight"
		checkpoint.setStage(stage)
		preflight, err := lifecycle.Runtime.Run(ctx, sandboxName, agent, "preflight", checkpoint.callback)
		if err != nil {
			return Health{}, checkpoint.stage(), watchdogLease, err
		}
		health = AuthenticatedHealth(agent, preflight)
		if health.Health == HealthReady {
			stage = "post_auth_probe"
			checkpoint.setStage(stage)
			probeResult, err = lifecycle.Runtime.Run(ctx, sandboxName, agent, "probe", checkpoint.callback)
			if err != nil {
				return Health{}, checkpoint.stage(), watchdogLease, err
			}
			health = StatusHealth(agent, probeResult)
		}
	}
	checkpoint.setStage(stage)
	if err := checkpoint.run(true); err != nil {
		return Health{}, checkpoint.stage(), watchdogLease, err
	}
	return health, stage, watchdogLease, nil
}

type operationCheckpoint struct {
	lifecycle *Lifecycle
	ctx       context.Context
	pending   map[string]any
	binding   map[string]any
	lease     time.Time
	deadline  time.Time
	watchdog  WatchdogLease
	last      time.Time
	stageName string
}

func newCheckpoint(
	lifecycle *Lifecycle,
	ctx context.Context,
	pending, binding map[string]any,
	lease, deadline time.Time,
	watchdog WatchdogLease,
) *operationCheckpoint {
	return &operationCheckpoint{
		lifecycle: lifecycle,
		ctx:       ctx,
		pending:   pending,
		binding:   binding,
		lease:     lease,
		deadline:  deadline,
		watchdog:  watchdog,
	}
}

func (checkpoint *operationCheckpoint) callback() error {
	return checkpoint.run(false)
}

func (checkpoint *operationCheckpoint) run(force bool) error {
	now := time.Now()
	if !now.Before(checkpoint.deadline) {
		checkpoint.stageName = "heartbeat"
		return errors.New("profile operation deadline expired")
	}
	if !now.Before(checkpoint.lease) {
		checkpoint.stageName = "heartbeat"
		return errors.New("profile operation lease expired")
	}
	if !force && !checkpoint.last.IsZero() && now.Sub(checkpoint.last) < checkpoint.lifecycle.heartbeatInterval {
		return nil
	}
	previous := checkpoint.stageName
	checkpoint.stageName = "heartbeat"
	profileID, _ := checkpoint.binding["profile_id"].(string)
	operationID, _ := checkpoint.pending["operation_id"].(string)
	renewal, err := checkpoint.lifecycle.ControlPlane.ProfileRequest(checkpoint.ctx, profileID, "operations/"+operationID+"/heartbeat", map[string]any{})
	if err != nil {
		return err
	}
	lease, err := validateBinding(renewal, profileID, operationID, time.Now())
	if err != nil {
		return err
	}
	if !sameStableBinding(renewal, checkpoint.binding) {
		return errors.New("profile binding changed during native authentication")
	}
	if err := checkpoint.watchdog.Renew(lease); err != nil {
		return err
	}
	checkpoint.lease = lease
	checkpoint.last = time.Now()
	checkpoint.stageName = previous
	return nil
}

func (checkpoint *operationCheckpoint) setLease(lease time.Time) { checkpoint.lease = lease }
func (checkpoint *operationCheckpoint) setStage(stage string)    { checkpoint.stageName = stage }
func (checkpoint *operationCheckpoint) stage() string            { return checkpoint.stageName }

func (lifecycle *Lifecycle) recover(ctx context.Context, store *Store, profileID string) error {
	pending, err := store.Read("pending")
	if err != nil || pending == nil {
		return err
	}
	if err := validatePendingJournal(pending, profileID); err != nil {
		return err
	}
	journalProfileID, _ := pending["profile_id"].(string)
	if journalProfileID != profileID {
		return errors.New("profile journal identity mismatch")
	}
	operationID, ok := pending["operation_id"].(string)
	sandboxName, sandboxOK := pending["sandbox"].(string)
	if !ok || protocol.ValidateOperationID(operationID) != nil || !sandboxOK || sandboxName != "shipmunk-profile-"+operationID {
		return errors.New("profile journal sandbox mismatch")
	}
	if err := lifecycle.stopPending(store, pending, nil); err != nil {
		return err
	}
	rawBinding, bindingExists := pending["binding"]
	binding, hasBinding := rawBinding.(map[string]any)
	if bindingExists && rawBinding != nil && !hasBinding {
		return errors.New("profile journal binding is invalid")
	}
	if !hasBinding {
		binding, err = lifecycle.begin(ctx, profileID, pending)
		if err != nil {
			return err
		}
		if _, err := validateBinding(binding, profileID, operationID, time.Now()); err != nil {
			if rejectErr := lifecycle.rejectBegin(ctx, store, profileID, pending, binding); rejectErr != nil {
				return errors.Join(err, rejectErr)
			}
			return err
		}
		pending["binding"] = binding
	}
	outcome, exists := healthFromJournal(pending["outcome"])
	if _, outcomePresent := pending["outcome"]; !outcomePresent {
		outcome = Health{Health: HealthError, Reason: "operation_stopped"}
	} else if !exists {
		return errors.New("profile journal outcome is invalid")
	}
	recoveryFailed := isTrue(pending["recovery_failed"])
	if outcome.Health == HealthReady {
		if err := store.ValidateHome(); err != nil {
			outcome = Health{Health: HealthError, Reason: "operation_stopped"}
			pending["outcome"] = healthJournal(outcome)
			pending["recovery_failed"] = true
			if err := store.Write("pending", pending); err != nil {
				return err
			}
			if err := store.Invalidate(); err != nil {
				return err
			}
			recoveryFailed = true
		}
	} else if err := store.Invalidate(); err != nil {
		return err
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), lifecycle.cleanupTimeout)
	response, err := lifecycle.complete(cleanupCtx, profileID, pending, outcome)
	cleanupCancel()
	if err != nil {
		var responseError *protocol.ControlPlaneError
		if !errors.As(err, &responseError) || responseError.StatusCode != 404 {
			return err
		}
		response = map[string]any{
			"profile_id": profileID, "operation_id": operationID, "stopped": true,
			"active": false, "health": "disconnected", "reason": "disconnected",
		}
	}
	if err := validateCompletionResponse(response, pending, outcome); err != nil {
		return err
	}
	responseHealth, _ := response["health"].(string)
	if recoveryFailed && (!isTrue(response["stopped"]) || isTrue(response["active"]) ||
		(responseHealth != HealthError && responseHealth != "disconnected")) {
		return errors.New("failed credential recovery requires confirmed profile disconnection")
	}
	if outcome.Health == HealthReady && isTrue(response["active"]) {
		if err := store.Write("active", identityJournal(binding)); err != nil {
			return err
		}
	} else if err := store.Invalidate(); err != nil {
		return err
	}
	if err := store.Write("completed", map[string]any{"operation_id": operationID}); err != nil {
		return err
	}
	return store.Forget("pending")
}

func (lifecycle *Lifecycle) rejectBegin(
	ctx context.Context,
	store *Store,
	profileID string,
	pending, binding map[string]any,
) error {
	operationID, _ := pending["operation_id"].(string)
	bindingProfileID, _ := binding["profile_id"].(string)
	bindingOperationID, _ := binding["operation_id"].(string)
	if bindingProfileID != profileID || bindingOperationID != operationID {
		return nil
	}
	stopped, stoppedOK := binding["stopped"].(bool)
	if stoppedOK && stopped {
		return store.Forget("pending")
	}
	version, _ := binding["runtime_version"].(string)
	if !stoppedOK || stopped || version == "" {
		return nil
	}
	pending["binding"] = binding
	if err := store.Write("pending", pending); err != nil {
		return err
	}
	if err := lifecycle.stopPending(store, pending, nil); err != nil {
		return err
	}
	if err := store.Invalidate(); err != nil {
		return err
	}
	outcome := Health{Health: HealthError, Reason: "operation_stopped"}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), lifecycle.cleanupTimeout)
	response, completionErr := lifecycle.complete(cleanupCtx, profileID, pending, outcome)
	cleanupCancel()
	if completionErr != nil {
		return completionErr
	}
	if err := validateCompletionResponse(response, pending, outcome); err != nil {
		return err
	}
	if err := store.Write("completed", map[string]any{"operation_id": operationID}); err != nil {
		return err
	}
	return store.Forget("pending")
}

func (lifecycle *Lifecycle) begin(ctx context.Context, profileID string, pending map[string]any) (map[string]any, error) {
	operation, _ := pending["operation"].(string)
	operationID, _ := pending["operation_id"].(string)
	return lifecycle.ControlPlane.ProfileRequest(ctx, profileID, "operations", map[string]any{
		"operation": operation, "operation_id": operationID,
	})
}

func (lifecycle *Lifecycle) complete(
	ctx context.Context,
	profileID string,
	pending map[string]any,
	outcome Health,
) (map[string]any, error) {
	operationID, _ := pending["operation_id"].(string)
	binding, _ := pending["binding"].(map[string]any)
	runtimeVersion, _ := binding["runtime_version"].(string)
	if runtimeVersion == "" {
		return nil, errors.New("profile runtime version is missing")
	}
	payload := healthJournal(outcome)
	payload["stopped"] = true
	payload["runtime_version"] = runtimeVersion
	return lifecycle.ControlPlane.ProfileRequest(ctx, profileID, "operations/"+operationID+"/completion", payload)
}

func (lifecycle *Lifecycle) stopAndDisarm(sandboxName string, createMayBeInFlight bool, lease WatchdogLease) error {
	ctx, cancel := context.WithTimeout(context.Background(), lifecycle.cleanupTimeout)
	defer cancel()
	if createMayBeInFlight {
		if err := lifecycle.Runtime.ReconcileCreate(ctx, sandboxName); err != nil {
			return fmt.Errorf("profile create reconciliation is unconfirmed: %w", err)
		}
	}
	if err := lifecycle.Runtime.Stop(ctx, sandboxName, false); err != nil {
		return fmt.Errorf("profile container cleanup is unconfirmed: %w", err)
	}
	if lease != nil {
		if err := lease.Disarm(); err != nil {
			return fmt.Errorf("profile watchdog cleanup is unconfirmed: %w", err)
		}
	}
	return nil
}

func (lifecycle *Lifecycle) stopPending(store *Store, pending map[string]any, lease WatchdogLease) error {
	createMayBeInFlight := isTrue(pending["create_attempted"])
	if err := lifecycle.stopAndDisarm(pending["sandbox"].(string), createMayBeInFlight, lease); err != nil {
		return err
	}
	if createMayBeInFlight {
		pending["create_attempted"] = false
		if err := store.Write("pending", pending); err != nil {
			return err
		}
	}
	return nil
}

func (lifecycle *Lifecycle) reportFailure(stage string) {
	if lifecycle.Diagnostic == nil {
		return
	}
	defer func() { _ = recover() }()
	lifecycle.Diagnostic(stage)
}

func validateBinding(binding map[string]any, profileID, operationID string, now time.Time) (time.Time, error) {
	if err := validateJournalBinding(binding, profileID, operationID); err != nil {
		return time.Time{}, err
	}
	responseProfileID, profileIDOK := binding["profile_id"].(string)
	responseOperationID, operationIDOK := binding["operation_id"].(string)
	stopRequested, stopOK := binding["stop_requested"].(bool)
	if !profileIDOK || responseProfileID != profileID || !operationIDOK || responseOperationID != operationID || !stopOK || stopRequested {
		return time.Time{}, errors.New("profile operation is no longer authorized")
	}
	if runtimeVersion, ok := binding["runtime_version"].(string); !ok || runtimeVersion == "" {
		return time.Time{}, errors.New("profile runtime version is missing")
	}
	leaseText, ok := binding["lease_expires_at"].(string)
	if !ok {
		return time.Time{}, errors.New("profile operation lease is invalid")
	}
	lease, err := time.Parse(time.RFC3339Nano, leaseText)
	if err != nil || !lease.After(now) {
		return time.Time{}, errors.New("profile operation lease is invalid or expired")
	}
	return lease, nil
}

func sameStableBinding(left, right map[string]any) bool {
	return reflect.DeepEqual(stableBindingFields(left), stableBindingFields(right))
}

func stableBindingFields(binding map[string]any) map[string]any {
	fields := []string{"profile_id", "credential_reference", "agent", "auth_mode", "runtime_version"}
	identity := make(map[string]any, len(fields))
	for _, field := range fields {
		identity[field] = binding[field]
	}
	return identity
}

func identityJournal(binding map[string]any) map[string]any {
	return stableBindingFields(binding)
}

func identityJournalFromMap(binding map[string]any) map[string]any {
	if binding == nil {
		return nil
	}
	return stableBindingFields(binding)
}

func healthJournal(health Health) map[string]any {
	var reason any
	if health.Reason != "" {
		reason = health.Reason
	}
	return map[string]any{"health": health.Health, "reason": reason}
}

func healthFromJournal(value any) (Health, bool) {
	data, ok := value.(map[string]any)
	if !ok {
		return Health{}, false
	}
	for key := range data {
		if key != "health" && key != "reason" {
			return Health{}, false
		}
	}
	health, ok := data["health"].(string)
	if !ok || health == "" {
		return Health{}, false
	}
	if _, exists := data["reason"]; !exists {
		return Health{}, false
	}
	reason := ""
	if raw, exists := data["reason"]; exists && raw != nil {
		var reasonOK bool
		reason, reasonOK = raw.(string)
		if !reasonOK {
			return Health{}, false
		}
	}
	result := Health{Health: health, Reason: reason}
	return result, validHealthOutcome(result)
}

func validatePendingJournal(pending map[string]any, profileID string) error {
	allowed := map[string]struct{}{
		"profile_id": {}, "operation_id": {}, "operation": {}, "sandbox": {},
		"binding": {}, "outcome": {}, "create_attempted": {}, "recovery_failed": {},
	}
	for key := range pending {
		if _, ok := allowed[key]; !ok {
			return errors.New("profile journal contains an unknown field")
		}
	}
	if pendingProfileID, ok := pending["profile_id"].(string); !ok || pendingProfileID != profileID {
		return errors.New("profile journal identity mismatch")
	}
	operationID, ok := pending["operation_id"].(string)
	if !ok || protocol.ValidateOperationID(operationID) != nil {
		return errors.New("profile journal operation identity is invalid")
	}
	sandboxName, ok := pending["sandbox"].(string)
	if !ok || sandboxName != "shipmunk-profile-"+operationID {
		return errors.New("profile journal sandbox mismatch")
	}
	operation, ok := pending["operation"].(string)
	if !ok || (operation != "login" && operation != "probe" && operation != "disconnect") {
		return errors.New("profile journal operation is invalid")
	}
	createAttempted, hasCreateAttempt := pending["create_attempted"]
	if hasCreateAttempt {
		if _, ok := createAttempted.(bool); !ok {
			return errors.New("profile journal create phase is invalid")
		}
	}
	recoveryFailed, hasRecoveryFailed := pending["recovery_failed"]
	if hasRecoveryFailed {
		if _, ok := recoveryFailed.(bool); !ok {
			return errors.New("profile journal recovery phase is invalid")
		}
	}
	rawBinding, hasBinding := pending["binding"]
	binding, bindingOK := rawBinding.(map[string]any)
	if hasBinding && (!bindingOK || binding == nil) {
		return errors.New("profile journal binding is invalid")
	}
	if hasBinding {
		if err := validateJournalBinding(binding, profileID, operationID); err != nil {
			return err
		}
	}
	rawOutcome, hasOutcome := pending["outcome"]
	if hasOutcome {
		if _, ok := healthFromJournal(rawOutcome); !ok {
			return errors.New("profile journal outcome is invalid")
		}
		if !hasBinding {
			return errors.New("profile journal outcome has no binding")
		}
	}
	if hasCreateAttempt && isTrue(createAttempted) && !hasBinding {
		return errors.New("profile journal create phase has no binding")
	}
	if isTrue(recoveryFailed) && (!hasOutcome || !hasBinding) {
		return errors.New("profile journal recovery failure phase is incomplete")
	}
	if isTrue(recoveryFailed) {
		outcome, _ := healthFromJournal(rawOutcome)
		if outcome != (Health{Health: HealthError, Reason: "operation_stopped"}) {
			return errors.New("profile journal recovery failure outcome is invalid")
		}
	}
	return nil
}

func validateJournalBinding(binding map[string]any, profileID, operationID string) error {
	if bindingProfileID, ok := binding["profile_id"].(string); !ok || bindingProfileID != profileID {
		return errors.New("profile journal binding identity is invalid")
	}
	if bindingOperationID, ok := binding["operation_id"].(string); !ok || bindingOperationID != operationID {
		return errors.New("profile journal binding operation is invalid")
	}
	credentialReference, ok := binding["credential_reference"].(string)
	if !ok || !credentialReferencePattern.MatchString(credentialReference) {
		return errors.New("profile journal credential reference is invalid")
	}
	if agent, ok := binding["agent"].(string); !ok || agent == "" {
		return errors.New("profile journal agent is invalid")
	}
	if authMode, ok := binding["auth_mode"].(string); !ok || authMode == "" {
		return errors.New("profile journal auth mode is invalid")
	}
	if runtimeVersion, ok := binding["runtime_version"].(string); !ok || runtimeVersion == "" {
		return errors.New("profile journal runtime version is invalid")
	}
	if _, ok := binding["stop_requested"].(bool); !ok {
		return errors.New("profile journal stop state is invalid")
	}
	leaseText, ok := binding["lease_expires_at"].(string)
	if !ok {
		return errors.New("profile journal lease is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, leaseText); err != nil {
		return errors.New("profile journal lease is invalid")
	}
	if stopped, exists := binding["stopped"]; exists {
		if _, ok := stopped.(bool); !ok {
			return errors.New("profile journal stopped state is invalid")
		}
	}
	return nil
}

func validHealthOutcome(outcome Health) bool {
	switch outcome.Health {
	case HealthReady:
		return outcome.Reason == ""
	case HealthExpired:
		return outcome.Reason == ReasonNativeLoginRequired
	case HealthUnsupported:
		return outcome.Reason == ReasonNativeModeMismatch || outcome.Reason == ReasonRuntimeMismatch
	case HealthRateLimited:
		return outcome.Reason == ReasonRateLimited
	case HealthError:
		return outcome.Reason == ReasonNativeProbeFailed || outcome.Reason == "operation_failed" || outcome.Reason == "operation_stopped"
	case "disconnected":
		return outcome.Reason == "disconnected"
	default:
		return false
	}
}

func validateCompletionResponse(response, pending map[string]any, submitted Health) error {
	operationID, _ := pending["operation_id"].(string)
	responseOperationID, operationOK := response["operation_id"].(string)
	stopped, stoppedOK := response["stopped"].(bool)
	active, activeOK := response["active"].(bool)
	health, healthOK := response["health"].(string)
	reasonValue, reasonExists := response["reason"]
	reason, reasonOK := reasonValue.(string)
	if reasonValue == nil {
		reasonOK = true
		reason = ""
	}
	if !operationOK || responseOperationID != operationID || !stoppedOK || !stopped || !activeOK ||
		!healthOK || !reasonExists || !reasonOK || !validCompletionHealth(Health{Health: health, Reason: reason}) {
		return errors.New("profile completion response is invalid")
	}
	if health == HealthReady && submitted.Health != HealthReady {
		return errors.New("profile completion response is inconsistent")
	}
	if active && (submitted.Health != HealthReady || health != HealthReady || reason != "") {
		return errors.New("profile completion response is inconsistent")
	}
	return nil
}

func validCompletionHealth(outcome Health) bool {
	if validHealthOutcome(outcome) {
		return true
	}
	switch outcome.Health {
	case HealthError:
		return outcome.Reason == "lease_expired"
	case "disconnected":
		return outcome.Reason == "profile_revoked" || outcome.Reason == "operation_stopped"
	default:
		return false
	}
}

func definitiveBeginStatus(status int) bool {
	switch status {
	case 401, 403, 404, 409, 422, 426:
		return true
	default:
		return false
	}
}

func initializeNativeHome(home string) error {
	for _, directory := range []string{".codex", filepath.Join(".codex", "tmp"), ".claude", ".config", ".cache"} {
		path := filepath.Join(home, directory)
		if err := os.Mkdir(path, 0700); err != nil {
			return errors.New("cannot create native configuration directory")
		}
		if err := os.Chmod(path, 0700); err != nil {
			return errors.New("cannot protect native configuration directory")
		}
	}
	for path, contents := range map[string]string{
		filepath.Join(home, ".codex", "config.toml"):    "cli_auth_credentials_store = \"file\"\nforced_login_method = \"chatgpt\"\n",
		filepath.Join(home, ".claude", "settings.json"): `{"forceLoginMethod":"claudeai"}`,
	} {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return errors.New("cannot write native configuration file")
		}
		written, writeErr := file.WriteString(contents)
		chmodErr := file.Chmod(0600)
		closeErr := file.Close()
		if writeErr != nil || chmodErr != nil || closeErr != nil || written != len(contents) {
			return errors.New("cannot write native configuration file")
		}
	}
	return nil
}

var credentialReferencePattern = regexp.MustCompile(`^credential:[A-Za-z0-9_-]+$`)

func isTrue(value any) bool {
	boolean, ok := value.(bool)
	return ok && boolean
}
