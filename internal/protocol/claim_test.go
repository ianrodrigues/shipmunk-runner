package protocol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validManifest = `{
	"protocol_version":"1.0",
	"run_id":"01k4w000000000000000000001",
	"attempt_id":"01k4w000000000000000000002",
	"fence":9007199254740991,
	"kind":"review",
	"repository_id":123,
	"target_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	"diff_base_sha":"9999999999999999999999999999999999999999",
	"head_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	"source_artifacts":[{"artifact_id":"01k4w000000000000000000003","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}],
	"instruction_artifacts":[{"artifact_id":"01k4w000000000000000000004","sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}],
	"profile_id":"01k4w000000000000000000005",
	"agent":"codex",
	"effective_config":{
		"model":"fixture-model",
		"max_turns":10,
		"network":"disabled",
		"review_path_rules":[["**"]],
		"pull_request_reviews":true,
		"mentions":true,
		"comment_limit":20,
		"instructions":"Review carefully.",
		"dashboard_version":1,
		"trusted_revision":null,
		"repository_configuration_sha256":null,
		"instructions_sha256":"1111111111111111111111111111111111111111111111111111111111111111",
		"effective_configuration_sha256":"2222222222222222222222222222222222222222222222222222222222222222"
	},
	"runtime_version":"fixture-1",
	"deadline":"2099-01-01T00:00:00Z",
	"task_context":"Review the supplied diff.",
	"supervisor":{"credential_reference":"credential:fixture"}
}`

func TestParseClaimAcceptsPinnedFullManifestFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "contracts", "v1", "fixtures", "valid", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseClaim(fixture, time.Now()); err != nil {
		t.Fatalf("rejected pinned manifest fixture: %v", err)
	}
}

func TestParseClaimValidatesTheCompleteManifestBeforeReturning(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	claim, err := ParseClaim([]byte(validManifest), now)
	if err != nil {
		t.Fatal(err)
	}
	if claim.RunID != "01k4w000000000000000000001" || claim.AttemptID != "01k4w000000000000000000002" {
		t.Fatalf("claim identifiers differ from the wire values: %#v", claim)
	}
	if claim.Fence != MaxSafeInteger || !claim.Deadline.Equal(time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("claim numeric or time fields differ from the wire values: %#v", claim)
	}
	if !claim.LeaseExpiresAt.Equal(now.UTC().Add(45 * time.Second)) {
		t.Fatalf("claim lease did not use the supplied clock: %#v", claim.LeaseExpiresAt)
	}

	for name, invalid := range map[string]string{
		"unknown root property":         strings.TrimSuffix(validManifest, "}") + `,"unexpected_secret":"never expose"}`,
		"unsupported nested enum":       strings.Replace(validManifest, `"network":"disabled"`, `"network":"unrestricted"`, 1),
		"nullable boolean":              strings.Replace(validManifest, `"pull_request_reviews":true`, `"pull_request_reviews":null`, 1),
		"missing nested required field": strings.Replace(validManifest, `"instructions":"Review carefully.",`, "", 1),
		"nested unknown property":       strings.Replace(validManifest, `"network":"disabled",`, `"network":"disabled","api_key":"secret",`, 1),
		"unsafe timestamp offset":       strings.Replace(validManifest, `"deadline":"2099-01-01T00:00:00Z"`, `"deadline":"2099-01-01T00:00:00+00:00"`, 1),
		"unsafe numeric fence":          strings.Replace(validManifest, `"fence":9007199254740991`, `"fence":9007199254740992`, 1),
		"unsupported protocol version":  strings.Replace(validManifest, `"protocol_version":"1.0"`, `"protocol_version":"2.0"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseClaim([]byte(invalid), now); err == nil {
				t.Fatal("accepted an invalid manifest")
			}
		})
	}

	decimalFence := strings.Replace(validManifest, `"fence":9007199254740991`, `"fence":1.0`, 1)
	if _, err := ParseClaim([]byte(decimalFence), now); err == nil {
		t.Fatal("accepted a decimal JSON number where the PHP claim decoder requires an integer token")
	}
}

func TestClaimFromManifestCannotBypassWholeSchemaValidation(t *testing.T) {
	value, err := Decode([]byte(validManifest), ManifestMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := object(value)
	if err != nil {
		t.Fatal(err)
	}
	config, ok := manifest["effective_config"].(map[string]any)
	if !ok {
		t.Fatalf("manifest effective_config has unexpected type %T", manifest["effective_config"])
	}
	config["api_key"] = "credential-like-value"

	if _, err := ClaimFromManifest(manifest, time.Now()); err == nil {
		t.Fatal("ClaimFromManifest accepted a nested property outside the pinned schema")
	} else if strings.Contains(err.Error(), "credential-like-value") || strings.Contains(err.Error(), "api_key") {
		t.Fatalf("claim validation leaked untrusted manifest data: %v", err)
	}
}
