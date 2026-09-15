package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
	"github.com/ianrodrigues/shipmunk-runner/internal/workspace"
)

type memoryState struct {
	mu              sync.Mutex
	state           *attemptstate.State
	failSave        bool
	failSaveOnCall  int
	saveCalls       int
	commitOnFailure bool
	onSave          func()
}

func (m *memoryState) Load() (*attemptstate.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		return nil, nil
	}
	copy := *m.state
	return &copy, nil
}
func (m *memoryState) Save(s attemptstate.State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveCalls++
	if m.onSave != nil {
		m.onSave()
	}
	if m.failSave || m.failSaveOnCall > 0 && m.saveCalls == m.failSaveOnCall {
		if m.commitOnFailure {
			m.state = &s
		}
		return errors.New("disk failure")
	}
	m.state = &s
	return nil
}
func (m *memoryState) Clear() error { m.mu.Lock(); defer m.mu.Unlock(); m.state = nil; return nil }

type fixtureClient struct {
	mu                               sync.Mutex
	claim                            *protocol.Claim
	claims, beats, acks, completions int
	stopAfter                        int
	heartbeatError                   error
	heartbeatErrorAfter              int
	ackError, completeError          error
	completed                        map[string]any
	uploadID                         string
	eventBatches, uploads            int
	ackCancelled, ackHasDeadline     bool
}

func (c *fixtureClient) Claim(context.Context, time.Time) (*protocol.Claim, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims++
	return c.claim, nil
}
func (c *fixtureClient) Heartbeat(ctx context.Context, _ protocol.Claim) (time.Time, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.beats++
	if c.heartbeatError != nil && (c.heartbeatErrorAfter <= 0 || c.beats >= c.heartbeatErrorAfter) {
		return time.Time{}, false, c.heartbeatError
	}
	return time.Now().Add(LeaseDuration), c.stopAfter > 0 && c.beats >= c.stopAfter, ctx.Err()
}
func (c *fixtureClient) AcknowledgeStopped(ctx context.Context, _ protocol.Claim) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acks++
	_, c.ackHasDeadline = ctx.Deadline()
	c.ackCancelled = ctx.Err() != nil
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.ackError
}
func (c *fixtureClient) DownloadArtifact(context.Context, protocol.Claim, string, string) ([]byte, error) {
	return nil, errors.New("unexpected download")
}
func (c *fixtureClient) SendEvents(ctx context.Context, _ protocol.Claim, _ []byte) error {
	c.mu.Lock()
	c.eventBatches++
	c.mu.Unlock()
	return ctx.Err()
}
func (c *fixtureClient) UploadArtifact(ctx context.Context, _ protocol.Claim, _ string, _ []byte, _ string) (string, error) {
	c.mu.Lock()
	c.uploads++
	c.mu.Unlock()
	return c.uploadID, ctx.Err()
}
func (c *fixtureClient) Complete(ctx context.Context, _ protocol.Claim, raw []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c.completions++
	_ = json.Unmarshal(raw, &c.completed)
	return c.completeError
}

type fixtureWorkspaces struct {
	root        string
	prepare     func(context.Context) error
	removes     int
	removeError error
}

func (w *fixtureWorkspaces) EnsureWritable() error { return nil }
func (w *fixtureWorkspaces) Path(c protocol.Claim) string {
	return filepath.Join(w.root, c.AttemptID+"-1")
}
func (w *fixtureWorkspaces) Prepare(ctx context.Context, c protocol.Claim, _ workspace.Downloader) (string, error) {
	if w.prepare != nil {
		if err := w.prepare(ctx); err != nil {
			return "", err
		}
	}
	return w.Path(c), ctx.Err()
}
func (w *fixtureWorkspaces) Remove(string) error { w.removes++; return w.removeError }

type fixtureSandbox struct {
	state                       *memoryState
	process                     *fixtureProcess
	watchdog                    *fixtureWatchdog
	creates, reconciles         int
	reconciledIDs               []string
	createError, reconcileError error
	createUncertainError        error
	armed                       *bool
}

func (b *fixtureSandbox) Name(c protocol.Claim) (string, error) {
	return "shipmunk-" + c.AttemptID + "-1", nil
}
func (b *fixtureSandbox) Create(ctx context.Context, c protocol.Claim, input map[string]any, _ string) (Process, error) {
	b.creates++
	state, _ := b.state.Load()
	name, _ := b.Name(c)
	if state == nil || state.SandboxID == nil || *state.SandboxID != name || !*b.armed || b.watchdog == nil || !b.watchdog.createStarted {
		return nil, errors.New("created before journal or watchdog")
	}
	if _, ok := input["supervisor"]; ok {
		return nil, errors.New("supervisor metadata escaped")
	}
	if b.createError != nil {
		return nil, b.createError
	}
	if b.createUncertainError != nil {
		return nil, b.createUncertainError
	}
	return b.process, ctx.Err()
}
func (b *fixtureSandbox) Reconcile(ctx context.Context, identifier string) error {
	b.reconciles++
	b.reconciledIDs = append(b.reconciledIDs, identifier)
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.reconcileError
}

type fixtureProcess struct {
	output []byte
	exit   int
	wait   func(context.Context) error
	start  func(context.Context) error
	starts int
}

type fixtureExecutor struct {
	execution       Execution
	executeError    error
	cleanupError    error
	executes        int
	cleanups        int
	input           map[string]any
	cleanupCanceled bool
	cleanupDeadline bool
	cleanupClaim    protocol.Claim
}

func (e *fixtureExecutor) Execute(ctx context.Context, _ protocol.Claim, input map[string]any, _ string) (Execution, error) {
	e.executes++
	e.input = input
	if err := ctx.Err(); err != nil {
		return Execution{}, err
	}
	return e.execution, e.executeError
}

func (e *fixtureExecutor) Cleanup(ctx context.Context, claim protocol.Claim) error {
	e.cleanups++
	e.cleanupCanceled = ctx.Err() != nil
	_, e.cleanupDeadline = ctx.Deadline()
	e.cleanupClaim = claim
	return e.cleanupError
}

func (p *fixtureProcess) ContainerID() string {
	return "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}
func (p *fixtureProcess) Start(ctx context.Context) error {
	p.starts++
	if p.start != nil {
		if err := p.start(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (p *fixtureProcess) Wait(ctx context.Context) (int, []byte, error) {
	if p.wait != nil {
		if err := p.wait(ctx); err != nil {
			return 0, nil, err
		}
	}
	return p.exit, p.output, ctx.Err()
}

type fixtureWatchdog struct {
	armed               bool
	createStarted       bool
	createStartedCalls  int
	createFinishedCalls int
	createFinishedIDs   []string
	disarms             int
	armError            error
	createStartedError  error
	createFinishedError error
	onCreateStarted     func()
	onArm               func()
}

func (w *fixtureWatchdog) Arm(string, time.Time, time.Time) (WatchdogLease, error) {
	if w.armError != nil {
		return nil, w.armError
	}
	w.armed = true
	if w.onArm != nil {
		w.onArm()
	}
	return w, nil
}
func (w *fixtureWatchdog) Renew(time.Time) error { return nil }
func (w *fixtureWatchdog) CreateStarted() error {
	w.createStartedCalls++
	if w.createStartedError != nil {
		return w.createStartedError
	}
	w.createStarted = true
	if w.onCreateStarted != nil {
		w.onCreateStarted()
	}
	return nil
}
func (w *fixtureWatchdog) CreateFinished(containerID string) error {
	w.createFinishedCalls++
	w.createFinishedIDs = append(w.createFinishedIDs, containerID)
	if w.createFinishedError != nil {
		return w.createFinishedError
	}
	w.createStarted = false
	return nil
}
func (w *fixtureWatchdog) Disarm() error { w.disarms++; return nil }

func fixtureClaim(t *testing.T) protocol.Claim {
	t.Helper()
	raw, err := os.ReadFile("../protocol/testdata/contracts/v1/fixtures/valid/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := protocol.Decode(raw, protocol.ManifestMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	manifest := value.(map[string]any)
	manifest["deadline"] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	claim, err := protocol.ClaimFromManifest(manifest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return claim
}
func normalizedOutput(t *testing.T, c protocol.Claim, outcome string) []byte {
	t.Helper()
	result := map[string]any{"protocol_version": protocol.Version, "run_id": c.RunID, "attempt_id": c.AttemptID, "fence": c.Fence, "summary": "Synthetic completion.", "outcome": outcome, "findings": []any{}, "patch_artifact": nil, "tests": []any{}, "usage": nil}
	if outcome == "findings" || outcome == "no_findings" {
		result["charter_version"] = "1"
		result["coverage"] = map[string]any{"files": []any{}, "context_gaps": []any{}}
		result["verification_state"] = "none"
	}
	raw, err := json.Marshal(map[string]any{"events": []any{}, "artifacts": []any{}, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func normalizedOutputWithEvents(t *testing.T, c protocol.Claim, outcome string, sequences ...int64) []byte {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(normalizedOutput(t, c, outcome), &envelope); err != nil {
		t.Fatal(err)
	}
	events := make([]any, 0, len(sequences))
	for _, sequence := range sequences {
		events = append(events, map[string]any{
			"protocol_version": protocol.Version,
			"attempt_id":       c.AttemptID,
			"fence":            c.Fence,
			"sequence":         sequence,
			"type":             "progress",
			"timestamp":        "2026-09-10T17:00:00Z",
			"payload":          map[string]any{"message": "Synthetic progress."},
		})
	}
	envelope["events"] = events
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func fixtureSupervisor(t *testing.T) (*Supervisor, *fixtureClient, *memoryState, *fixtureSandbox, *fixtureWorkspaces, *fixtureWatchdog) {
	t.Helper()
	claim := fixtureClaim(t)
	client := &fixtureClient{claim: &claim}
	state := &memoryState{}
	watchdog := &fixtureWatchdog{}
	process := &fixtureProcess{output: normalizedOutput(t, claim, "no_findings")}
	sandbox := &fixtureSandbox{state: state, armed: &watchdog.armed, watchdog: watchdog, process: process}
	process.start = func(context.Context) error {
		saved, _ := state.Load()
		if saved == nil || saved.SandboxID == nil || *saved.SandboxID != process.ContainerID() {
			return errors.New("container ID was not journaled before start")
		}
		if watchdog.createFinishedCalls != 1 || watchdog.createStarted || len(watchdog.createFinishedIDs) != 1 || watchdog.createFinishedIDs[0] != process.ContainerID() {
			return errors.New("watchdog creation phase was not finished before start")
		}
		return nil
	}
	workspaces := &fixtureWorkspaces{root: t.TempDir()}
	s := &Supervisor{Client: client, State: state, Workspaces: workspaces, Sandbox: sandbox, Watchdog: watchdog, heartbeatInterval: 10 * time.Millisecond}
	return s, client, state, sandbox, workspaces, watchdog
}

func TestRunOnceJournalsBeforeCreationAndReportsOnlyAfterCleanup(t *testing.T) {
	s, c, state, b, w, d := fixtureSupervisor(t)
	out, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !out.Worked || out.Result != "no_findings" || b.creates != 1 || b.reconciles != 1 || len(b.reconciledIDs) != 1 || b.reconciledIDs[0] != b.process.ContainerID() || w.removes != 1 || d.disarms != 1 || d.createStartedCalls != 1 || d.createFinishedCalls != 1 || len(d.createFinishedIDs) != 1 || d.createFinishedIDs[0] != b.process.ContainerID() || c.acks != 1 || c.completions != 1 {
		t.Fatalf("incorrect lifecycle: %+v", out)
	}
	if saved, _ := state.Load(); saved != nil {
		t.Fatal("journal retained after confirmed cleanup")
	}
}

func TestCompositeExecutorPublishesAndCleansBeforeAcknowledgement(t *testing.T) {
	s, c, state, _, w, _ := fixtureSupervisor(t)
	execution, err := DecodeExecution(*c.claim, 0, normalizedOutput(t, *c.claim, "no_findings"))
	if err != nil {
		t.Fatal(err)
	}
	executor := &fixtureExecutor{execution: execution}
	s.Executor = executor
	s.Sandbox = nil
	s.Watchdog = nil

	out, err := s.RunOnce(context.Background())
	if err != nil || !out.Worked || out.Result != "no_findings" {
		t.Fatalf("composite execution failed: %+v %v", out, err)
	}
	if executor.executes != 1 || executor.cleanups != 1 || executor.cleanupCanceled || !executor.cleanupDeadline || c.completions != 1 || c.acks != 1 || w.removes != 1 {
		t.Fatalf("incorrect composite lifecycle: executor=%+v completions=%d acks=%d removes=%d", executor, c.completions, c.acks, w.removes)
	}
	if _, ok := executor.input["supervisor"]; ok {
		t.Fatal("supervisor metadata escaped to composite executor")
	}
	if saved, _ := state.Load(); saved != nil {
		t.Fatal("journal retained after composite cleanup")
	}
}

func TestCompositeExecutorFailureStillCleansWithRevokedContext(t *testing.T) {
	s, c, state, _, _, _ := fixtureSupervisor(t)
	executor := &fixtureExecutor{executeError: errors.New("synthetic native failure")}
	s.Executor = executor
	s.Sandbox = nil
	s.Watchdog = nil

	_, err := s.RunOnce(context.Background())
	if err == nil || executor.executes != 1 || executor.cleanups != 1 || executor.cleanupCanceled || !executor.cleanupDeadline || c.completions != 0 || c.acks != 1 {
		t.Fatalf("composite failure cleanup failed: executor=%+v completions=%d acks=%d err=%v", executor, c.completions, c.acks, err)
	}
	if saved, _ := state.Load(); saved != nil {
		t.Fatal("journal retained after confirmed composite cleanup")
	}
}

func TestCompositeCleanupFailureRetainsJournalAndPreventsAcknowledgement(t *testing.T) {
	s, c, state, _, _, _ := fixtureSupervisor(t)
	execution, err := DecodeExecution(*c.claim, 0, normalizedOutput(t, *c.claim, "no_findings"))
	if err != nil {
		t.Fatal(err)
	}
	executor := &fixtureExecutor{execution: execution, cleanupError: errors.New("synthetic cleanup uncertainty")}
	s.Executor = executor
	s.Sandbox = nil
	s.Watchdog = nil

	out, err := s.RunOnce(context.Background())
	if !errors.Is(err, ErrCleanupUnconfirmed) || out.Worked || executor.cleanups != 1 || c.acks != 0 {
		t.Fatalf("unconfirmed composite cleanup was released: %+v executor=%+v acks=%d err=%v", out, executor, c.acks, err)
	}
	if saved, _ := state.Load(); saved == nil {
		t.Fatal("journal lost after unconfirmed composite cleanup")
	}
}

func TestCompositeRecoveryCarriesDurableProfileBinding(t *testing.T) {
	s, c, state, _, w, _ := fixtureSupervisor(t)
	claim := *c.claim
	profileID := claim.Manifest["profile_id"].(string)
	state.state = &attemptstate.State{
		RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence,
		ProfileID: &profileID, LeaseExpiresAt: claim.LeaseExpiresAt,
		Deadline: claim.Deadline, Workspace: w.Path(claim),
	}
	executor := &fixtureExecutor{}
	s.Executor = executor
	s.Sandbox = nil
	s.Watchdog = nil
	c.claim = nil

	out, err := s.RunOnce(context.Background())
	if err != nil || out.Worked || executor.cleanups != 1 || executor.cleanupClaim.Manifest["profile_id"] != profileID || c.acks != 1 {
		t.Fatalf("composite recovery lost profile binding: %+v executor=%+v acks=%d err=%v", out, executor, c.acks, err)
	}
}

func TestCompositeRecoveryRefusesLegacySandboxWithoutReconciler(t *testing.T) {
	s, c, state, _, w, _ := fixtureSupervisor(t)
	claim := *c.claim
	legacyID := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	state.state = &attemptstate.State{
		RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence,
		LeaseExpiresAt: claim.LeaseExpiresAt, Deadline: claim.Deadline,
		Workspace: w.Path(claim), SandboxID: &legacyID,
	}
	executor := &fixtureExecutor{}
	s.Executor = executor
	s.Sandbox = nil
	s.Watchdog = nil
	c.claim = nil

	out, err := s.RunOnce(context.Background())
	if !errors.Is(err, ErrCleanupUnconfirmed) || out.Worked || executor.cleanups != 0 || c.acks != 0 || w.removes != 0 {
		t.Fatalf("legacy sandbox was released without a reconciler: %+v cleanups=%d acks=%d removes=%d err=%v", out, executor.cleanups, c.acks, w.removes, err)
	}
	if saved, _ := state.Load(); saved == nil || saved.SandboxID == nil || *saved.SandboxID != legacyID {
		t.Fatal("legacy sandbox journal was not retained")
	}
}

func TestCleanupFailureRetainsJournalAndBlocksReplacement(t *testing.T) {
	for _, mode := range []string{"container", "workspace", "acknowledgement"} {
		t.Run(mode, func(t *testing.T) {
			s, c, state, b, w, _ := fixtureSupervisor(t)
			failure := errors.New("synthetic uncertainty")
			switch mode {
			case "container":
				b.reconcileError = failure
			case "workspace":
				w.removeError = failure
			case "acknowledgement":
				c.ackError = failure
			}
			out, err := s.RunOnce(context.Background())
			if !errors.Is(err, ErrCleanupUnconfirmed) || out.Worked {
				t.Fatalf("reported unconfirmed result: %+v %v", out, err)
			}
			if saved, _ := state.Load(); saved == nil {
				t.Fatal("journal lost")
			}
			_, _ = s.RunOnce(context.Background())
			if c.claims != 1 {
				t.Fatal("claimed replacement with unresolved cleanup")
			}
			if mode != "acknowledgement" && c.acks != 0 {
				t.Fatal("acknowledged before cleanup")
			}
		})
	}
}

func TestLeaseRenewsDuringPreparationAndCancelsOnStop(t *testing.T) {
	s, c, state, b, w, _ := fixtureSupervisor(t)
	c.stopAfter = 3
	w.prepare = func(ctx context.Context) error {
		saved, _ := state.Load()
		if saved == nil {
			return errors.New("preparation before journal")
		}
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := s.RunOnce(ctx)
	if err == nil || c.beats != 3 || b.creates != 0 || c.acks != 1 {
		t.Fatalf("renewal/stop failed: beats=%d creates=%d acks=%d err=%v", c.beats, b.creates, c.acks, err)
	}
}

func TestLeaseHeartbeatFailureCancelsPreparationAndCleans(t *testing.T) {
	s, c, state, b, w, _ := fixtureSupervisor(t)
	c.heartbeatError = errors.New("heartbeat unavailable")
	c.heartbeatErrorAfter = 2
	w.prepare = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	_, err := s.RunOnce(context.Background())
	if err == nil || c.beats != 2 || b.creates != 0 || c.acks != 1 {
		t.Fatalf("heartbeat failure did not cancel and clean the attempt: beats=%d creates=%d acks=%d err=%v", c.beats, b.creates, c.acks, err)
	}
	if saved, _ := state.Load(); saved != nil {
		t.Fatal("journal retained after confirmed cleanup")
	}
}

func TestDefinitiveCreationFailureClearsReservationBeforeAcknowledgement(t *testing.T) {
	s, c, _, b, _, _ := fixtureSupervisor(t)
	b.createError = errors.New("invalid agent input")
	_, err := s.RunOnce(context.Background())
	if err == nil || b.reconciles != 0 || c.acks != 1 {
		t.Fatalf("definitive create failure cleanup failed: %v", err)
	}
	if saved, _ := s.State.Load(); saved != nil {
		t.Fatal("journal retained after definitive pre-side-effect failure")
	}
	if b.creates != 1 || c.completions != 0 {
		t.Fatal("definitive create failure executed or completed work")
	}
}

func TestUncertainCreationRetainsReservationWithoutAcknowledgement(t *testing.T) {
	s, c, state, b, w, watchdog := fixtureSupervisor(t)
	b.createUncertainError = fmt.Errorf("lost Docker reply: %w", sandbox.ErrCreateUncertain)
	_, err := s.RunOnce(context.Background())
	if !errors.Is(err, ErrCleanupUnconfirmed) {
		t.Fatalf("uncertain creation was not retained: %v", err)
	}
	saved, loadErr := state.Load()
	if loadErr != nil || saved == nil || saved.SandboxID == nil {
		t.Fatalf("uncertain creation reservation was lost: %#v %v", saved, loadErr)
	}
	name, _ := b.Name(*c.claim)
	if *saved.SandboxID != name || c.acks != 0 || b.reconciles != 0 || w.removes != 0 || watchdog.disarms != 0 || watchdog.createFinishedCalls != 0 {
		t.Fatalf("unknown create was acknowledged or disarmed: state=%#v acks=%d reconciles=%d removes=%d disarms=%d finished=%d", saved, c.acks, b.reconciles, w.removes, watchdog.disarms, watchdog.createFinishedCalls)
	}
	// A fresh process has no way to reconnect the old pipe watchdog.
	// Reconcile cannot distinguish delayed creation from confirmed absence, so it leaves the reservation for manual recovery.
	freshClient := &fixtureClient{}
	freshWatchdog := &fixtureWatchdog{}
	freshSandbox := &fixtureSandbox{state: state, watchdog: freshWatchdog, armed: &freshWatchdog.armed}
	fresh := &Supervisor{Client: freshClient, State: state, Workspaces: w, Sandbox: freshSandbox, Watchdog: freshWatchdog}
	_, err = fresh.RunOnce(context.Background())
	if !errors.Is(err, sandbox.ErrCreateUncertain) || freshClient.acks != 0 || freshClient.claims != 0 || freshSandbox.reconciles != 0 || w.removes != 0 {
		t.Fatalf("fresh recovery operated on an unresolved name: err=%v acks=%d claims=%d reconciles=%d removes=%d", err, freshClient.acks, freshClient.claims, freshSandbox.reconciles, w.removes)
	}
	if watchdog.disarms != 0 || watchdog.createFinishedCalls != 0 {
		t.Fatalf("old watchdog was unexpectedly changed: disarms=%d finished=%d", watchdog.disarms, watchdog.createFinishedCalls)
	}
}

func TestContainerIDPersistenceFailureRecoversByRecordedID(t *testing.T) {
	s, c, state, b, w, watchdog := fixtureSupervisor(t)
	state.failSaveOnCall = 4
	state.commitOnFailure = true
	_, err := s.RunOnce(context.Background())
	if !errors.Is(err, ErrCleanupUnconfirmed) {
		t.Fatalf("container ID persistence failure was not retained: %v", err)
	}
	saved, loadErr := state.Load()
	if loadErr != nil || saved == nil || saved.SandboxID == nil || *saved.SandboxID != b.process.ContainerID() {
		t.Fatalf("ambiguous ID save did not retain the actual container ID: %#v %v", saved, loadErr)
	}
	if b.process.starts != 0 || c.acks != 0 || watchdog.createFinishedCalls != 1 || watchdog.createFinishedIDs[0] != b.process.ContainerID() || watchdog.disarms != 0 {
		t.Fatalf("execution continued before durable ID confirmation: starts=%d acks=%d finished=%d disarms=%d", b.process.starts, c.acks, watchdog.createFinishedCalls, watchdog.disarms)
	}
	// Restart with a new supervisor and no access to the original watchdog. The
	// ambiguous save committed the immutable ID, so reconciliation is safe.
	freshClient := &fixtureClient{}
	freshWatchdog := &fixtureWatchdog{}
	freshSandbox := &fixtureSandbox{state: state, watchdog: freshWatchdog, armed: &freshWatchdog.armed}
	fresh := &Supervisor{Client: freshClient, State: state, Workspaces: w, Sandbox: freshSandbox, Watchdog: freshWatchdog}
	if out, err := fresh.RunOnce(context.Background()); err != nil || out.Worked {
		t.Fatalf("recovery through durable container ID failed: %+v %v", out, err)
	}
	if len(freshSandbox.reconciledIDs) != 1 || freshSandbox.reconciledIDs[0] != b.process.ContainerID() || freshWatchdog.createFinishedCalls != 0 || freshWatchdog.disarms != 0 || freshClient.acks != 1 {
		t.Fatalf("fresh recovery did not reconcile ID and acknowledge: ids=%v finished=%d disarms=%d acks=%d", freshSandbox.reconciledIDs, freshWatchdog.createFinishedCalls, freshWatchdog.disarms, freshClient.acks)
	}
}

func TestFreshRecoveryBlocksWhenIDPromotionWasNotDurable(t *testing.T) {
	s, _, state, b, w, watchdog := fixtureSupervisor(t)
	state.failSaveOnCall = 4 // fail the ID promotion after the watchdog ACK
	_, err := s.RunOnce(context.Background())
	if !errors.Is(err, ErrCleanupUnconfirmed) {
		t.Fatalf("ID promotion failure was not retained: %v", err)
	}
	saved, loadErr := state.Load()
	name, _ := b.Name(fixtureClaim(t))
	if loadErr != nil || saved == nil || saved.SandboxID == nil || *saved.SandboxID != name {
		t.Fatalf("failed ID promotion did not retain the name reservation: %#v %v", saved, loadErr)
	}
	if watchdog.createFinishedCalls != 1 || watchdog.createFinishedIDs[0] != b.process.ContainerID() || b.process.starts != 0 {
		t.Fatalf("expected confirmed watchdog ID before failed promotion: finished=%v starts=%d", watchdog.createFinishedIDs, b.process.starts)
	}

	freshClient := &fixtureClient{}
	freshWatchdog := &fixtureWatchdog{}
	freshSandbox := &fixtureSandbox{state: state, watchdog: freshWatchdog, armed: &freshWatchdog.armed}
	fresh := &Supervisor{Client: freshClient, State: state, Workspaces: w, Sandbox: freshSandbox, Watchdog: freshWatchdog}
	_, err = fresh.RunOnce(context.Background())
	if !errors.Is(err, sandbox.ErrCreateUncertain) || freshSandbox.reconciles != 0 || freshClient.acks != 0 || freshClient.claims != 0 || w.removes != 0 {
		t.Fatalf("fresh recovery was not conservative for name-only journal: err=%v reconciles=%d acks=%d claims=%d removes=%d", err, freshSandbox.reconciles, freshClient.acks, freshClient.claims, w.removes)
	}
}

func TestRecoveryUnknownNameAbsenceDoesNotAcknowledge(t *testing.T) {
	s, c, state, b, w, _ := fixtureSupervisor(t)
	claim := *c.claim
	name, _ := b.Name(claim)
	state.state = &attemptstate.State{
		RunID:          claim.RunID,
		AttemptID:      claim.AttemptID,
		Fence:          claim.Fence,
		SandboxID:      &name,
		LeaseExpiresAt: claim.LeaseExpiresAt,
		Deadline:       claim.Deadline,
		Workspace:      w.Path(claim),
	}
	_, err := s.RunOnce(context.Background())
	if !errors.Is(err, sandbox.ErrCreateUncertain) || c.acks != 0 || c.claims != 0 || b.reconciles != 0 || w.removes != 0 {
		t.Fatalf("unresolved reserved name was acknowledged or reconciled: err=%v acks=%d claims=%d reconciles=%d removes=%d", err, c.acks, c.claims, b.reconciles, w.removes)
	}
}

func TestWatchdogCreateStartedFailureFinishesReservationBeforeCleanup(t *testing.T) {
	s, c, state, b, _, watchdog := fixtureSupervisor(t)
	watchdog.createStartedError = errors.New("watchdog marker failed")
	_, err := s.RunOnce(context.Background())
	if err == nil || b.creates != 0 || watchdog.createStartedCalls != 1 || watchdog.createFinishedCalls != 1 || len(watchdog.createFinishedIDs) != 1 || watchdog.createFinishedIDs[0] != "" || watchdog.disarms != 1 || c.acks != 1 {
		t.Fatalf("pre-create watchdog failure was not cleaned: err=%v creates=%d started=%d finished=%d disarms=%d acks=%d", err, b.creates, watchdog.createStartedCalls, watchdog.createFinishedCalls, watchdog.disarms, c.acks)
	}
	if saved, _ := state.Load(); saved != nil {
		t.Fatal("journal retained after confirmed no-create cleanup")
	}
	if state.saveCalls < 4 {
		t.Fatalf("reservation was not durably cleared before completion: saves=%d", state.saveCalls)
	}
}

func TestLeaseRevocationAfterCreateStartedAckPreventsCreate(t *testing.T) {
	s, c, state, b, _, watchdog := fixtureSupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())
	watchdog.onCreateStarted = cancel
	_, err := s.RunOnce(ctx)
	cancel()
	if err == nil || b.creates != 0 || watchdog.createStartedCalls != 1 || watchdog.createFinishedCalls != 1 || watchdog.createFinishedIDs[0] != "" || watchdog.disarms != 1 || c.acks != 1 {
		t.Fatalf("revoked lease crossed Docker create boundary: err=%v creates=%d started=%d finished=%v disarms=%d acks=%d", err, b.creates, watchdog.createStartedCalls, watchdog.createFinishedIDs, watchdog.disarms, c.acks)
	}
	if saved, _ := state.Load(); saved != nil {
		t.Fatal("journal retained after confirmed no-create cleanup")
	}
}

func TestLeaseRevocationBeforeCreateStartedDoesNotSendFinish(t *testing.T) {
	s, c, state, b, _, watchdog := fixtureSupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())
	watchdog.onArm = cancel
	_, err := s.RunOnce(ctx)
	cancel()
	if err == nil || b.creates != 0 || watchdog.createStartedCalls != 0 || watchdog.createFinishedCalls != 0 || watchdog.disarms != 1 || c.acks != 1 {
		t.Fatalf("revoked lease before create phase was not cleaned: err=%v creates=%d started=%d finished=%d disarms=%d acks=%d", err, b.creates, watchdog.createStartedCalls, watchdog.createFinishedCalls, watchdog.disarms, c.acks)
	}
	if saved, _ := state.Load(); saved != nil {
		t.Fatal("journal retained after confirmed no-create cleanup")
	}
}

func TestRevokedContextCannotPreventCleanup(t *testing.T) {
	s, c, _, b, _, _ := fixtureSupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())
	b.process.wait = func(context.Context) error { cancel(); return context.Canceled }
	_, err := s.RunOnce(ctx)
	if err == nil || b.reconciles != 1 || c.acks != 1 || c.completions != 0 {
		t.Fatalf("cleanup failed after cancellation: %v", err)
	}
}

func TestClaimPersistenceFailurePoisonsSupervisor(t *testing.T) {
	s, c, state, b, _, _ := fixtureSupervisor(t)
	state.failSave = true
	_, err := s.RunOnce(context.Background())
	if err == nil {
		t.Fatal("accepted failed journal")
	}
	state.failSave = false
	_, _ = s.RunOnce(context.Background())
	if c.claims != 1 || c.acks != 1 || b.creates != 0 {
		t.Fatal("replacement claim after persistence failure")
	}
}

func TestClaimPersistenceFailureAcknowledgesWithIndependentBoundedContext(t *testing.T) {
	s, c, state, b, w, _ := fixtureSupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())
	state.failSave = true
	state.onSave = cancel
	_, err := s.RunOnce(ctx)
	defer cancel()
	if err == nil {
		t.Fatal("accepted failed journal")
	}
	if c.acks != 1 || c.ackCancelled || !c.ackHasDeadline || b.creates != 0 || w.removes != 0 {
		t.Fatalf("failed claim was not independently acknowledged before side effects: acks=%d cancelled=%t deadline=%t creates=%d removes=%d", c.acks, c.ackCancelled, c.ackHasDeadline, b.creates, w.removes)
	}
}

func TestAmbiguousClaimPersistenceRetainsJournalWhenAcknowledgementFails(t *testing.T) {
	s, c, state, b, _, _ := fixtureSupervisor(t)
	state.failSave = true
	state.commitOnFailure = true
	ackFailure := errors.New("acknowledgement uncertain")
	c.ackError = ackFailure
	_, err := s.RunOnce(context.Background())
	if !errors.Is(err, ackFailure) {
		t.Fatalf("acknowledgement failure was lost: %v", err)
	}
	if saved, _ := state.Load(); saved == nil {
		t.Fatal("ambiguous durable claim was cleared")
	}
	_, _ = s.RunOnce(context.Background())
	if c.claims != 1 || b.creates != 0 {
		t.Fatal("supervisor claimed after ambiguous persistence and acknowledgement")
	}
}

func TestEventSequenceGapIsRejectedBeforeAnyPublication(t *testing.T) {
	s, c, _, b, _, _ := fixtureSupervisor(t)
	b.process.output = normalizedOutputWithEvents(t, *c.claim, "no_findings", 1, 3)
	_, err := s.RunOnce(context.Background())
	if err == nil {
		t.Fatal("accepted a discontinuous event sequence")
	}
	if c.eventBatches != 0 || c.uploads != 0 || c.completions != 0 {
		t.Fatalf("published output before validating all events: batches=%d uploads=%d completions=%d", c.eventBatches, c.uploads, c.completions)
	}
}

func TestIdleDoesNotCreateAttempt(t *testing.T) {
	s, c, _, b, _, _ := fixtureSupervisor(t)
	c.claim = nil
	out, err := s.RunOnce(context.Background())
	if err != nil || out.Worked || b.creates != 0 {
		t.Fatalf("idle failure %+v %v", out, err)
	}
}

func TestWatchdogArmFailurePreventsCreation(t *testing.T) {
	s, _, _, b, _, d := fixtureSupervisor(t)
	d.armError = errors.New("watchdog unavailable")
	_, err := s.RunOnce(context.Background())
	if err == nil || b.creates != 0 {
		t.Fatal("created without independent watchdog")
	}
}

func TestCompletionRejectionIsNotReportedAsSuccess(t *testing.T) {
	s, c, _, b, _, _ := fixtureSupervisor(t)
	c.completeError = errors.New("stale fence")
	out, err := s.RunOnce(context.Background())
	if err == nil || out.Worked || c.acks != 1 || b.reconciles != 1 {
		t.Fatalf("incorrect rejected completion: %+v %v", out, err)
	}
}

func TestRecoveryRejectsWorkspaceOutsideExpectedAttempt(t *testing.T) {
	s, c, state, b, _, _ := fixtureSupervisor(t)
	claim := *c.claim
	state.state = &attemptstate.State{RunID: claim.RunID, AttemptID: claim.AttemptID, Fence: claim.Fence, Workspace: "/unrelated", LeaseExpiresAt: claim.LeaseExpiresAt, Deadline: claim.Deadline}
	_, err := s.RunOnce(context.Background())
	if err == nil || c.claims != 0 || b.reconciles != 0 {
		t.Fatal("unsafe recovery permitted")
	}
}
