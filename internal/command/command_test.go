package command

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

type fakePoller struct {
	claim *protocol.Claim
	err   error
}

func (poller fakePoller) Claim(context.Context) (*protocol.Claim, error) {
	return poller.claim, poller.err
}

func TestRunOnceTerminalOutputIsBoundedToIdentifiers(t *testing.T) {
	var output bytes.Buffer
	claim := &protocol.Claim{RunID: "01k4w000000000000000000001", AttemptID: "01k4w000000000000000000002"}
	if received, exitCode := RunOnce(context.Background(), &output, fakePoller{claim: claim}); received != claim || exitCode != 0 || output.String() != "Claimed run 01k4w000000000000000000001 attempt 01k4w000000000000000000002.\n" {
		t.Fatalf("unexpected claimed output: %q, %d", output.String(), exitCode)
	}
	output.Reset()
	if claim, exitCode := RunOnce(context.Background(), &output, fakePoller{}); claim != nil || exitCode != 0 || output.String() != "No eligible work was returned.\n" {
		t.Fatalf("unexpected idle output: %q, %d", output.String(), exitCode)
	}
	output.Reset()
	if _, exitCode := RunOnce(context.Background(), &output, fakePoller{err: errors.New("token=secret")}); exitCode != 1 || output.String() != "Runner connection failed.\n" {
		t.Fatalf("unsafe error output: %q", output.String())
	}
}
