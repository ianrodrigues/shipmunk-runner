package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

const developmentVersion = "development"

// RunRunner implements the fail-closed Go runner command contract.
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

	// Durable supervision and cleanup are implemented in a later compatibility
	// slice. Never reserve a claim until that implementation owns recovery.
	fmt.Fprintln(stderr, "Go runner supervision is not available in this compatibility foundation.")
	return 1
}

// RunProfile implements the fail-closed profile command contract.
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
	fmt.Fprintln(stderr, "Go profile lifecycle is not available in this compatibility foundation.")
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
