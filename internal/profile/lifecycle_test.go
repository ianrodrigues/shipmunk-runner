package profile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const (
	operationOne   = "01k4w000000000000000000002"
	operationTwo   = "01k4w000000000000000000003"
	operationThree = "01k4w000000000000000000004"
)

type lifecycleCall struct {
	suffix  string
	payload map[string]any
}

type fakeProfileControlPlane struct {
	mu            sync.Mutex
	profileID     string
	operationID   string
	binding       map[string]any
	response      map[string]any
	calls         []lifecycleCall
	beginErr      error
	heartbeatErr  error
	completionErr error
	onHeartbeat   func()
	log           *[]string
}

func newFakeProfileControlPlane() *fakeProfileControlPlane {
	return &fakeProfileControlPlane{
		profileID:   testProfileID,
		operationID: operationOne,
		binding: map[string]any{
			"profile_id":           testProfileID,
			"operation_id":         operationOne,
			"credential_reference": "credential:fixture",
			"agent":                AgentCodex,
			"auth_mode":            AuthSubscription,
			"runtime_version":      CodexVersion,
			"stop_requested":       false,
			"stopped":              false,
			"lease_expires_at":     time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		},
	}
}

func (control *fakeProfileControlPlane) ProfileRequest(_ context.Context, profileID, suffix string, payload map[string]any) (map[string]any, error) {
	control.mu.Lock()
	defer control.mu.Unlock()
	copyPayload := make(map[string]any, len(payload))
	for key, value := range payload {
		copyPayload[key] = value
	}
	control.calls = append(control.calls, lifecycleCall{suffix: suffix, payload: copyPayload})
	if control.log != nil {
		*control.log = append(*control.log, "http:"+suffix)
	}
	if profileID != control.profileID {
		return nil, errors.New("wrong profile ID")
	}
	switch {
	case suffix == "operations":
		if control.beginErr != nil {
			return nil, control.beginErr
		}
		binding := cloneMap(control.binding)
		binding["operation_id"] = payload["operation_id"]
		return binding, nil
	case strings.HasSuffix(suffix, "/heartbeat"):
		if control.heartbeatErr != nil {
			return nil, control.heartbeatErr
		}
		control.binding["lease_expires_at"] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
		if control.onHeartbeat != nil {
			control.onHeartbeat()
		}
		binding := cloneMap(control.binding)
		binding["operation_id"] = control.operationIDForSuffix(suffix)
		return binding, nil
	case strings.HasSuffix(suffix, "/completion"):
		if control.completionErr != nil {
			return nil, control.completionErr
		}
		response := cloneMap(control.response)
		if control.response == nil {
			response = map[string]any{
				"active": payload["health"] == HealthReady, "stopped": true,
				"health": payload["health"], "reason": payload["reason"],
			}
		}
		response["profile_id"] = profileID
		if _, exists := response["operation_id"]; !exists {
			response["operation_id"] = control.operationIDForSuffix(suffix)
		}
		return response, nil
	default:
		return nil, fmt.Errorf("unexpected profile suffix %q", suffix)
	}
}

func (control *fakeProfileControlPlane) operationIDForSuffix(suffix string) string {
	parts := strings.Split(suffix, "/")
	if len(parts) >= 2 {
		return parts[1]
	}
	return control.operationID
}

func (control *fakeProfileControlPlane) setBinding(values map[string]any) {
	control.mu.Lock()
	defer control.mu.Unlock()
	control.binding = cloneMap(values)
}

func (control *fakeProfileControlPlane) setCompletion(response map[string]any) {
	control.mu.Lock()
	defer control.mu.Unlock()
	control.response = cloneMap(response)
}

func (control *fakeProfileControlPlane) snapshotCalls() []lifecycleCall {
	control.mu.Lock()
	defer control.mu.Unlock()
	return append([]lifecycleCall(nil), control.calls...)
}

type fakeLifecycleRuntime struct {
	mu                sync.Mutex
	starts            int
	stops             int
	stopMayBeInFlight []bool
	commands          []string
	startErr          error
	stopErr           error
	stopInFlightErr   error
	reconcileErr      error
	reconciles        int
	runErrors         map[string]error
	runResults        map[string]CommandResult
	onRun             func(string)
	onStart           func()
	log               *[]string
}

func newFakeLifecycleRuntime() *fakeLifecycleRuntime {
	return &fakeLifecycleRuntime{
		runErrors: map[string]error{},
		runResults: map[string]CommandResult{
			"version": {ExitCode: 0, Stdout: "codex-cli " + CodexVersion},
			"login":   {ExitCode: 0},
			"probe":   {ExitCode: 0, Stdout: "Logged in using ChatGPT"},
			"preflight": {ExitCode: 0, Stdout: strings.Join([]string{
				`{"type":"thread.started","thread_id":"thread"}`,
				`{"type":"turn.started"}`,
				`{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"SHIPMUNK_AUTH_OK"}}`,
				`{"type":"turn.completed"}`,
				"",
			}, "\n")},
		},
	}
}

func (runtime *fakeLifecycleRuntime) Start(_ context.Context, _ string, _ string, checkpoint Checkpoint, _ CreatePhase) error {
	runtime.mu.Lock()
	runtime.starts++
	if runtime.log != nil {
		*runtime.log = append(*runtime.log, "runtime:start")
	}
	runtime.mu.Unlock()
	if runtime.onStart != nil {
		runtime.onStart()
	}
	if err := checkpoint(); err != nil {
		return err
	}
	return runtime.startErr
}

func (runtime *fakeLifecycleRuntime) Run(_ context.Context, _ string, _ string, operation string, checkpoint Checkpoint) (CommandResult, error) {
	if err := checkpoint(); err != nil {
		return CommandResult{}, err
	}
	runtime.mu.Lock()
	runtime.commands = append(runtime.commands, operation)
	if runtime.log != nil {
		*runtime.log = append(*runtime.log, "runtime:"+operation)
	}
	if runtime.onRun != nil {
		runtime.onRun(operation)
	}
	err := runtime.runErrors[operation]
	result := runtime.runResults[operation]
	runtime.mu.Unlock()
	return result, err
}

func (runtime *fakeLifecycleRuntime) Stop(ctx context.Context, _ string, createMayBeInFlight bool) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("cleanup context is not independent")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.stops++
	runtime.stopMayBeInFlight = append(runtime.stopMayBeInFlight, createMayBeInFlight)
	if runtime.log != nil {
		*runtime.log = append(*runtime.log, "runtime:stop")
	}
	if createMayBeInFlight && runtime.stopInFlightErr != nil {
		return runtime.stopInFlightErr
	}
	return runtime.stopErr
}

func (runtime *fakeLifecycleRuntime) ReconcileCreate(context.Context, string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.reconciles++
	return runtime.reconcileErr
}

func TestLifecycleUncertainCreateRetainsJournalAndRequiresConfirmedAbsence(t *testing.T) {
	store, lifecycle, control, runtime, _ := newLifecycleFixture(t)
	runtime.startErr = ErrCreateUncertain
	runtime.reconcileErr = ErrCreateUncertain
	_, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne)
	if err == nil {
		t.Fatal("uncertain create was not surfaced")
	}
	pending, readErr := store.Read("pending")
	if readErr != nil || pending == nil || !isTrue(pending["create_attempted"]) {
		t.Fatalf("uncertain create intent was not retained: %#v, %v", pending, readErr)
	}
	if runtime.reconciles != 1 || runtime.stops != 0 {
		t.Fatalf("cleanup did not require create reconciliation: reconciles=%d stops=%d", runtime.reconciles, runtime.stops)
	}
	for _, call := range control.snapshotCalls() {
		if strings.HasSuffix(call.suffix, "/completion") {
			t.Fatal("uncertain create was acknowledged before absence was confirmed")
		}
	}
}

func TestLifecycleCompletesUncertainCreateOnlyAfterReconciliation(t *testing.T) {
	store, lifecycle, _, runtime, _ := newLifecycleFixture(t)
	runtime.startErr = ErrCreateUncertain
	if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne); err != nil {
		t.Fatalf("Operate() = %v", err)
	}
	if runtime.reconciles != 1 || runtime.stops != 0 || len(runtime.stopMayBeInFlight) != 0 {
		t.Fatalf("create recovery calls = reconciles %d, stops %d flags %v", runtime.reconciles, runtime.stops, runtime.stopMayBeInFlight)
	}
	if pending, err := store.Read("pending"); err != nil || pending != nil {
		t.Fatalf("pending journal after reconciled completion = %#v, %v", pending, err)
	}
}

func TestLifecycleRevalidatesTombstoneAfterCrashBeforeCompletion(t *testing.T) {
	_, lifecycle, _, runtime, _ := newLifecycleFixture(t)
	pending := map[string]any{
		"sandbox":          "shipmunk-profile-" + operationOne,
		"create_attempted": true,
	}
	if err := lifecycle.stopPending(pending, nil); err != nil {
		t.Fatal(err)
	}
	if !isTrue(pending["create_attempted"]) {
		t.Fatal("tombstone phase was cleared before the pending journal was retired")
	}
	if err := lifecycle.stopPending(pending, nil); err != nil {
		t.Fatalf("recovery did not revalidate the retained tombstone: %v", err)
	}
	if runtime.reconciles != 2 || runtime.stops != 0 {
		t.Fatalf("tombstone recovery calls = reconciles %d, stops %d", runtime.reconciles, runtime.stops)
	}
}

func TestLifecycleClearsCreateIntentAfterDefinitiveStartFailure(t *testing.T) {
	store, lifecycle, _, runtime, _ := newLifecycleFixture(t)
	runtime.startErr = errors.New("checkpoint failed before create")
	_, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.stops != 1 || !reflect.DeepEqual(runtime.stopMayBeInFlight, []bool{false}) {
		t.Fatalf("definitive create failure used wrong cleanup mode: stops=%d flags=%v", runtime.stops, runtime.stopMayBeInFlight)
	}
}

type fakeProfileWatchdog struct {
	mu     sync.Mutex
	leases []*fakeProfileWatchdogLease
	err    error
}

func (watchdog *fakeProfileWatchdog) Arm(name string, lease, deadline time.Time) (WatchdogLease, error) {
	if name == "" || !lease.After(time.Now()) || !deadline.After(lease) {
		return nil, errors.New("invalid watchdog lease")
	}
	if watchdog.err != nil {
		return nil, watchdog.err
	}
	value := &fakeProfileWatchdogLease{}
	watchdog.mu.Lock()
	watchdog.leases = append(watchdog.leases, value)
	watchdog.mu.Unlock()
	return value, nil
}

type fakeProfileWatchdogLease struct {
	mu       sync.Mutex
	renewals []time.Time
	disarmed int
	err      error
}

func (lease *fakeProfileWatchdogLease) CreateStarted() error        { return nil }
func (lease *fakeProfileWatchdogLease) CreateFinished(string) error { return nil }

func (lease *fakeProfileWatchdogLease) Renew(expiry time.Time) error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	lease.renewals = append(lease.renewals, expiry)
	return lease.err
}

func (lease *fakeProfileWatchdogLease) Disarm() error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	lease.disarmed++
	return lease.err
}

func newLifecycleFixture(t *testing.T) (*Store, *Lifecycle, *fakeProfileControlPlane, *fakeLifecycleRuntime, *fakeProfileWatchdog) {
	t.Helper()
	store, _ := openTestStore(t)
	control := newFakeProfileControlPlane()
	runtime := newFakeLifecycleRuntime()
	watchdog := &fakeProfileWatchdog{}
	lifecycle := NewLifecycle(control, runtime, watchdog)
	lifecycle.heartbeatInterval = 0
	lifecycle.cleanupTimeout = time.Second
	return store, lifecycle, control, runtime, watchdog
}

func TestLifecycleLoginActivatesOnlyAfterAuthenticatedPreflightAndCleanup(t *testing.T) {
	store, lifecycle, control, runtime, watchdog := newLifecycleFixture(t)
	var order []string
	control.log = &order
	runtime.log = &order
	createIntentAtStart := false
	runtime.onStart = func() {
		pending, err := store.Read("pending")
		createIntentAtStart = err == nil && isTrue(pending["create_attempted"])
	}
	outcome, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne)
	if err != nil || outcome != (Health{Health: HealthReady}) {
		t.Fatalf("Operate() = (%#v, %v)", outcome, err)
	}
	if !reflect.DeepEqual(runtime.commands, []string{"version", "login", "probe", "preflight", "probe"}) {
		t.Fatalf("native command order = %#v", runtime.commands)
	}
	if runtime.starts != 1 || runtime.stops != 1 || len(watchdog.leases) != 1 || watchdog.leases[0].disarmed != 1 {
		t.Fatalf("runtime/watchdog lifecycle = starts %d stops %d leases %#v", runtime.starts, runtime.stops, watchdog.leases)
	}
	if !createIntentAtStart {
		t.Fatal("create intent was not durable before Runtime.Start")
	}
	active, err := store.Read("active")
	if err != nil || active == nil || active["credential_reference"] != "credential:fixture" {
		t.Fatalf("active identity = %#v, %v", active, err)
	}
	if pending, err := store.Read("pending"); err != nil || pending != nil {
		t.Fatalf("pending after completion = %#v, %v", pending, err)
	}
	if len(order) < 3 || order[len(order)-2] != "runtime:stop" || !strings.HasPrefix(order[len(order)-1], "http:operations/") {
		t.Fatalf("completion was not after cleanup: %#v", order)
	}
	completion := control.snapshotCalls()[len(control.snapshotCalls())-1]
	if completion.payload["stopped"] != true || completion.payload["health"] != HealthReady || completion.payload["runtime_version"] != CodexVersion {
		t.Fatalf("completion payload = %#v", completion.payload)
	}
	if reason, exists := completion.payload["reason"]; !exists || reason != nil {
		t.Fatalf("ready completion reason = %#v (present %v)", reason, exists)
	}
	if err := store.WithExclusive(func(locked *Store) error { return locked.ValidateHome() }); err != nil {
		t.Fatalf("home was not normalized before activation: %v", err)
	}
}

func TestLifecycleReturnsStoppedErrorWhenReadyCredentialsAreNotActivated(t *testing.T) {
	store, lifecycle, control, _, _ := newLifecycleFixture(t)
	control.setCompletion(map[string]any{
		"profile_id": testProfileID, "active": false, "stopped": true,
		"health": HealthError, "reason": "operation_stopped",
	})
	outcome, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne)
	if err != nil || outcome != (Health{Health: HealthError, Reason: "operation_stopped"}) {
		t.Fatalf("Operate() = (%#v, %v)", outcome, err)
	}
	completion := control.snapshotCalls()[len(control.snapshotCalls())-1]
	if completion.payload["health"] != HealthReady || completion.payload["reason"] != nil {
		t.Fatalf("completion payload did not preserve the authenticated outcome: %#v", completion.payload)
	}
	if active, err := store.Read("active"); err != nil || active != nil {
		t.Fatalf("inactive completion retained credentials: %#v, %v", active, err)
	}
}

func TestLifecyclePersistsBeforeBeginAndDistinguishesDefinitiveErrors(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		wantPending bool
	}{
		{name: "authorization rejection", status: 403, wantPending: false},
		{name: "ambiguous server error", status: 500, wantPending: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, lifecycle, control, _, _ := newLifecycleFixture(t)
			control.beginErr = &protocol.ControlPlaneError{StatusCode: test.status}
			_, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne)
			if err == nil {
				t.Fatal("begin error was swallowed")
			}
			pending, readErr := store.Read("pending")
			if readErr != nil || (pending != nil) != test.wantPending {
				t.Fatalf("pending = %#v, %v", pending, readErr)
			}
			calls := control.snapshotCalls()
			if len(calls) != 1 || calls[0].suffix != "operations" || calls[0].payload["operation_id"] != operationOne {
				t.Fatalf("begin calls = %#v", calls)
			}
		})
	}
}

func TestLifecycleRejectsMalformedRecoveryJournalWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unsupported operation", mutate: func(pending map[string]any) { pending["operation"] = "execute" }},
		{name: "unknown outcome health", mutate: func(pending map[string]any) {
			pending["outcome"] = map[string]any{"health": "mystery", "reason": nil}
		}},
		{name: "malformed outcome", mutate: func(pending map[string]any) { pending["outcome"] = "ready" }},
		{name: "unknown outcome reason", mutate: func(pending map[string]any) {
			pending["outcome"] = map[string]any{"health": HealthError, "reason": "private-error"}
		}},
		{name: "invalid create phase", mutate: func(pending map[string]any) { pending["create_attempted"] = "yes" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, lifecycle, control, runtime, _ := newLifecycleFixture(t)
			home := createTestHome(t, store)
			marker := filepath.Join(home, "must-remain")
			if err := os.WriteFile(marker, []byte("secret"), 0600); err != nil {
				t.Fatal(err)
			}
			pending := map[string]any{
				"profile_id": testProfileID, "operation_id": operationOne, "operation": "login",
				"sandbox": "shipmunk-profile-" + operationOne,
				"binding": cloneMap(control.binding),
			}
			test.mutate(pending)
			if err := store.Write("pending", pending); err != nil {
				t.Fatal(err)
			}
			before, err := store.Read("pending")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "probe", operationTwo); err == nil {
				t.Fatal("malformed recovery journal was accepted")
			}
			after, err := store.Read("pending")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("recovery mutated pending journal: before=%#v after=%#v err=%v", before, after, err)
			}
			if runtime.stops != 0 || len(control.snapshotCalls()) != 0 {
				t.Fatalf("recovery performed external effects: stops=%d calls=%#v", runtime.stops, control.snapshotCalls())
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("recovery mutated credential home: %v", err)
			}
		})
	}
}

func TestLifecycleRecoversValidInterruptedJournalWithoutOutcome(t *testing.T) {
	store, lifecycle, control, runtime, _ := newLifecycleFixture(t)
	pending := map[string]any{
		"profile_id": testProfileID, "operation_id": operationOne, "operation": "login",
		"sandbox": "shipmunk-profile-" + operationOne,
		"binding": cloneMap(control.binding),
	}
	if err := store.Write("pending", pending); err != nil {
		t.Fatal(err)
	}
	control.operationID = operationTwo
	control.binding["operation_id"] = operationTwo
	if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "disconnect", operationTwo); err != nil {
		t.Fatalf("valid interrupted journal without an outcome was rejected: %v", err)
	}
	var recovered *lifecycleCall
	for _, call := range control.snapshotCalls() {
		if call.suffix == "operations/"+operationOne+"/completion" {
			copyCall := call
			recovered = &copyCall
		}
	}
	if recovered == nil || recovered.payload["health"] != HealthError || recovered.payload["reason"] != "operation_stopped" {
		t.Fatalf("interrupted operation did not preserve default recovery outcome: %#v", recovered)
	}
	if runtime.starts != 0 {
		t.Fatalf("recovery replayed login for an interrupted journal: %d starts", runtime.starts)
	}
}

func TestLifecycleRejectsExecutionOverlapAndCompletedReplay(t *testing.T) {
	store, lifecycle, control, runtime, _ := newLifecycleFixture(t)
	if err := store.Write("execution", map[string]any{"profile_id": testProfileID}); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne); err == nil {
		t.Fatal("profile operation overlapped execution")
	}
	if len(control.snapshotCalls()) != 0 || runtime.starts != 0 {
		t.Fatal("execution overlap caused external side effects")
	}
	if err := store.Forget("execution"); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne); err != nil {
		t.Fatal(err)
	}
	count := len(control.snapshotCalls())
	if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne); err == nil {
		t.Fatal("completed operation ID was replayed")
	}
	if len(control.snapshotCalls()) != count {
		t.Fatal("replay made control-plane requests")
	}
}

func TestLifecycleRejectsChangedOrRevokedHeartbeatAndSanitizesDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "revoked", mutate: func(binding map[string]any) { binding["stop_requested"] = true }},
		{name: "credential rotated", mutate: func(binding map[string]any) { binding["credential_reference"] = "credential:other" }},
		{name: "lease expired", mutate: func(binding map[string]any) {
			binding["lease_expires_at"] = time.Now().Add(-time.Second).Format(time.RFC3339Nano)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, lifecycle, control, runtime, _ := newLifecycleFixture(t)
			stages := []string{}
			lifecycle.Diagnostic = func(stage string) { stages = append(stages, stage) }
			control.onHeartbeat = func() {
				test.mutate(control.binding)
			}
			runtime.runResults["login"] = CommandResult{ExitCode: 1, Stderr: "PRIVATE_NATIVE_OUTPUT"}
			outcome, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne)
			if err != nil || outcome != (Health{Health: HealthError, Reason: "operation_failed"}) {
				t.Fatalf("Operate() = (%#v, %v)", outcome, err)
			}
			if !reflect.DeepEqual(stages, []string{"heartbeat"}) {
				t.Fatalf("diagnostic stages = %#v", stages)
			}
			if runtime.stops != 1 || strings.Contains(fmt.Sprint(control.snapshotCalls()), "PRIVATE_NATIVE_OUTPUT") {
				t.Fatal("private provider output leaked or cleanup was skipped")
			}
		})
	}
}

func TestLifecycleCleanupFailureRetainsPendingAndNeverAcknowledgesStopped(t *testing.T) {
	store, lifecycle, control, runtime, _ := newLifecycleFixture(t)
	runtime.stopErr = errors.New("synthetic cleanup error")
	_, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne)
	if err == nil {
		t.Fatal("cleanup failure was not returned")
	}
	pending, readErr := store.Read("pending")
	if readErr != nil || pending == nil {
		t.Fatalf("cleanup failure did not retain pending: %#v, %v", pending, readErr)
	}
	for _, call := range control.snapshotCalls() {
		if strings.HasSuffix(call.suffix, "/completion") {
			t.Fatal("completion acknowledged a container whose absence was unconfirmed")
		}
	}
}

func TestLifecycleRecoversLostCompletionWithoutReplayingLogin(t *testing.T) {
	store, lifecycle, control, runtime, _ := newLifecycleFixture(t)
	control.completionErr = &protocol.ControlPlaneError{StatusCode: 500}
	if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne); err == nil {
		t.Fatal("lost completion response was not surfaced")
	}
	pending, err := store.Read("pending")
	if err != nil || pending == nil {
		t.Fatalf("pending recovery journal = %#v, %v", pending, err)
	}
	storedOutcome, ok := healthFromJournal(pending["outcome"])
	if !ok || storedOutcome.Health != HealthReady {
		t.Fatalf("stored outcome = %#v, %v", storedOutcome, ok)
	}
	control.completionErr = nil
	control.operationID = operationTwo
	control.binding["operation_id"] = operationTwo
	if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "probe", operationTwo); err != nil {
		t.Fatal(err)
	}
	loginCount := 0
	for _, command := range runtime.commands {
		if command == "login" {
			loginCount++
		}
	}
	if loginCount != 1 {
		t.Fatalf("recovery replayed native login %d times", loginCount)
	}
	calls := control.snapshotCalls()
	var oldCompletion int
	for index, call := range calls {
		if call.suffix == "operations/"+operationOne+"/completion" {
			oldCompletion = index
			if call.payload["health"] != HealthReady {
				t.Fatalf("recovered completion changed original outcome: %#v", call.payload)
			}
		}
	}
	if oldCompletion == 0 || oldCompletion >= len(calls)-1 {
		t.Fatalf("recovery did not complete old operation before new work: %#v", calls)
	}
}

func TestLifecycleRetainsReadyRecoveryUntilInactiveAcknowledgement(t *testing.T) {
	store, lifecycle, control, runtime, _ := newLifecycleFixture(t)
	control.completionErr = errors.New("lost completion")
	if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne); err == nil {
		t.Fatal("expected lost completion")
	}
	control.completionErr = nil
	if err := store.WithExclusive(func(locked *Store) error { return locked.Invalidate() }); err != nil {
		t.Fatal(err)
	}
	control.setCompletion(map[string]any{"profile_id": testProfileID, "active": true, "stopped": true, "health": HealthReady, "reason": nil})
	if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "probe", operationTwo); err == nil {
		t.Fatal("ready replay response incorrectly acknowledged a missing home")
	}
	pending, err := store.Read("pending")
	if err != nil || pending == nil || !isTrue(pending["recovery_failed"]) {
		t.Fatalf("failed recovery did not retain its quarantine journal: %#v, %v", pending, err)
	}
	if _, statErr := os.Stat(store.Home()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed ready home was not invalidated: %v", statErr)
	}
	control.setCompletion(map[string]any{"profile_id": testProfileID, "active": false, "stopped": true, "health": HealthError, "reason": "operation_stopped"})
	control.operationID = operationThree
	control.binding["operation_id"] = operationThree
	if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "disconnect", operationThree); err != nil {
		t.Fatalf("confirmed inactive recovery did not release the journal: %v", err)
	}
	if runtime.starts != 1 {
		t.Fatalf("disconnect unexpectedly started native runtime after recovery: %d", runtime.starts)
	}
}

func TestLifecycleDisconnectIsLocalAndServerMustAcceptInactiveState(t *testing.T) {
	store, lifecycle, control, runtime, _ := newLifecycleFixture(t)
	if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Home(), "credential"), []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	control.operationID = operationTwo
	control.binding["operation_id"] = operationTwo
	control.setCompletion(map[string]any{"profile_id": testProfileID, "active": false, "stopped": true, "health": "disconnected", "reason": "disconnected"})
	outcome, err := lifecycle.Operate(context.Background(), store, testProfileID, "disconnect", operationTwo)
	if err != nil || outcome.Health != "disconnected" || outcome.Reason != "disconnected" {
		t.Fatalf("disconnect = (%#v, %v)", outcome, err)
	}
	if _, statErr := os.Stat(store.Home()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("local home remains after disconnect: %v", statErr)
	}
	if got := len(runtime.commands); got != 5 {
		t.Fatalf("disconnect launched native commands; total = %d", got)
	}
}

func TestLifecycleRetainsPendingForInvalidCompletionResponses(t *testing.T) {
	responses := []map[string]any{
		{"profile_id": testProfileID, "operation_id": operationTwo, "active": true, "stopped": true, "health": HealthReady, "reason": nil},
		{"profile_id": testProfileID, "active": true, "health": HealthReady, "reason": nil},
		{"profile_id": testProfileID, "active": true, "stopped": false, "health": HealthReady, "reason": nil},
		{"profile_id": testProfileID, "active": "yes", "stopped": true, "health": HealthReady, "reason": nil},
		{"profile_id": testProfileID, "active": true, "stopped": true, "health": HealthError, "reason": "operation_failed"},
		{"profile_id": testProfileID, "active": false, "stopped": true, "health": "future_health", "reason": nil},
	}
	for index, response := range responses {
		t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
			store, lifecycle, control, _, _ := newLifecycleFixture(t)
			control.setCompletion(response)
			if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne); err == nil {
				t.Fatal("invalid completion response cleared recovery state")
			}
			pending, err := store.Read("pending")
			if err != nil || pending == nil {
				t.Fatalf("pending recovery state = %#v, %v", pending, err)
			}
		})
	}
}

func TestLifecycleRecoveryRetainsPendingForInvalidCompletionResponses(t *testing.T) {
	responses := []map[string]any{
		{"profile_id": testProfileID, "operation_id": operationTwo, "active": true, "stopped": true, "health": HealthReady, "reason": nil},
		{"profile_id": testProfileID, "active": true, "health": HealthReady, "reason": nil},
		{"profile_id": testProfileID, "active": "yes", "stopped": true, "health": HealthReady, "reason": nil},
		{"profile_id": testProfileID, "active": false, "stopped": true, "health": "future_health", "reason": nil},
	}
	for index, response := range responses {
		t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
			store, lifecycle, control, runtime, _ := newLifecycleFixture(t)
			control.completionErr = errors.New("lost completion")
			if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne); err == nil {
				t.Fatal("expected lost completion")
			}
			control.completionErr = nil
			control.setCompletion(response)
			control.operationID = operationTwo
			control.binding["operation_id"] = operationTwo
			if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "probe", operationTwo); err == nil {
				t.Fatal("invalid recovery completion response was accepted")
			}
			pending, err := store.Read("pending")
			if err != nil || pending == nil || pending["operation_id"] != operationOne {
				t.Fatalf("pending recovery state = %#v, %v", pending, err)
			}
			if runtime.starts != 1 {
				t.Fatalf("recovery replayed native work: %d starts", runtime.starts)
			}
		})
	}
}

func TestLifecycleRejectsHistoricalBeginWithoutDamagingNewerHome(t *testing.T) {
	store, lifecycle, control, _, _ := newLifecycleFixture(t)
	if _, err := lifecycle.Operate(context.Background(), store, testProfileID, "login", operationOne); err != nil {
		t.Fatal(err)
	}
	activeBefore, err := store.Read("active")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Home(), "newer-secret"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	control.beginErr = nil
	control.binding["stop_requested"] = false
	control.binding["stopped"] = true
	control.binding["runtime_version"] = ""
	control.operationID = operationTwo
	_, err = lifecycle.Operate(context.Background(), store, testProfileID, "login", operationTwo)
	if err == nil {
		t.Fatal("invalid historical binding unexpectedly proceeded")
	}
	activeAfter, readErr := store.Read("active")
	if readErr != nil || !reflect.DeepEqual(activeBefore, activeAfter) {
		t.Fatalf("historical begin changed newer active identity: %#v, %v", activeAfter, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(store.Home(), "newer-secret")); statErr != nil {
		t.Fatalf("historical begin removed newer credentials: %v", statErr)
	}
}

func TestLifecycleStopsAndCompletesUsingIndependentCleanupContext(t *testing.T) {
	store, lifecycle, control, runtime, _ := newLifecycleFixture(t)
	var order []string
	control.log = &order
	runtime.log = &order
	ctx, cancel := context.WithCancel(context.Background())
	runtime.runResults["login"] = CommandResult{ExitCode: 1}
	runtime.onRun = func(operation string) {
		if operation == "login" {
			cancel()
		}
	}
	_, err := lifecycle.Operate(ctx, store, testProfileID, "login", operationOne)
	if err != nil {
		t.Fatalf("canceled native login did not clean up and acknowledge: %v", err)
	}
	stopIndex, completionIndex := -1, -1
	for index, item := range order {
		if item == "runtime:stop" {
			stopIndex = index
		}
		if strings.HasSuffix(item, "/completion") {
			completionIndex = index
		}
	}
	if stopIndex < 0 || completionIndex <= stopIndex {
		t.Fatalf("stopped acknowledgement preceded cleanup: %#v", order)
	}
}

func cloneMap(value map[string]any) map[string]any {
	copyValue := make(map[string]any, len(value))
	for key, item := range value {
		copyValue[key] = item
	}
	return copyValue
}
