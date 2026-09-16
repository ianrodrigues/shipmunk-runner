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
		fmt.Fprintln(stderr, "Invalid runner options. Run shipmunk-runner run --help for usage.")
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

// RunProfile is the raw flag surface shared by connect, probe and disconnect; the caller sets --operation.
func RunProfile(args []string, stdout, stderr io.Writer) int {
	parsed, err := parseProfileOptions(args, stdout)
	if errors.Is(err, flagHelpRequested) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "Invalid options. Run shipmunk-runner connect --help, shipmunk-runner probe --help or shipmunk-runner disconnect --help for usage.")
		return 2
	}
	if parsed.version {
		fmt.Fprintln(stdout, "shipmunk-runner "+Version)
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
		if parsed.options.JSON {
			_ = writeProfileHealth(stdout, profile.Health{Health: profile.HealthError, Reason: "operation_failed"}, true)
		}
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

// RunCLI dispatches subcommands; a positional root after the subcommand selects the installed launcher path.
func RunCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: shipmunk-runner <setup|connect|probe|run|disconnect> ...\nRun shipmunk-runner --help for usage.")
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
	case "run", "connect", "probe", "disconnect":
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
	if hasFlag(args, "help") || hasFlag(args, "h") {
		writeUsage(stdout, name)
		return 0
	}
	if hasFlag(args, "operation") {
		fmt.Fprintf(stderr, "The %s command sets --operation itself; remove it.\n", name)
		return 2
	}
	operation := map[string]string{"connect": "login", "probe": "probe", "disconnect": "disconnect"}[name]
	return RunProfile(append([]string{"--operation=" + operation}, args...), stdout, stderr)
}

func hasFlag(args []string, name string) bool {
	for _, argument := range args {
		if argument == "-"+name || argument == "--"+name || strings.HasPrefix(argument, "-"+name+"=") || strings.HasPrefix(argument, "--"+name+"=") {
			return true
		}
	}
	return false
}

func writeTopLevelUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage: shipmunk-runner <command> [options]")
	fmt.Fprintln(output, "Commands:")
	fmt.Fprintln(output, "  setup       Guided release install; ends by running connect unless --skip-connect")
	fmt.Fprintln(output, "  connect     Native device login and readiness check")
	fmt.Fprintln(output, "  probe       Readiness check only")
	fmt.Fprintln(output, "  run         Poll and execute queued work")
	fmt.Fprintln(output, "  disconnect  Remove local native profile access")
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
	case "run":
		fmt.Fprintln(output, "Usage: shipmunk-runner run --base-url URL --token-file FILE --state-dir DIR --image IMAGE [options]")
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
	case "connect", "probe", "disconnect":
		fmt.Fprintf(output, "Usage: shipmunk-runner %s --base-url URL --token-file FILE --profiles-dir DIR --image IMAGE --profile ULID --operation-id ULID\n", command)
		fmt.Fprintln(output, "Options:")
		fmt.Fprintln(output, "  --base-url URL        Control-plane URL")
		fmt.Fprintln(output, "  --token-file FILE     Mode-0600 profile token file")
		fmt.Fprintln(output, "  --profiles-dir DIR    Protected profile directory")
		fmt.Fprintln(output, "  --image IMAGE         Native profile image")
		fmt.Fprintln(output, "  --profile ULID        Profile identifier")
		fmt.Fprintln(output, "  --operation-id ULID   Profile operation identifier")
		fmt.Fprintln(output, "  --json                Print the machine-readable health object instead of a sentence")
		fmt.Fprintln(output, "  --version             Print version")
	case "setup":
		fmt.Fprintln(output, "Usage: shipmunk-runner setup --release-manifest FILE --release-archive FILE [--server-url URL] SETUP-FILE")
		fmt.Fprintln(output, "Options:")
		fmt.Fprintln(output, "  --server-url URL  Override the setup bundle server URL")
		fmt.Fprintln(output, "  --release-manifest FILE  Verified public release manifest")
		fmt.Fprintln(output, "  --release-archive FILE   Platform release archive")
		fmt.Fprintln(output, "  --skip-connect    Finish without running the connect step")
		fmt.Fprintln(output, "  --version         Print version")
	}
}

var flagHelpRequested = flag.ErrHelp
