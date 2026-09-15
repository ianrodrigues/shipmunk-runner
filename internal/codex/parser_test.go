package codex

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validStream(result string) string {
	return `{"type":"thread.started","thread_id":"thread-1"}` + "\n" +
		`{"type":"turn.started"}` + "\n" +
		`{"type":"item.started","item":{"id":"tool-1","type":"command_execution"}}` + "\n" +
		`{"type":"item.completed","item":{"id":"tool-1","type":"command_execution"}}` + "\n" +
		`{"type":"item.completed","item":{"id":"message-1","type":"agent_message","text":` + quote(result) + `}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":3,"output_tokens":4,"reasoning_output_tokens":2}}` + "\n"
}

func quote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

const (
	sampleBaselineSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sampleHeadSHA     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// finding, coverage and question builders below mirror
// contracts/v1/result.schema.json exactly (internal/protocol/contracts/v1/result.schema.json),
// minus the envelope fields normalizeExecution adds.

func findingJSON(path string, lineStart, lineEnd int64) string {
	return `{"category":"correctness","severity":"high","relation":"introduced","scenario":"A scenario.","consequence":"A consequence.","action":"An action.","explanation":"An explanation.","evidence":[` + evidenceJSON(sampleHeadSHA, path, lineStart, lineEnd) + `]}`
}

func evidenceJSON(snapshot, path string, lineStart, lineEnd int64) string {
	raw, err := json.Marshal(map[string]any{"snapshot": snapshot, "path": path, "line_start": lineStart, "line_end": lineEnd})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func coverageJSON(path string) string {
	raw, err := json.Marshal(map[string]any{"files": []any{map[string]any{"path": path, "status": "reviewed"}}, "context_gaps": []any{}})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

const findingPath = "internal/codex/parser.go"

func noFindingsResult() string {
	return `{"summary":"Done.","outcome":"no_findings","charter_version":"1","findings":[],"coverage":` + coverageJSON(findingPath) + `,"verification_state":"none","tests":[]}`
}

func findingsResult() string {
	return `{"summary":"Done.","outcome":"findings","charter_version":"1","findings":[` + findingJSON(findingPath, 1, 2) + `],"coverage":` + coverageJSON(findingPath) + `,"verification_state":"none","tests":[]}`
}

var validResult = noFindingsResult()

func TestParseValidCompleteStream(t *testing.T) {
	stream, err := Parse([]byte(validStream(validResult)), []byte("private provider diagnostics"))
	if err != nil {
		t.Fatal(err)
	}
	if stream.ThreadID != "thread-1" || stream.Result.Summary != "Done." || stream.Result.Outcome != "no_findings" || stream.Usage == nil || stream.Usage.CachedInputTokens == nil || *stream.Usage.CachedInputTokens != 3 {
		t.Fatalf("unexpected stream: %#v", stream)
	}
	if stream.Result.CharterVersion != ReviewCharterVersion || stream.Result.VerificationState != "none" || stream.Result.Coverage == nil || len(stream.Result.Coverage.Files) != 1 {
		t.Fatalf("unexpected charter fields: %#v", stream.Result)
	}
	if len(stream.Events) != 3 || stream.Events[0] != (Event{Type: "item.started", ItemID: "tool-1", ItemType: "command_execution"}) {
		t.Fatalf("unexpected sanitized events: %#v", stream.Events)
	}
	if strings.Contains(stream.String(), "private") {
		t.Fatal("provider diagnostics escaped parsing")
	}
}

// Codex 0.154 adds cache_write_input_tokens to turn usage; the contract has no field for it, so it is validated and dropped.
func TestParseAcceptsCacheWriteUsageFromCodex0154(t *testing.T) {
	stream := strings.Replace(
		validStream(validResult),
		`{"input_tokens":10,"cached_input_tokens":3,"output_tokens":4,"reasoning_output_tokens":2}`,
		`{"input_tokens":65430,"cached_input_tokens":44800,"cache_write_input_tokens":0,"output_tokens":2374,"reasoning_output_tokens":809}`,
		1,
	)

	parsed, err := Parse([]byte(stream), nil)
	if err != nil || parsed.Usage == nil || parsed.Usage.InputTokens == nil || *parsed.Usage.InputTokens != 65430 || *parsed.Usage.CachedInputTokens != 44800 || *parsed.Usage.OutputTokens != 2374 {
		t.Fatalf("usage = %#v, err = %v", parsed.Usage, err)
	}
}

func TestParseRejectsInvalidKnownUsageFields(t *testing.T) {
	for name, usage := range map[string]string{
		"negative cache write": `{"input_tokens":10,"output_tokens":4,"cache_write_input_tokens":-1}`,
		"negative reasoning":   `{"input_tokens":10,"output_tokens":4,"reasoning_output_tokens":-1}`,
		"non-numeric input":    `{"input_tokens":"10","output_tokens":4}`,
		"cached not a number":  `{"input_tokens":10,"output_tokens":4,"cached_input_tokens":true}`,
	} {
		stream := strings.Replace(validStream(validResult), `{"input_tokens":10,"cached_input_tokens":3,"output_tokens":4,"reasoning_output_tokens":2}`, usage, 1)
		if _, err := Parse([]byte(stream), nil); !errors.Is(err, ErrMalformedOutput) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

// A future Codex release can add a usage key this parser does not read yet;
// the CLI source shows the shape can grow, so an unrecognized key is ignored
// rather than rejected, and a legal cached > input value (codex's own clamp is
// provider-side, not a stream guarantee) no longer rejects the stream either.
func TestParseIgnoresUnknownUsageKeysAndClampedCache(t *testing.T) {
	for name, usage := range map[string]string{
		"unknown key":       `{"input_tokens":10,"output_tokens":4,"future_tokens":1}`,
		"cached over input": `{"input_tokens":10,"output_tokens":4,"cached_input_tokens":20}`,
	} {
		stream := strings.Replace(validStream(validResult), `{"input_tokens":10,"cached_input_tokens":3,"output_tokens":4,"reasoning_output_tokens":2}`, usage, 1)
		if _, err := Parse([]byte(stream), nil); err != nil {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestParsePreservesUnavailableNativeUsageAsNull(t *testing.T) {
	stream := strings.Replace(
		validStream(validResult),
		`{"input_tokens":10,"cached_input_tokens":3,"output_tokens":4,"reasoning_output_tokens":2}`,
		`{"input_tokens":null,"output_tokens":4}`,
		1,
	)

	parsed, err := Parse([]byte(stream), nil)
	if err != nil || parsed.Usage == nil || parsed.Usage.InputTokens != nil || parsed.Usage.CachedInputTokens != nil || parsed.Usage.OutputTokens == nil || *parsed.Usage.OutputTokens != 4 {
		t.Fatalf("usage = %#v, err = %v", parsed.Usage, err)
	}
}

func TestParseRejectsMalformedAndIncompleteStreams(t *testing.T) {
	valid := validStream(validResult)
	lines := strings.Split(valid, "\n")
	tests := map[string]struct {
		stdout string
		stderr string
		want   error
	}{
		"empty":                 {want: ErrMissingResult},
		"truncated":             {stdout: strings.TrimSuffix(valid, "\n"), want: ErrMalformedOutput},
		"empty record":          {stdout: "\n", want: ErrMalformedOutput},
		"invalid JSON":          {stdout: "{bad}\n", want: ErrMalformedOutput},
		"scalar":                {stdout: "42\n", want: ErrMalformedOutput},
		"duplicate key":         {stdout: `{"type":"thread.started","type":"turn.started"}` + "\n", want: ErrMalformedOutput},
		"escaped duplicate key": {stdout: `{"type":"thread.started","t\u0079pe":"turn.started"}` + "\n", want: ErrMalformedOutput},
		"trailing JSON":         {stdout: `{"type":"thread.started"}{}` + "\n", want: ErrMalformedOutput},
		"turn before thread":    {stdout: `{"type":"turn.started"}` + "\n" + valid, want: ErrMalformedOutput},
		"duplicate terminal":    {stdout: valid + lines[len(lines)-2] + "\n", want: ErrMalformedOutput},
		"after terminal":        {stdout: valid + `{"type":"turn.started"}` + "\n", want: ErrMalformedOutput},
		"missing terminal":      {stdout: strings.Join(lines[:5], "\n") + "\n", want: ErrMissingResult},
		"missing message":       {stdout: strings.Join(append(lines[:2], lines[5]), "\n") + "\n", want: ErrMissingResult},
		"unfinished item":       {stdout: strings.Replace(valid, `"item.completed","item":{"id":"tool-1"`, `"item.updated","item":{"id":"tool-1"`, 1), want: ErrMalformedOutput},
		"update without start":  {stdout: strings.Replace(valid, `"item.started"`, `"item.updated"`, 1), want: ErrMalformedOutput},
		"duplicate completed":   {stdout: strings.Replace(valid, lines[3]+"\n", lines[3]+"\n"+lines[3]+"\n", 1), want: ErrMalformedOutput},
		"changed item type":     {stdout: strings.Replace(valid, lines[3], strings.Replace(lines[3], "command_execution", "file_change", 1), 1), want: ErrMalformedOutput},
		"unknown event":         {stdout: strings.Replace(valid, "turn.started", "turn.future", 1), want: ErrMalformedOutput},
		"invalid usage":         {stdout: strings.Replace(valid, `"input_tokens":10`, `"input_tokens":-1`, 1), want: ErrMalformedOutput},
		"native failure":        {stdout: `{"type":"error","message":"private"}` + "\n", want: ErrNativeFailure},
		"output bound":          {stdout: valid, stderr: strings.Repeat("x", MaxOutputBytes-len(valid)+1), want: ErrMalformedOutput},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(test.stdout), []byte(test.stderr))
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

// Codex 0.154 reports configuration warnings, deprecation notices and model reroutes as completed error items.
// It also reports a retried transient backend error as a top-level error event.
// Both precede a normal turn.
func TestParseAcceptsReportedErrorsBeforeACompletedTurn(t *testing.T) {
	for name, test := range map[string]struct{ reported, before string }{
		"startup item":  {`{"type":"item.completed","item":{"id":"startup","type":"error","message":"private configuration warning"}}`, `{"type":"turn.started"}`},
		"warning item":  {`{"type":"item.completed","item":{"id":"warning-1","type":"error","message":"private deprecation notice"}}`, `{"type":"item.started"`},
		"retried event": {`{"type":"error","message":"private transient diagnostic"}`, `{"type":"item.started"`},
	} {
		t.Run(name, func(t *testing.T) {
			stream := strings.Replace(validStream(validResult), test.before, test.reported+"\n"+test.before, 1)
			parsed, err := Parse([]byte(stream), nil)
			if err != nil || parsed.Result.Outcome != "no_findings" {
				t.Fatalf("result = %#v, err = %v", parsed.Result, err)
			}
			if len(parsed.Events) != 4 {
				t.Fatalf("reported error was not recorded as one progress event: %#v", parsed.Events)
			}
			if strings.Contains(parsed.String(), "private") {
				t.Fatal("provider diagnostics escaped parsing")
			}
		})
	}
}

// An item type this parser does not know yet (collab_tool_call today) is
// recorded as an event, with its payload otherwise ignored, instead of
// rejecting the whole stream: the CLI source shows this set can grow ahead of
// a parser update.
func TestParseRecordsAnUnknownItemTypeInsteadOfRejectingTheStream(t *testing.T) {
	reported := `{"type":"item.started","item":{"id":"collab-1","type":"collab_tool_call","tool":"delegate","sender_thread_id":"thread-1","receiver_thread_ids":["thread-2"],"status":"in_progress"}}` + "\n" +
		`{"type":"item.completed","item":{"id":"collab-1","type":"collab_tool_call","tool":"delegate","sender_thread_id":"thread-1","receiver_thread_ids":["thread-2"],"status":"completed"}}` + "\n"
	stream := strings.Replace(validStream(validResult), `{"type":"item.started"`, reported+`{"type":"item.started"`, 1)
	parsed, err := Parse([]byte(stream), nil)
	if err != nil || parsed.Result.Outcome != "no_findings" {
		t.Fatalf("result = %#v, err = %v", parsed.Result, err)
	}
	found := false
	for _, event := range parsed.Events {
		if event.ItemID == "collab-1" && event.ItemType == "collab_tool_call" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown item type was not recorded: %#v", parsed.Events)
	}
}

// ThreadItemDetails' #[serde(flatten)] can legally emit a web_search item with
// id repeated at the same level (the item's own id plus the inner action's
// id); serde resolves that last value wins, and this parser now decodes exec
// stream lines the same way instead of rejecting the whole line.
func TestParseAcceptsAStreamEventWithADuplicateKeyLikeWebSearch(t *testing.T) {
	reported := `{"type":"item.completed","item":{"id":"item-1","type":"web_search","query":"redis eviction policy","id":"call-1","action":"search"}}` + "\n"
	stream := strings.Replace(validStream(validResult), `{"type":"item.started"`, reported+`{"type":"item.started"`, 1)
	parsed, err := Parse([]byte(stream), nil)
	if err != nil || parsed.Result.Outcome != "no_findings" {
		t.Fatalf("result = %#v, err = %v", parsed.Result, err)
	}
}

func TestParseClassifiesTheLastReportedErrorWhenNoResultFollows(t *testing.T) {
	prefix := `{"type":"thread.started","thread_id":"thread-1"}` + "\n" + `{"type":"turn.started"}` + "\n"
	warning := `{"type":"error","message":"private deprecation notice"}` + "\n"
	for name, test := range map[string]struct {
		stdout string
		reason FailureReason
	}{
		"truncated after a warning": {prefix + warning + `{"type":"error","message":"usage limit reached"}` + "\n", FailureRateLimited},
		"failed turn after a warning": {prefix + warning +
			`{"type":"turn.failed","error":{"code":"approval_required","message":"private"}}` + "\n", FailureApprovalRequired},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(test.stdout), nil)
			var failure *ClassifiedFailure
			if !errors.As(err, &failure) || failure.Reason != test.reason {
				t.Fatalf("failure = %#v (%v), want %q", failure, err, test.reason)
			}
		})
	}
}

func TestParseEnforcesLineAndEventLimits(t *testing.T) {
	if _, err := Parse([]byte(strings.Repeat(" ", MaxLineBytes+1)+"\n"), nil); !errors.Is(err, ErrMalformedOutput) {
		t.Fatalf("oversized line error = %v", err)
	}
	if _, err := Parse([]byte(strings.Repeat("{}\n", MaxEvents+1)), nil); !errors.Is(err, ErrMalformedOutput) {
		t.Fatalf("event limit error = %v", err)
	}
}

func TestParseClassifiesOnlyCompletedPreTurnErrorItemAsNativeFailure(t *testing.T) {
	prefix := `{"type":"thread.started","thread_id":"thread-1"}` + "\n"
	startupError := `{"type":"item.completed","item":{"id":"startup","type":"error","message":"private provider diagnostic"}}` + "\n"
	if _, err := Parse([]byte(prefix+startupError), nil); !errors.Is(err, ErrNativeFailure) {
		t.Fatalf("completed startup error = %v, want native failure", err)
	}

	for name, item := range map[string]string{
		"ordinary completed item": `{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"hello"}}`,
		"started error item":      `{"type":"item.started","item":{"id":"startup","type":"error"}}`,
		"updated error item":      `{"type":"item.updated","item":{"id":"startup","type":"error"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(prefix+item+"\n"), nil); !errors.Is(err, ErrMalformedOutput) {
				t.Fatalf("pre-turn item error = %v, want malformed output", err)
			}
		})
	}
}

func TestParseReturnsOnlyClosedClassifiedFailureReasons(t *testing.T) {
	for name, test := range map[string]struct {
		event  string
		reason FailureReason
	}{
		"approval code":      {`{"type":"error","code":"approval_required","message":"private"}` + "\n", FailureApprovalRequired},
		"rate limit message": {`{"type":"error","message":"usage limit reached"}` + "\n", FailureRateLimited},
		"nested model code":  {`{"type":"error","message":"{\"error\":{\"code\":\"model_not_found\",\"message\":\"private\"}}"}` + "\n", FailureModelUnavailable},
		// The bare provider message the pinned CLI actually writes (protocol/src/error.rs unwraps the envelope first), wrapped in the CLI's own status prefix and matched case-insensitively as a substring, not equality.
		"bare wrapped message": {`{"type":"error","message":"unexpected status 401 Unauthorized: Authentication Expired for this account"}` + "\n", FailureAuthExpired},
		// The provider relays its own rejection body, whose error object sits beside type and status.
		"relayed schema rejection": {`{"type":"turn.failed","error":{"message":"{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_json_schema\",\"message\":\"private schema diagnostic\",\"param\":\"text.format.schema\"},\"status\":400}"}}` + "\n", FailureInvalidOutputSchema},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(test.event), nil)
			var failure *ClassifiedFailure
			if !errors.As(err, &failure) || failure.Reason != test.reason || !errors.Is(err, ErrNativeFailure) {
				t.Fatalf("failure = %#v (%v), want %q", failure, err, test.reason)
			}
		})
	}

	for name, event := range map[string]string{
		"unknown code":    `{"type":"error","code":"future_code","message":"private"}` + "\n",
		"unknown message": `{"type":"error","message":"private provider diagnostic"}` + "\n",
		"loose envelope":  `{"type":"error","message":"{\"error\":{\"code\":\"approval_required\"},\"extra\":true}"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(event), nil)
			var failure *ClassifiedFailure
			if !errors.Is(err, ErrNativeFailure) || errors.As(err, &failure) {
				t.Fatalf("unclassified diagnostic became publishable: %#v (%v)", failure, err)
			}
		})
	}
}

func TestParseUsesLastCompletedMessageAndRequiresItToBeStructured(t *testing.T) {
	commentary := `{"type":"item.completed","item":{"id":"commentary","type":"agent_message","text":"private commentary"}}` + "\n"
	stream := strings.Replace(validStream(validResult), `{"type":"item.completed","item":{"id":"message-1"`, commentary+`{"type":"item.completed","item":{"id":"message-1"`, 1)
	parsed, err := Parse([]byte(stream), nil)
	if err != nil || parsed.Result.Summary != "Done." {
		t.Fatalf("last structured result failed: stream=%#v err=%v", parsed, err)
	}
	invalidFinal := strings.Replace(stream, `{"type":"turn.completed"`, `{"type":"item.completed","item":{"id":"last","type":"agent_message","text":"private final"}}`+"\n"+`{"type":"turn.completed"`, 1)
	if _, err := Parse([]byte(invalidFinal), nil); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("invalid final message error = %v", err)
	}
}

func TestParseRejectsInvalidStructuredResults(t *testing.T) {
	invalid := map[string]string{
		"duplicate key": `{"summary":"one","summary":"two","outcome":"no_findings","findings":[],"tests":[]}`,
		"extra field":   strings.Replace(noFindingsResult(), `"tests":[]`, `"tests":[],"secret":true`, 1),
		"missing finding": `{"summary":"Done.","outcome":"findings","charter_version":"1","findings":[],"coverage":` +
			coverageJSON(findingPath) + `,"verification_state":"none","tests":[]}`,
		"finding on clean": `{"summary":"Done.","outcome":"no_findings","charter_version":"1","findings":[` + findingJSON(findingPath, 1, 2) + `],"coverage":` +
			coverageJSON(findingPath) + `,"verification_state":"none","tests":[]}`,
		"parent path in evidence": `{"summary":"Done.","outcome":"findings","charter_version":"1","findings":[` + findingJSON("..", 1, 2) + `],"coverage":` +
			coverageJSON(findingPath) + `,"verification_state":"none","tests":[]}`,
		"unsafe path in evidence": `{"summary":"Done.","outcome":"findings","charter_version":"1","findings":[` + findingJSON("../secret", 1, 2) + `],"coverage":` +
			coverageJSON(findingPath) + `,"verification_state":"none","tests":[]}`,
		"fractional evidence line":              strings.Replace(findingsResult(), `"line_end":2,"line_start":1`, `"line_end":2,"line_start":1.5`, 1),
		"inverted evidence range":               strings.Replace(findingsResult(), `"line_end":2,"line_start":1`, `"line_end":2,"line_start":5`, 1),
		"evidence wrong snapshot length":        strings.Replace(findingsResult(), `"snapshot":"`+sampleHeadSHA+`"`, `"snapshot":"short"`, 1),
		"finding evidence empty":                strings.Replace(findingsResult(), `"evidence":[`+evidenceJSON(sampleHeadSHA, findingPath, 1, 2)+`]`, `"evidence":[]`, 1),
		"unexpected test key":                   `{"summary":"Done.","outcome":"no_findings","findings":[],"tests":[{"command":"go test ./...","status":"passed","summary":"ok","raw":"private"}]}`,
		"missing charter fields":                `{"summary":"Done.","outcome":"no_findings","findings":[],"tests":[]}`,
		"wrong verification state":              strings.Replace(noFindingsResult(), `"verification_state":"none"`, `"verification_state":"partial"`, 1),
		"charter forbidden on changes_proposed": strings.Replace(noFindingsResult(), `"outcome":"no_findings"`, `"outcome":"changes_proposed"`, 1),
		"coverage forbidden on needs_input":     `{"summary":"Done.","outcome":"needs_input","findings":[],"coverage":` + coverageJSON(findingPath) + `,"tests":[]}`,
		"question with severity": strings.Replace(
			noFindingsResult(),
			`"tests":[]`,
			`"questions":[{"topic":"t","question":"q","why_material":"w","severity":"medium"}],"tests":[]`,
			1,
		),
		"question array too long": strings.Replace(
			noFindingsResult(),
			`"tests":[]`,
			`"questions":[`+strings.TrimRight(strings.Repeat(`{"topic":"t","question":"q","why_material":"w"},`, MaxQuestions+1), ",")+`],"tests":[]`,
			1,
		),
		"unreviewed file without reason": strings.Replace(
			noFindingsResult(),
			coverageJSON(findingPath),
			`{"files":[{"path":"`+findingPath+`","status":"unreviewed"}],"context_gaps":[]}`,
			1,
		),
		"reviewed file with reason": strings.Replace(
			noFindingsResult(),
			coverageJSON(findingPath),
			`{"files":[{"path":"`+findingPath+`","status":"reviewed","reason":"read in full"}],"context_gaps":[]}`,
			1,
		),
	}
	for name, result := range invalid {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(validStream(result)), nil)
			if !errors.Is(err, ErrInvalidResult) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestParseAcceptsSchemaAuthorizedFindingsAndEnums(t *testing.T) {
	testsClause := `[{"command":"go test ./...","status":"error","summary":"infrastructure failed"}]`
	for _, outcome := range []string{"findings", "changes_proposed"} {
		result := `{"summary":"Done.","outcome":"` + outcome + `","findings":[` + findingJSON(findingPath, 1, 2) + `],"tests":` + testsClause + `}`
		if outcome == "findings" {
			result = `{"summary":"Done.","outcome":"findings","charter_version":"1","findings":[` + findingJSON(findingPath, 1, 2) + `],"coverage":` +
				coverageJSON(findingPath) + `,"verification_state":"none","tests":` + testsClause + `}`
		}
		stream, err := Parse([]byte(validStream(result)), nil)
		if err != nil {
			t.Fatalf("outcome %q rejected schema-authorized result: %v", outcome, err)
		}
		if stream.Result.Findings[0].Severity != "high" || stream.Result.Tests[0].Status != "error" {
			t.Fatalf("outcome %q changed result: %#v", outcome, stream.Result)
		}
	}
	// incomplete and needs_input forbid findings entirely. Only the tests enum is exercised here.
	for _, outcome := range []string{"incomplete", "needs_input"} {
		result := `{"summary":"Done.","outcome":"` + outcome + `","findings":[],"tests":` + testsClause + `}`
		stream, err := Parse([]byte(validStream(result)), nil)
		if err != nil {
			t.Fatalf("outcome %q rejected schema-authorized result: %v", outcome, err)
		}
		if stream.Result.Tests[0].Status != "error" {
			t.Fatalf("outcome %q changed result: %#v", outcome, stream.Result)
		}
	}
}

func TestParseRejectsFindingsOnIncompleteOrNeedsInput(t *testing.T) {
	for _, outcome := range []string{"incomplete", "needs_input"} {
		t.Run(outcome, func(t *testing.T) {
			result := `{"summary":"Done.","outcome":"` + outcome + `","findings":[` + findingJSON(findingPath, 1, 2) + `],"tests":[]}`
			if _, err := Parse([]byte(validStream(result)), nil); !errors.Is(err, ErrInvalidResult) {
				t.Fatalf("outcome %q accepted a non-empty findings array: %v", outcome, err)
			}
		})
	}
}

func TestParseAcceptsIncompleteWithOptionalCoverage(t *testing.T) {
	result := `{"summary":"Ran out of budget.","outcome":"incomplete","findings":[],"coverage":` + coverageJSON(findingPath) + `,"tests":[]}`
	stream, err := Parse([]byte(validStream(result)), nil)
	if err != nil {
		t.Fatalf("incomplete with coverage rejected: %v", err)
	}
	if stream.Result.Coverage == nil || len(stream.Result.Coverage.Files) != 1 {
		t.Fatalf("incomplete coverage not parsed: %#v", stream.Result)
	}

	bare := `{"summary":"Ran out of budget.","outcome":"incomplete","findings":[],"tests":[]}`
	stream, err = Parse([]byte(validStream(bare)), nil)
	if err != nil || stream.Result.Coverage != nil {
		t.Fatalf("incomplete without coverage = %#v, err=%v", stream.Result, err)
	}
}

func TestParseAcceptsQuestionsWithAndWithoutEvidence(t *testing.T) {
	withEvidence := strings.Replace(noFindingsResult(), `"tests":[]`,
		`"questions":[{"topic":"External policy","question":"Does it still apply?","why_material":"It gates the change.","evidence":[`+evidenceJSON(sampleHeadSHA, findingPath, 1, 2)+`]}],"tests":[]`, 1)
	stream, err := Parse([]byte(validStream(withEvidence)), nil)
	if err != nil || len(stream.Result.Questions) != 1 || len(stream.Result.Questions[0].Evidence) != 1 {
		t.Fatalf("question with evidence rejected: %#v err=%v", stream.Result, err)
	}

	withoutEvidence := strings.Replace(noFindingsResult(), `"tests":[]`,
		`"questions":[{"topic":"External policy","question":"Does it still apply?","why_material":"It gates the change."}],"tests":[]`, 1)
	stream, err = Parse([]byte(validStream(withoutEvidence)), nil)
	if err != nil || len(stream.Result.Questions) != 1 || stream.Result.Questions[0].Evidence != nil {
		t.Fatalf("question without evidence rejected: %#v err=%v", stream.Result, err)
	}
}

func TestParseRejectsEveryC0ControlInEvidencePath(t *testing.T) {
	for control := rune(0); control <= 0x1f; control++ {
		name := "src/a" + string(control) + "b.go"
		result := `{"summary":"Done.","outcome":"findings","charter_version":"1","findings":[` + findingJSON(name, 1, 2) + `],"coverage":` +
			coverageJSON(findingPath) + `,"verification_state":"none","tests":[]}`
		if _, err := Parse([]byte(validStream(result)), nil); !errors.Is(err, ErrInvalidResult) {
			t.Fatalf("control U+%04X path error = %v", control, err)
		}
	}
}

// strictFindingJSON is the wire shape the strict output schema forces: anchor is
// always present because strict mode cannot omit a property.
func strictFindingJSON(anchor string) string {
	return `{"category":"correctness","severity":"high","relation":"introduced","scenario":"A scenario.","consequence":"A consequence.","action":"An action.","explanation":"An explanation.","evidence":[` +
		evidenceJSON(sampleHeadSHA, findingPath, 1, 2) + `],"anchor":` + anchor + `}`
}

func strictCoverageJSON(reason string) string {
	return `{"files":[{"path":"` + findingPath + `","status":"reviewed","reason":` + reason + `}],"context_gaps":[]}`
}

func TestParseNormalizesStrictSchemaNullsToAbsentFields(t *testing.T) {
	clean := `{"summary":"Done.","outcome":"no_findings","charter_version":"1","findings":[],"questions":null,"coverage":` +
		strictCoverageJSON("null") + `,"verification_state":"none","tests":[]}`
	stream, err := Parse([]byte(validStream(clean)), nil)
	if err != nil || stream.Result.Questions != nil || stream.Result.Coverage == nil || stream.Result.Coverage.Files[0].Reason != "" {
		t.Fatalf("nullable charter fields not normalized: %#v err=%v", stream.Result, err)
	}

	withNullAnchor := `{"summary":"Done.","outcome":"findings","charter_version":"1","findings":[` + strictFindingJSON("null") +
		`],"questions":null,"coverage":` + strictCoverageJSON("null") + `,"verification_state":"none","tests":[]}`
	stream, err = Parse([]byte(validStream(withNullAnchor)), nil)
	if err != nil || stream.Result.Findings[0].Anchor != nil {
		t.Fatalf("null anchor not normalized: %#v err=%v", stream.Result, err)
	}

	anchor := `{"path":"` + findingPath + `","line":2,"side":"RIGHT"}`
	withAnchor := strings.Replace(withNullAnchor, `"anchor":null`, `"anchor":`+anchor, 1)
	stream, err = Parse([]byte(validStream(withAnchor)), nil)
	if err != nil || stream.Result.Findings[0].Anchor == nil || stream.Result.Findings[0].Anchor.Side != "RIGHT" {
		t.Fatalf("real anchor dropped with the nulls: %#v err=%v", stream.Result, err)
	}

	// Strict mode makes the model emit all four charter keys on every outcome, so
	// each outcome that forbids them depends on the nulls normalizing away.
	for _, outcome := range []string{"changes_proposed", "incomplete", "needs_input"} {
		findings := "[]"
		if outcome == "changes_proposed" {
			findings = `[` + strictFindingJSON("null") + `]`
		}
		result := `{"summary":"Done.","outcome":"` + outcome + `","charter_version":null,"findings":` + findings +
			`,"questions":null,"coverage":null,"verification_state":null,"tests":[]}`
		t.Run(outcome, func(t *testing.T) {
			parsed, err := Parse([]byte(validStream(result)), nil)
			if err != nil || parsed.Result.CharterVersion != "" || parsed.Result.VerificationState != "" || parsed.Result.Coverage != nil || parsed.Result.Questions != nil {
				t.Fatalf("outcome-forbidden fields not normalized away: %#v err=%v", parsed.Result, err)
			}
		})
	}

	for name, result := range map[string]string{
		"null summary":  strings.Replace(clean, `"summary":"Done."`, `"summary":null`, 1),
		"null findings": strings.Replace(clean, `"findings":[]`, `"findings":null`, 1),
		"null tests":    strings.Replace(clean, `"tests":[]`, `"tests":null`, 1),
		"null evidence": strings.Replace(withNullAnchor, `"evidence":[`+evidenceJSON(sampleHeadSHA, findingPath, 1, 2)+`]`, `"evidence":null`, 1),
		"null in array": strings.Replace(clean, `"findings":[]`, `"findings":[null]`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(validStream(result)), nil); !errors.Is(err, ErrInvalidResult) {
				t.Fatalf("null in a required position was accepted: %v", err)
			}
		})
	}
}

// The strict output schema cannot carry minLength, maxLength or the array counts
// it once declared, so every bound below is now enforced only here.
func TestParseEnforcesLimitsTheOutputSchemaCannotDeclare(t *testing.T) {
	text := func(length int) string { return strings.Repeat("x", length) }
	repeat := func(entry string, count int) string {
		return strings.TrimSuffix(strings.Repeat(entry+",", count), ",")
	}
	question := `{"topic":"t","question":"q","why_material":"w"}`
	coverageFile := `{"path":"` + findingPath + `","status":"reviewed"}`

	for name, result := range map[string]string{
		"empty summary":         strings.Replace(noFindingsResult(), `"summary":"Done."`, `"summary":""`, 1),
		"summary too long":      strings.Replace(noFindingsResult(), `"summary":"Done."`, `"summary":"`+text(16_385)+`"`, 1),
		"empty scenario":        strings.Replace(findingsResult(), `"scenario":"A scenario."`, `"scenario":""`, 1),
		"scenario too long":     strings.Replace(findingsResult(), `"scenario":"A scenario."`, `"scenario":"`+text(2001)+`"`, 1),
		"explanation too long":  strings.Replace(findingsResult(), `"explanation":"An explanation."`, `"explanation":"`+text(8193)+`"`, 1),
		"charter version long":  strings.Replace(noFindingsResult(), `"charter_version":"1"`, `"charter_version":"`+text(9)+`"`, 1),
		"snapshot not a SHA":    strings.Replace(findingsResult(), `"snapshot":"`+sampleHeadSHA+`"`, `"snapshot":"`+text(40)+`"`, 1),
		"evidence line below 1": strings.Replace(findingsResult(), `"line_start":1`, `"line_start":0`, 1),
		"evidence path long":    strings.Replace(findingsResult(), `"path":"`+findingPath+`"`, `"path":"`+text(1025)+`"`, 1),
		"question topic long":   strings.Replace(noFindingsResult(), `"tests":[]`, `"questions":[{"topic":"`+text(201)+`","question":"q","why_material":"w"}],"tests":[]`, 1),
		"question text long":    strings.Replace(noFindingsResult(), `"tests":[]`, `"questions":[{"topic":"t","question":"`+text(2001)+`","why_material":"w"}],"tests":[]`, 1),
		"coverage reason long":  strings.Replace(noFindingsResult(), coverageJSON(findingPath), `{"files":[{"path":"`+findingPath+`","status":"unreviewed","reason":"`+text(501)+`"}],"context_gaps":[]}`, 1),
		"context gap long":      strings.Replace(noFindingsResult(), coverageJSON(findingPath), `{"files":[`+coverageFile+`],"context_gaps":["`+text(501)+`"]}`, 1),
		"test command long":     strings.Replace(noFindingsResult(), `"tests":[]`, `"tests":[{"command":"`+text(2049)+`","status":"not_run","summary":"s"}]`, 1),
		"test summary long":     strings.Replace(noFindingsResult(), `"tests":[]`, `"tests":[{"command":"c","status":"not_run","summary":"`+text(4097)+`"}]`, 1),
		"evidence beyond five":  strings.Replace(findingsResult(), `"evidence":[`+evidenceJSON(sampleHeadSHA, findingPath, 1, 2)+`]`, `"evidence":[`+repeat(evidenceJSON(sampleHeadSHA, findingPath, 1, 2), MaxFindingEvidence+1)+`]`, 1),
		"findings beyond limit": strings.Replace(findingsResult(), `"findings":[`+findingJSON(findingPath, 1, 2)+`]`, `"findings":[`+repeat(findingJSON(findingPath, 1, 2), MaxFindings+1)+`]`, 1),
		"questions beyond limit": strings.Replace(noFindingsResult(), `"tests":[]`,
			`"questions":[`+repeat(question, MaxQuestions+1)+`],"tests":[]`, 1),
		"coverage files beyond limit": strings.Replace(noFindingsResult(), coverageJSON(findingPath),
			`{"files":[`+repeat(coverageFile, MaxCoverageFiles+1)+`],"context_gaps":[]}`, 1),
		"context gaps beyond limit": strings.Replace(noFindingsResult(), coverageJSON(findingPath),
			`{"files":[`+coverageFile+`],"context_gaps":[`+repeat(`"gap"`, MaxCoverageContextGaps+1)+`]}`, 1),
		"tests beyond limit": strings.Replace(noFindingsResult(), `"tests":[]`,
			`"tests":[`+repeat(`{"command":"c","status":"not_run","summary":"s"}`, 101)+`]`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(validStream(result)), nil); !errors.Is(err, ErrInvalidResult) {
				t.Fatalf("limit was not enforced after parsing: %v", err)
			}
		})
	}
}

func TestParseClassifiesBackendSchemaRejectionHoweverTheCLIWrapsIt(t *testing.T) {
	nested := `"error":{"message":"Invalid schema for response_format 'codex_output_schema'.","type":"invalid_request_error","param":"text.format.schema","code":"invalid_json_schema"}`
	body := `{` + nested + `}`
	// The relayed body carries the error object beside the envelope's type and status.
	relayed := `{"type":"error",` + nested + `,"status":400}`
	for name, message := range map[string]string{
		"bare body":              body,
		"prefixed body":          "invalid request: " + body,
		"body with notes":        body + " (retrying)",
		"relayed body":           relayed,
		"prefixed relayed body":  "invalid request: " + relayed,
		"relayed body with note": relayed + " (retrying)",
	} {
		t.Run(name, func(t *testing.T) {
			for _, event := range []string{
				`{"type":"error","message":` + quote(message) + `}` + "\n",
				`{"type":"turn.failed","error":{"message":` + quote(message) + `}}` + "\n",
			} {
				_, err := Parse([]byte(event), nil)
				var failure *ClassifiedFailure
				if !errors.As(err, &failure) || failure.Reason != FailureInvalidOutputSchema {
					t.Fatalf("schema rejection = %#v (%v)", failure, err)
				}
			}
		})
	}
}
