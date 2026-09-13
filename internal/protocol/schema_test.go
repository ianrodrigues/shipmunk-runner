package protocol

import (
	"strings"
	"testing"
)

func TestValidateUsesEmbeddedContractSchema(t *testing.T) {
	if err := Validate("manifest", []byte(validManifest)); err != nil {
		t.Fatalf("rejected complete manifest: %v", err)
	}
	if err := Validate("unknown-contract", []byte(`{}`)); err == nil {
		t.Fatal("accepted an unknown contract name")
	}

	invalid := strings.Replace(validManifest, `"network":"disabled"`, `"network":"open"`, 1)
	if err := Validate("manifest", []byte(invalid)); err == nil {
		t.Fatal("embedded schema accepted an unsupported nested enum value")
	}
}

func TestSchemaValidationFailsClosedAndAppliesEveryAssertion(t *testing.T) {
	t.Run("sibling type still applies after anyOf", func(t *testing.T) {
		schema := `{"type":"integer","anyOf":[{"type":"string"},{"type":"null"}]}`
		if _, err := decodeValidated("manifest", []byte(schema), []byte(`"accepted by anyOf"`)); err == nil {
			t.Fatal("anyOf success skipped its sibling type assertion")
		}
	})

	t.Run("unsupported nested keyword is rejected before instance evaluation", func(t *testing.T) {
		schema := `{"anyOf":[{"type":"string","not":{"const":"forbidden"}},{"type":"null"}]}`
		if _, err := decodeValidated("manifest", []byte(schema), []byte(`null`)); err == nil {
			t.Fatal("silently ignored an unsupported schema keyword")
		}
	})
}

func TestSchemaValidationEnforcesNestedAllowListsAndConditionals(t *testing.T) {
	schema := `{
		"type":"object",
		"properties":{
			"outcome":{"enum":["empty","work"]},
			"findings":{"type":"array","items":{"type":"object","properties":{"severity":{"enum":["low","high"]}},"required":["severity"],"additionalProperties":false},"maxItems":3},
			"settings":{"type":"object","properties":{"network":{"enum":["disabled","restricted"]}},"required":["network"],"additionalProperties":false}
		},
		"required":["outcome","findings","settings"],
		"additionalProperties":false,
		"allOf":[{"if":{"properties":{"outcome":{"const":"empty"}}},"then":{"properties":{"findings":{"maxItems":0}}}}]
	}`

	for name, raw := range map[string]string{
		"valid conditional branch":     `{"outcome":"empty","findings":[],"settings":{"network":"disabled"}}`,
		"valid non-conditional branch": `{"outcome":"work","findings":[{"severity":"high"}],"settings":{"network":"restricted"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeValidated("manifest", []byte(schema), []byte(raw)); err != nil {
				t.Fatalf("rejected valid document: %v", err)
			}
		})
	}

	for name, raw := range map[string]string{
		"conditional minimum":       `{"outcome":"empty","findings":[{"severity":"low"}],"settings":{"network":"disabled"}}`,
		"nested property allowlist": `{"outcome":"work","findings":[{"severity":"low","comment":"extra"}],"settings":{"network":"disabled"}}`,
		"nested enum":               `{"outcome":"work","findings":[],"settings":{"network":"open"}}`,
		"nested required field":     `{"outcome":"work","findings":[],"settings":{}}`,
		"root allowlist":            `{"outcome":"work","findings":[],"settings":{"network":"disabled"},"surprise":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeValidated("manifest", []byte(schema), []byte(raw)); err == nil {
				t.Fatal("accepted a document that violates the schema")
			}
		})
	}
}

func TestSchemaValidationDistinguishesRequiredNullAndNullableFields(t *testing.T) {
	schema := `{"type":"object","properties":{"nullable":{"anyOf":[{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false},{"type":"null"}]},"plain":{"type":"string"}},"required":["nullable","plain"],"additionalProperties":false}`

	valid := `{"nullable":null,"plain":"ok"}`
	if _, err := decodeValidated("manifest", []byte(schema), []byte(valid)); err != nil {
		t.Fatalf("rejected explicit nullable value: %v", err)
	}
	for name, raw := range map[string]string{
		"missing nullable property":  `{"plain":"ok"}`,
		"missing object member":      `{"nullable":{},"plain":"ok"}`,
		"null in non-nullable field": `{"nullable":null,"plain":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeValidated("manifest", []byte(schema), []byte(raw)); err == nil {
				t.Fatal("accepted a missing or invalid null value")
			}
		})
	}
}

func TestSchemaValidationUsesExactNumericSemantics(t *testing.T) {
	constSchema := `{"const":9007199254740991}`
	if _, err := decodeValidated("manifest", []byte(constSchema), []byte(`9007199254740991.0`)); err != nil {
		t.Fatalf("treated numerically equal JSON numbers as different: %v", err)
	}

	integerSchema := `{"type":"integer","minimum":9007199254740991,"maximum":9007199254740991}`
	if _, err := decodeValidated("manifest", []byte(integerSchema), []byte(`9007199254740991.0`)); err != nil {
		t.Fatalf("rejected an exact integer representation: %v", err)
	}
	if _, err := decodeValidated("manifest", []byte(integerSchema), []byte(`9007199254740992`)); err == nil {
		t.Fatal("accepted a number above the exact safe identity bound")
	}
	if _, err := decodeValidated("manifest", []byte(`{"type":"integer"}`), []byte(`1.5`)); err == nil {
		t.Fatal("accepted a fractional value as an integer")
	}
	for _, raw := range []string{`1e1000000000`, `1e-1000000000`} {
		if _, err := decodeValidated("manifest", []byte(`{"type":"integer"}`), []byte(raw)); err == nil {
			t.Fatalf("accepted a number outside supported exponent bounds: %s", raw)
		}
	}
}

func TestSchemaValidationCountsUnicodeCharactersAndChecksUTC(t *testing.T) {
	textSchema := `{"type":"string","maxLength":2}`
	if _, err := decodeValidated("manifest", []byte(textSchema), []byte(`"界界"`)); err != nil {
		t.Fatalf("counted UTF-8 bytes instead of characters: %v", err)
	}
	if _, err := decodeValidated("manifest", []byte(textSchema), []byte(`"界界界"`)); err == nil {
		t.Fatal("accepted a string above its character limit")
	}

	dateSchema := `{"type":"string","format":"date-time","pattern":"Z$"}`
	if _, err := decodeValidated("manifest", []byte(dateSchema), []byte(`"2099-01-01T00:00:00Z"`)); err != nil {
		t.Fatalf("rejected UTC timestamp: %v", err)
	}
	if _, err := decodeValidated("manifest", []byte(dateSchema), []byte(`"2099-01-01T00:00:00+00:00"`)); err == nil {
		t.Fatal("accepted a timestamp that does not use UTC Z notation")
	}
}
