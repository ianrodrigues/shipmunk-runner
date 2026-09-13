package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/codexsession"
	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

type executorTransport struct {
	calls   [][]string
	stopped bool
}

func (t *executorTransport) Start(context.Context) error                  { return nil }
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
	workspace := t.TempDir()
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
	root := t.TempDir()
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
