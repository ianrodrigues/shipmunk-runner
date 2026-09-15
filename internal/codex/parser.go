package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const (
	// A single command_execution.aggregated_output or mcp_tool_call.result.content
	// entry can legally hold a whole review_read window or command log; both
	// budgets are sized to that, not to the handful of fields the parser reads.
	MaxOutputBytes = 16 * 1024 * 1024
	MaxLineBytes   = 1024 * 1024
	MaxEvents      = 10_000

	// Mirror internal/protocol/contracts/v1/result.schema.json exactly.
	MaxFindings             = 100
	MinFindingEvidence      = 1
	MaxFindingEvidence      = 5
	MaxQuestions            = 20
	MaxQuestionEvidence     = 5
	MaxCoverageFiles        = 300
	MaxCoverageContextGaps  = 20
	MaxMaterialTextBytes    = 2000
	MaxCoverageReasonBytes  = 500
	MaxQuestionTopicBytes   = 200
	MaxCharterVersionBytes  = 8
	MaxVerificationStateLen = 16
)

var evidenceSnapshotPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

var (
	ErrMalformedOutput = errors.New("malformed Codex output")
	ErrMissingResult   = errors.New("Codex output has no structured result")
	ErrInvalidResult   = errors.New("invalid Codex structured result")
	ErrNativeFailure   = errors.New("Codex reported a native failure")
)

type FailureReason string

const (
	FailureAuthExpired         FailureReason = "auth_expired"
	FailureRateLimited         FailureReason = "rate_limited"
	FailureApprovalRequired    FailureReason = "approval_required"
	FailureModelUnavailable    FailureReason = "model_unavailable"
	FailureInvalidOutputSchema FailureReason = "invalid_output_schema"
	// FailureInvalidResult is the executor's own classification of a structured result the contract rejects; no provider code maps to it.
	FailureInvalidResult FailureReason = "invalid_result"
	// FailureBudgetExhausted is the executor's own classification of an attempt the mediator could not answer once its request budget was spent.
	FailureBudgetExhausted FailureReason = "review_budget_exhausted"
	// FailureMalformedOutput and FailureMissingResult classify a stream the parser rejects; no provider code maps to them.
	FailureMalformedOutput FailureReason = "malformed_output"
	FailureMissingResult   FailureReason = "missing_result"
)

// ClassifiedFailure carries only a closed failure reason; raw provider text is deliberately excluded so callers can safely turn it into protocol output.
type ClassifiedFailure struct{ Reason FailureReason }

func (f *ClassifiedFailure) Error() string { return "Codex reported a classified native failure" }
func (f *ClassifiedFailure) Unwrap() error { return ErrNativeFailure }

// Event is sanitized transport metadata; provider text and tool arguments are deliberately excluded so they cannot be published as runner diagnostics.
type Event struct {
	Type     string
	ItemID   string
	ItemType string
}

// EvidenceRef.Snapshot is the real 40-character SHA of one attempt snapshot, never a "baseline"/"workspace" label.
type EvidenceRef struct {
	Snapshot  string
	Path      string
	LineStart int64
	LineEnd   int64
}

// Anchor is an optional, presentation-only pointer to one line; it is never used in place of Evidence for the checks that matter.
type Anchor struct {
	Path string
	Line int64
	Side string
}

type Finding struct {
	Category    string
	Severity    string
	Relation    string
	Scenario    string
	Consequence string
	Action      string
	Explanation string
	Evidence    []EvidenceRef
	Anchor      *Anchor
}

type Question struct {
	Topic       string
	Question    string
	WhyMaterial string
	Evidence    []EvidenceRef
}

type CoverageFile struct {
	Path   string
	Status string
	Reason string
}

type Coverage struct {
	Files       []CoverageFile
	ContextGaps []string
}

type Test struct {
	Command string
	Status  string
	Summary string
}

// Result mirrors contracts/v1/result.schema.json minus the envelope fields normalizeExecution adds.
type Result struct {
	Summary           string
	Outcome           string
	CharterVersion    string
	Findings          []Finding
	Questions         []Question
	Coverage          *Coverage
	VerificationState string
	Tests             []Test
}

type Usage struct {
	InputTokens       *int64
	CachedInputTokens *int64
	OutputTokens      *int64
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

// Parse validates a complete newline-delimited Codex exec stream; stderr counts against the output budget but never appears in returned errors.
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
	// The pinned CLI reports warnings, notices and retried backend errors as error items or events, then still completes the turn.
	// An error is therefore terminal only when no result follows.
	var reportedFailure error
	for _, line := range lines {
		if line == "" || len(line) > MaxLineBytes || state == "complete" {
			return Stream{}, ErrMalformedOutput
		}
		event, err := decodeStreamEvent([]byte(line))
		if err != nil {
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
			// The CLI source can add an item type (collab_tool_call today) ahead
			// of any parser update; an unrecognized kind is still recorded as an
			// event, with its payload otherwise ignored below.
			kind, kindOK := boundedString(item["type"], 128)
			if !idOK || !kindOK {
				return Stream{}, ErrMalformedOutput
			}
			// Codex may report a completed startup error after allocating a
			// thread but before a turn starts. No other pre-turn item is valid.
			if state == "thread" && typeName == "item.completed" && kind == "error" {
				stream.Events = append(stream.Events, Event{Type: typeName, ItemID: id, ItemType: kind})
				reportedFailure = nativeFailure(item)
				continue
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
				reportedFailure = nativeFailure(item)
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
		case "error":
			stream.Events = append(stream.Events, Event{Type: typeName, ItemType: "native"})
			reportedFailure = nativeFailure(event)
		case "turn.failed":
			failure, ok := event["error"].(map[string]any)
			if !ok {
				return Stream{}, ErrNativeFailure
			}
			return Stream{}, nativeFailure(failure)
		default:
			return Stream{}, ErrMalformedOutput
		}
	}

	if state != "complete" || finalMessage == "" {
		// The last error the stream reported is the one that explains why it stopped.
		if reportedFailure != nil {
			return Stream{}, reportedFailure
		}
		return Stream{}, ErrMissingResult
	}
	result, err := parseResult([]byte(finalMessage))
	if err != nil {
		return Stream{}, err
	}
	stream.Result = result
	return stream, nil
}

// decodeStreamEvent decodes one exec stream line the way encoding/json's
// default decoder does on a duplicate key: last value wins. codex-rs's
// #[serde(flatten)] can legally emit an item object with a field (id) repeated
// at the same level (ThreadItemDetails, exec_events.rs L97-102), and serde
// resolves that the same way. Every other document this package decodes,
// including the final structured result, still goes through protocol.Decode,
// which keeps rejecting a duplicate key.
func decodeStreamEvent(line []byte) (map[string]any, error) {
	if !utf8.Valid(line) {
		return nil, fmt.Errorf("codex stream line is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	value, err := decodeLenientValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("codex stream line has trailing JSON")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("codex stream line is not an object")
	}
	return object, nil
}

func decodeLenientValue(decoder *json.Decoder, depth int) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		if depth >= protocol.MaxJSONDepth-1 {
			return nil, fmt.Errorf("codex stream line exceeded its nesting limit")
		}
		object := make(map[string]any)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("object key is not a string")
			}
			value, err := decodeLenientValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("unterminated object")
		}
		return object, nil
	case '[':
		if depth >= protocol.MaxJSONDepth-1 {
			return nil, fmt.Errorf("codex stream line exceeded its nesting limit")
		}
		array := make([]any, 0)
		for decoder.More() {
			value, err := decodeLenientValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("unterminated array")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func nativeFailure(value map[string]any) error {
	if reason, ok := failureCode(value["code"]); ok {
		return &ClassifiedFailure{Reason: reason}
	}
	message, _ := value["message"].(string)
	message = strings.TrimSpace(message)
	if nested, ok := backendErrorEnvelope(message); ok {
		if reason, ok := failureCode(nested["code"]); ok {
			return &ClassifiedFailure{Reason: reason}
		}
	}
	if reason, ok := messageFailureReason(message); ok {
		return &ClassifiedFailure{Reason: reason}
	}
	return ErrNativeFailure
}

// messageFailureReason is best-effort. TurnError and ErrorItem carry no
// machine-readable code, and protocol/src/error.rs's extract_error_message
// unwraps the backend's own JSON envelope into a bare string before the CLI
// ever writes it to the stream, so the only thing left to classify against is
// prose the CLI may wrap in a prefix or trailing detail. Matching is therefore
// a case-insensitive substring, not equality.
func messageFailureReason(message string) (FailureReason, bool) {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "authentication expired"), strings.Contains(lower, "not logged in"), strings.Contains(lower, "unauthorized"), strings.Contains(lower, "refresh token expired"):
		return FailureAuthExpired, true
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "usage limit"):
		return FailureRateLimited, true
	case strings.Contains(lower, "approval required"):
		return FailureApprovalRequired, true
	case strings.Contains(lower, "model_not_found"), strings.Contains(lower, "model not found"):
		return FailureModelUnavailable, true
	case strings.Contains(lower, "invalid_json_schema"), strings.Contains(lower, "invalid json schema"), strings.Contains(lower, "invalid output schema"):
		return FailureInvalidOutputSchema, true
	default:
		return "", false
	}
}

// backendErrorEnvelope locates the one error object the pinned CLI copies out of
// a backend rejection. The CLI may add a prefix or trailing text around the body,
// so this searches for it instead of parsing the whole message. The located
// object is still decoded strictly.
func backendErrorEnvelope(message string) (map[string]any, bool) {
	start := strings.IndexByte(message, '{')
	if start < 0 {
		return nil, false
	}
	var envelope json.RawMessage
	if json.NewDecoder(strings.NewReader(message[start:])).Decode(&envelope) != nil {
		return nil, false
	}
	decoded, err := protocol.Decode(envelope, MaxLineBytes)
	body, bodyOK := decoded.(map[string]any)
	if err != nil || !bodyOK || !relayedErrorEnvelope(body) {
		return nil, false
	}
	nested, nestedOK := body["error"].(map[string]any)
	return nested, nestedOK
}

// relayedErrorEnvelope accepts only the provider's own envelope shape: type, error, and status, nothing else. A looser diagnostic that merely carries a code stays unclassified.
func relayedErrorEnvelope(body map[string]any) bool {
	for key := range body {
		switch key {
		case "type", "error", "status":
		default:
			return false
		}
	}
	return true
}

// failureCode matches an explicit machine-readable code field. codex-rs's
// TurnError and ErrorItem carry only message today, so this path is not known
// to fire against the pinned CLI; it stays as defense in depth for a future
// build that adds one, and messageFailureReason below is the path that
// actually classifies current output, including the unverified
// invalid_json_schema literal.
func failureCode(value any) (FailureReason, bool) {
	code, ok := value.(string)
	if !ok {
		return "", false
	}
	switch code {
	case "token_expired", "auth_expired", "unauthorized", "refresh_token_expired":
		return FailureAuthExpired, true
	case "rate_limit_exceeded", "usage_limit_reached":
		return FailureRateLimited, true
	case "approval_required", "approval_request":
		return FailureApprovalRequired, true
	case "model_not_found":
		return FailureModelUnavailable, true
	case "invalid_json_schema":
		return FailureInvalidOutputSchema, true
	default:
		return "", false
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
	input, ok := nullableNonnegativeInteger(object["input_tokens"])
	if !ok {
		return nil, ErrMalformedOutput
	}
	output, ok := nullableNonnegativeInteger(object["output_tokens"])
	if !ok {
		return nil, ErrMalformedOutput
	}
	var cached *int64
	// codex itself clamps cached_input_tokens to input_tokens (protocol.rs), but
	// that clamp is provider-side; a provider can still report cached > input, and
	// the contract carries no field that would reject it, so it is not rejected here.
	if raw, exists := object["cached_input_tokens"]; exists {
		cached, ok = nullableNonnegativeInteger(raw)
		if !ok {
			return nil, ErrMalformedOutput
		}
	}
	// reasoning_output_tokens and cache_write_input_tokens are validated when
	// present but carry no contract field, so both are dropped after validation.
	if raw, exists := object["reasoning_output_tokens"]; exists {
		if _, ok := nonnegativeInteger(raw); !ok {
			return nil, ErrMalformedOutput
		}
	}
	if raw, exists := object["cache_write_input_tokens"]; exists {
		if _, ok := nonnegativeInteger(raw); !ok {
			return nil, ErrMalformedOutput
		}
	}
	// Any other key is future usage accounting the pinned CLI does not emit yet; it is ignored rather than rejected.
	return &Usage{InputTokens: input, CachedInputTokens: cached, OutputTokens: output}, nil
}

// dropNullMembers turns the strict output schema's nullable stand-ins back into
// the absent fields contracts/v1/result.schema.json expects, because strict
// structured outputs cannot omit a property. A null array element stays, since
// an element is a value the model chose to emit, not an omitted field.
func dropNullMembers(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, member := range typed {
			if member == nil {
				delete(typed, key)
				continue
			}
			dropNullMembers(member)
		}
	case []any:
		for _, member := range typed {
			dropNullMembers(member)
		}
	}
}

// parseResult's allowed-keys-by-outcome mirrors result.schema.json's allOf
// conditionals. findings and no_findings require charter_version, coverage,
// and verification_state, plus optional questions. incomplete permits
// coverage alone, and every other outcome permits none of the four.
func parseResult(raw []byte) (Result, error) {
	value, err := protocol.Decode(raw, MaxLineBytes)
	if err != nil {
		return Result{}, ErrInvalidResult
	}
	object, ok := value.(map[string]any)
	if !ok {
		return Result{}, ErrInvalidResult
	}
	dropNullMembers(object)
	allowed := map[string]bool{"summary": true, "outcome": true, "findings": true, "tests": true}
	summary, summaryOK := boundedString(object["summary"], 16_384)
	outcome, outcomeOK := boundedString(object["outcome"], 64)
	if !summaryOK || !outcomeOK || (outcome != "findings" && outcome != "no_findings" && outcome != "changes_proposed" && outcome != "incomplete" && outcome != "needs_input") {
		return Result{}, ErrInvalidResult
	}
	extended := outcome == "findings" || outcome == "no_findings"
	switch {
	case extended:
		allowed["charter_version"], allowed["coverage"], allowed["verification_state"], allowed["questions"] = true, true, true, true
	case outcome == "incomplete":
		allowed["coverage"] = true
	}
	for key := range object {
		if !allowed[key] {
			return Result{}, ErrInvalidResult
		}
	}
	findingsRaw, findingsOK := object["findings"].([]any)
	testsRaw, testsOK := object["tests"].([]any)
	emptyFindingsRequired := outcome == "no_findings" || outcome == "incomplete" || outcome == "needs_input"
	if !findingsOK || !testsOK || len(findingsRaw) > MaxFindings || len(testsRaw) > 100 || (outcome == "findings" && len(findingsRaw) == 0) || (emptyFindingsRequired && len(findingsRaw) != 0) {
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

	if extended {
		charterVersion, ok := boundedString(object["charter_version"], MaxCharterVersionBytes)
		if !ok {
			return Result{}, ErrInvalidResult
		}
		result.CharterVersion = charterVersion
		verificationState, ok := boundedString(object["verification_state"], MaxVerificationStateLen)
		if !ok || verificationState != "none" {
			return Result{}, ErrInvalidResult
		}
		result.VerificationState = verificationState
		coverage, ok := parseCoverage(object["coverage"])
		if !ok {
			return Result{}, ErrInvalidResult
		}
		result.Coverage = &coverage
		if rawQuestions, exists := object["questions"]; exists {
			questionsRaw, ok := rawQuestions.([]any)
			if !ok || len(questionsRaw) > MaxQuestions {
				return Result{}, ErrInvalidResult
			}
			result.Questions = make([]Question, 0, len(questionsRaw))
			for _, rawQuestion := range questionsRaw {
				question, ok := parseQuestion(rawQuestion)
				if !ok {
					return Result{}, ErrInvalidResult
				}
				result.Questions = append(result.Questions, question)
			}
		}
	} else if outcome == "incomplete" {
		if rawCoverage, exists := object["coverage"]; exists {
			coverage, ok := parseCoverage(rawCoverage)
			if !ok {
				return Result{}, ErrInvalidResult
			}
			result.Coverage = &coverage
		}
	}
	return result, nil
}

// validRelativePath mirrors result.schema.json's $defs.repository_path pattern.
func validRelativePath(name string) bool {
	clean := path.Clean(name)
	return !strings.Contains(name, "\\") && !containsC0(name) && clean == name && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../") && !strings.HasPrefix(clean, "/")
}

func parseFinding(value any) (Finding, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return Finding{}, false
	}
	_, hasAnchor := object["anchor"]
	required := []string{"category", "severity", "relation", "scenario", "consequence", "action", "explanation", "evidence"}
	expected := len(required)
	if hasAnchor {
		expected++
	}
	if len(object) != expected {
		return Finding{}, false
	}
	for _, key := range required {
		if _, exists := object[key]; !exists {
			return Finding{}, false
		}
	}
	category, categoryOK := boundedString(object["category"], 32)
	severity, severityOK := boundedString(object["severity"], 16)
	relation, relationOK := boundedString(object["relation"], 16)
	scenario, scenarioOK := boundedString(object["scenario"], MaxMaterialTextBytes)
	consequence, consequenceOK := boundedString(object["consequence"], MaxMaterialTextBytes)
	action, actionOK := boundedString(object["action"], MaxMaterialTextBytes)
	explanation, explanationOK := boundedString(object["explanation"], 8192)
	validCategory := categoryOK && oneOf(category, "correctness", "security", "contract", "maintenance", "test_adequacy", "performance", "other")
	validSeverity := severityOK && oneOf(severity, "info", "low", "medium", "high", "critical")
	validRelation := relationOK && oneOf(relation, "introduced", "modified", "preexisting")

	evidenceRaw, evidenceOK := object["evidence"].([]any)
	if !evidenceOK || len(evidenceRaw) < MinFindingEvidence || len(evidenceRaw) > MaxFindingEvidence {
		return Finding{}, false
	}
	evidence := make([]EvidenceRef, 0, len(evidenceRaw))
	for _, rawEvidence := range evidenceRaw {
		ref, ok := parseEvidenceRef(rawEvidence)
		if !ok {
			return Finding{}, false
		}
		evidence = append(evidence, ref)
	}

	var anchor *Anchor
	if hasAnchor {
		parsed, ok := parseAnchor(object["anchor"])
		if !ok {
			return Finding{}, false
		}
		anchor = &parsed
	}

	valid := validCategory && validSeverity && validRelation && scenarioOK && consequenceOK && actionOK && explanationOK
	return Finding{Category: category, Severity: severity, Relation: relation, Scenario: scenario, Consequence: consequence, Action: action, Explanation: explanation, Evidence: evidence, Anchor: anchor}, valid
}

func parseEvidenceRef(value any) (EvidenceRef, bool) {
	object, ok := exactObject(value, "snapshot", "path", "line_start", "line_end")
	if !ok {
		return EvidenceRef{}, false
	}
	snapshot, snapshotOK := boundedString(object["snapshot"], 40)
	name, nameOK := boundedString(object["path"], 1024)
	lineStart, startOK := positiveInteger(object["line_start"])
	lineEnd, endOK := positiveInteger(object["line_end"])
	validSnapshot := snapshotOK && evidenceSnapshotPattern.MatchString(snapshot)
	validPath := nameOK && validRelativePath(name)
	validRange := startOK && endOK && lineEnd >= lineStart
	return EvidenceRef{Snapshot: snapshot, Path: name, LineStart: lineStart, LineEnd: lineEnd}, validSnapshot && validPath && validRange
}

func parseAnchor(value any) (Anchor, bool) {
	object, ok := exactObject(value, "path", "line", "side")
	if !ok {
		return Anchor{}, false
	}
	name, nameOK := boundedString(object["path"], 1024)
	line, lineOK := positiveInteger(object["line"])
	side, sideOK := boundedString(object["side"], 16)
	validPath := nameOK && validRelativePath(name)
	validSide := sideOK && (side == "LEFT" || side == "RIGHT")
	return Anchor{Path: name, Line: line, Side: side}, validPath && lineOK && validSide
}

// parseQuestion requires an exact key set per evidence presence, which
// structurally forbids any extra property such as an invented "severity".
func parseQuestion(value any) (Question, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return Question{}, false
	}
	_, hasEvidence := object["evidence"]
	expected := 3
	if hasEvidence {
		expected = 4
	}
	if len(object) != expected {
		return Question{}, false
	}
	for _, key := range []string{"topic", "question", "why_material"} {
		if _, exists := object[key]; !exists {
			return Question{}, false
		}
	}
	topic, topicOK := boundedString(object["topic"], MaxQuestionTopicBytes)
	question, questionOK := boundedString(object["question"], MaxMaterialTextBytes)
	why, whyOK := boundedString(object["why_material"], MaxMaterialTextBytes)
	var evidence []EvidenceRef
	if hasEvidence {
		rawEvidence, ok := object["evidence"].([]any)
		if !ok || len(rawEvidence) > MaxQuestionEvidence {
			return Question{}, false
		}
		evidence = make([]EvidenceRef, 0, len(rawEvidence))
		for _, entry := range rawEvidence {
			ref, ok := parseEvidenceRef(entry)
			if !ok {
				return Question{}, false
			}
			evidence = append(evidence, ref)
		}
	}
	return Question{Topic: topic, Question: question, WhyMaterial: why, Evidence: evidence}, topicOK && questionOK && whyOK
}

func parseCoverage(value any) (Coverage, bool) {
	object, ok := exactObject(value, "files", "context_gaps")
	if !ok {
		return Coverage{}, false
	}
	filesRaw, filesOK := object["files"].([]any)
	gapsRaw, gapsOK := object["context_gaps"].([]any)
	if !filesOK || !gapsOK || len(filesRaw) > MaxCoverageFiles || len(gapsRaw) > MaxCoverageContextGaps {
		return Coverage{}, false
	}
	coverage := Coverage{Files: make([]CoverageFile, 0, len(filesRaw)), ContextGaps: make([]string, 0, len(gapsRaw))}
	for _, rawFile := range filesRaw {
		file, ok := parseCoverageFile(rawFile)
		if !ok {
			return Coverage{}, false
		}
		coverage.Files = append(coverage.Files, file)
	}
	for _, rawGap := range gapsRaw {
		gap, ok := boundedString(rawGap, MaxCoverageReasonBytes)
		if !ok {
			return Coverage{}, false
		}
		coverage.ContextGaps = append(coverage.ContextGaps, gap)
	}
	return coverage, true
}

// parseCoverageFile enforces the schema's conditional exactly: reason is
// required when status is unreviewed and forbidden otherwise.
func parseCoverageFile(value any) (CoverageFile, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return CoverageFile{}, false
	}
	name, nameOK := boundedString(object["path"], 1024)
	status, statusOK := boundedString(object["status"], 16)
	validStatus := statusOK && (status == "reviewed" || status == "unreviewed")
	_, hasReason := object["reason"]
	expected := 2
	if hasReason {
		expected = 3
	}
	if len(object) != expected || hasReason != (status == "unreviewed") {
		return CoverageFile{}, false
	}
	var reason string
	if hasReason {
		text, ok := boundedString(object["reason"], MaxCoverageReasonBytes)
		if !ok {
			return CoverageFile{}, false
		}
		reason = text
	}
	validPath := nameOK && validRelativePath(name)
	return CoverageFile{Path: name, Status: status, Reason: reason}, validPath && validStatus
}

func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
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

func nullableNonnegativeInteger(value any) (*int64, bool) {
	if value == nil {
		return nil, true
	}
	integer, ok := nonnegativeInteger(value)
	if !ok {
		return nil, false
	}
	return &integer, true
}

func positiveInteger(value any) (int64, bool) {
	integer, ok := nonnegativeInteger(value)
	return integer, ok && integer > 0
}

func (s Stream) String() string {
	return fmt.Sprintf("Codex stream %q (%d events, %s)", s.ThreadID, len(s.Events), s.Result.Outcome)
}
