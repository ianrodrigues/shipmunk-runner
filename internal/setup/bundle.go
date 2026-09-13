// Package setup validates the short-lived setup bundle before the installer
// slice is allowed to create any local state.
package setup

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const MaxBundleBytes = 16 * 1024

var tokenPattern = regexp.MustCompile(`^[0-9]+\|[A-Za-z0-9_]{40,160}$`)

type Bundle struct {
	BaseURL        string
	RunnerID       string
	ProfileID      string
	ExpiresAt      time.Time
	ProfileToken   string
	ExecutionToken string
}

func Read(path, serverURL string, now time.Time) (Bundle, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxBundleBytes || info.Mode()&0o077 != 0 {
		return Bundle{}, fmt.Errorf("setup file must be a private small regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Bundle{}, fmt.Errorf("read setup file: %w", err)
	}
	bundle, err := Parse(raw, now)
	if err != nil {
		return Bundle{}, err
	}
	if serverURL != "" {
		if _, err := protocol.NewHTTPClient(serverURL, "setup-token", nil); err != nil {
			return Bundle{}, fmt.Errorf("setup server override is invalid")
		}
		bundle.BaseURL = serverURL
	}
	return bundle, nil
}

func Parse(raw []byte, now time.Time) (Bundle, error) {
	value, err := protocol.Decode(raw, MaxBundleBytes)
	if err != nil {
		return Bundle{}, err
	}
	data, ok := value.(map[string]any)
	if !ok {
		return Bundle{}, fmt.Errorf("setup file must contain an object")
	}
	version, ok := data["version"].(jsonNumber)
	if !ok || version.String() != "1" {
		return Bundle{}, fmt.Errorf("setup bundle version is unsupported")
	}
	runtime, ok := data["runtime_version"].(string)
	if !ok || runtime != "0.154.0" {
		return Bundle{}, fmt.Errorf("setup runtime version is unsupported")
	}
	baseURL, ok := data["base_url"].(string)
	if !ok {
		return Bundle{}, fmt.Errorf("setup bundle base_url is invalid")
	}
	if _, err := protocol.NewHTTPClient(baseURL, "setup-token", nil); err != nil {
		return Bundle{}, fmt.Errorf("setup bundle base_url is invalid")
	}
	runnerID, ok := data["runner_id"].(string)
	if !ok || !protocol.IsULID(runnerID) {
		return Bundle{}, fmt.Errorf("setup runner_id is invalid")
	}
	profileID, ok := data["profile_id"].(string)
	if !ok || !protocol.IsULID(profileID) {
		return Bundle{}, fmt.Errorf("setup profile_id is invalid")
	}
	expiresText, ok := data["expires_at"].(string)
	if !ok {
		return Bundle{}, fmt.Errorf("setup expiry is invalid")
	}
	expiresAt, err := time.Parse(time.RFC3339, expiresText)
	if err != nil || !expiresAt.After(now) {
		return Bundle{}, fmt.Errorf("setup tokens expired or expiry is invalid")
	}
	profileToken, ok := data["profile_token"].(string)
	if !ok || !tokenPattern.MatchString(profileToken) {
		return Bundle{}, fmt.Errorf("setup profile token is invalid")
	}
	executionToken, ok := data["execution_token"].(string)
	if !ok || !tokenPattern.MatchString(executionToken) {
		return Bundle{}, fmt.Errorf("setup execution token is invalid")
	}
	return Bundle{BaseURL: baseURL, RunnerID: runnerID, ProfileID: profileID, ExpiresAt: expiresAt, ProfileToken: profileToken, ExecutionToken: executionToken}, nil
}

// jsonNumber is deliberately local so setup cannot accidentally coerce its
// bundle version through float64. protocol.Decode always returns this shape.
type jsonNumber interface{ String() string }
