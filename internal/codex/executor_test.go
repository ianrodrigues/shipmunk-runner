package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/codexsession"
	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

type executorTransport struct {
	calls    [][]string
	stopped  bool
	startErr error
}

func (t *executorTransport) Start(context.Context) error                  { return t.startErr }
func (t *executorTransport) Stop(context.Context) error                   { t.stopped = true; return nil }
func (t *executorTransport) CollectPatch(context.Context) (*Patch, error) { return nil, nil }
func (t *executorTransport) RunNative(_ context.Context, argv []string, _ []byte) (CommandResult, error) {
	t.calls = append(t.calls, argv)
	joined := strings.Join(argv, " ")
	switch {
	case strings.Contains(joined, "--version"):
		return CommandResult{Stdout: "codex-cli " + profile.CodexVersion + "\n"}, nil
	case strings.Contains(joined, "login status"):
		return CommandResult{Stdout: "Logged in using ChatGPT\n"}, nil
	case strings.Contains(joined, "--ephemeral"):
		return CommandResult{Stdout: "{\"type\":\"thread.started\",\"thread_id\":\"0199a213-81c0-7800-8aa1-bbab2a035a53\"}\n{\"type\":\"turn.started\"}\n{\"type\":\"item.completed\",\"item\":{\"id\":\"auth\",\"type\":\"agent_message\",\"text\":\"SHIPMUNK_AUTH_OK\"}}\n{\"type\":\"turn.completed\"}\n"}, nil
	default:
		return CommandResult{Stdout: "{\"type\":\"thread.started\",\"thread_id\":\"0199a213-81c0-7800-8aa1-bbab2a035a53\"}\n{\"type\":\"turn.started\"}\n{\"type\":\"item.completed\",\"item\":{\"id\":\"result\",\"type\":\"agent_message\",\"text\":\"{\\\"summary\\\":\\\"Review complete.\\\",\\\"outcome\\\":\\\"no_findings\\\",\\\"findings\\\":[],\\\"tests\\\":[]}\"}}\n{\"type\":\"turn.completed\"}\n"}, nil
	}
}

type executorWatchdogLease struct {
	started, finished, disarmed int
	finishedID                  string
	disarmErr                   error
}

func (l *executorWatchdogLease) Renew(time.Time) error { return nil }
func (l *executorWatchdogLease) CreateStarted() error  { l.started++; return nil }
func (l *executorWatchdogLease) CreateFinished(id string) error {
	l.finished++
	l.finishedID = id
	return nil
}
func (l *executorWatchdogLease) Disarm() error { l.disarmed++; return l.disarmErr }

func TestExecutorFinishesWatchdogAfterDefinitiveSetupFailure(t *testing.T) {
	executor, transport, lease, claim := setupFailureExecutor(t, errors.New("definitive setup failure"))
	if _, err := executor.Execute(context.Background(), claim, nil, setupFailureWorkspace(t)); err == nil {
		t.Fatal("definitive setup failure was lost")
	}
	if lease.started != 1 || lease.finished != 1 || lease.finishedID != "" {
		t.Fatalf("watchdog phase = started %d finished %d id %q", lease.started, lease.finished, lease.finishedID)
	}
	if err := executor.Cleanup(context.Background(), claim); err != nil || !transport.stopped || lease.disarmed != 1 {
		t.Fatalf("definitive failure cleanup = stopped %t disarmed %d err %v", transport.stopped, lease.disarmed, err)
	}
}

func TestExecutorPreservesWatchdogUncertaintyWhenSetupCleanupUnconfirmed(t *testing.T) {
	uncertain := errors.Join(errors.New("setup failed"), ErrTransportCleanupUnconfirmed)
	executor, transport, lease, claim := setupFailureExecutor(t, uncertain)
	lease.disarmErr = errors.New("create phase remains uncertain")
	if _, err := executor.Execute(context.Background(), claim, nil, setupFailureWorkspace(t)); !errors.Is(err, ErrTransportCleanupUnconfirmed) {
		t.Fatalf("uncertainty marker was lost: %v", err)
	}
	if lease.finished != 0 {
		t.Fatal("uncertain setup was marked definitively absent")
	}
	if err := executor.Cleanup(context.Background(), claim); err == nil || !transport.stopped {
		t.Fatalf("uncertain cleanup was released: stopped %t err %v", transport.stopped, err)
	}
}

func setupFailureWorkspace(t *testing.T) string {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "sources", "0"), 0700); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func setupFailureExecutor(t *testing.T, startErr error) (*Executor, *executorTransport, *executorWatchdogLease, protocol.Claim) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := codexsession.Open(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	transport := &executorTransport{startErr: startErr}
	lease := new(executorWatchdogLease)
	executor, err := NewExecutor(ExecutorConfig{ProfileHome: t.TempDir(), NativeImage: "native", RepositoryImage: "repo", Sessions: sessions, NewTransport: func(TransportConfig) (agentTransport, error) { return transport, nil }, ArmWatchdog: func(string, time.Time, time.Time) (codexWatchdogLease, error) { return lease, nil }})
	if err != nil {
		t.Fatal(err)
	}
	claim := protocol.Claim{RunID: "01k4w000000000000000000001", AttemptID: "01k4w000000000000000000002", Fence: 7, LeaseExpiresAt: time.Now().Add(time.Minute), Deadline: time.Now().Add(2 * time.Minute), Manifest: map[string]any{"agent": "codex", "runtime_version": profile.CodexVersion}}
	return executor, transport, lease, claim
}

func TestExecutorRunsPinnedChecksAndNormalizesResult(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := codexsession.Open(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	transport := new(executorTransport)
	executor, err := NewExecutor(ExecutorConfig{ProfileHome: t.TempDir(), NativeImage: "native", RepositoryImage: "repo", Sessions: sessions, NewTransport: func(TransportConfig) (agentTransport, error) { return transport, nil }})
	if err != nil {
		t.Fatal(err)
	}
	claim := protocol.Claim{RunID: "01k4w000000000000000000001", AttemptID: "01k4w000000000000000000002", Fence: 7, Manifest: map[string]any{"agent": "codex", "runtime_version": profile.CodexVersion, "kind": "review", "repository_id": 1, "base_sha": strings.Repeat("a", 40), "head_sha": strings.Repeat("b", 40), "profile_id": "01k4w000000000000000000003", "task_context": "Review carefully.", "effective_config": map[string]any{"model": "gpt-5", "instructions": "Stay focused."}, "supervisor": map[string]any{"credential_reference": "credential:test"}}}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "sources", "0"), 0700); err != nil {
		t.Fatal(err)
	}
	execution, err := executor.Execute(context.Background(), claim, nil, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Result["outcome"] != "no_findings" || len(execution.Artifacts) != 0 {
		t.Fatalf("unexpected execution: %#v", execution)
	}
	if len(transport.calls) != 5 {
		t.Fatalf("native calls=%d, want version + two probes around execution", len(transport.calls))
	}
	joined := strings.Join(transport.calls[3], " ")
	if !strings.Contains(joined, "--strict-config") || strings.Contains(joined, " resume ") {
		t.Fatalf("unsafe execution command: %s", joined)
	}
	if err = executor.Cleanup(context.Background(), claim); err != nil || !transport.stopped {
		t.Fatalf("cleanup=%v stopped=%t", err, transport.stopped)
	}
}

func TestSelectSourceUsesReviewHead(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"0", "1"} {
		if err := os.MkdirAll(filepath.Join(root, "sources", index), 0700); err != nil {
			t.Fatal(err)
		}
	}
	got, err := selectSource(root)
	if err != nil || got != filepath.Join(root, "sources", "1") {
		t.Fatalf("source=%q err=%v", got, err)
	}
}
