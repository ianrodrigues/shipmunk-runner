package codex

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestParseCorpus replays the recorded and constructed Codex 0.154.0 streams
// under testdata/stream/0.154.0 (see that directory's README for which is
// which) so a version bump's replacement corpus is checked by the same test.
func TestParseCorpus(t *testing.T) {
	tests := map[string]struct {
		file    string
		wantErr func(error) bool
	}{
		"success":                      {"success.jsonl", func(err error) bool { return err == nil }},
		"turn failed":                  {"turn-failed.jsonl", func(err error) bool { return errors.Is(err, ErrNativeFailure) }},
		"transient error then success": {"transient-error-then-success.jsonl", func(err error) bool { return err == nil }},
		"tolerant shapes":              {"tolerant-shapes.jsonl", func(err error) bool { return err == nil }},
		"split findings":               {"split-findings.jsonl", func(err error) bool { return err == nil }},
		"missing finding title":        {"missing-finding-title.jsonl", func(err error) bool { return errors.Is(err, ErrInvalidResult) }},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "stream", "0.154.0", test.file))
			if err != nil {
				t.Fatal(err)
			}
			stream, err := Parse(raw, nil)
			if !test.wantErr(err) {
				t.Fatalf("%s: err = %v", test.file, err)
			}
			if err == nil && stream.ThreadID == "" {
				t.Fatalf("%s: parsed stream has no thread id", test.file)
			}
		})
	}
}

func TestParseCorpusSuccessCarriesTheDocumentedFindings(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "stream", "0.154.0", "success.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := Parse(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stream.Result.Outcome != "findings" || len(stream.Result.Findings) != 2 {
		t.Fatalf("result = %#v", stream.Result)
	}
	if stream.Usage == nil || stream.Usage.InputTokens == nil || *stream.Usage.InputTokens != 65430 {
		t.Fatalf("usage = %#v", stream.Usage)
	}
}

func TestParseCorpusTransientErrorIsRecordedNotFatal(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "stream", "0.154.0", "transient-error-then-success.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := Parse(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stream.Result.Outcome != "no_findings" {
		t.Fatalf("result = %#v", stream.Result)
	}
	found := false
	for _, event := range stream.Events {
		if event.ItemType == "error" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reported error was not recorded as a progress event: %#v", stream.Events)
	}
}

func TestParseCorpusTurnFailedClassifiesTheBareMessage(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "stream", "0.154.0", "turn-failed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Parse(raw, nil)
	var failure *ClassifiedFailure
	if !errors.As(err, &failure) || failure.Reason != FailureAuthExpired {
		t.Fatalf("failure = %#v (%v)", failure, err)
	}
}

// TestParseCorpusTolerantShapesExercisesEveryAdditiveTolerance is the
// regression anchor for every shape this PR started tolerating: an unknown
// item type (collab_tool_call), a duplicate key inside a flattened item
// (web_search), an item.updated event, a reasoning item, an unrecognized
// turn usage key, and cached_input_tokens above input_tokens.
func TestParseCorpusTolerantShapesExercisesEveryAdditiveTolerance(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "stream", "0.154.0", "tolerant-shapes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := Parse(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stream.Result.Outcome != "no_findings" {
		t.Fatalf("result = %#v", stream.Result)
	}
	var sawCollabToolCall, sawWebSearch, sawItemUpdated, sawReasoning bool
	for _, event := range stream.Events {
		sawCollabToolCall = sawCollabToolCall || event.ItemType == "collab_tool_call"
		sawWebSearch = sawWebSearch || event.ItemType == "web_search"
		sawItemUpdated = sawItemUpdated || event.Type == "item.updated"
		sawReasoning = sawReasoning || event.ItemType == "reasoning"
	}
	if !sawCollabToolCall || !sawWebSearch || !sawItemUpdated || !sawReasoning {
		t.Fatalf("missing tolerated shapes: collab=%v websearch=%v updated=%v reasoning=%v (%#v)", sawCollabToolCall, sawWebSearch, sawItemUpdated, sawReasoning, stream.Events)
	}
	if stream.Usage == nil || stream.Usage.CachedInputTokens == nil || stream.Usage.InputTokens == nil || *stream.Usage.CachedInputTokens <= *stream.Usage.InputTokens {
		t.Fatalf("cached_input_tokens over input_tokens was not preserved: %#v", stream.Usage)
	}
}

func TestParseCorpusSplitFindingsKeepsTwoIndependentFixesSeparate(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "stream", "0.154.0", "split-findings.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := Parse(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stream.Result.Findings) != 2 {
		t.Fatalf("result = %#v", stream.Result)
	}
	first, second := stream.Result.Findings[0], stream.Result.Findings[1]
	if first.Title == "" || second.Title == "" || first.Title == second.Title {
		t.Fatalf("split findings do not carry distinct titles: %q and %q", first.Title, second.Title)
	}
	if first.Action == second.Action {
		t.Fatalf("split findings share one action: %q", first.Action)
	}
}
