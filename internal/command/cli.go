package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
)

const developmentVersion = "development"

// RunRunner supports isolated fixture execution; native profile-backed drivers
// remain unavailable until their separate lifecycle migration is complete.
func RunRunner(args []string, stdout, stderr io.Writer) int {
	parsed, err := parseRunnerOptions(args, stdout)
	if errors.Is(err, flagHelpRequested) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "Invalid runner options. Run shipmunk-runner --help for usage.")
		return 2
	}
	if parsed.version {
		fmt.Fprintln(stdout, "shipmunk-runner "+developmentVersion)
		return 0
	}
	if err := validateRunnerOptions(&parsed.options); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}

	if parsed.options.Driver != "fixture" {
		fmt.Fprintln(stderr, "Go native profile execution is not available yet.")
		return 1
	}
	return runFixtureRunner(parsed.options, stdout, stderr)
}

// RunProfile executes the protected native profile lifecycle.
func RunProfile(args []string, stdout, stderr io.Writer) int {
	parsed, err := parseProfileOptions(args, stdout)
	if errors.Is(err, flagHelpRequested) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "Invalid profile options. Run shipmunk-profile --help for usage.")
		return 2
	}
	if parsed.version {
		fmt.Fprintln(stdout, "shipmunk-profile "+developmentVersion)
		return 0
	}
	if err := validateProfileOptions(parsed.options); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if parsed.options.Operation == "login" {
		stdoutFile, stdoutOK := stdout.(*os.File)
		stderrFile, stderrOK := stderr.(*os.File)
		if !stdoutOK || !stderrOK || !isTerminal(os.Stdin) || !isTerminal(stdoutFile) || !isTerminal(stderrFile) {
			fmt.Fprintln(stderr, "Native login requires an operator terminal.")
			return 1
		}
	}
	health, err := runNativeProfile(parsed.options, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "Profile operation failed; protected recovery state was retained when needed.")
		return 1
	}
	if err := writeProfileHealth(stdout, health); err != nil {
		fmt.Fprintln(stderr, "Profile operation failed; protected recovery state was retained when needed.")
		return 1
	}
	if health.Health == profile.HealthReady || health.Health == "disconnected" {
		return 0
	}
	return 1
}

func writeUsage(output io.Writer, command string) {
	switch command {
	case "shipmunk-runner":
		fmt.Fprintln(output, "Usage: shipmunk-runner --base-url URL --token-file FILE --state-dir DIR --image IMAGE [options]")
		fmt.Fprintln(output, "Options:")
		fmt.Fprintln(output, "  --base-url URL          Control-plane URL")
		fmt.Fprintln(output, "  --token-file FILE       Mode-0600 runner token file")
		fmt.Fprintln(output, "  --state-dir DIR         Mode-0700 state directory")
		fmt.Fprintln(output, "  --image IMAGE           Repository image")
		fmt.Fprintln(output, "  --once                  Handle at most one claim")
		fmt.Fprintln(output, "  --driver NAME           fixture or codex (default fixture)")
		fmt.Fprintln(output, "  --profiles-dir DIR      Protected profile directory")
		fmt.Fprintln(output, "  --repository-image IMG  Repository command image (default --image)")
		fmt.Fprintln(output, "  --version               Print version")
	case "shipmunk-profile":
		fmt.Fprintln(output, "Usage: shipmunk-profile --base-url URL --token-file FILE --profiles-dir DIR --image IMAGE --profile ULID --operation NAME --operation-id ULID")
		fmt.Fprintln(output, "Options:")
		fmt.Fprintln(output, "  --base-url URL        Control-plane URL")
		fmt.Fprintln(output, "  --token-file FILE     Mode-0600 profile token file")
		fmt.Fprintln(output, "  --profiles-dir DIR    Protected profile directory")
		fmt.Fprintln(output, "  --image IMAGE         Native profile image")
		fmt.Fprintln(output, "  --profile ULID        Profile identifier")
		fmt.Fprintln(output, "  --operation NAME      login, probe or disconnect")
		fmt.Fprintln(output, "  --operation-id ULID   Profile operation identifier")
		fmt.Fprintln(output, "  --version             Print version")
	case "shipmunk-setup":
		fmt.Fprintln(output, "Usage: shipmunk-setup [--server-url URL] SETUP-FILE")
		fmt.Fprintln(output, "       shipmunk-setup SETUP-FILE [--server-url URL]")
		fmt.Fprintln(output, "Options:")
		fmt.Fprintln(output, "  --server-url URL  Override the setup bundle server URL")
		fmt.Fprintln(output, "  --version         Print version")
	}
}

var flagHelpRequested = flag.ErrHelp
