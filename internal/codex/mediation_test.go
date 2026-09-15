package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

type recordingExecutor struct {
	mu       sync.Mutex
	commands []string
	result   CommandResult
	err      error
}

func (e *recordingExecutor) ExecuteRepositoryCommand(_ context.Context, command string, limit int) (CommandResult, error) {
	if limit != MaxCommandOutputBytes {
		panic("unexpected output limit")
	}
	e.mu.Lock()
	e.commands = append(e.commands, command)
	e.mu.Unlock()
	return e.result, e.err
}

func TestMediatorBindsFenceSequenceAndBudget(t *testing.T) {
	executor := &recordingExecutor{result: CommandResult{Stdout: "ok\n"}}
	mediator, err := NewMediator(7, 2, executor)
	if err != nil {
		t.Fatal(err)
	}
	response, err := mediator.Handle(context.Background(), []byte(`{"fence":7,"id":1,"command":"printf ok"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded commandResponse
	if json.Unmarshal(response, &decoded) != nil || decoded.Fence != 7 || decoded.ID != 1 || decoded.Stdout != "ok\n" {
		t.Fatalf("unexpected response: %s", response)
	}

	for name, request := range map[string]string{
		"replay":      `{"fence":7,"id":1,"command":"true"}`,
		"gap":         `{"fence":7,"id":3,"command":"true"}`,
		"wrong fence": `{"fence":8,"id":2,"command":"true"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := mediator.Handle(context.Background(), []byte(request)); err == nil {
				t.Fatal("accepted request")
			}
		})
	}
	if _, err := mediator.Handle(context.Background(), []byte(`{"fence":7,"id":2,"command":"true"}`)); err != nil {
		t.Fatal(err)
	}
	// An exhausted budget answers the model instead of ending the attempt.
	exhausted, err := mediator.Handle(context.Background(), []byte(`{"fence":7,"id":3,"command":"true"}`))
	if err != nil {
		t.Fatalf("exhausted budget ended the attempt: %v", err)
	}
	if json.Unmarshal(exhausted, &decoded) != nil || decoded.ID != 3 || decoded.ExitCode != 1 || !strings.Contains(decoded.Stderr, "budget exhausted") {
		t.Fatalf("unexpected exhaustion response: %s", exhausted)
	}
	if len(executor.commands) != 2 {
		t.Fatalf("executed %d commands", len(executor.commands))
	}
}

func TestMediatorClosedRequestSchema(t *testing.T) {
	for name, request := range map[string]string{
		"duplicate":      `{"fence":7,"fence":7,"id":1,"command":"true"}`,
		"unknown":        `{"fence":7,"id":1,"command":"true","extra":false}`,
		"fraction":       `{"fence":7,"id":1.0,"command":"true"}`,
		"unsafe integer": `{"fence":7,"id":9007199254740992,"command":"true"}`,
		"empty":          `{"fence":7,"id":1,"command":""}`,
		"nul":            `{"fence":7,"id":1,"command":"x\u0000y"}`,
		"trailing":       `{"fence":7,"id":1,"command":"true"} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			mediator, _ := NewMediator(7, 1, &recordingExecutor{})
			if _, err := mediator.Handle(context.Background(), []byte(request)); err == nil {
				t.Fatal("accepted invalid schema")
			}
		})
	}
	mediator, _ := NewMediator(7, 1, &recordingExecutor{})
	if _, err := mediator.Handle(context.Background(), []byte(`{"fence":7,"id":1,"command":"`+strings.Repeat("x", MaxCommandBytes+1)+`"}`)); err == nil {
		t.Fatal("accepted oversized command")
	}
	if _, err := mediator.Handle(context.Background(), make([]byte, MaxCommandRequestBytes+1)); err == nil {
		t.Fatal("accepted oversized frame")
	}
}

func TestMediatorSpendsUncertainSequenceAndSeals(t *testing.T) {
	executor := &recordingExecutor{err: errors.New("SECRET timeout details")}
	mediator, _ := NewMediator(7, 2, executor)
	if _, err := mediator.Handle(context.Background(), []byte(`{"fence":7,"id":1,"command":"mutate"}`)); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("expected execution error")
	}
	if _, err := mediator.Handle(context.Background(), []byte(`{"fence":7,"id":1,"command":"mutate"}`)); !strings.Contains(errorText(err), "sequence") {
		t.Fatalf("replayed uncertain request: %v", err)
	}
	mediator.Seal()
	if _, err := mediator.Handle(context.Background(), []byte(`{"fence":7,"id":2,"command":"true"}`)); !strings.Contains(errorText(err), "sealed") {
		t.Fatalf("unexpected seal error: %v", err)
	}
}

func TestMediatorBoundsAndValidatesResult(t *testing.T) {
	for name, result := range map[string]CommandResult{
		"invalid exit":              {ExitCode: 256},
		"invalid utf8":              {Stdout: string([]byte{0xff})},
		"excessive combined output": {Stdout: strings.Repeat("x", MaxCommandOutputBytes), Stderr: "x"},
	} {
		t.Run(name, func(t *testing.T) {
			mediator, _ := NewMediator(7, 1, &recordingExecutor{result: result})
			if _, err := mediator.Handle(context.Background(), []byte(`{"fence":7,"id":1,"command":"true"}`)); err == nil {
				t.Fatal("accepted invalid result")
			}
		})
	}
	mediator, _ := NewMediator(7, 1, &recordingExecutor{result: CommandResult{Stdout: strings.Repeat("x", MaxCommandResponseBytes)}})
	response, err := mediator.Handle(context.Background(), []byte(`{"fence":7,"id":1,"command":"true"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), "exceeds its frame limit") || len(response) > MaxCommandResponseBytes {
		t.Fatalf("response was not safely replaced: %s", response)
	}
}

func TestMediatorSerializesConcurrentSequence(t *testing.T) {
	mediator, _ := NewMediator(7, 2, &recordingExecutor{})
	var wait sync.WaitGroup
	wait.Add(2)
	errorsSeen := make(chan error, 2)
	for range 2 {
		go func() {
			defer wait.Done()
			_, err := mediator.Handle(context.Background(), []byte(`{"fence":7,"id":1,"command":"true"}`))
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	successes := 0
	for err := range errorsSeen {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("got %d successes", successes)
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
