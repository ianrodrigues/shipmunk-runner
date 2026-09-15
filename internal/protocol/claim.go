package protocol

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

var (
	ulidPattern         = regexp.MustCompile(`^[0-7][0-9a-hjkmnp-tv-z]{25}$`)
	sha256Pattern       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	utcTimestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$`)
)

// Claim holds a validated, fenced response that callers must treat as immutable, despite mutable fields.
type Claim struct {
	RunID          string
	AttemptID      string
	Fence          int64
	LeaseExpiresAt time.Time
	Deadline       time.Time
	Manifest       map[string]any
}

// ParseClaim validates the manifest and starts a local 45-second lease; the supervisor enforces the deadline.
func ParseClaim(raw []byte, now time.Time) (Claim, error) {
	schema, err := schemaBytes("manifest")
	if err != nil {
		return Claim{}, fmt.Errorf("manifest contract is unavailable")
	}
	value, err := decodeValidated("manifest", schema, raw)
	if err != nil {
		return Claim{}, err
	}
	manifest, err := object(value)
	if err != nil {
		return Claim{}, err
	}
	return claimFromValidatedManifest(manifest, now)
}

// ClaimFromManifest validates a detached copy of the manifest using the same schema checks as ParseClaim.
func ClaimFromManifest(manifest map[string]any, now time.Time) (Claim, error) {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return Claim{}, fmt.Errorf("claim manifest is invalid")
	}
	schema, err := schemaBytes("manifest")
	if err != nil {
		return Claim{}, fmt.Errorf("manifest contract is unavailable")
	}
	value, err := decodeValidated("manifest", schema, raw)
	if err != nil {
		return Claim{}, fmt.Errorf("claim manifest is invalid")
	}
	validatedManifest, err := object(value)
	if err != nil {
		return Claim{}, fmt.Errorf("claim manifest is invalid")
	}
	return claimFromValidatedManifest(validatedManifest, now)
}

func claimFromValidatedManifest(manifest map[string]any, now time.Time) (Claim, error) {
	if version, err := stringField(manifest, "protocol_version"); err != nil || version != Version {
		return Claim{}, fmt.Errorf("unsupported manifest protocol version")
	}
	runID, err := stringField(manifest, "run_id")
	if err != nil || !ulidPattern.MatchString(runID) {
		return Claim{}, fmt.Errorf("claim run_id must be a lowercase ULID")
	}
	attemptID, err := stringField(manifest, "attempt_id")
	if err != nil || !ulidPattern.MatchString(attemptID) {
		return Claim{}, fmt.Errorf("claim attempt_id must be a lowercase ULID")
	}
	fence, err := positiveInteger(manifest["fence"])
	if err != nil {
		return Claim{}, fmt.Errorf("claim fence is invalid: %w", err)
	}
	deadlineText, err := stringField(manifest, "deadline")
	if err != nil {
		return Claim{}, err
	}
	deadline, err := utcTime(deadlineText)
	if err != nil {
		return Claim{}, fmt.Errorf("claim deadline is invalid: %w", err)
	}
	return Claim{RunID: runID, AttemptID: attemptID, Fence: fence, LeaseExpiresAt: now.UTC().Add(45 * time.Second), Deadline: deadline, Manifest: manifest}, nil
}

func positiveInteger(value any) (int64, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("must be an integer")
	}
	integer, err := number.Int64()
	if err != nil || integer < 1 || integer > MaxSafeInteger {
		return 0, fmt.Errorf("outside supported positive integer range")
	}
	return integer, nil
}

func utcTime(value string) (time.Time, error) {
	if !utcTimestampPattern.MatchString(value) {
		return time.Time{}, fmt.Errorf("timestamp must use strict RFC3339 UTC syntax")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("not an RFC3339 UTC timestamp")
	}
	return parsed, nil
}
