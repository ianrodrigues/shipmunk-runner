package protocol

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxSchemaBytes = 128 * 1024

const safeRelativePathPattern = `^(?!/)(?!.*(?:^|/)\.\.(?:/|$))(?!.*\\)[^\x00-\x1f]+$`

var supportedSchemaKeywords = map[string]struct{}{
	"$schema": {}, "$id": {}, "$ref": {}, "$defs": {},
	"type": {}, "const": {}, "enum": {},
	"anyOf": {}, "allOf": {}, "if": {}, "then": {}, "else": {},
	"properties": {}, "required": {}, "additionalProperties": {}, "items": {},
	"minLength": {}, "maxLength": {}, "pattern": {}, "format": {},
	"minimum": {}, "maximum": {}, "minItems": {}, "maxItems": {},
}

// refPrefix is the only $ref shape resolved: a same-document pointer into the
// root schema's own $defs. Anything else is rejected, not ignored.
const refPrefix = "#/$defs/"

func resolveRef(root map[string]any, ref string) (any, error) {
	if root == nil || !strings.HasPrefix(ref, refPrefix) {
		return nil, fmt.Errorf("unsupported $ref target %q", ref)
	}
	name := ref[len(refPrefix):]
	if name == "" || strings.ContainsAny(name, "/") {
		return nil, fmt.Errorf("unsupported $ref target %q", ref)
	}
	defs, ok := root["$defs"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema has no $defs for %q", ref)
	}
	target, exists := defs[name]
	if !exists {
		return nil, fmt.Errorf("unknown $ref target %q", ref)
	}
	return target, nil
}

// Validate checks a document against the runner's embedded contract schema,
// pinned at build time and never fetched at runtime.
func Validate(contract string, raw []byte) error {
	schema, err := schemaBytes(contract)
	if err != nil {
		return err
	}
	_, err = decodeValidated(contract, schema, raw)
	return err
}

// ValidateFixture loads a pinned schema from a fixture directory for one
// document; runtime callers should use Validate to keep schemas embedded.
func ValidateFixture(contract, schemaDirectory string, raw []byte) error {
	schema, err := os.ReadFile(filepath.Join(schemaDirectory, contract+".schema.json"))
	if err != nil {
		return fmt.Errorf("read protocol contract schema: %w", err)
	}
	_, err = decodeValidated(contract, schema, raw)
	return err
}

func decodeValidated(contract string, schemaRaw, raw []byte) (any, error) {
	limit, err := documentLimit(contract)
	if err != nil {
		return nil, err
	}
	if len(schemaRaw) > maxSchemaBytes {
		return nil, fmt.Errorf("protocol contract schema exceeded its byte limit")
	}
	schemaValue, err := Decode(schemaRaw, maxSchemaBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid protocol contract schema")
	}
	schema, err := object(schemaValue)
	if err != nil {
		return nil, fmt.Errorf("protocol contract schema must be an object")
	}
	if err := validateSchema(schema, "$schema", schema); err != nil {
		return nil, fmt.Errorf("unsupported protocol contract schema: %w", err)
	}
	document, err := Decode(raw, limit)
	if err != nil {
		return nil, err
	}
	if err := validateNode(schema, document, "$", 0, schema); err != nil {
		return nil, err
	}
	if contract == "result" {
		if err := assertEvidenceRanges(document); err != nil {
			return nil, err
		}
	}
	return document, nil
}

// assertEvidenceRanges rejects line_end below line_start. JSON Schema cannot
// compare sibling properties, so this mirrors the server's ProtocolValidator.
func assertEvidenceRanges(document any) error {
	root, ok := document.(map[string]any)
	if !ok {
		return nil
	}
	var citing []any
	if findings, ok := root["findings"].([]any); ok {
		citing = append(citing, findings...)
	}
	if questions, ok := root["questions"].([]any); ok {
		citing = append(citing, questions...)
	}
	for _, item := range citing {
		itemObject, ok := item.(map[string]any)
		if !ok {
			continue
		}
		evidence, ok := itemObject["evidence"].([]any)
		if !ok {
			continue
		}
		for _, entry := range evidence {
			evidenceObject, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			start, startErr := numberValue(evidenceObject["line_start"])
			end, endErr := numberValue(evidenceObject["line_end"])
			if startErr == nil && endErr == nil && end.Cmp(start) < 0 {
				return fmt.Errorf("result evidence line range is invalid")
			}
		}
	}
	return nil
}

func documentLimit(contract string) (int, error) {
	switch contract {
	case "manifest", "run-input":
		return ManifestMaxBytes, nil
	case "result", "patch-artifact":
		return ResultMaxBytes, nil
	case "worker-event":
		return WorkerEventMaxBytes, nil
	case "event-batch":
		return EventBatchMaxBytes, nil
	default:
		return 0, fmt.Errorf("unknown protocol contract")
	}
}

func validateSchema(node any, path string, root map[string]any) error {
	switch schema := node.(type) {
	case bool:
		return nil
	case map[string]any:
		for keyword := range schema {
			if _, supported := supportedSchemaKeywords[keyword]; !supported {
				return fmt.Errorf("%s uses an unsupported keyword", path)
			}
		}
		if refValue, exists := schema["$ref"]; exists {
			ref, ok := refValue.(string)
			if !ok {
				return fmt.Errorf("%s has an invalid $ref keyword", path)
			}
			if _, err := resolveRef(root, ref); err != nil {
				return fmt.Errorf("%s has an invalid $ref keyword: %w", path, err)
			}
		}
		if defsValue, exists := schema["$defs"]; exists {
			defs, ok := defsValue.(map[string]any)
			if !ok {
				return fmt.Errorf("%s has an invalid $defs keyword", path)
			}
			for name, def := range defs {
				if err := validateSchema(def, path+".$defs."+name, root); err != nil {
					return err
				}
			}
		}
		for _, keyword := range []string{"$schema", "$id", "pattern", "format"} {
			if value, exists := schema[keyword]; exists {
				if _, ok := value.(string); !ok {
					return fmt.Errorf("%s has an invalid %s keyword", path, keyword)
				}
			}
		}
		if value, exists := schema["format"]; exists && value != "date-time" {
			return fmt.Errorf("%s uses an unsupported format", path)
		}
		if pattern, exists := schema["pattern"]; exists {
			value := pattern.(string)
			if _, err := regexp.Compile(value); err != nil && value != safeRelativePathPattern {
				return fmt.Errorf("%s has an unsupported pattern", path)
			}
		}
		if value, exists := schema["type"]; exists {
			if err := validateSchemaTypes(value, path); err != nil {
				return err
			}
		}
		for _, keyword := range []string{"minLength", "maxLength", "minItems", "maxItems"} {
			if value, exists := schema[keyword]; exists {
				bound, ok := integerSchema(value)
				if !ok || bound < 0 {
					return fmt.Errorf("%s has an invalid %s keyword", path, keyword)
				}
			}
		}
		for _, keyword := range []string{"minimum", "maximum"} {
			if value, exists := schema[keyword]; exists {
				if _, err := numberValue(value); err != nil {
					return fmt.Errorf("%s has an invalid %s keyword", path, keyword)
				}
			}
		}
		if value, exists := schema["enum"]; exists {
			enumeration, ok := value.([]any)
			if !ok || len(enumeration) == 0 {
				return fmt.Errorf("%s has an invalid enum keyword", path)
			}
		}
		for _, keyword := range []string{"anyOf", "allOf"} {
			if value, exists := schema[keyword]; exists {
				alternatives, ok := value.([]any)
				if !ok || len(alternatives) == 0 {
					return fmt.Errorf("%s has an invalid %s keyword", path, keyword)
				}
				for _, alternative := range alternatives {
					if err := validateSchema(alternative, path+"."+keyword, root); err != nil {
						return err
					}
				}
			}
		}
		for _, keyword := range []string{"if", "then", "else", "items", "additionalProperties"} {
			if value, exists := schema[keyword]; exists {
				if err := validateSchema(value, path+"."+keyword, root); err != nil {
					return err
				}
			}
		}
		if propertiesValue, exists := schema["properties"]; exists {
			properties, ok := propertiesValue.(map[string]any)
			if !ok {
				return fmt.Errorf("%s has an invalid properties keyword", path)
			}
			for name, property := range properties {
				if err := validateSchema(property, path+".properties."+name, root); err != nil {
					return err
				}
			}
		}
		if requiredValue, exists := schema["required"]; exists {
			required, ok := requiredValue.([]any)
			if !ok {
				return fmt.Errorf("%s has an invalid required keyword", path)
			}
			for _, property := range required {
				if _, ok := property.(string); !ok {
					return fmt.Errorf("%s has an invalid required keyword", path)
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("%s is not a schema object", path)
	}
}

func validateSchemaTypes(value any, path string) error {
	valid := map[string]struct{}{
		"object": {}, "array": {}, "string": {}, "integer": {}, "number": {}, "boolean": {}, "null": {},
	}
	check := func(kind any) bool {
		name, ok := kind.(string)
		if !ok {
			return false
		}
		_, ok = valid[name]
		return ok
	}
	switch types := value.(type) {
	case string:
		if !check(types) {
			return fmt.Errorf("%s has an unsupported type", path)
		}
	case []any:
		if len(types) == 0 {
			return fmt.Errorf("%s has an invalid type keyword", path)
		}
		seen := make(map[string]struct{}, len(types))
		for _, kind := range types {
			if !check(kind) {
				return fmt.Errorf("%s has an unsupported type", path)
			}
			name := kind.(string)
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("%s has an invalid type keyword", path)
			}
			seen[name] = struct{}{}
		}
	default:
		return fmt.Errorf("%s has an invalid type keyword", path)
	}
	return nil
}

func validateNode(node any, value any, path string, depth int, root map[string]any) error {
	if depth > MaxJSONDepth {
		return fmt.Errorf("%s exceeds the protocol schema depth limit", path)
	}
	switch schema := node.(type) {
	case bool:
		if schema {
			return nil
		}
		return fmt.Errorf("%s is not allowed", path)
	case map[string]any:
		if ref, ok := schema["$ref"].(string); ok {
			resolved, err := resolveRef(root, ref)
			if err != nil {
				return fmt.Errorf("%s has an unresolved $ref: %w", path, err)
			}
			if err := validateNode(resolved, value, path, depth+1, root); err != nil {
				return err
			}
		}
		if constant, ok := schema["const"]; ok && !sameJSON(constant, value) {
			return fmt.Errorf("%s does not match its required value", path)
		}
		if enumeration, ok := schema["enum"].([]any); ok {
			matched := false
			for _, candidate := range enumeration {
				if sameJSON(candidate, value) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("%s is not an allowed value", path)
			}
		}
		if typeValue, ok := schema["type"]; ok && !hasType(typeValue, value) {
			return fmt.Errorf("%s has the wrong JSON type", path)
		}
		if alternatives, ok := schema["anyOf"].([]any); ok {
			matched := false
			for _, alternative := range alternatives {
				if validateNode(alternative, value, path, depth+1, root) == nil {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("%s does not match an allowed schema", path)
			}
		}
		if alternatives, ok := schema["allOf"].([]any); ok {
			for _, alternative := range alternatives {
				if err := validateNode(alternative, value, path, depth+1, root); err != nil {
					return err
				}
			}
		}
		if condition, ok := schema["if"]; ok {
			selected := "then"
			if validateNode(condition, value, path, depth+1, root) != nil {
				selected = "else"
			}
			if branch, exists := schema[selected]; exists {
				if err := validateNode(branch, value, path, depth+1, root); err != nil {
					return err
				}
			}
		}
		if err := stringRules(schema, value, path); err != nil {
			return err
		}
		if err := numberRules(schema, value, path); err != nil {
			return err
		}
		if err := arrayRules(schema, value, path, depth, root); err != nil {
			return err
		}
		if err := objectRules(schema, value, path, depth, root); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("%s has an invalid schema", path)
	}
}

func hasType(typeValue any, value any) bool {
	switch expected := typeValue.(type) {
	case string:
		return hasSingleType(expected, value)
	case []any:
		for _, candidate := range expected {
			kind, ok := candidate.(string)
			if ok && hasSingleType(kind, value) {
				return true
			}
		}
	}
	return false
}

func hasSingleType(kind string, value any) bool {
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
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		rational, err := numberValue(number)
		return err == nil && rational.IsInt()
	case "number":
		_, ok := value.(json.Number)
		return ok
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
		return fmt.Errorf("%s does not match its required pattern", path)
	}
	if format, ok := schema["format"].(string); ok && format == "date-time" {
		if _, err := utcTime(text); err != nil {
			return fmt.Errorf("%s is not a UTC RFC3339 timestamp", path)
		}
	}
	return nil
}

func numberRules(schema map[string]any, value any, path string) error {
	number, ok := value.(json.Number)
	if !ok {
		return nil
	}
	rational, err := numberValue(number)
	if err != nil {
		return fmt.Errorf("%s has an unsupported numeric value", path)
	}
	if minimum, exists := schema["minimum"]; exists {
		bound, err := numberValue(minimum)
		if err != nil {
			return fmt.Errorf("invalid pinned minimum")
		}
		if rational.Cmp(bound) < 0 {
			return fmt.Errorf("%s is below its minimum", path)
		}
	}
	if maximum, exists := schema["maximum"]; exists {
		bound, err := numberValue(maximum)
		if err != nil {
			return fmt.Errorf("invalid pinned maximum")
		}
		if rational.Cmp(bound) > 0 {
			return fmt.Errorf("%s is above its maximum", path)
		}
	}
	return nil
}

func arrayRules(schema map[string]any, value any, path string, depth int, root map[string]any) error {
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
	if items, ok := schema["items"]; ok {
		for index, item := range array {
			if err := validateNode(items, item, fmt.Sprintf("%s[%d]", path, index), depth+1, root); err != nil {
				return err
			}
		}
	}
	return nil
}

func objectRules(schema map[string]any, value any, path string, depth int, root map[string]any) error {
	data, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	properties, _ := schema["properties"].(map[string]any)
	if required, ok := schema["required"].([]any); ok {
		for _, field := range required {
			name := field.(string)
			if _, exists := data[name]; !exists {
				return fmt.Errorf("%s.%s is required", path, name)
			}
		}
	}
	additional, hasAdditional := schema["additionalProperties"]
	for name, field := range data {
		if propertySchema, known := properties[name]; known {
			if err := validateNode(propertySchema, field, path+"."+name, depth+1, root); err != nil {
				return err
			}
			continue
		}
		if !hasAdditional {
			continue
		}
		switch additional {
		case false:
			return fmt.Errorf("%s contains an unsupported property", path)
		case true:
			continue
		default:
			if err := validateNode(additional, field, path, depth+1, root); err != nil {
				return err
			}
		}
	}
	return nil
}

func matchesPattern(pattern, value string) bool {
	if pattern == safeRelativePathPattern {
		return safeRelativePath(value)
	}
	compiled, err := regexp.Compile(pattern)
	return err == nil && compiled.MatchString(value)
}

func safeRelativePath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return false
		}
	}
	for _, character := range value {
		if character < 0x20 {
			return false
		}
	}
	return true
}

func numberValue(value any) (*big.Rat, error) {
	number, ok := value.(json.Number)
	if !ok {
		return nil, fmt.Errorf("not a JSON number")
	}
	text := number.String()
	if len(text) == 0 || len(text) > 128 {
		return nil, fmt.Errorf("outside supported numeric bounds")
	}
	if exponentIndex := strings.IndexAny(text, "eE"); exponentIndex >= 0 {
		exponent, err := strconv.ParseInt(text[exponentIndex+1:], 10, 32)
		if err != nil || exponent < -128 || exponent > 128 {
			return nil, fmt.Errorf("outside supported numeric bounds")
		}
	}
	rational, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, fmt.Errorf("invalid JSON number")
	}
	return rational, nil
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
	if leftNumber, ok := left.(json.Number); ok {
		rightNumber, ok := right.(json.Number)
		if !ok {
			return false
		}
		leftRational, leftErr := numberValue(leftNumber)
		rightRational, rightErr := numberValue(rightNumber)
		return leftErr == nil && rightErr == nil && leftRational.Cmp(rightRational) == 0
	}
	switch left := left.(type) {
	case nil:
		return right == nil
	case bool:
		right, ok := right.(bool)
		return ok && left == right
	case string:
		right, ok := right.(string)
		return ok && left == right
	case []any:
		right, ok := right.([]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for index := range left {
			if !sameJSON(left[index], right[index]) {
				return false
			}
		}
		return true
	case map[string]any:
		right, ok := right.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for key, value := range left {
			other, exists := right[key]
			if !exists || !sameJSON(value, other) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}
