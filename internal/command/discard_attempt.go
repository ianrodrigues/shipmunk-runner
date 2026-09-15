package command

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/workspace"
)

// runDiscardAttempt lets an operator release a local attempt journal
// immediately, for the case where waiting on supervisor.reconcile's bounded
// stopped-refusal convergence is not acceptable. It never creates a sandbox,
// so it works even when Docker is unavailable; the stopped acknowledgement it
// sends is best effort, since the journal is discarded either way.
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
	fmt.Fprintf(stdout, "Discarding attempt %s for run %s (workspace %s).\n", state.AttemptID, state.RunID, state.Workspace)
	if !options.Confirmed && !confirmDiscard(stdout) {
		fmt.Fprintln(stdout, "Discard cancelled.")
		return 0
	}
	acknowledgeStoppedBestEffort(options, *state, stdout)
	workspaces, err := workspace.New(filepath.Join(options.StateDir, "workspaces"))
	if err != nil {
		fmt.Fprintln(stderr, "Runner workspace directory is unsafe.")
		return 1
	}
	if err := workspaces.Remove(state.Workspace); err != nil {
		fmt.Fprintf(stdout, "Could not remove the attempt workspace; the journal is discarded regardless: %v\n", err)
	}
	if err := store.Clear(); err != nil {
		fmt.Fprintln(stderr, "Could not clear the attempt journal.")
		return 1
	}
	fmt.Fprintf(stdout, "Discarded attempt %s for run %s.\n", state.AttemptID, state.RunID)
	return 0
}

func confirmDiscard(stdout io.Writer) bool {
	fmt.Fprint(stdout, "This abandons local recovery of that attempt even if the application has not converged. Continue? [y/N] ")
	answer, _ := bufio.NewReader(setupRuntime.stdin).ReadString('\n')
	return strings.ToLower(strings.TrimSpace(answer)) == "y"
}

// acknowledgeStoppedBestEffort tells the application the attempt is
// discarded when reachable; a discard never blocks on that, since an
// operator override exists precisely because the ordinary bounded
// convergence is not acceptable to wait for.
func acknowledgeStoppedBestEffort(options RunnerOptions, state attemptstate.State, stdout io.Writer) {
	token, err := readRunnerToken(options.TokenFile)
	if err != nil {
		return
	}
	client, err := protocol.NewHTTPClient(options.BaseURL, token, nil)
	if err != nil {
		return
	}
	claim := protocol.Claim{RunID: state.RunID, AttemptID: state.AttemptID, Fence: state.Fence, LeaseExpiresAt: state.LeaseExpiresAt, Deadline: state.Deadline}
	if state.ProfileID != nil {
		claim.Manifest = map[string]any{"profile_id": *state.ProfileID}
	}
	ctx, cancel := context.WithTimeout(context.Background(), protocol.HTTPTimeoutSeconds*time.Second)
	defer cancel()
	if err := client.AcknowledgeStopped(ctx, claim); err != nil {
		fmt.Fprintln(stdout, "The application did not confirm the stopped acknowledgement; the local journal is discarded regardless.")
	}
}
