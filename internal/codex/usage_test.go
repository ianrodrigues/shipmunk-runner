package codex

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

func TestNormalizedNativeUsagePassesTheResultContract(t *testing.T) {
	stream, err := Parse([]byte(validStream(validResult)), nil)
	if err != nil {
		t.Fatal(err)
	}

	review := &reviewEvidence{baselineSHA: sampleBaselineSHA, headSHA: sampleHeadSHA, changedFiles: []string{findingPath}}
	execution, err := normalizeExecution(context.Background(), protocol.Claim{
		RunID:     "01k4w000000000000000000001",
		AttemptID: "01k4w000000000000000000002",
		Fence:     1,
		Manifest:  map[string]any{"kind": "review"},
	}, stream, nil, review)
	if err != nil {
		t.Fatal(err)
	}

	result, err := json.Marshal(execution.Result)
	if err != nil {
		t.Fatal(err)
	}

	if err := protocol.Validate("result", result); err != nil {
		t.Fatalf("normalized result violates the contract: %v\n%s", err, result)
	}
}

func TestFailedNativeExecutionDoesNotPublishUsage(t *testing.T) {
	executor, transport, _, claim := setupFailureExecutor(t, nil)
	claim.Manifest = executionManifest()
	transport.executionResult = &CommandResult{
		ExitCode: 1,
		Stdout:   validStream(validResult),
	}

	execution, err := executor.Execute(context.Background(), claim, nil, setupFailureWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	if execution.Result["outcome"] != "incomplete" || execution.Result["usage"] != nil {
		t.Fatalf("failed native execution = %#v", execution.Result)
	}

	result, err := json.Marshal(execution.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.Validate("result", result); err != nil {
		t.Fatalf("failed result violates the contract: %v\n%s", err, result)
	}
}
