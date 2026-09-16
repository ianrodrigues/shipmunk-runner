package supervisor

import (
	"context"
	"sync"
	"testing"
)

// recordingLogger captures event names in call order for assertions, without depending on runlog's file format.
type recordingLogger struct {
	mu     sync.Mutex
	events []string
	fields map[string]map[string]any
}

func (r *recordingLogger) Event(event string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	if r.fields == nil {
		r.fields = make(map[string]map[string]any)
	}
	r.fields[event] = fields
}

func (r *recordingLogger) has(event string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e == event {
			return true
		}
	}
	return false
}

func TestRunOnceLogsSandboxLifecycleAndResultDelivery(t *testing.T) {
	s, _, _, _, _, _ := fixtureSupervisor(t)
	logger := &recordingLogger{}
	s.Log = logger

	out, err := s.RunOnce(context.Background())
	if err != nil || !out.Worked {
		t.Fatalf("RunOnce failed: %+v %v", out, err)
	}
	for _, event := range []string{
		"claim", "workspace_prepare_start", "workspace_prepare_done",
		"sandbox_create_started", "sandbox_created", "sandbox_exit",
		"result_delivered", "cleanup_start", "cleanup_done",
	} {
		if !logger.has(event) {
			t.Fatalf("expected event %q to be logged, got %v", event, logger.events)
		}
	}
	if fields := logger.fields["claim"]; fields["run_id"] == nil || fields["attempt_id"] == nil {
		t.Fatalf("claim event missing identity fields: %+v", fields)
	}
}

func TestRunOnceLogsClassifiedFailureFromExecution(t *testing.T) {
	s, c, _, _, w, _ := fixtureSupervisor(t)
	execution, err := DecodeExecution(*c.claim, 0, normalizedOutput(t, *c.claim, "incomplete"))
	if err != nil {
		t.Fatal(err)
	}
	execution.FailureReason = "malformed_output"
	executor := &fixtureExecutor{execution: execution}
	s.Executor = executor
	s.Sandbox = nil
	s.Watchdog = nil
	logger := &recordingLogger{}
	s.Log = logger

	out, err := s.RunOnce(context.Background())
	if err != nil || !out.Worked {
		t.Fatalf("RunOnce failed: %+v %v", out, err)
	}
	_ = w
	if !logger.has("classified_failure") {
		t.Fatalf("expected classified_failure event, got %v", logger.events)
	}
	if fields := logger.fields["classified_failure"]; fields["reason"] != "malformed_output" {
		t.Fatalf("expected classified failure reason to be logged: %+v", fields)
	}
}

func TestRunOnceWithoutLoggerDoesNotPanic(t *testing.T) {
	s, _, _, _, _, _ := fixtureSupervisor(t)
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce with nil Log failed: %v", err)
	}
}
