package command

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/install"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

var setupRunRunner = RunRunner
var setupRunProfile = RunProfile

func runInstalledSetup(args []string, stdout, stderr io.Writer) int {
	if setupRuntime.effectiveUID() == 0 {
		fmt.Fprintln(stderr, "Run commands as the dedicated non-root runner account.")
		return 1
	}
	if len(args) < 2 || (args[0] != "run" && args[0] != "connect") {
		fmt.Fprintln(stderr, "Invalid installed runner command.")
		return 2
	}
	root := args[1]
	configuration, err := install.LoadConfiguration(root)
	if err != nil {
		fmt.Fprintln(stderr, "Installed runner configuration is unsafe or incomplete.")
		return 1
	}
	guard := install.Guard{Root: root, Identity: configuration.Identity}
	// The supervised run recovers an interrupted attempt; every other command must refuse one.
	if args[0] == "run" {
		configuration, err = guard.RuntimeConfiguration()
	} else {
		configuration, err = guard.Configuration()
	}
	if err != nil {
		fmt.Fprintln(stderr, launchRefusal(err))
		return 1
	}
	if Version != configuration.ReleaseVersion {
		fmt.Fprintln(stderr, "Installed runner release does not match this launcher. Run setup again for the installed release.")
		return 1
	}
	returnCode := 1
	err = install.WithLaunchLock(root, func() error {
		if install.ActivationPending(root) {
			return errActivationPending
		}
		current, err := install.LoadConfiguration(root)
		if err != nil || current.ReleaseVersion != Version {
			return errConfigurationChanged
		}
		returnCode = dispatchInstalled(args, root, current, stdout, stderr)
		return nil
	})
	if err != nil {
		fmt.Fprintln(stderr, launchRefusal(err))
		return 1
	}
	return returnCode
}

var (
	errActivationPending    = errors.New("activation recovery is pending")
	errConfigurationChanged = errors.New("installed configuration changed during launch")
)

// launchRefusal names the condition the operator must resolve, without repeating an internal error.
func launchRefusal(err error) string {
	switch {
	case errors.Is(err, install.ErrRecoveryPending):
		return "An interrupted runner attempt awaits recovery. Run the runner command to recover it."
	case errors.Is(err, errActivationPending):
		return "Runner activation is unfinished. Run setup again to complete it."
	case errors.Is(err, errConfigurationChanged):
		return "Installed runner configuration changed during launch. Try again."
	case errors.Is(err, syscall.EWOULDBLOCK):
		return "Another runner command is already using this installation."
	default:
		return "Installed runner state is unsafe or incomplete."
	}
}

func dispatchInstalled(args []string, root string, configuration install.Configuration, stdout, stderr io.Writer) int {
	base := []string{"--base-url=" + configuration.Identity.BaseURL, "--profiles-dir=" + filepath.Join(root, "profiles"), "--image=" + configuration.ImageID}
	if args[0] == "run" {
		if len(args) > 3 || len(args) == 3 && args[2] != "--once" {
			fmt.Fprintln(stderr, "The runner command accepts only --once.")
			return 2
		}
		commandArgs := append(base, "--token-file="+filepath.Join(root, "execution.token"), "--state-dir="+filepath.Join(root, "state"), "--driver=codex", "--repository-image="+configuration.ImageID)
		if len(args) == 3 {
			commandArgs = append(commandArgs, "--once")
		}
		return setupRunRunner(commandArgs, stdout, stderr)
	}
	{
		operation := "login"
		if len(args) == 3 {
			operation = args[2]
		}
		if len(args) > 3 || (operation != "login" && operation != "probe") {
			fmt.Fprintln(stderr, "The connection command accepts only login or probe.")
			return 2
		}
		if !setupRuntime.isTerminal(setupRuntime.stdin) || !setupRuntime.isTerminal(stdout) || !setupRuntime.isTerminal(stderr) {
			fmt.Fprintln(stderr, "Native connection operations require an operator terminal.")
			return 1
		}
		operationID, err := newOperationID(setupRuntime.now())
		if err != nil {
			fmt.Fprintln(stderr, "Cannot create a protected profile operation identifier.")
			return 1
		}
		commandArgs := append(base, "--token-file="+filepath.Join(root, "profile.token"), "--profile="+configuration.Identity.ProfileID, "--operation="+operation, "--operation-id="+operationID)
		return setupRunProfile(commandArgs, stdout, stderr)
	}
}

func newOperationID(now time.Time) (string, error) {
	var raw [16]byte
	milliseconds := uint64(now.UnixMilli())
	binary.BigEndian.PutUint64(raw[:8], milliseconds)
	copy(raw[:6], raw[2:8])
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", err
	}
	id := strings.ToLower(base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding).EncodeToString(raw[:]))
	if err := protocol.ValidateOperationID(id); err != nil {
		return "", err
	}
	return id, nil
}

func setupLaunchers(setupBinary, root string) map[string][]byte {
	launchers := make(map[string][]byte, 2)
	for _, name := range []string{"run", "connect"} {
		launchers[name] = []byte("#!/bin/sh\nexec " + shellQuote(setupBinary) + " " + name + " " + shellQuote(root) + " \"$@\"\n")
	}
	return launchers
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
