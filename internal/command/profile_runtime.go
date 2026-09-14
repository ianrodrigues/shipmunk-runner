package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
)

var runNativeProfile = executeNativeProfile
var profileStartupAfterLock = func() {}

func executeNativeProfile(options ProfileOptions, stderr io.Writer) (profile.Health, error) {
	if os.Geteuid() == 0 {
		return profile.Health{}, errors.New("native profiles require a dedicated non-root runner account")
	}
	store, err := profile.Open(options.ProfilesDir, options.Profile)
	if err != nil {
		return profile.Health{}, err
	}
	defer store.Close()
	var health profile.Health
	err = store.WithExclusive(func(locked *profile.Store) error {
		var operationErr error
		health, operationErr = executeNativeProfileLocked(options, stderr, locked)
		return operationErr
	})
	return health, err
}

func executeNativeProfileLocked(options ProfileOptions, stderr io.Writer, store *profile.Store) (profile.Health, error) {
	profileStartupAfterLock()
	token, err := readRunnerToken(options.TokenFile)
	if err != nil {
		return profile.Health{}, err
	}
	client, err := protocol.NewHTTPClient(options.BaseURL, token, nil)
	if err != nil {
		return profile.Health{}, err
	}
	runtime, err := profile.NewDockerRuntime(options.Image)
	if err != nil {
		return profile.Health{}, err
	}
	watchdogExecutable, err := adjacentWatchdogExecutable()
	if err != nil {
		return profile.Health{}, err
	}
	dockerExecutable, err := exec.LookPath("docker")
	if err != nil {
		return profile.Health{}, err
	}
	watchdog := profile.SandboxWatchdog{Watchdog: sandbox.NewWatchdog(sandbox.Config{
		DockerExecutable:   dockerExecutable,
		WatchdogExecutable: watchdogExecutable,
	})}
	lifecycle := profile.NewLifecycle(client, runtime, watchdog)
	lifecycle.Diagnostic = func(stage string) {
		fmt.Fprintf(stderr, "Profile operation failed at stage: %s.\n", stage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return lifecycle.OperateLocked(ctx, store, options.Profile, options.Operation, options.OperationID)
}

func adjacentWatchdogExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	watchdog := filepath.Join(filepath.Dir(executable), "shipmunk-watchdog")
	info, err := os.Lstat(watchdog)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return "", errors.New("independent watchdog executable is unavailable or unsafe")
	}
	return watchdog, nil
}

func writeProfileHealth(output io.Writer, health profile.Health) error {
	var reason any
	if health.Reason != "" {
		reason = health.Reason
	}
	encoded, err := json.Marshal(map[string]any{"health": health.Health, "reason": reason})
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = output.Write(encoded)
	return err
}
