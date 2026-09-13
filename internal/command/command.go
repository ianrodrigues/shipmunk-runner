// Package command owns terminal-facing behavior. Runtime execution is wired by
// the supervision slice; keeping this boundary small prevents a compatibility
// scaffold from reserving work before it can provide the cleanup guarantees.
package command

import (
	"context"
	"fmt"
	"io"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

type Poller interface {
	Claim(context.Context) (*protocol.Claim, error)
}

// RunOnce preserves the observable idle/claimed terminal contract without
// rendering untrusted server or provider output. A claimed attempt is handed
// to the caller for supervised execution; this package never completes it.
func RunOnce(ctx context.Context, output io.Writer, poller Poller) (*protocol.Claim, int) {
	claim, err := poller.Claim(ctx)
	if err != nil {
		fmt.Fprintln(output, "Runner connection failed.")
		return nil, 1
	}
	if claim == nil {
		fmt.Fprintln(output, "No eligible work was returned.")
		return nil, 0
	}
	fmt.Fprintf(output, "Claimed run %s attempt %s.\n", claim.RunID, claim.AttemptID)
	return claim, 0
}
