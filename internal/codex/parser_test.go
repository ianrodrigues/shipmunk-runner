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

const validResult = `{"summary":"Done.","outcome":"no_findings","findings":[],"tests":[]}`

func TestParseValidCompleteStream(t *testing.T) {
	stream, err := Parse([]byte(validStream(validResult)), []byte("private provider diagnostics"))
	if err != nil {
		t.Fatal(err)
	}
	if stream.ThreadID != "thread-1" || stream.Result.Summary != "Done." || stream.Result.Outcome != "no_findings" || stream.Usage == nil || stream.Usage.CachedInputTokens == nil || *stream.Usage.CachedInputTokens != 3 {
		t.Fatalf("unexpected stream: %#v", stream)
	}
	if len(stream.Events) != 3 || stream.Events[0] != (Event{Type: "item.started", ItemID: "tool-1", ItemType: "command_execution"}) {
		t.Fatalf("unexpected sanitized events: %#v", stream.Events)
	}
	if strings.Contains(stream.String(), "private") {
		t.Fatal("provider diagnostics escaped parsing")
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
		"unknown item":          {stdout: strings.Replace(valid, "command_execution", "untrusted_tool", 1), want: ErrMalformedOutput},
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
		"duplicate key":       `{"summary":"one","summary":"two","outcome":"no_findings","findings":[],"tests":[]}`,
		"extra field":         `{"summary":"Done.","outcome":"no_findings","findings":[],"tests":[],"secret":true}`,
		"missing finding":     `{"summary":"Done.","outcome":"findings","findings":[],"tests":[]}`,
		"finding on clean":    `{"summary":"Done.","outcome":"no_findings","findings":[{"path":"a","line":1,"side":"RIGHT","severity":"high","explanation":"x","evidence":"y"}],"tests":[]}`,
		"parent path":         `{"summary":"Done.","outcome":"findings","findings":[{"path":"..","line":1,"side":"RIGHT","severity":"high","explanation":"x","evidence":"y"}],"tests":[]}`,
		"unsafe path":         `{"summary":"Done.","outcome":"findings","findings":[{"path":"../secret","line":1,"side":"RIGHT","severity":"high","explanation":"x","evidence":"y"}],"tests":[]}`,
		"fractional line":     `{"summary":"Done.","outcome":"findings","findings":[{"path":"a.go","line":1.5,"side":"RIGHT","severity":"high","explanation":"x","evidence":"y"}],"tests":[]}`,
		"unexpected test key": `{"summary":"Done.","outcome":"no_findings","findings":[],"tests":[{"command":"go test ./...","status":"passed","summary":"ok","raw":"private"}]}`,
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
	finding := `{"path":"internal/codex/parser.go","line":1,"side":"RIGHT","severity":"info","explanation":"context","evidence":"evidence"}`
	for _, outcome := range []string{"findings", "changes_proposed", "incomplete", "needs_input"} {
		result := `{"summary":"Done.","outcome":"` + outcome + `","findings":[` + finding + `],"tests":[{"command":"go test ./...","status":"error","summary":"infrastructure failed"}]}`
		stream, err := Parse([]byte(validStream(result)), nil)
		if err != nil {
			t.Fatalf("outcome %q rejected schema-authorized result: %v", outcome, err)
		}
		if stream.Result.Findings[0].Severity != "info" || stream.Result.Tests[0].Status != "error" {
			t.Fatalf("outcome %q changed result: %#v", outcome, stream.Result)
		}
	}
}

func TestParseRejectsEveryC0ControlInFindingPath(t *testing.T) {
	for control := rune(0); control <= 0x1f; control++ {
		name := "src/a" + string(control) + "b.go"
		result := `{"summary":"Done.","outcome":"findings","findings":[{"path":` + quote(name) + `,"line":1,"side":"RIGHT","severity":"high","explanation":"x","evidence":"y"}],"tests":[]}`
		if _, err := Parse([]byte(validStream(result)), nil); !errors.Is(err, ErrInvalidResult) {
			t.Fatalf("control U+%04X path error = %v", control, err)
		}
	}
}
