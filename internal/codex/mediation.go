// Package codex contains the trusted host-side boundaries used by Codex runs.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const (
	MaxCommandRequestBytes  = 64 * 1024
	MaxCommandBytes         = 8 * 1024
	MaxCommandResponseBytes = 64 * 1024
	MaxCommandOutputBytes   = 128 * 1024
)

// CommandResult is the bounded result returned by the isolated repository.
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// CommandExecutor executes only inside the separately isolated repository boundary.
type CommandExecutor interface {
	ExecuteRepositoryCommand(context.Context, string, int) (CommandResult, error)
}

// Mediator enforces one attempt's fence, ordering and repository-command budget.
// It deliberately does not interpret shell text: confinement belongs to the
// network-disabled disposable repository container used by the executor.
type Mediator struct {
	mu        sync.Mutex
	fence     int64
	last      int64
	remaining int
	sealed    bool
	executor  CommandExecutor
}

func NewMediator(fence int64, maxCommands int, executor CommandExecutor) (*Mediator, error) {
	if fence < 1 || fence > protocol.MaxSafeInteger || maxCommands < 1 || executor == nil {
		return nil, errors.New("repository mediator configuration is invalid")
	}
	return &Mediator{fence: fence, remaining: maxCommands, executor: executor}, nil
}

// Seal permanently rejects new commands before patch collection begins.
func (m *Mediator) Seal() {
	m.mu.Lock()
	m.sealed = true
	m.mu.Unlock()
}

// Handle validates and executes exactly one closed-schema request. Calls are
// serialized so concurrent or replayed bridge frames cannot spend one sequence twice.
func (m *Mediator) Handle(ctx context.Context, raw []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	request, err := decodeCommandRequest(raw)
	if err != nil {
		return nil, err
	}
	if request.Fence != m.fence {
		return nil, errors.New("repository command fence is invalid")
	}
	if request.ID != m.last+1 {
		return nil, errors.New("repository command sequence is invalid")
	}
	if m.sealed {
		return nil, errors.New("repository snapshot is sealed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// An exhausted budget is an answer the model can act on, not a reason to kill work that is already done.
	if m.remaining == 0 {
		m.last = request.ID
		exhausted := commandResponse{Fence: request.Fence, ID: request.ID, Stderr: "repository command budget exhausted; finish with the work already done", ExitCode: 1}
		encoded, err := json.Marshal(exhausted)
		if err != nil || len(encoded) > MaxCommandResponseBytes {
			return nil, ErrBudgetExhausted
		}
		return encoded, nil
	}

	// Spend the sequence before execution. An uncertain command must never be retried.
	m.last = request.ID
	m.remaining--
	result, err := m.executor.ExecuteRepositoryCommand(ctx, request.Command, MaxCommandOutputBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("repository command execution failed")
	}
	if result.ExitCode < 0 || result.ExitCode > 255 || len(result.Stdout)+len(result.Stderr) > MaxCommandOutputBytes ||
		!utf8.ValidString(result.Stdout) || !utf8.ValidString(result.Stderr) {
		return nil, errors.New("repository command result is invalid")
	}

	response := commandResponse{Fence: request.Fence, ID: request.ID, Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded) > MaxCommandResponseBytes {
		response.Stdout = ""
		response.Stderr = "Repository command response exceeds its frame limit."
		response.ExitCode = 1
		encoded, err = json.Marshal(response)
	}
	if err != nil || len(encoded) > MaxCommandResponseBytes {
		return nil, errors.New("repository command response is invalid")
	}
	return encoded, nil
}

type commandRequest struct {
	Fence   int64  `json:"fence"`
	ID      int64  `json:"id"`
	Command string `json:"command"`
}

type commandResponse struct {
	Fence    int64  `json:"fence"`
	ID       int64  `json:"id"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func decodeCommandRequest(raw []byte) (commandRequest, error) {
	value, err := protocol.Decode(raw, MaxCommandRequestBytes)
	if err != nil {
		return commandRequest{}, errors.New("repository command request schema is invalid")
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) != 3 {
		return commandRequest{}, errors.New("repository command request schema is invalid")
	}
	fence, fenceOK := safeInteger(object["fence"])
	id, idOK := safeInteger(object["id"])
	command, commandOK := object["command"].(string)
	if !fenceOK || !idOK || command == "" || !commandOK || len(command) > MaxCommandBytes ||
		strings.IndexByte(command, 0) >= 0 || !utf8.ValidString(command) {
		return commandRequest{}, errors.New("repository command request schema is invalid")
	}
	return commandRequest{Fence: fence, ID: id, Command: command}, nil
}

func safeInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	return parsed, err == nil && parsed >= 1 && parsed <= protocol.MaxSafeInteger
}
