package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const setupBundleMaxBytes = 16 * 1024

var (
	setupTokenPattern  = regexp.MustCompile(`^[0-9]+\|[A-Za-z0-9_]{40,160}$`)
	setupExpiryPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:Z|[+-]\d{2}:\d{2})$`)
)

// SetupOptions contains parsed setup command arguments. ServerURL overrides
// the bundle's base_url when provided.
type SetupOptions struct {
	SetupFile string
	ServerURL string
	Help      bool
	Version   bool
}

// SetupBundle is the validated setup file data needed by the installation
// workflow. Tokens are held only in memory and are never rendered to output.
type SetupBundle struct {
	Version        int
	RuntimeVersion string
	BaseURL        string
	RunnerID       string
	ProfileID      string
	ExpiresAt      time.Time
	ProfileToken   string
	ExecutionToken string
}

// ParseSetupOptions accepts options before or after the setup file operand.
func ParseSetupOptions(args []string) (SetupOptions, error) {
	var options SetupOptions
	serverURLProvided := false
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--help" || argument == "-h" || argument == "-help":
			options.Help = true
		case argument == "--version" || argument == "-version":
			options.Version = true
		case argument == "--server-url" || argument == "-server-url":
			if index+1 >= len(args) {
				return SetupOptions{}, errors.New("missing --server-url value")
			}
			index++
			options.ServerURL = args[index]
			serverURLProvided = true
		case strings.HasPrefix(argument, "--server-url="):
			options.ServerURL = strings.TrimPrefix(argument, "--server-url=")
			serverURLProvided = true
		case strings.HasPrefix(argument, "-"):
			return SetupOptions{}, errors.New("unknown setup option")
		default:
			if options.SetupFile != "" {
				return SetupOptions{}, errors.New("setup accepts exactly one setup file")
			}
			options.SetupFile = argument
		}
	}
	if serverURLProvided && options.ServerURL == "" {
		return SetupOptions{}, errors.New("--server-url cannot be empty")
	}
	if !options.Help && !options.Version && options.SetupFile == "" {
		return SetupOptions{}, errors.New("setup file is required")
	}
	return options, nil
}

// ParseSetupBundle strictly parses and validates a setup bundle without
// changing the source bytes. now is injectable so expiry checks stay
// deterministic in tests.
func ParseSetupBundle(raw []byte, serverURLOverride string, now time.Time) (SetupBundle, error) {
	value, err := protocol.Decode(raw, setupBundleMaxBytes)
	if err != nil {
		return SetupBundle{}, errors.New("The setup file is not valid JSON or exceeds its size limit.")
	}
	data, err := setupObject(value)
	if err != nil {
		return SetupBundle{}, errors.New("The setup file must contain a JSON object.")
	}

	version, versionErr := parseSetupVersion(data["version"])
	if versionErr != nil || version != 1 {
		return SetupBundle{}, errors.New("Download a setup file for the supported Codex runtime.")
	}
	runtimeVersion, ok := data["runtime_version"].(string)
	if !ok || runtimeVersion != "0.154.0" {
		return SetupBundle{}, errors.New("Download a setup file for the supported Codex runtime.")
	}

	readString := func(key string) (string, bool) {
		value, exists := data[key].(string)
		return value, exists && value != ""
	}
	baseURL, baseURLOK := readString("base_url")
	runnerID, runnerIDOK := readString("runner_id")
	profileID, profileIDOK := readString("profile_id")
	expiresAtText, expiresAtOK := readString("expires_at")
	profileToken, profileTokenOK := readString("profile_token")
	executionToken, executionTokenOK := readString("execution_token")
	if !baseURLOK || !runnerIDOK || !profileIDOK || !expiresAtOK || !profileTokenOK || !executionTokenOK {
		return SetupBundle{}, errors.New("The setup file is incomplete. Download it again.")
	}
	if serverURLOverride != "" {
		baseURL = serverURLOverride
	}
	if err := protocol.ValidateProfileID(runnerID); err != nil {
		return SetupBundle{}, errors.New("Unsafe setup identifier, ownership, type or permissions.")
	}
	if err := protocol.ValidateProfileID(profileID); err != nil {
		return SetupBundle{}, errors.New("Unsafe setup identifier, ownership, type or permissions.")
	}
	if err := validateSetupURL(baseURL); err != nil {
		return SetupBundle{}, err
	}
	if !setupTokenPattern.MatchString(profileToken) || !setupTokenPattern.MatchString(executionToken) {
		return SetupBundle{}, errors.New("The setup file contains an invalid runner token.")
	}
	if !setupExpiryPattern.MatchString(expiresAtText) {
		return SetupBundle{}, errors.New("The setup expiry must be an ISO 8601 timestamp.")
	}
	expiresAt, err := time.Parse(time.RFC3339, expiresAtText)
	if err != nil {
		return SetupBundle{}, errors.New("The setup expiry must be an ISO 8601 timestamp.")
	}
	if !expiresAt.After(now) {
		return SetupBundle{}, errors.New("The setup tokens expired. Download setup again for this runner.")
	}
	return SetupBundle{
		Version:        1,
		RuntimeVersion: runtimeVersion,
		BaseURL:        baseURL,
		RunnerID:       runnerID,
		ProfileID:      profileID,
		ExpiresAt:      expiresAt,
		ProfileToken:   profileToken,
		ExecutionToken: executionToken,
	}, nil
}

func RunSetup(args []string, stdout, stderr io.Writer) int {
	options, err := ParseSetupOptions(args)
	if err != nil {
		fmt.Fprintln(stderr, "Invalid setup options. Run shipmunk-setup --help for usage.")
		return 2
	}
	if options.Help {
		writeUsage(stdout, "shipmunk-setup")
		return 0
	}
	if options.Version {
		fmt.Fprintln(stdout, "shipmunk-setup "+developmentVersion)
		return 0
	}
	fmt.Fprintln(stderr, "Go setup installation is not available in this compatibility foundation.")
	return 1
}

func validateSetupURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(value, "#") || parsed.Opaque != "" || hasForbiddenURLRune(value) {
		return errors.New("The server URL must be HTTP(S), without credentials, query, fragment or control characters.")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("The server URL must be HTTP(S), without credentials, query, fragment or control characters.")
	}
	if parsed.Scheme == "http" && !isSetupLoopbackHost(strings.ToLower(parsed.Hostname())) {
		return errors.New("Use HTTPS for a remote control plane, or HTTP on localhost/127.0.0.1 for local development.")
	}
	return nil
}

func hasForbiddenURLRune(value string) bool {
	for _, character := range value {
		if character <= 0x20 || character == 0x7f || character == '\\' {
			return true
		}
	}
	return false
}

func isSetupLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1"
}

func setupObject(value any) (map[string]any, error) {
	data, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("not an object")
	}
	return data, nil
}

func parseSetupVersion(value any) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("version must be an integer")
	}
	version, err := strconv.Atoi(number.String())
	if err != nil {
		return 0, err
	}
	return version, nil
}
