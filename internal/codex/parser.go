// Package codex implements the native Codex execution boundary.
package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const (
	MaxOutputBytes = 2 * 1024 * 1024
	MaxLineBytes   = 64 * 1024
	MaxEvents      = 10_000
)

var (
	ErrMalformedOutput = errors.New("malformed Codex output")
	ErrMissingResult   = errors.New("Codex output has no structured result")
	ErrInvalidResult   = errors.New("invalid Codex structured result")
	ErrNativeFailure   = errors.New("Codex reported a native failure")
)

// Event is sanitized transport metadata. Provider text and tool arguments are
// deliberately excluded so they cannot be published as runner diagnostics.
type Event struct {
	Type     string
	ItemID   string
	ItemType string
}

type Finding struct {
	Path        string
	Line        int64
	Side        string
	Severity    string
	Explanation string
	Evidence    string
}

type Test struct {
	Command string
	Status  string
	Summary string
}

type Result struct {
	Summary  string
	Outcome  string
	Findings []Finding
	Tests    []Test
}

type Usage struct {
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
}

type Stream struct {
	ThreadID string
	Events   []Event
	Result   Result
	Usage    *Usage
}

type itemState struct {
	kind     string
	complete bool
}

// Parse validates a complete newline-delimited Codex exec stream. stderr is
// counted against the output budget but never included in returned errors.
func Parse(stdout, stderr []byte) (Stream, error) {
	if len(stdout)+len(stderr) > MaxOutputBytes {
		return Stream{}, ErrMalformedOutput
	}
	if len(stdout) == 0 {
		return Stream{}, ErrMissingResult
	}
	if stdout[len(stdout)-1] != '\n' {
		return Stream{}, ErrMalformedOutput
	}

	lines := strings.Split(string(stdout[:len(stdout)-1]), "\n")
	if len(lines) > MaxEvents {
		return Stream{}, ErrMalformedOutput
	}

	var stream Stream
	state := "initial"
	items := make(map[string]itemState)
	var finalMessage string
	for _, line := range lines {
		if line == "" || len(line) > MaxLineBytes || state == "complete" || state == "failed" {
			return Stream{}, ErrMalformedOutput
		}
		value, err := protocol.Decode([]byte(line), MaxLineBytes)
		if err != nil {
			return Stream{}, ErrMalformedOutput
		}
		event, ok := value.(map[string]any)
		if !ok {
			return Stream{}, ErrMalformedOutput
		}
		typeName, ok := boundedString(event["type"], 64)
		if !ok {
			return Stream{}, ErrMalformedOutput
		}

		switch typeName {
		case "thread.started":
			threadID, ok := boundedString(event["thread_id"], 128)
			if !ok || state != "initial" {
				return Stream{}, ErrMalformedOutput
			}
			stream.ThreadID, state = threadID, "thread"
		case "turn.started":
			if state != "thread" {
				return Stream{}, ErrMalformedOutput
			}
			state = "turn"
		case "item.started", "item.updated", "item.completed":
			item, ok := event["item"].(map[string]any)
			if !ok {
				return Stream{}, ErrMalformedOutput
			}
			id, idOK := boundedString(item["id"], 128)
			kind, kindOK := boundedString(item["type"], 128)
			if !idOK || !kindOK || !knownItemType(kind) {
				return Stream{}, ErrMalformedOutput
			}
			// Codex may report a completed startup error after allocating a
			// thread but before a turn starts. No other pre-turn item is valid.
			if state == "thread" && typeName == "item.completed" && kind == "error" {
				return Stream{}, ErrNativeFailure
			}
			if state != "turn" {
				return Stream{}, ErrMalformedOutput
			}
			previous, exists := items[id]
			if (exists && (previous.complete || previous.kind != kind || typeName == "item.started")) || (!exists && typeName == "item.updated") {
				return Stream{}, ErrMalformedOutput
			}
			items[id] = itemState{kind: kind, complete: typeName == "item.completed"}
			stream.Events = append(stream.Events, Event{Type: typeName, ItemID: id, ItemType: kind})
			if kind == "error" {
				state = "failed"
				return Stream{}, ErrNativeFailure
			}
			if kind == "agent_message" && typeName == "item.completed" {
				message, ok := boundedString(item["text"], MaxLineBytes)
				if !ok {
					return Stream{}, ErrInvalidResult
				}
				finalMessage = message
			}
		case "turn.completed":
			if state != "turn" || hasUnfinished(items) {
				return Stream{}, ErrMalformedOutput
			}
			usage, err := parseUsage(event["usage"])
			if err != nil {
				return Stream{}, err
			}
			stream.Usage, state = usage, "complete"
		case "error", "turn.failed":
			return Stream{}, ErrNativeFailure
		default:
			return Stream{}, ErrMalformedOutput
		}
	}

	if state != "complete" || finalMessage == "" {
		return Stream{}, ErrMissingResult
	}
	result, err := parseResult([]byte(finalMessage))
	if err != nil {
		return Stream{}, err
	}
	stream.Result = result
	return stream, nil
}

func knownItemType(kind string) bool {
	switch kind {
	case "agent_message", "reasoning", "command_execution", "file_change", "mcp_tool_call", "web_search", "todo_list", "error":
		return true
	default:
		return false
	}
}

func hasUnfinished(items map[string]itemState) bool {
	for _, item := range items {
		if !item.complete {
			return true
		}
	}
	return false
}

func boundedString(value any, limit int) (string, bool) {
	text, ok := value.(string)
	return text, ok && text != "" && len(text) <= limit
}

func parseUsage(value any) (*Usage, error) {
	if value == nil {
		return nil, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrMalformedOutput
	}
	allowed := map[string]bool{"input_tokens": true, "cached_input_tokens": true, "output_tokens": true, "reasoning_output_tokens": true}
	for key := range object {
		if !allowed[key] {
			return nil, ErrMalformedOutput
		}
	}
	input, ok := nonnegativeInteger(object["input_tokens"])
	if !ok {
		return nil, ErrMalformedOutput
	}
	output, ok := nonnegativeInteger(object["output_tokens"])
	if !ok {
		return nil, ErrMalformedOutput
	}
	cached := int64(0)
	if raw, exists := object["cached_input_tokens"]; exists {
		cached, ok = nonnegativeInteger(raw)
		if !ok || cached > input {
			return nil, ErrMalformedOutput
		}
	}
	if raw, exists := object["reasoning_output_tokens"]; exists {
		if _, ok := nonnegativeInteger(raw); !ok {
			return nil, ErrMalformedOutput
		}
	}
	return &Usage{InputTokens: input, CachedInputTokens: cached, OutputTokens: output}, nil
}

func parseResult(raw []byte) (Result, error) {
	value, err := protocol.Decode(raw, MaxLineBytes)
	if err != nil {
		return Result{}, ErrInvalidResult
	}
	object, ok := value.(map[string]any)
	if !ok {
		return Result{}, ErrInvalidResult
	}
	allowed := map[string]bool{"summary": true, "outcome": true, "findings": true, "tests": true}
	for key := range object {
		if !allowed[key] {
			return Result{}, ErrInvalidResult
		}
	}
	summary, summaryOK := boundedString(object["summary"], 16_384)
	outcome, outcomeOK := boundedString(object["outcome"], 64)
	if !summaryOK || !outcomeOK || (outcome != "findings" && outcome != "no_findings" && outcome != "changes_proposed" && outcome != "incomplete" && outcome != "needs_input") {
		return Result{}, ErrInvalidResult
	}
	findingsRaw, findingsOK := object["findings"].([]any)
	testsRaw, testsOK := object["tests"].([]any)
	if !findingsOK || !testsOK || len(findingsRaw) > 100 || len(testsRaw) > 100 || (outcome == "findings" && len(findingsRaw) == 0) || (outcome == "no_findings" && len(findingsRaw) != 0) {
		return Result{}, ErrInvalidResult
	}
	result := Result{Summary: summary, Outcome: outcome, Findings: make([]Finding, 0, len(findingsRaw)), Tests: make([]Test, 0, len(testsRaw))}
	for _, rawFinding := range findingsRaw {
		finding, ok := parseFinding(rawFinding)
		if !ok {
			return Result{}, ErrInvalidResult
		}
		result.Findings = append(result.Findings, finding)
	}
	for _, rawTest := range testsRaw {
		test, ok := parseTest(rawTest)
		if !ok {
			return Result{}, ErrInvalidResult
		}
		result.Tests = append(result.Tests, test)
	}
	return result, nil
}

func parseFinding(value any) (Finding, bool) {
	object, ok := exactObject(value, "path", "line", "side", "severity", "explanation", "evidence")
	if !ok {
		return Finding{}, false
	}
	name, nameOK := boundedString(object["path"], 1024)
	line, lineOK := positiveInteger(object["line"])
	side, sideOK := boundedString(object["side"], 16)
	severity, severityOK := boundedString(object["severity"], 16)
	explanation, explanationOK := boundedString(object["explanation"], 8192)
	evidence, evidenceOK := boundedString(object["evidence"], 8192)
	clean := path.Clean(name)
	validPath := nameOK && !strings.Contains(name, "\\") && !containsC0(name) && clean == name && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../") && !strings.HasPrefix(clean, "/")
	validEnum := sideOK && (side == "LEFT" || side == "RIGHT") && severityOK && (severity == "info" || severity == "low" || severity == "medium" || severity == "high" || severity == "critical")
	return Finding{Path: name, Line: line, Side: side, Severity: severity, Explanation: explanation, Evidence: evidence}, validPath && lineOK && validEnum && explanationOK && evidenceOK
}

func parseTest(value any) (Test, bool) {
	object, ok := exactObject(value, "command", "status", "summary")
	if !ok {
		return Test{}, false
	}
	command, commandOK := boundedString(object["command"], 2048)
	status, statusOK := boundedString(object["status"], 16)
	summary, summaryOK := boundedString(object["summary"], 4096)
	return Test{Command: command, Status: status, Summary: summary}, commandOK && statusOK && summaryOK && (status == "passed" || status == "failed" || status == "error" || status == "not_run")
}

func containsC0(value string) bool {
	for _, character := range value {
		if character >= 0 && character <= 0x1f {
			return true
		}
	}
	return false
}

func exactObject(value any, keys ...string) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	if !ok || len(object) != len(keys) {
		return nil, false
	}
	for _, key := range keys {
		if _, exists := object[key]; !exists {
			return nil, false
		}
	}
	return object, true
}

func nonnegativeInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	integer, err := number.Int64()
	return integer, err == nil && integer >= 0 && integer <= protocol.MaxSafeInteger
}

func positiveInteger(value any) (int64, bool) {
	integer, ok := nonnegativeInteger(value)
	return integer, ok && integer > 0
}

func (s Stream) String() string {
	return fmt.Sprintf("Codex stream %q (%d events, %s)", s.ThreadID, len(s.Events), s.Result.Outcome)
}
