package codex

import (
	"context"
	"encoding/json"
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
	calls           [][]string
	stopped         bool
	startErr        error
	executionResult *CommandResult
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
		if t.executionResult != nil {
			return *t.executionResult, nil
		}
		// The fixture's two source snapshots are both empty directories (see
		// setupFailureWorkspace/TestExecutorRunsPinnedChecksAndNormalizesResult),
		// so the planned changed-file set is empty and coverage.files must be too.
		result := `{"summary":"Review complete.","outcome":"no_findings","charter_version":"` + ReviewCharterVersion + `","findings":[],"questions":null,"coverage":{"files":[],"context_gaps":[]},"verification_state":"none","tests":[]}`
		return CommandResult{Stdout: `{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}` + "\n" +
			`{"type":"turn.started"}` + "\n" +
			`{"type":"item.completed","item":{"id":"result","type":"agent_message","text":` + quote(result) + `}}` + "\n" +
			`{"type":"turn.completed"}` + "\n"}, nil
	}
}

func TestExecutorNormalizesOnlyClassifiedFailureStreams(t *testing.T) {
	for name, test := range map[string]struct {
		stream, outcome, reason string
		wantError               bool
	}{
		"approval":   {`{"type":"error","code":"approval_required","message":"private"}` + "\n", "needs_input", "approval_required", false},
		"rate limit": {`{"type":"turn.failed","error":{"code":"rate_limit_exceeded","message":"private"}}` + "\n", "incomplete", "rate_limited", false},
		// The shape a live backend schema rejection arrives in: no code field, the
		// raw provider error body copied into the message.
		"schema rejection": {`{"type":"turn.failed","error":{"message":"{\"error\":{\"message\":\"private\",\"type\":\"invalid_request_error\",\"param\":\"text.format.schema\",\"code\":\"invalid_json_schema\"}}"}}` + "\n", "incomplete", "invalid_output_schema", false},
		// The same rejection as the live runner observed it, with the error object beside the envelope's type and status.
		"relayed schema rejection": {`{"type":"turn.failed","error":{"message":"{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_json_schema\",\"message\":\"private schema diagnostic\"},\"status\":400}"}}` + "\n", "incomplete", "invalid_output_schema", false},
		// A structured result outside the contract fails the attempt; only unusable output fails the runner.
		"invalid result": {validStream(`{"summary":"private summary","outcome":"findings","charter_version":"1","findings":[` + findingJSON(findingPath, 5, 2) + `],"coverage":` + coverageJSON(findingPath) + `,"verification_state":"none","tests":[]}`), "incomplete", "invalid_result", false},
		"unknown":        {`{"type":"error","code":"future_code","message":"private"}` + "\n", "", "", true},
		"malformed":      {`{"type":"error","code":"approval_required"}`, "", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			executor, transport, _, claim := setupFailureExecutor(t, nil)
			claim.Manifest = executionManifest()
			transport.executionResult = &CommandResult{ExitCode: 1, Stdout: test.stream, Stderr: "private stderr"}
			execution, err := executor.Execute(context.Background(), claim, nil, setupFailureWorkspace(t))
			if test.wantError {
				if err == nil || execution.Result != nil {
					t.Fatalf("untrusted failure was normalized: %#v err=%v", execution, err)
				}
				return
			}
			if err != nil || execution.Result["outcome"] != test.outcome || len(execution.Events) != 1 || len(execution.Artifacts) != 0 {
				t.Fatalf("classified failure was not normalized: %#v err=%v", execution, err)
			}
			resultJSON, marshalErr := json.Marshal(execution.Result)
			if marshalErr != nil || protocol.Validate("result", resultJSON) != nil || protocol.Validate("worker-event", execution.Events[0]) != nil {
				t.Fatalf("classified failure violated publication contracts: result=%s event=%s marshal=%v", resultJSON, execution.Events[0], marshalErr)
			}
			summary, _ := execution.Result["summary"].(string)
			if !strings.Contains(summary, "Reason: "+test.reason+".") || strings.Contains(summary, "private") {
				t.Fatalf("unsafe or incomplete summary: %q", summary)
			}
		})
	}
}

// A non-charter outcome still arrives with all four charter keys set to null,
// because strict mode cannot omit a property.
func TestExecutorPublishesANonCharterOutcomeCarryingStrictNulls(t *testing.T) {
	executor, transport, _, claim := setupFailureExecutor(t, nil)
	claim.Manifest = executionManifest()
	result := `{"summary":"The command budget ran out.","outcome":"incomplete","charter_version":null,"findings":[],"questions":null,"coverage":null,"verification_state":null,"tests":[]}`
	transport.executionResult = &CommandResult{Stdout: `{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}` + "\n" +
		`{"type":"turn.started"}` + "\n" +
		`{"type":"item.completed","item":{"id":"result","type":"agent_message","text":` + quote(result) + `}}` + "\n" +
		`{"type":"turn.completed"}` + "\n"}
	execution, err := executor.Execute(context.Background(), claim, nil, setupFailureWorkspace(t))
	if err != nil || execution.Result["outcome"] != "incomplete" {
		t.Fatalf("strict null result was not published: %#v err=%v", execution, err)
	}
	for _, key := range []string{"charter_version", "questions", "coverage", "verification_state"} {
		if _, present := execution.Result[key]; present {
			t.Fatalf("normalized result still carries %q", key)
		}
	}
	resultJSON, marshalErr := json.Marshal(execution.Result)
	if marshalErr != nil || protocol.Validate("result", resultJSON) != nil {
		t.Fatalf("strict null result violated the v1 contract: %s (%v)", resultJSON, marshalErr)
	}
}

func executionManifest() map[string]any {
	return map[string]any{"agent": "codex", "runtime_version": profile.CodexVersion, "kind": "review", "source_artifacts": sourceReferences(2), "repository_id": 1, "target_sha": strings.Repeat("9", 40), "diff_base_sha": strings.Repeat("a", 40), "head_sha": strings.Repeat("b", 40), "profile_id": "01k4w000000000000000000003", "task_context": "Review carefully.", "effective_config": map[string]any{"model": "gpt-5", "instructions": "Stay focused.", "max_turns": json.Number("10")}}
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
	for _, index := range []string{"0", "1"} {
		if err := os.MkdirAll(filepath.Join(workspace, "sources", index), 0700); err != nil {
			t.Fatal(err)
		}
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
	claim := protocol.Claim{RunID: "01k4w000000000000000000001", AttemptID: "01k4w000000000000000000002", Fence: 7, LeaseExpiresAt: time.Now().Add(time.Minute), Deadline: time.Now().Add(2 * time.Minute), Manifest: map[string]any{"agent": "codex", "runtime_version": profile.CodexVersion, "kind": "review", "source_artifacts": sourceReferences(2), "effective_config": map[string]any{"max_turns": json.Number("2")}}}
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
	var receivedBudget int
	var preservedSource bool
	executor, err := NewExecutor(ExecutorConfig{ProfileHome: t.TempDir(), NativeImage: "native", RepositoryImage: "repo", Sessions: sessions, NewTransport: func(cfg TransportConfig) (agentTransport, error) {
		receivedBudget = cfg.MaxCommands
		if cfg.sourceHandle == nil {
			return nil, errors.New("source identity was not handed to transport")
		}
		moved := cfg.Source + "-moved"
		if err := os.Rename(cfg.Source, moved); err != nil {
			return nil, err
		}
		if err := os.Mkdir(cfg.Source, 0700); err != nil {
			return nil, err
		}
		original, originalErr := os.Stat(moved)
		opened, openedErr := cfg.sourceHandle.Stat()
		preservedSource = originalErr == nil && openedErr == nil && os.SameFile(original, opened)
		return transport, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	claim := protocol.Claim{RunID: "01k4w000000000000000000001", AttemptID: "01k4w000000000000000000002", Fence: 7, Manifest: map[string]any{"agent": "codex", "runtime_version": profile.CodexVersion, "kind": "review", "source_artifacts": sourceReferences(2), "repository_id": 1, "target_sha": strings.Repeat("9", 40), "diff_base_sha": strings.Repeat("a", 40), "head_sha": strings.Repeat("b", 40), "profile_id": "01k4w000000000000000000003", "task_context": "Review carefully.", "effective_config": map[string]any{"model": "gpt-5", "instructions": "Stay focused.", "max_turns": json.Number("2")}, "supervisor": map[string]any{"credential_reference": "credential:test"}}}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"0", "1"} {
		if err := os.MkdirAll(filepath.Join(workspace, "sources", index), 0700); err != nil {
			t.Fatal(err)
		}
	}
	execution, err := executor.Execute(context.Background(), claim, nil, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Result["outcome"] != "no_findings" || len(execution.Artifacts) != 0 {
		t.Fatalf("unexpected execution: %#v", execution)
	}
	if receivedBudget != 2 || !preservedSource {
		t.Fatalf("transport input handoff: budget=%d preserved_source=%t", receivedBudget, preservedSource)
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
