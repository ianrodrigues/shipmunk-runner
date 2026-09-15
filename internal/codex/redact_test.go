package codex

import (
	"encoding/json"
	"sort"
	"testing"
)

// redactJSONValues walks a decoded JSON value (as produced by
// encoding/json.Unmarshal into any) and returns a copy with every scalar
// passed through rewrite, which receives the dotted path to that value
// ("item.arguments.path", "findings.0.evidence.0.snapshot") and returns the
// replacement. No map key and no array element is ever added or removed:
// a map is only ever rebuilt key by key and a slice only ever rebuilt
// element by element, so the shape codex-rs actually emitted survives
// redaction exactly, PROVIDED it survived encoding/json.Unmarshal first.
// It does not: encoding/json collapses a duplicate object key to its last
// value while decoding, before this function ever sees the result, so a
// duplicate-key line (a web_search item, ThreadItemDetails' flatten) loses
// that shape here. TestRedactJSONValuesCollapsesDuplicateKeysBeforeRedaction
// demonstrates this. Redact such a line by hand instead, the way
// tolerant-shapes.jsonl's web_search line was built, and verify the
// duplicate survived with a byte-level diff against the source line. This
// is the tool used to build and refresh the recordings under
// testdata/stream/<codex version>/ (see that directory's README and
// docs/compatibility/codex.md's version-bump checklist) for every other
// (non-duplicate-key) value.
func redactJSONValues(value any, path string, rewrite func(path string, value any) any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, member := range typed {
			out[key] = redactJSONValues(member, joinPath(path, key), rewrite)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, member := range typed {
			out[index] = redactJSONValues(member, path+"[]", rewrite)
		}
		return out
	default:
		return rewrite(path, value)
	}
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// collectKeyPaths lists every map key path present in a decoded JSON value,
// so two structures can be compared for exactly the same keys regardless of
// their (redacted) values.
func collectKeyPaths(value any, path string, out map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, member := range typed {
			keyPath := joinPath(path, key)
			out[keyPath] = true
			collectKeyPaths(member, keyPath, out)
		}
	case []any:
		for _, member := range typed {
			collectKeyPaths(member, path+"[]", out)
		}
	}
}

func TestRedactJSONValuesPreservesEveryKey(t *testing.T) {
	const sample = `{
		"type": "item.completed",
		"item": {
			"id": "item_1",
			"type": "mcp_tool_call",
			"arguments": {"snapshot": "workspace", "path": "app/Models/User.php"},
			"result": {"content": [{"type": "text", "text": "line one\nline two"}], "structured_content": null},
			"error": null,
			"status": "completed"
		}
	}`
	var decoded any
	if err := json.Unmarshal([]byte(sample), &decoded); err != nil {
		t.Fatal(err)
	}

	redacted := redactJSONValues(decoded, "", func(path string, value any) any {
		if path == "item.arguments.snapshot" {
			return "[redacted:" + path + "]"
		}
		return value
	})

	original, after := map[string]bool{}, map[string]bool{}
	collectKeyPaths(decoded, "", original)
	collectKeyPaths(redacted, "", after)
	if len(original) == 0 {
		t.Fatal("sample fixture carries no keys to compare")
	}
	if !sameKeySet(original, after) {
		t.Fatalf("redaction changed the key set:\noriginal = %v\nredacted = %v", sortedKeysOf(original), sortedKeysOf(after))
	}

	if got := redacted.(map[string]any)["item"].(map[string]any)["arguments"].(map[string]any)["snapshot"]; got != "[redacted:item.arguments.snapshot]" {
		t.Fatalf("value was not rewritten: %v", got)
	}
	if got := redacted.(map[string]any)["item"].(map[string]any)["status"]; got != "completed" {
		t.Fatalf("non-target value was unexpectedly rewritten: %v", got)
	}
}

func sameKeySet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if !b[key] {
			return false
		}
	}
	return true
}

func sortedKeysOf(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// This is the known limitation documented on redactJSONValues: a duplicate
// object key never reaches it, because encoding/json.Unmarshal has already
// collapsed the object to one value per key by the time this function walks
// it. A line shaped like this must be redacted by hand, and the duplicate's
// survival checked with a byte-level diff against the raw source line, not
// by trusting this helper or TestRedactJSONValuesPreservesEveryKey.
func TestRedactJSONValuesCollapsesDuplicateKeysBeforeRedaction(t *testing.T) {
	const duplicateKeyLine = `{"id":"item-1","type":"web_search","id":"call-1"}`
	var decoded any
	if err := json.Unmarshal([]byte(duplicateKeyLine), &decoded); err != nil {
		t.Fatal(err)
	}
	object, ok := decoded.(map[string]any)
	if !ok || len(object) != 2 {
		t.Fatalf("json.Unmarshal did not collapse the duplicate key as expected: %#v", decoded)
	}
	if object["id"] != "call-1" {
		t.Fatalf("expected the last value to win, got %#v", object["id"])
	}
}
