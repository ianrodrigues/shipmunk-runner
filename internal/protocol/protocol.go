// Package protocol implements the versioned runner control-plane boundary.
//
// It intentionally uses json.Number and a duplicate-key-aware decoder. The
// control plane treats numeric identifiers as integers, and decoding through
// float64 would silently change valid identities above 2^53.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	Version               = "1.0"
	HTTPTimeoutSeconds    = 5
	InputArtifactMaxBytes = 100 * 1024 * 1024
	ManifestMaxBytes      = 128 * 1024
	ResultMaxBytes        = 2 * 1024 * 1024
	WorkerEventMaxBytes   = 64 * 1024
	EventBatchMaxBytes    = 6_553_600
	MaxSafeInteger        = int64(9_007_199_254_740_991)
)

// Decode strictly decodes one JSON value. It rejects invalid UTF-8, duplicate
// object keys at every depth, and any non-whitespace bytes after that value.
func Decode(raw []byte, maxBytes int) (any, error) {
	if len(raw) > maxBytes {
		return nil, fmt.Errorf("protocol document exceeded its byte limit")
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("protocol document is not valid UTF-8")
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeValue(decoder)
	if err != nil {
		return nil, fmt.Errorf("invalid protocol JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("protocol document contains trailing JSON")
		}
		return nil, fmt.Errorf("invalid protocol JSON suffix: %w", err)
	}
	return value, nil
}

func decodeValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}

	switch token := token.(type) {
	case json.Delim:
		switch token {
		case '{':
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
				if _, exists := object[name]; exists {
					return nil, fmt.Errorf("duplicate object key %q", name)
				}
				value, err := decodeValue(decoder)
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
			array := make([]any, 0)
			for decoder.More() {
				value, err := decodeValue(decoder)
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
			return nil, fmt.Errorf("unexpected JSON delimiter %q", token)
		}
	default:
		return token, nil
	}
}

func object(value any) (map[string]any, error) {
	result, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("protocol document must be an object")
	}
	return result, nil
}

func stringField(data map[string]any, key string) (string, error) {
	value, ok := data[key].(string)
	if !ok || value == "" {
		return "", fmt.Errorf("protocol field %q must be a non-empty string", key)
	}
	return value, nil
}
