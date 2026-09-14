package codex

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrDockerRequirements is safe to show during setup or before claiming work.
var ErrDockerRequirements = errors.New("Codex reviews require Docker client and Linux engine 26.0 or newer for immutable snapshot mounts")

const dockerVersionFormat = "{{.Client.Version}}\n{{.Server.Version}}\n{{.Server.Os}}"

// RequireDocker checks both sides of the volume-subpath boundary before setup
// installs operational configuration or the runner starts claiming attempts.
func RequireDocker(ctx context.Context, executable string) error {
	return requireDocker(ctx, execDockerCommand{executable: executable})
}

func requireDocker(ctx context.Context, docker dockerCommand) error {
	result, err := docker.Run(ctx, 10*time.Second, 4096, nil, "version", "--format", dockerVersionFormat)
	if err != nil || result.exitCode != 0 {
		return ErrDockerRequirements
	}
	lines := strings.Split(strings.TrimSpace(string(result.stdout)), "\n")
	if len(lines) != 3 || lines[2] != "linux" {
		return ErrDockerRequirements
	}
	for _, version := range lines[:2] {
		parts := regexp.MustCompile(`^([0-9]+)\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$`).FindStringSubmatch(version)
		if len(parts) != 2 {
			return ErrDockerRequirements
		}
		major, err := strconv.Atoi(parts[1])
		if err != nil || major < 26 {
			return ErrDockerRequirements
		}
	}
	return nil
}
