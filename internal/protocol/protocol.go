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
	"strconv"
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
	MaxJSONDepth          = 64
)

// Decode strictly decodes one JSON value. It rejects invalid UTF-8, duplicate
// object keys at every depth, and any non-whitespace bytes after that value.
func Decode(raw []byte, maxBytes int) (any, error) {
	if maxBytes < 0 || len(raw) > maxBytes {
		return nil, fmt.Errorf("protocol document exceeded its byte limit")
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("protocol document is not valid UTF-8")
	}
	if err := validateUnicodeEscapes(raw); err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return nil, fmt.Errorf("invalid protocol JSON")
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("protocol document contains trailing JSON")
		}
		return nil, fmt.Errorf("invalid protocol JSON suffix")
	}
	return value, nil
}

func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}

	switch token := token.(type) {
	case json.Delim:
		switch token {
		case '{':
			if depth >= MaxJSONDepth-1 {
				return nil, fmt.Errorf("protocol document exceeded its nesting limit")
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
				if _, exists := object[name]; exists {
					return nil, fmt.Errorf("protocol object contains a duplicate key")
				}
				value, err := decodeValue(decoder, depth+1)
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
			if depth >= MaxJSONDepth-1 {
				return nil, fmt.Errorf("protocol document exceeded its nesting limit")
			}
			array := make([]any, 0)
			for decoder.More() {
				value, err := decodeValue(decoder, depth+1)
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

// validateUnicodeEscapes rejects invalid UTF-16 surrogate escapes before
// encoding/json can replace an unpaired surrogate with U+FFFD.
func validateUnicodeEscapes(raw []byte) error {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '"' {
			continue
		}

		for index++; index < len(raw); index++ {
			switch raw[index] {
			case '"':
				goto nextString
			case '\\':
				if index+1 >= len(raw) {
					return fmt.Errorf("protocol document contains an invalid Unicode escape")
				}
				escape := raw[index+1]
				if escape != 'u' {
					index++
					continue
				}
				codeUnit, ok := unicodeCodeUnit(raw[index+2 : min(index+6, len(raw))])
				if !ok {
					return fmt.Errorf("protocol document contains an invalid Unicode escape")
				}
				if codeUnit >= 0xDC00 && codeUnit <= 0xDFFF {
					return fmt.Errorf("protocol document contains an unpaired Unicode surrogate")
				}
				if codeUnit >= 0xD800 && codeUnit <= 0xDBFF {
					pairStart := index + 6
					if pairStart+6 > len(raw) || raw[pairStart] != '\\' || raw[pairStart+1] != 'u' {
						return fmt.Errorf("protocol document contains an unpaired Unicode surrogate")
					}
					lowSurrogate, ok := unicodeCodeUnit(raw[pairStart+2 : pairStart+6])
					if !ok || lowSurrogate < 0xDC00 || lowSurrogate > 0xDFFF {
						return fmt.Errorf("protocol document contains an unpaired Unicode surrogate")
					}
					index = pairStart + 5
					continue
				}
				index += 5
			case '\n', '\r':
				return fmt.Errorf("protocol document contains invalid JSON string data")
			}
		}

		return fmt.Errorf("protocol document contains an unterminated JSON string")
	nextString:
	}

	return nil
}

func unicodeCodeUnit(digits []byte) (uint16, bool) {
	if len(digits) != 4 {
		return 0, false
	}
	value, err := strconv.ParseUint(string(digits), 16, 16)
	return uint16(value), err == nil
}
