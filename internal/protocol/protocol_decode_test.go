package protocol

import (
	"strings"
	"testing"
)

func TestDecodeRejectsMalformedSurrogateEscapes(t *testing.T) {
	for name, raw := range map[string]string{
		"unpaired high surrogate":  `{"text":"\ud800"}`,
		"unpaired low surrogate":   `{"text":"\udc00"}`,
		"high followed by non-low": `{"text":"\ud800\u0041"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(raw), 1024); err == nil {
				t.Fatal("accepted malformed UTF-16 surrogate escape")
			}
		})
	}

	value, err := Decode([]byte(`{"text":"\ud83d\ude80"}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	data, err := object(value)
	if err != nil {
		t.Fatal(err)
	}
	if data["text"] != "🚀" {
		t.Fatalf("decoded paired surrogate as %#v", data["text"])
	}
}

func TestDecodeEnforcesPHPCompatibleDepthAndByteLimits(t *testing.T) {
	withinDepth := strings.Repeat("[", MaxJSONDepth-1) + "0" + strings.Repeat("]", MaxJSONDepth-1)
	if _, err := Decode([]byte(withinDepth), len(withinDepth)); err != nil {
		t.Fatalf("rejected PHP-compatible maximum depth: %v", err)
	}

	overDepth := strings.Repeat("[", MaxJSONDepth) + "0" + strings.Repeat("]", MaxJSONDepth)
	if _, err := Decode([]byte(overDepth), len(overDepth)); err == nil {
		t.Fatal("accepted input one container deeper than PHP's depth-64 boundary")
	}
	if _, err := Decode([]byte(`{}`), 1); err == nil {
		t.Fatal("accepted input above its byte limit")
	}
}
