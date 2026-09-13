package protocol

import (
	"testing"
	"time"
)

func TestClaimRejectsMissingNullableAndUnknownNestedFields(t *testing.T) {
	raw := readContractFixture(t, "manifest")
	value, err := Decode(raw, ManifestMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := object(value)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"unknown nested property":            func(data map[string]any) { data["effective_config"].(map[string]any)["unsafe"] = true },
		"missing required nullable property": func(data map[string]any) { delete(data["effective_config"].(map[string]any), "trusted_revision") },
		"null where string is required":      func(data map[string]any) { data["deadline"] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			copy := deepCopyObject(manifest)
			mutate(copy)
			if _, err := ClaimFromManifest(copy, time.Now()); err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
}

func deepCopyObject(source map[string]any) map[string]any {
	value, _ := Decode([]byte(mustJSON(source)), ManifestMaxBytes)
	copy, _ := object(value)
	return copy
}
