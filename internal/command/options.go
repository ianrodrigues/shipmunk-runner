package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

// RunnerOptions contains the validated command-line configuration for the
// execution host. A valid value still cannot start work in this foundation.
type RunnerOptions struct {
	BaseURL         string
	TokenFile       string
	StateDir        string
	Image           string
	Once            bool
	Driver          string
	ProfilesDir     string
	RepositoryImage string
}

// ProfileOptions contains the validated command-line configuration for a
// native profile operation.
type ProfileOptions struct {
	BaseURL     string
	TokenFile   string
	ProfilesDir string
	Image       string
	Profile     string
	Operation   string
	OperationID string
}

type parsedRunnerOptions struct {
	options RunnerOptions
	version bool
}

type parsedProfileOptions struct {
	options ProfileOptions
	version bool
}

func parseRunnerOptions(args []string, output io.Writer) (parsedRunnerOptions, error) {
	flags := newFlagSet("shipmunk-runner", output)
	flags.String("base-url", "", "control-plane URL")
	flags.String("token-file", "", "mode-0600 runner token file")
	flags.String("state-dir", "", "mode-0700 state directory")
	flags.String("image", "", "repository image")
	flags.Bool("once", false, "handle at most one claim")
	flags.String("driver", "fixture", "fixture or codex")
	flags.String("profiles-dir", "", "protected profile directory")
	flags.String("repository-image", "", "repository command image")
	version := flags.Bool("version", false, "print version")
	if err := flags.Parse(args); err != nil {
		return parsedRunnerOptions{}, err
	}
	if flags.NArg() != 0 {
		return parsedRunnerOptions{}, errUnexpectedArguments
	}
	options := RunnerOptions{
		BaseURL:         flags.Lookup("base-url").Value.String(),
		TokenFile:       flags.Lookup("token-file").Value.String(),
		StateDir:        flags.Lookup("state-dir").Value.String(),
		Image:           flags.Lookup("image").Value.String(),
		Once:            flags.Lookup("once").Value.String() == "true",
		Driver:          flags.Lookup("driver").Value.String(),
		ProfilesDir:     flags.Lookup("profiles-dir").Value.String(),
		RepositoryImage: flags.Lookup("repository-image").Value.String(),
	}
	return parsedRunnerOptions{options: options, version: *version}, nil
}

// ParseRunnerOptions parses and validates options without opening credentials,
// creating directories, or contacting the control plane.
func ParseRunnerOptions(args []string) (RunnerOptions, error) {
	parsed, err := parseRunnerOptions(args, io.Discard)
	if err != nil {
		return RunnerOptions{}, err
	}
	if err := validateRunnerOptions(&parsed.options); err != nil {
		return RunnerOptions{}, err
	}
	return parsed.options, nil
}

func validateRunnerOptions(options *RunnerOptions) error {
	if options.BaseURL == "" || options.TokenFile == "" || options.StateDir == "" || options.Image == "" {
		return errMissingRunnerOptions
	}
	if err := validateControlPlaneURL(options.BaseURL); err != nil {
		return err
	}
	if options.Driver != "fixture" && options.Driver != "codex" {
		return errUnsupportedDriver
	}
	if options.Driver == "codex" && options.ProfilesDir == "" {
		return errCodexProfilesDirRequired
	}
	if options.RepositoryImage == "" {
		options.RepositoryImage = options.Image
	}
	return nil
}

func parseProfileOptions(args []string, output io.Writer) (parsedProfileOptions, error) {
	flags := newFlagSet("shipmunk-profile", output)
	flags.String("base-url", "", "control-plane URL")
	flags.String("token-file", "", "mode-0600 profile token file")
	flags.String("profiles-dir", "", "protected profile directory")
	flags.String("image", "", "native profile image")
	flags.String("profile", "", "profile identifier")
	flags.String("operation", "", "login, probe or disconnect")
	flags.String("operation-id", "", "profile operation ULID")
	version := flags.Bool("version", false, "print version")
	if err := flags.Parse(args); err != nil {
		return parsedProfileOptions{}, err
	}
	if flags.NArg() != 0 {
		return parsedProfileOptions{}, errUnexpectedArguments
	}
	options := ProfileOptions{
		BaseURL:     flags.Lookup("base-url").Value.String(),
		TokenFile:   flags.Lookup("token-file").Value.String(),
		ProfilesDir: flags.Lookup("profiles-dir").Value.String(),
		Image:       flags.Lookup("image").Value.String(),
		Profile:     flags.Lookup("profile").Value.String(),
		Operation:   flags.Lookup("operation").Value.String(),
		OperationID: flags.Lookup("operation-id").Value.String(),
	}
	return parsedProfileOptions{options: options, version: *version}, nil
}

// ParseProfileOptions validates the safe, static portion of a profile command.
func ParseProfileOptions(args []string) (ProfileOptions, error) {
	parsed, err := parseProfileOptions(args, io.Discard)
	if err != nil {
		return ProfileOptions{}, err
	}
	if err := validateProfileOptions(parsed.options); err != nil {
		return ProfileOptions{}, err
	}
	return parsed.options, nil
}

func validateProfileOptions(options ProfileOptions) error {
	for _, option := range []struct {
		name  string
		value string
	}{
		{name: "base-url", value: options.BaseURL},
		{name: "token-file", value: options.TokenFile},
		{name: "profiles-dir", value: options.ProfilesDir},
		{name: "image", value: options.Image},
		{name: "profile", value: options.Profile},
		{name: "operation", value: options.Operation},
		{name: "operation-id", value: options.OperationID},
	} {
		if option.value == "" {
			return fmt.Errorf("Missing required --%s option.", option.name)
		}
	}
	if err := validateControlPlaneURL(options.BaseURL); err != nil {
		return err
	}
	if err := protocol.ValidateProfileID(options.Profile); err != nil {
		return errors.New("Profile identifier is invalid.")
	}
	if err := protocol.ValidateOperationID(options.OperationID); err != nil {
		return errors.New("Profile operation identifier is invalid.")
	}
	if options.Operation != "login" && options.Operation != "probe" && options.Operation != "disconnect" {
		return errUnsupportedProfileOperation
	}
	return nil
}

func newFlagSet(name string, output io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {
		writeUsage(output, name)
	}
	return flags
}

func validateControlPlaneURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(value, "#") || parsed.Opaque != "" {
		return errors.New("Control-plane URL is invalid.")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHostname(parsed.Hostname())) {
		return errors.New("The control plane must use HTTPS outside local development.")
	}
	if strings.ContainsAny(value, "\r\n\t \x00\\") {
		return errors.New("Control-plane URL is invalid.")
	}
	return nil
}

func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return host == "127.0.0.1" || host == "::1"
}

var (
	errUnexpectedArguments         = errors.New("unexpected positional arguments")
	errMissingRunnerOptions        = errors.New("Missing required --base-url, --token-file, --state-dir or --image option.")
	errUnsupportedDriver           = errors.New("Unsupported runner driver.")
	errCodexProfilesDirRequired    = errors.New("Codex requires an explicit protected --profiles-dir.")
	errUnsupportedProfileOperation = errors.New("Unsupported profile operation.")
)
