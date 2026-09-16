package command

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/codex"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
	"github.com/ianrodrigues/shipmunk-runner/internal/workspace"
)

// sandboxCleanupTimeout bounds the best-effort Docker reconcile discard
// attempts before releasing the journal regardless, matching the recovery
// cleanup timeout in package supervisor.
const sandboxCleanupTimeout = 90 * time.Second

// runDiscardAttempt lets an operator release a local attempt journal
// immediately, for the case where waiting on supervisor.reconcile's bounded
// stopped-refusal convergence is not acceptable. The journal is always
// discarded, but it still attempts the same sandbox reconciliation and
// stopped acknowledgement recovery would have made, and names the sandbox it
// could not confirm removed so an operator can check Docker by hand.
func runDiscardAttempt(options RunnerOptions, stdout, stderr io.Writer) int {
	if setupRuntime.effectiveUID() == 0 {
		fmt.Fprintln(stderr, "Run commands as the dedicated non-root runner account.")
		return 1
	}
	store, err := attemptstate.Open(filepath.Join(options.StateDir, "active-attempt.json"))
	if err != nil {
		fmt.Fprintln(stderr, "Runner state directory is unsafe or already in use.")
		return 1
	}
	defer func() { _ = store.Close() }()
	state, err := store.Load()
	if err != nil {
		fmt.Fprintln(stderr, "Runner attempt journal is unreadable.")
		return 1
	}
	if state == nil {
		fmt.Fprintln(stdout, "No interrupted attempt journal to discard.")
		return 0
	}
	workspaces, err := workspace.New(filepath.Join(options.StateDir, "workspaces"))
	if err != nil {
		fmt.Fprintln(stderr, "Runner workspace directory is unsafe.")
		return 1
	}
	identityClaim := protocol.Claim{RunID: state.RunID, AttemptID: state.AttemptID, Fence: state.Fence}
	if state.Workspace != workspaces.Path(identityClaim) {
		fmt.Fprintln(stderr, "Runner attempt journal workspace does not match its identity.")
		return 1
	}
	sandboxName := sandboxIdentity(*state)
	fmt.Fprintf(stdout, "Discarding attempt %s for run %s (workspace %s, sandbox %s).\n", state.AttemptID, state.RunID, state.Workspace, sandboxName)
	if !options.Confirmed {
		if !setupRuntime.isTerminal(setupRuntime.stdin) {
			fmt.Fprintln(stderr, "Discarding a journal outside an operator terminal requires --yes.")
			return 1
		}
		if !confirmDiscard(stdout) {
			fmt.Fprintln(stdout, "Discard cancelled.")
			return 0
		}
	}
	sandboxConfirmed := discardSandboxCleanup(options, *state)
	acknowledgeStoppedBestEffort(options, *state, stdout)
	if err := workspaces.Remove(state.Workspace); err != nil {
		fmt.Fprintf(stdout, "Could not remove the attempt workspace; the journal is discarded regardless: %v\n", err)
	}
	if err := store.Clear(); err != nil {
		fmt.Fprintln(stderr, "Could not clear the attempt journal.")
		return 1
	}
	if !sandboxConfirmed {
		fmt.Fprintf(stdout, "Sandbox cleanup was not confirmed; remove container and volume %q manually if they still exist.\n", sandboxName)
	}
	fmt.Fprintf(stdout, "Discarded attempt %s for run %s. The application's capacity reservation for this run releases only once it confirms a stopped acknowledgement or a server-side sweep reclaims it, so this discard alone may leave that run's slot reserved.\n", state.AttemptID, state.RunID)
	return 0
}

// sandboxIdentity names the container discard should try to reconcile: the
// legacy journaled ID for a raw Docker attempt, or the deterministic name
// internal/codex.Executor derives for a composite-driver attempt.
func sandboxIdentity(state attemptstate.State) string {
	if state.SandboxID != nil {
		return *state.SandboxID
	}
	return "shipmunk-codex-" + state.AttemptID + "-" + strconv.FormatInt(state.Fence, 10)
}

// discardSandboxCleanup is a package var so tests can stub it instead of
// exercising the host Docker CLI.
var discardSandboxCleanup = bestEffortSandboxCleanup

// bestEffortSandboxCleanup mirrors supervisor.reconcile's Docker reconciliation
// so discard does not silently strand a live container; it never blocks the
// discard on failure, since the operator override exists precisely to avoid
// waiting on that path.
func bestEffortSandboxCleanup(options RunnerOptions, state attemptstate.State) bool {
	ctx, cancel := context.WithTimeout(context.Background(), sandboxCleanupTimeout)
	defer cancel()
	if state.SandboxID != nil {
		docker, err := sandbox.New(sandbox.Config{Image: options.Image})
		if err != nil {
			return false
		}
		return docker.Reconcile(ctx, *state.SandboxID) == nil
	}
	return codex.CleanupDockerTransport(ctx, codex.TransportConfig{Name: sandboxIdentity(state)}) == nil
}

func confirmDiscard(stdout io.Writer) bool {
	fmt.Fprint(stdout, "This abandons local recovery of that attempt even if the application has not converged. Continue? [y/N] ")
	answer, _ := bufio.NewReader(setupRuntime.stdin).ReadString('\n')
	return strings.ToLower(strings.TrimSpace(answer)) == "y"
}

// acknowledgeStoppedBestEffort tells the application the attempt is
// discarded when reachable; a discard never blocks on that, since an
// operator override exists precisely because the ordinary bounded
// convergence is not acceptable to wait for. Every failure to confirm,
// including an unreadable token or an invalid client, reports the same line
// so the operator knows the server-side state was not verified.
func acknowledgeStoppedBestEffort(options RunnerOptions, state attemptstate.State, stdout io.Writer) {
	if !attemptAcknowledgeStopped(options, state) {
		fmt.Fprintln(stdout, "The application did not confirm the stopped acknowledgement; the local journal is discarded regardless.")
	}
}

func attemptAcknowledgeStopped(options RunnerOptions, state attemptstate.State) bool {
	token, err := readRunnerToken(options.TokenFile)
	if err != nil {
		return false
	}
	client, err := protocol.NewHTTPClient(options.BaseURL, token, nil)
	if err != nil {
		return false
	}
	claim := protocol.Claim{RunID: state.RunID, AttemptID: state.AttemptID, Fence: state.Fence, LeaseExpiresAt: state.LeaseExpiresAt, Deadline: state.Deadline}
	if state.ProfileID != nil {
		claim.Manifest = map[string]any{"profile_id": *state.ProfileID}
	}
	// No per-call timeout here: the client already enforces
	// protocol.HTTPTimeoutSeconds, and a caller deadline set to the same
	// budget would race it and always lose, surfacing a bare
	// context.DeadlineExceeded instead of *protocol.HTTPTimeoutError.
	return client.AcknowledgeStopped(context.Background(), claim) == nil
}
