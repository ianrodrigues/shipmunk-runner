package codex

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

const (
	outputSchemaPath   = "../../runner/containers/codex-result.schema.json"
	resultContractPath = "../protocol/contracts/v1/result.schema.json"
)

// strictKeywords is the keyword set OpenAI's structured-outputs documentation
// names as supported. The backend rejects the whole schema on anything else, so
// the shipped file may not reintroduce minLength, maxLength or a conditional.
var strictKeywords = map[string]bool{
	"type": true, "enum": true, "description": true,
	"properties": true, "required": true, "additionalProperties": true,
	"items": true, "anyOf": true, "$ref": true,
	"pattern": true, "format": true,
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true,
	"minItems": true, "maxItems": true,
}

// Documented strict-mode ceilings; the walk below accumulates against them.
const (
	maxSchemaNestingDepth   = 10
	maxSchemaProperties     = 5000
	maxSchemaEnumValues     = 1000
	maxSchemaNameCharacters = 120_000
)

func loadSchema(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func resolveSchemaRef(t *testing.T, root, node map[string]any) map[string]any {
	t.Helper()
	ref, ok := node["$ref"].(string)
	if !ok {
		return node
	}
	defs, ok := root["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no $defs for %q", ref)
	}
	target, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
	if !ok {
		t.Fatalf("unresolved $ref %q", ref)
	}
	return target
}

// nullableSchema reports whether a node can actually hold null, in either of the
// two forms the documentation sanctions: a union type or an anyOf branch typed
// null. type and enum are ANDed, so a union type whose enum omits null cannot
// hold null at all and does not count.
func nullableSchema(node map[string]any) bool {
	for _, branch := range branchesOf(node) {
		if branch["type"] == "null" {
			return true
		}
	}
	types, ok := node["type"].([]any)
	if !ok {
		return false
	}
	nullTyped := false
	for _, name := range types {
		nullTyped = nullTyped || name == "null"
	}
	return nullTyped && enumAdmitsNull(node)
}

func enumAdmitsNull(node map[string]any) bool {
	values, bounded := node["enum"].([]any)
	if !bounded {
		return true
	}
	for _, value := range values {
		if value == nil {
			return true
		}
	}
	return false
}

func branchesOf(node map[string]any) []map[string]any {
	raw, ok := node["anyOf"].([]any)
	if !ok {
		return nil
	}
	branches := make([]map[string]any, 0, len(raw))
	for _, branch := range raw {
		object, ok := branch.(map[string]any)
		if ok {
			branches = append(branches, object)
		}
	}
	return branches
}

// valueBranch strips a nullable wrapper down to the one branch that carries the
// value, so a paired walk compares like with like.
func valueBranch(t *testing.T, node map[string]any) map[string]any {
	t.Helper()
	branches := branchesOf(node)
	if len(branches) == 0 {
		return node
	}
	var value []map[string]any
	for _, branch := range branches {
		if branch["type"] != "null" {
			value = append(value, branch)
		}
	}
	if len(value) != 1 {
		t.Fatalf("anyOf must wrap exactly one value branch, got %d", len(value))
	}
	return value[0]
}

func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func requiredSet(t *testing.T, node map[string]any, pointer string) map[string]bool {
	t.Helper()
	raw, ok := node["required"].([]any)
	if !ok {
		t.Fatalf("%s: object has no required list", pointer)
	}
	required := make(map[string]bool, len(raw))
	for _, name := range raw {
		text, ok := name.(string)
		if !ok {
			t.Fatalf("%s: required entry is not a string", pointer)
		}
		required[text] = true
	}
	return required
}

type schemaBudget struct {
	properties, enumValues, nameCharacters int
}

func TestOutputSchemaSatisfiesStrictStructuredOutputs(t *testing.T) {
	root := loadSchema(t, outputSchemaPath)
	if root["type"] != "object" || root["anyOf"] != nil {
		t.Fatalf("root must be a plain object schema: %v", root["type"])
	}
	budget := &schemaBudget{}
	for name, def := range root["$defs"].(map[string]any) {
		budget.nameCharacters += len(name)
		walkStrictSchema(t, def, "#/$defs/"+name, 1, budget)
	}
	walkStrictSchema(t, root, "#", 1, budget)
	if budget.properties > maxSchemaProperties || budget.enumValues > maxSchemaEnumValues || budget.nameCharacters > maxSchemaNameCharacters {
		t.Fatalf("schema exceeds a documented ceiling: %+v", *budget)
	}
}

func walkStrictSchema(t *testing.T, node any, pointer string, depth int, budget *schemaBudget) {
	t.Helper()
	object, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("%s: schema node is not an object", pointer)
	}
	if depth > maxSchemaNestingDepth {
		t.Fatalf("%s: nesting depth %d exceeds %d", pointer, depth, maxSchemaNestingDepth)
	}
	for _, keyword := range sortedKeys(object) {
		if keyword == "$defs" && pointer == "#" {
			continue
		}
		if !strictKeywords[keyword] {
			t.Fatalf("%s: keyword %q is outside the strict subset", pointer, keyword)
		}
	}
	if values, ok := object["enum"].([]any); ok {
		budget.enumValues += len(values)
		for _, value := range values {
			if text, ok := value.(string); ok {
				budget.nameCharacters += len(text)
			}
		}
	}
	if types, ok := object["type"].([]any); ok && !enumAdmitsNull(object) {
		for _, name := range types {
			if name == "null" {
				t.Fatalf("%s: type admits null but enum does not, so null is unsatisfiable", pointer)
			}
		}
	}
	for _, branch := range branchesOf(object) {
		walkStrictSchema(t, branch, pointer+"/anyOf", depth, budget)
	}
	if items, exists := object["items"]; exists {
		walkStrictSchema(t, items, pointer+"/items", depth, budget)
	}
	properties, exists := object["properties"].(map[string]any)
	if !exists {
		if _, hasRequired := object["required"]; hasRequired {
			t.Fatalf("%s: required without properties", pointer)
		}
		return
	}
	if object["additionalProperties"] != false {
		t.Fatalf("%s: additionalProperties must be false", pointer)
	}
	required := requiredSet(t, object, pointer)
	if len(required) != len(properties) {
		t.Fatalf("%s: required lists %d of %d properties", pointer, len(required), len(properties))
	}
	for _, name := range sortedKeys(properties) {
		if !required[name] {
			t.Fatalf("%s: property %q is missing from required", pointer, name)
		}
		budget.properties++
		budget.nameCharacters += len(name)
		walkStrictSchema(t, properties[name], pointer+"/"+name, depth+1, budget)
	}
}

// The contract stays the authority on which fields are optional. Strict mode
// cannot omit a property. Every contract-optional field must therefore be
// nullable in the model-facing schema, then normalized back to absent on parse.
func TestOutputSchemaMirrorsResultContractRequiredness(t *testing.T) {
	model := loadSchema(t, outputSchemaPath)
	contract := loadSchema(t, resultContractPath)
	// normalizeExecution derives these from the claim, never from the model.
	envelope := map[string]bool{"protocol_version": true, "run_id": true, "attempt_id": true, "fence": true, "patch_artifact": true, "usage": true}
	comparePairedSchemas(t, pairedRoots{model: model, contract: contract}, model, contract, "#", envelope)
}

type pairedRoots struct{ model, contract map[string]any }

func comparePairedSchemas(t *testing.T, roots pairedRoots, model, contract map[string]any, pointer string, omitted map[string]bool) {
	t.Helper()
	model = valueBranch(t, resolveSchemaRef(t, roots.model, model))
	contract = resolveSchemaRef(t, roots.contract, contract)
	contractProperties, ok := contract["properties"].(map[string]any)
	if !ok {
		if items, exists := contract["items"]; exists {
			modelItems, exists := model["items"]
			if !exists {
				t.Fatalf("%s: model schema has no items", pointer)
			}
			comparePairedSchemas(t, roots, modelItems.(map[string]any), items.(map[string]any), pointer+"/items", nil)
		}
		return
	}
	modelProperties, ok := model["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s: model schema has no properties", pointer)
	}
	contractRequired := requiredSet(t, contract, pointer)
	for _, name := range sortedKeys(contractProperties) {
		if omitted[name] {
			continue
		}
		property, exists := modelProperties[name]
		if !exists {
			t.Fatalf("%s: model schema is missing contract property %q", pointer, name)
		}
		if nullableSchema(property.(map[string]any)) == contractRequired[name] {
			t.Fatalf("%s/%s: nullability does not match contract requiredness", pointer, name)
		}
		comparePairedSchemas(t, roots, property.(map[string]any), contractProperties[name].(map[string]any), pointer+"/"+name, nil)
	}
	for _, name := range sortedKeys(modelProperties) {
		if _, exists := contractProperties[name]; !exists {
			t.Fatalf("%s: model schema adds property %q the contract does not define", pointer, name)
		}
	}
}
