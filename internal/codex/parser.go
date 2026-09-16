package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const (
	// MaxOutputBytes bounds the combined stdout and stderr length; MaxLineBytes bounds each parsed stream line.
	MaxOutputBytes = 16 * 1024 * 1024
	MaxLineBytes   = 1024 * 1024
	MaxEvents      = 10_000
	// Bounds only the final structured agent_message text; independent of MaxLineBytes so a larger tool-call payload doesn't grow this too.
	MaxResultBytes = 64 * 1024

	// Mirror internal/protocol/contracts/v1/result.schema.json exactly.
	MaxFindings             = 100
	MinFindingEvidence      = 1
	MaxFindingEvidence      = 5
	MaxQuestions            = 20
	MaxQuestionEvidence     = 5
	MaxCoverageFiles        = 300
	MaxCoverageContextGaps  = 20
	MaxMaterialTextBytes    = 2000
	MinFindingTitleChars    = 5
	MaxFindingTitleChars    = 80
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
	Title       string
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

	// Scanned as byte slices, not string-split, to avoid copying tens of MB; line count mirrors strings.Split's N+1-records rule.
	body := stdout[:len(stdout)-1]
	lineCount := bytes.Count(body, []byte{'\n'}) + 1
	if lineCount > MaxEvents {
		return Stream{}, ErrMalformedOutput
	}

	var stream Stream
	state := "initial"
	items := make(map[string]itemState)
	var finalMessage string
	// Warnings, notices and retried errors surface as error items while the turn continues; an error is terminal only without a result.
	var reportedFailure *ClassifiedFailure
	for i := 0; i < lineCount; i++ {
		var line []byte
		if index := bytes.IndexByte(body, '\n'); index >= 0 {
			line, body = body[:index], body[index+1:]
		} else {
			line, body = body, nil
		}
		if len(line) == 0 || len(line) > MaxLineBytes || state == "complete" {
			return Stream{}, ErrMalformedOutput
		}
		event, err := decodeStreamEvent(line, MaxLineBytes)
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
			// An unrecognized item type is still recorded as an event; only its payload is ignored.
			kind, kindOK := boundedString(item["type"], 128)
			if !idOK || !kindOK {
				return Stream{}, ErrMalformedOutput
			}
			// A completed startup error can arrive after thread allocation but before turn start; no other pre-turn item is valid.
			if state == "thread" && typeName == "item.completed" && kind == "error" {
				stream.Events = append(stream.Events, Event{Type: typeName, ItemID: id, ItemType: kind})
				if failure := progressFailure(item); failure != nil {
					reportedFailure = failure
				}
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
				if failure := progressFailure(item); failure != nil {
					reportedFailure = failure
				}
			}
			if kind == "agent_message" && typeName == "item.completed" {
				message, ok := boundedString(item["text"], MaxResultBytes)
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
			if failure := progressFailure(event); failure != nil {
				reportedFailure = failure
			}
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

// Allows a duplicate key, matching encoding/json's last-value-wins rule, because codex-rs's #[serde(flatten)] can legally repeat an item id.
// For a web_search item, Event.ItemID reports the action id, not the item id, by design; ItemID is bookkeeping, never a lookup key.
func decodeStreamEvent(line []byte, maxBytes int) (map[string]any, error) {
	value, err := protocol.DecodeAllowingDuplicateKeys(line, maxBytes)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("codex stream line is not an object")
	}
	return object, nil
}

// Classifies the terminal turn.failed error; free-form message substring matching is scoped to this path only.
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

// progressFailure classifies a non-fatal error item or event by explicit code only, never by messageFailureReason's substring match, since its free-form text can share words with a fatal message.
// nil means unclassified; the caller must not record that as "the" reported failure, or a truncated stream misreports ErrNativeFailure instead of ErrMissingResult.
func progressFailure(value map[string]any) *ClassifiedFailure {
	if reason, ok := failureCode(value["code"]); ok {
		return &ClassifiedFailure{Reason: reason}
	}
	message, _ := value["message"].(string)
	if nested, ok := backendErrorEnvelope(strings.TrimSpace(message)); ok {
		if reason, ok := failureCode(nested["code"]); ok {
			return &ClassifiedFailure{Reason: reason}
		}
	}
	return nil
}

// Best-effort: TurnError/ErrorItem carry no code, so this matches a case-insensitive substring against the CLI's free-form message text.
func messageFailureReason(message string) (FailureReason, bool) {
	lower := strings.ToLower(message)
	switch {
	case containsAny(lower, "authentication expired", "not logged in", "unauthorized", "refresh token expired"):
		return FailureAuthExpired, true
	case containsAny(lower, "rate limit", "usage limit"):
		return FailureRateLimited, true
	case containsAny(lower, "approval required"):
		return FailureApprovalRequired, true
	case containsAny(lower, "model_not_found", "model not found"):
		return FailureModelUnavailable, true
	case containsAny(lower, "invalid_json_schema", "invalid json schema", "invalid output schema"):
		return FailureInvalidOutputSchema, true
	default:
		return "", false
	}
}

func containsAny(text string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(text, candidate) {
			return true
		}
	}
	return false
}

// Searches for the JSON error object instead of parsing the whole message, since the CLI may wrap it in extra text.
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

// Accepts only the provider's own envelope shape (type, error, status); a diagnostic that merely carries a code stays unclassified.
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

// Matches an explicit code field; kept as defense in depth since the pinned CLI's TurnError/ErrorItem carry only message today.
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
	// Provider-side clamping of cached <= input is not guaranteed here; the contract has no field to reject a violation.
	if raw, exists := object["cached_input_tokens"]; exists {
		cached, ok = nullableNonnegativeInteger(raw)
		if !ok {
			return nil, ErrMalformedOutput
		}
	}
	// Validated when present but dropped: neither has a contract field.
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

// Turns the schema's nullable stand-ins back into absent fields, because strict structured outputs cannot omit a property.
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

// allowed-keys-by-outcome mirrors result.schema.json's allOf conditionals.
func parseResult(raw []byte) (Result, error) {
	value, err := protocol.Decode(raw, MaxResultBytes)
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
	required := []string{"category", "title", "severity", "relation", "scenario", "consequence", "action", "explanation", "evidence"}
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
	title, titleOK := object["title"].(string)
	severity, severityOK := boundedString(object["severity"], 16)
	relation, relationOK := boundedString(object["relation"], 16)
	scenario, scenarioOK := boundedString(object["scenario"], MaxMaterialTextBytes)
	consequence, consequenceOK := boundedString(object["consequence"], MaxMaterialTextBytes)
	action, actionOK := boundedString(object["action"], MaxMaterialTextBytes)
	explanation, explanationOK := boundedString(object["explanation"], 8192)
	validTitle := titleOK && singleLineTitle(title)
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

	valid := validCategory && validTitle && validSeverity && validRelation && scenarioOK && consequenceOK && actionOK && explanationOK
	return Finding{Category: category, Title: title, Severity: severity, Relation: relation, Scenario: scenario, Consequence: consequence, Action: action, Explanation: explanation, Evidence: evidence, Anchor: anchor}, valid
}

// Counts runes, not bytes, to match the contract's minLength/maxLength.
func singleLineTitle(title string) bool {
	count := utf8.RuneCountInString(title)
	if count < MinFindingTitleChars || count > MaxFindingTitleChars || strings.TrimSpace(title) == "" {
		return false
	}
	for _, r := range title {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
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

// Exact key set per evidence presence structurally forbids an extra property like an invented "severity".
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

// reason is required when status is unreviewed, forbidden otherwise.
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
