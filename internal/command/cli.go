package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
)

// Release builds replace Version with the exact release tag.
var Version = "development"

// RunRunner supports isolated fixture execution; native profile-backed drivers stay unavailable until a separate lifecycle migration finishes.
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
		fmt.Fprintln(stdout, "shipmunk-runner "+Version)
		return 0
	}
	if err := validateRunnerOptions(&parsed.options); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}

	if parsed.options.DiscardAttempt {
		return runDiscardAttempt(parsed.options, stdout, stderr)
	}
	if parsed.options.Driver == "codex" {
		return runCodexRunner(parsed.options, stdout, stderr)
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
		fmt.Fprintln(stdout, "shipmunk-profile "+Version)
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
	if err := writeProfileHealth(stdout, health, parsed.options.JSON); err != nil {
		fmt.Fprintln(stderr, "Profile operation failed; protected recovery state was retained when needed.")
		return 1
	}
	if health.Health == profile.HealthReady || health.Health == "disconnected" {
		return 0
	}
	return 1
}

// RunCLI is the merged operator binary's entrypoint: setup, connect, probe
// and run are its subcommands. A positional install root after the
// subcommand (as an installed launcher supplies) dispatches through the
// installed runner; otherwise the subcommand's raw flag surface runs
// directly, for manual and diagnostic use against an uninstalled profile or
// runner.
func RunCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: shipmunk-runner <setup|connect|probe|run> ...\nRun shipmunk-runner --help for usage.")
		return 2
	}
	switch args[0] {
	case "--version", "-version":
		fmt.Fprintln(stdout, "shipmunk-runner "+Version)
		return 0
	case "--help", "-h", "-help":
		writeTopLevelUsage(stdout)
		return 0
	case "setup":
		return RunSetup(args[1:], stdout, stderr)
	case "run", "connect", "probe":
		if len(args) >= 2 && !strings.HasPrefix(args[1], "-") {
			return runInstalledSetup(args, stdout, stderr)
		}
		return runManualCommand(args[0], args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "Unknown command. Run shipmunk-runner --help for usage.")
		return 2
	}
}

func runManualCommand(name string, args []string, stdout, stderr io.Writer) int {
	if name == "run" {
		return RunRunner(args, stdout, stderr)
	}
	operation := "probe"
	if name == "connect" {
		operation = "login"
	}
	return RunProfile(append([]string{"--operation=" + operation}, args...), stdout, stderr)
}

func writeTopLevelUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage: shipmunk-runner <command> [options]")
	fmt.Fprintln(output, "Commands:")
	fmt.Fprintln(output, "  setup    Guided release install; ends by running connect unless --skip-connect")
	fmt.Fprintln(output, "  connect  Native device login and readiness check")
	fmt.Fprintln(output, "  probe    Readiness check only")
	fmt.Fprintln(output, "  run      Poll and execute queued work")
	fmt.Fprintln(output, "Run 'shipmunk-runner <command> --help' for that command's options.")
}

// RunWatchdog exposes the same version contract as the operator commands.
func RunWatchdog(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-version") {
		fmt.Fprintln(stdout, "shipmunk-watchdog "+Version)
		return 0
	}
	if err := sandbox.RunWatchdogCommand(args, stdin, stdout); err != nil {
		fmt.Fprintln(stderr, "shipmunk-watchdog:", err)
		return 1
	}
	return 0
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
		fmt.Fprintln(output, "  --discard-attempt       Discard the local attempt journal instead of running")
		fmt.Fprintln(output, "  --yes                   Skip the --discard-attempt confirmation prompt")
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
		fmt.Fprintln(output, "  --json                Print the machine-readable health object instead of a sentence")
		fmt.Fprintln(output, "  --version             Print version")
	case "shipmunk-setup":
		fmt.Fprintln(output, "Usage: shipmunk-setup --release-manifest FILE --release-archive FILE [--server-url URL] SETUP-FILE")
		fmt.Fprintln(output, "Options:")
		fmt.Fprintln(output, "  --server-url URL  Override the setup bundle server URL")
		fmt.Fprintln(output, "  --release-manifest FILE  Verified public release manifest")
		fmt.Fprintln(output, "  --release-archive FILE   Platform release archive")
		fmt.Fprintln(output, "  --skip-connect    Finish without running the connect step")
		fmt.Fprintln(output, "  --version         Print version")
	}
}

var flagHelpRequested = flag.ErrHelp
