package supervisor

import (
	"context"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
)

// DockerBackend adapts the isolated container implementation to supervision.
type DockerBackend struct{ *sandbox.Docker }

func (backend DockerBackend) Create(ctx context.Context, claim protocol.Claim, input map[string]any, path string) (Process, error) {
	return backend.Docker.Create(ctx, claim, input, path)
}

// HostWatchdog adapts the independent watchdog process, not a goroutine fallback.
type HostWatchdog struct{ *sandbox.Watchdog }

func (watchdog HostWatchdog) Arm(name string, lease, deadline time.Time) (WatchdogLease, error) {
	return watchdog.Watchdog.Arm(name, lease, deadline)
}
