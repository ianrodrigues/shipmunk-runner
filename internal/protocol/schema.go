package protocol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ValidateFixture loads the pinned server schema named by contract and checks
// one document. Schemas remain in the pinned contracts submodule; this runner
// never makes a mutable copy of server protocol authority.
func ValidateFixture(contract, schemaDirectory string, raw []byte) error {
	schemaRaw, err := os.ReadFile(filepath.Join(schemaDirectory, contract+".schema.json"))
	if err != nil {
		return fmt.Errorf("read pinned %s schema: %w", contract, err)
	}
	schemaValue, err := Decode(schemaRaw, 128*1024)
	if err != nil {
		return err
	}
	schema, err := object(schemaValue)
	if err != nil {
		return err
	}
	limit := documentLimit(contract)
	document, err := Decode(raw, limit)
	if err != nil {
		return err
	}
	return validate(schema, document, "$")
}

// ValidateManifest applies the complete pinned manifest contract. Decode has
// already rejected duplicate keys before callers construct the map, while
// re-encoding preserves null and empty-array distinctions for schema checks.
func ValidateManifest(manifest map[string]any) error {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode manifest for validation: %w", err)
	}
	return ValidateFixture("manifest", pinnedSchemaDirectory(), raw)
}

func documentLimit(contract string) int {
	switch contract {
	case "manifest", "run-input":
		return ManifestMaxBytes
	case "result", "patch-artifact":
		return ResultMaxBytes
	case "worker-event":
		return WorkerEventMaxBytes
	case "event-batch":
		return EventBatchMaxBytes
	default:
		return 128 * 1024
	}
}

func pinnedSchemaDirectory() string {
	directory, err := os.Getwd()
	if err != nil {
		return "contracts-source/contracts/v1"
	}
	for {
		candidate := filepath.Join(directory, "contracts-source", "contracts", "v1")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "contracts-source/contracts/v1"
		}
		directory = parent
	}
}

func validate(schema map[string]any, value any, path string) error {
	if alternatives, ok := schema["anyOf"].([]any); ok {
		for _, alternative := range alternatives {
			candidate, ok := alternative.(map[string]any)
			if ok && validate(candidate, value, path) == nil {
				return nil
			}
		}
		return fmt.Errorf("%s did not match an allowed schema", path)
	}
	if constant, ok := schema["const"]; ok && !sameJSON(constant, value) {
		return fmt.Errorf("%s did not match required value", path)
	}
	if enum, ok := schema["enum"].([]any); ok {
		for _, candidate := range enum {
			if sameJSON(candidate, value) {
				goto enumOK
			}
		}
		return fmt.Errorf("%s was not an allowed value", path)
	}
enumOK:
	if kind, ok := schema["type"].(string); ok && !hasType(kind, value) {
		return fmt.Errorf("%s must be %s", path, kind)
	}
	if err := stringRules(schema, value, path); err != nil {
		return err
	}
	if err := numberRules(schema, value, path); err != nil {
		return err
	}
	if err := arrayRules(schema, value, path); err != nil {
		return err
	}
	if err := objectRules(schema, value, path); err != nil {
		return err
	}
	if rules, ok := schema["allOf"].([]any); ok {
		for _, rule := range rules {
			if child, ok := rule.(map[string]any); ok {
				if err := conditionalRules(child, value, path); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func conditionalRules(rule map[string]any, value any, path string) error {
	condition, hasCondition := rule["if"].(map[string]any)
	then, hasThen := rule["then"].(map[string]any)
	if hasCondition && hasThen && validate(condition, value, path) == nil {
		return validate(then, value, path)
	}
	return validate(rule, value, path)
}

func hasType(kind string, value any) bool {
	switch kind {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		_, err := positiveOrZero(value)
		return err == nil
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return false
	}
}

func stringRules(schema map[string]any, value any, path string) error {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	length := utf8.RuneCountInString(text)
	if minimum, ok := integerSchema(schema["minLength"]); ok && int64(length) < minimum {
		return fmt.Errorf("%s is shorter than allowed", path)
	}
	if maximum, ok := integerSchema(schema["maxLength"]); ok && int64(length) > maximum {
		return fmt.Errorf("%s is longer than allowed", path)
	}
	if pattern, ok := schema["pattern"].(string); ok && !matchesPattern(pattern, text) {
		return fmt.Errorf("%s did not match required pattern", path)
	}
	if format, ok := schema["format"].(string); ok && format == "date-time" {
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || !strings.HasSuffix(text, "Z") || parsed.Location() != time.UTC {
			return fmt.Errorf("%s is not a UTC RFC3339 timestamp", path)
		}
	}
	return nil
}

func numberRules(schema map[string]any, value any, path string) error {
	number, err := positiveOrZero(value)
	if err != nil {
		return nil
	}
	if minimum, ok := integerSchema(schema["minimum"]); ok && number < minimum {
		return fmt.Errorf("%s is below minimum", path)
	}
	if maximum, ok := integerSchema(schema["maximum"]); ok && number > maximum {
		return fmt.Errorf("%s is above maximum", path)
	}
	return nil
}

func arrayRules(schema map[string]any, value any, path string) error {
	array, ok := value.([]any)
	if !ok {
		return nil
	}
	if minimum, ok := integerSchema(schema["minItems"]); ok && int64(len(array)) < minimum {
		return fmt.Errorf("%s has too few items", path)
	}
	if maximum, ok := integerSchema(schema["maxItems"]); ok && int64(len(array)) > maximum {
		return fmt.Errorf("%s has too many items", path)
	}
	if items, ok := schema["items"].(map[string]any); ok {
		for index, item := range array {
			if err := validate(items, item, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func objectRules(schema map[string]any, value any, path string) error {
	data, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	properties, _ := schema["properties"].(map[string]any)
	if required, ok := schema["required"].([]any); ok {
		for _, field := range required {
			name, ok := field.(string)
			if !ok {
				continue
			}
			if _, exists := data[name]; !exists {
				return fmt.Errorf("%s.%s is required", path, name)
			}
		}
	}
	if falseValue, ok := schema["additionalProperties"].(bool); ok && !falseValue {
		for name := range data {
			if _, known := properties[name]; !known {
				return fmt.Errorf("%s.%s is not allowed", path, name)
			}
		}
	}
	for name, child := range properties {
		if field, exists := data[name]; exists {
			childSchema, ok := child.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid pinned schema property %s", name)
			}
			if err := validate(childSchema, field, path+"."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

func matchesPattern(pattern, value string) bool {
	// Go's regexp engine deliberately does not implement lookarounds. The
	// pinned schemas use one lookahead-heavy pattern for safe relative paths;
	// evaluate that policy directly rather than weakening the schema boundary.
	if strings.Contains(pattern, "(?!/") && strings.Contains(pattern, "\\\\") {
		if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.Contains(value, "\x00") {
			return false
		}
		for _, segment := range strings.Split(value, "/") {
			if segment == ".." {
				return false
			}
		}
		for _, rune := range value {
			if rune < 0x20 {
				return false
			}
		}
		return true
	}
	compiled, err := regexp.Compile(pattern)
	return err == nil && compiled.MatchString(value)
}

func positiveOrZero(value any) (int64, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("not JSON number")
	}
	integer, err := number.Int64()
	if err != nil {
		return 0, err
	}
	return integer, nil
}
func integerSchema(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	integer, err := number.Int64()
	return integer, err == nil
}
func sameJSON(left, right any) bool {
	return strings.TrimSpace(mustJSON(left)) == strings.TrimSpace(mustJSON(right))
}
func mustJSON(value any) string {
	bytes, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(bytes)
}
