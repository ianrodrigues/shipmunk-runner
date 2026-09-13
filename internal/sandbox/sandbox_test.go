package sandbox

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const (
	testRunID     = "01arz3ndektsv4rrffq69g5fav"
	testAttemptID = "01arz3ndektsv4rrffq69g5faw"
	testFence     = int64(7)
	testName      = "shipmunk-01arz3ndektsv4rrffq69g5faw-7"
	testContainer = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestNameIsStableAndValidatesClaimIdentity(t *testing.T) {
	docker, err := New(Config{Image: "fixture"})
	if err != nil {
		t.Fatal(err)
	}

	name, err := docker.Name(testClaim())
	if err != nil {
		t.Fatal(err)
	}
	if name != testName {
		t.Fatalf("Name() = %q, want %q", name, testName)
	}

	claim := testClaim()
	claim.Fence = 0
	if _, err := docker.Name(claim); err == nil {
		t.Fatal("Name() accepted an invalid fence")
	}
}

func TestClientEnvironmentPreservesDockerSettingsOnly(t *testing.T) {
	allowed := map[string]string{
		"HOME":              "/synthetic/docker-home",
		"DOCKER_HOST":       "tcp://docker.example.test:2376",
		"DOCKER_CONTEXT":    "synthetic-context",
		"DOCKER_CONFIG":     "/synthetic/docker-config",
		"DOCKER_CERT_PATH":  "/synthetic/docker-certs",
		"DOCKER_TLS_VERIFY": "1",
	}
	for name, value := range allowed {
		t.Setenv(name, value)
	}
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "SHIPMUNK_RUNNER_TOKEN", "SHIPMUNK_CONTROL_PLANE_TOKEN", "GITHUB_TOKEN", "UNRELATED_SECRET"} {
		t.Setenv(name, "synthetic-secret")
	}
	environment := ClientEnvironment()
	for name, value := range allowed {
		if !contains(environment, name+"="+value) {
			t.Errorf("ClientEnvironment() omitted %s", name)
		}
	}
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "SHIPMUNK_RUNNER_TOKEN", "SHIPMUNK_CONTROL_PLANE_TOKEN", "GITHUB_TOKEN", "UNRELATED_SECRET"} {
		for _, entry := range environment {
			if strings.HasPrefix(entry, name+"=") {
				t.Errorf("ClientEnvironment() included %s", name)
			}
		}
	}
}

func TestDockerSandboxLifecycleUsesIsolationAndRemovesAfterConfirmation(t *testing.T) {
	fixture := newFakeDocker(t, false, true)
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "fixture.txt"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "synthetic-secret")
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-secret")
	t.Setenv("SHIPMUNK_RUNNER_TOKEN", "synthetic-control-plane-secret")
	t.Setenv("SHIPMUNK_CONTROL_PLANE_TOKEN", "synthetic-control-plane-secret")
	t.Setenv("GITHUB_TOKEN", "synthetic-github-secret")
	setSyntheticDockerEnvironment(t)

	docker, err := New(Config{Image: "fixture", DockerExecutable: fixture.executable})
	if err != nil {
		t.Fatal(err)
	}
	process, err := docker.Create(context.Background(), testClaim(), map[string]any{"safe_marker": "copied"}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if process.ID() != testName {
		t.Fatalf("Process.ID() = %q, want durable name %q", process.ID(), testName)
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	exitCode, output, err := process.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 7 || string(output) != "synthetic output" {
		t.Fatalf("Wait() = (%d, %q), want (7, synthetic output)", exitCode, output)
	}
	if err := process.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture.marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("container remains after Remove(): stat error = %v", err)
	}
	if _, err := os.Stat(fixture.environmentLeak); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Docker CLI inherited credentials: stat error = %v", err)
	}
	dockerEnvironment, err := os.ReadFile(fixture.clientEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"HOME=/synthetic/docker-home", "DOCKER_HOST=tcp://docker.example.test:2376", "DOCKER_CONTEXT=synthetic-context", "DOCKER_CONFIG=/synthetic/docker-config", "DOCKER_CERT_PATH=/synthetic/docker-certs", "DOCKER_TLS_VERIFY=1"} {
		if !strings.Contains(string(dockerEnvironment), expected) {
			t.Errorf("Docker CLI environment omitted %q: %s", expected, dockerEnvironment)
		}
	}

	arguments, err := os.ReadFile(fixture.arguments)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"--read-only", "--user 65532:65532", "--cap-drop ALL", "--network none", "--memory-swap 268435456", "--pids-limit 64", "shipmunk.run=" + testRunID, "shipmunk.attempt=" + testAttemptID, "shipmunk.fence=7",
		"--env HTTP_PROXY=", "--env http_proxy=", "--env HTTPS_PROXY=", "--env https_proxy=", "--env FTP_PROXY=", "--env ftp_proxy=", "--env ALL_PROXY=", "--env all_proxy=", "--env NO_PROXY=", "--env no_proxy="} {
		if !strings.Contains(string(arguments), expected) {
			t.Errorf("Docker arguments do not contain %q", expected)
		}
	}
}

func TestCreateRejectsOccupiedUnrelatedNameBeforeDockerCreate(t *testing.T) {
	fixture := newFakeDocker(t, true, false)
	docker, err := New(Config{Image: "fixture", DockerExecutable: fixture.executable})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := docker.Create(context.Background(), testClaim(), nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "unrelated") {
		t.Fatalf("Create() error = %v, want unrelated-name refusal", err)
	}
	arguments, err := os.ReadFile(fixture.arguments)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(arguments), "create ") {
		t.Fatal("Create() performed a Docker create after finding an unrelated object")
	}
}

func TestReconcileRequiresOwnedLabelsForNameAndLegacyID(t *testing.T) {
	fixture := newFakeDocker(t, true, false)
	docker, err := New(Config{Image: "fixture", DockerExecutable: fixture.executable})
	if err != nil {
		t.Fatal(err)
	}
	for _, identifier := range []string{testName, testContainer[:12]} {
		if err := docker.Reconcile(context.Background(), identifier); err == nil || !strings.Contains(err.Error(), "not owned") {
			t.Errorf("Reconcile(%q) error = %v, want ownership refusal", identifier, err)
		}
	}
	arguments, err := os.ReadFile(fixture.arguments)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(arguments), "stop ") || strings.Contains(string(arguments), "rm ") {
		t.Fatal("Reconcile() operated on an unrelated container")
	}
}

func TestReconcileRetainsUncertaintyWhenDockerStillReportsTheContainer(t *testing.T) {
	fixture := newFakeDocker(t, true, true)
	if err := os.WriteFile(fixture.keepOnRemove, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	docker, err := New(Config{Image: "fixture", DockerExecutable: fixture.executable})
	if err != nil {
		t.Fatal(err)
	}
	if err := docker.Reconcile(context.Background(), testName); err == nil || !strings.Contains(err.Error(), "did not confirm") {
		t.Fatalf("Reconcile() error = %v, want removal uncertainty", err)
	}
	if _, err := os.Stat(fixture.marker); err != nil {
		t.Fatalf("fixture removed the container despite the configured uncertain outcome: %v", err)
	}
}

func TestReconcileDoesNotAcknowledgeAnAbsentReservedName(t *testing.T) {
	fixture := newFakeDocker(t, false, true)
	docker, err := New(Config{Image: "fixture", DockerExecutable: fixture.executable})
	if err != nil {
		t.Fatal(err)
	}
	if err := docker.Reconcile(context.Background(), testName); !errors.Is(err, ErrCreateUncertain) {
		t.Fatalf("Reconcile(reserved name) error = %v, want ErrCreateUncertain", err)
	}
	if err := docker.Reconcile(context.Background(), testContainer); err != nil {
		t.Fatalf("Reconcile(confirmed immutable ID) error = %v, want nil", err)
	}
}

func TestCreateFailureResponsePreservesUncertaintyAndCatchesLateContainer(t *testing.T) {
	fixture := newFakeDocker(t, false, true)
	script, err := os.ReadFile(fixture.executable)
	if err != nil {
		t.Fatal(err)
	}
	original := fmt.Sprintf("create) touch %q; printf '%%s\\n' '%s'; exit 0 ;;", fixture.marker, testContainer)
	failedCreate := "create) printf 'synthetic daemon failure\\n' >&2; exit 1 ;;"
	replaced := strings.Replace(string(script), original, failedCreate, 1)
	if replaced == string(script) {
		t.Fatal("could not install the failed fake create response")
	}
	if err := os.WriteFile(fixture.executable, []byte(replaced), 0700); err != nil {
		t.Fatal(err)
	}
	docker, err := New(Config{Image: "fixture", DockerExecutable: fixture.executable})
	if err != nil {
		t.Fatal(err)
	}
	_, err = docker.Create(context.Background(), testClaim(), nil, t.TempDir())
	if !errors.Is(err, ErrCreateUncertain) {
		t.Fatalf("Create() error = %v, want ErrCreateUncertain", err)
	}
	if err := docker.Reconcile(context.Background(), testName); !errors.Is(err, ErrCreateUncertain) {
		t.Fatalf("Reconcile before late container appears error = %v, want ErrCreateUncertain", err)
	}
	if _, err := os.Stat(fixture.marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed fake create unexpectedly left a container marker: %v", err)
	}
	// Model the daemon completing a create after the client received an ambiguous failure.
	if err := os.WriteFile(fixture.marker, []byte("late create"), 0600); err != nil {
		t.Fatalf("materialize the late fake container: %v", err)
	}
	if err := docker.Reconcile(context.Background(), testName); err != nil {
		t.Fatalf("Reconcile after delayed side effect: %v", err)
	}
	if _, err := os.Stat(fixture.marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late-created sandbox remains: %v", err)
	}
}

func TestStopEscalatesToKillAndVerifiesTheStoppedState(t *testing.T) {
	fixture := newFakeDocker(t, false, true)
	if err := os.WriteFile(fixture.ignoreStop, []byte("ignore"), 0600); err != nil {
		t.Fatal(err)
	}
	docker, err := New(Config{Image: "fixture", DockerExecutable: fixture.executable})
	if err != nil {
		t.Fatal(err)
	}
	process, err := docker.Create(context.Background(), testClaim(), nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := process.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(fixture.arguments)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(arguments), "stop --time 2 "+testContainer) || !strings.Contains(string(arguments), "kill "+testContainer) {
		t.Fatalf("stop did not escalate as expected: %s", arguments)
	}
	if err := process.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceArchiveRejectsSymlinkAndCopiesRegularFile(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "fixture.txt"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	archiveBytes, err := workspaceArchive(workspace, 1024)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(strings.NewReader(string(archiveBytes)))
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "fixture.txt" {
		t.Fatalf("archive entry = %q, want fixture.txt", header.Name)
	}
	content, err := io.ReadAll(reader)
	if err != nil || string(content) != "fixture" {
		t.Fatalf("archive entry contents = %q, error = %v", content, err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(workspace, "secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceArchive(workspace, 1024); err == nil || !strings.Contains(err.Error(), "unsupported file") {
		t.Fatalf("workspaceArchive() error = %v, want symlink refusal", err)
	}
}

func TestIndependentWatchdogSubprocessGetsRenewalAndDisarmWithoutCredentials(t *testing.T) {
	fixture := newFakeDocker(t, false, false)
	watchdogExecutable := filepath.Join(t.TempDir(), "watchdog-fixture")
	watchdogLog := filepath.Join(t.TempDir(), "watchdog-input")
	environmentLeak := filepath.Join(t.TempDir(), "watchdog-environment-leak")
	watchdogScript := fmt.Sprintf("#!/bin/sh\nif [ -n \"${OPENAI_API_KEY+x}\" ] || [ -n \"${ANTHROPIC_API_KEY+x}\" ] || [ -n \"${SHIPMUNK_RUNNER_TOKEN+x}\" ] || [ -n \"${SHIPMUNK_CONTROL_PLANE_TOKEN+x}\" ] || [ -n \"${GITHUB_TOKEN+x}\" ]; then touch %q; fi\nenv | sort > %q\nprintf 'READY\\n'\ncat >> %q\n", environmentLeak, watchdogLog, watchdogLog)
	if err := os.WriteFile(watchdogExecutable, []byte(watchdogScript), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "synthetic-secret")
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-secret")
	t.Setenv("SHIPMUNK_RUNNER_TOKEN", "synthetic-control-plane-secret")
	t.Setenv("SHIPMUNK_CONTROL_PLANE_TOKEN", "synthetic-control-plane-secret")
	t.Setenv("GITHUB_TOKEN", "synthetic-github-secret")
	setSyntheticDockerEnvironment(t)
	watchdog := NewWatchdog(Config{
		DockerExecutable:   fixture.executable,
		WatchdogExecutable: watchdogExecutable,
	})
	lease, err := watchdog.Arm(testName, time.Now().Add(time.Minute), time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Renew(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := lease.Disarm(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(watchdogLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "renew:") || !strings.Contains(string(contents), "disarm") {
		t.Fatalf("watchdog control stream = %q, want renewal and disarm", contents)
	}
	if _, err := os.Stat(environmentLeak); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("watchdog inherited credentials: stat error = %v", err)
	}
	for _, expected := range []string{"HOME=/synthetic/docker-home", "DOCKER_HOST=tcp://docker.example.test:2376", "DOCKER_CONTEXT=synthetic-context", "DOCKER_CONFIG=/synthetic/docker-config", "DOCKER_CERT_PATH=/synthetic/docker-certs", "DOCKER_TLS_VERIFY=1"} {
		if !strings.Contains(string(contents), expected) {
			t.Errorf("watchdog environment omitted %q: %s", expected, contents)
		}
	}
	for _, forbidden := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "SHIPMUNK_RUNNER_TOKEN", "SHIPMUNK_CONTROL_PLANE_TOKEN", "GITHUB_TOKEN"} {
		if strings.Contains(string(contents), forbidden) {
			t.Errorf("watchdog environment included %s", forbidden)
		}
	}
}

func TestRunWatchdogTreatsPipeClosureAsParentDeathAndRemovesOwnedSandbox(t *testing.T) {
	fixture := newFakeDocker(t, true, true)
	if err := os.WriteFile(fixture.marker, []byte("present"), 0600); err != nil {
		t.Fatal(err)
	}
	docker := fixture.executable
	arguments := []string{
		"--docker", docker,
		"--name", testName,
		"--lease", fmt.Sprint(time.Now().Add(time.Minute).UnixNano()),
		"--deadline", fmt.Sprint(time.Now().Add(2 * time.Minute).UnixNano()),
		"--poll-interval", "1ms",
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- RunWatchdog(arguments, reader)
	}()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not complete cleanup after parent pipe closure")
	}
	if _, err := os.Stat(fixture.marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("watchdog left sandbox present: %v", err)
	}
}

func TestProfileWatchdogRemovesOnlyProfileOwnedSandbox(t *testing.T) {
	for _, test := range []struct {
		name      string
		label     string
		wantError bool
	}{
		{name: "owned profile container", label: "true"},
		{name: "unrelated profile container", label: "false", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			name := "shipmunk-profile-01k4w000000000000000000001"
			present := filepath.Join(root, "present")
			running := filepath.Join(root, "running")
			docker := filepath.Join(root, "docker-fixture")
			script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  inspect)
    if [ -f %q ]; then
      state=false
      if [ -f %q ]; then state=true; fi
      cat <<JSON
[{"Id":"%s","Name":"/%s","Config":{"Labels":{"shipmunk.profile-runtime":"%s","shipmunk.profile-sandbox":"%s"}},"State":{"Running":$state}}]
JSON
      exit 0
    fi
    echo "Error: No such object: $2" >&2
    exit 1
    ;;
  stop) rm -f %q; exit 0 ;;
  kill) rm -f %q; exit 0 ;;
  rm) rm -f %q %q; exit 0 ;;
  *) exit 0 ;;
esac
`, present, running, testContainer, name, test.label, name, running, running, present, running)
			if err := os.WriteFile(docker, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(present, []byte("present"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(running, []byte("running"), 0600); err != nil {
				t.Fatal(err)
			}
			arguments := []string{
				"--docker", docker, "--name", name, "--profile-mode", "true",
				"--lease", fmt.Sprint(time.Now().Add(time.Minute).UnixNano()),
				"--deadline", fmt.Sprint(time.Now().Add(2 * time.Minute).UnixNano()),
				"--poll-interval", "1ms",
			}
			reader, writer := io.Pipe()
			done := make(chan error, 1)
			go func() { done <- RunWatchdog(arguments, reader) }()
			_ = writer.Close()
			select {
			case err := <-done:
				if (err != nil) != test.wantError {
					t.Fatalf("RunWatchdog() error = %v, wantError %v", err, test.wantError)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("profile watchdog did not complete cleanup")
			}
			_, statErr := os.Stat(present)
			if test.wantError && statErr != nil {
				t.Fatal("profile watchdog removed an unrelated container")
			}
			if !test.wantError && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("profile watchdog left its owned container: %v", statErr)
			}
		})
	}
}

func TestProfileWatchdogWaitsForDelayedCreateAfterParentDeath(t *testing.T) {
	root := t.TempDir()
	name := "shipmunk-profile-01k4w000000000000000000001"
	present := filepath.Join(root, "present")
	running := filepath.Join(root, "running")
	docker := filepath.Join(root, "docker-fixture")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  inspect)
    if [ -f %q ]; then
      state=false
      if [ -f %q ]; then state=true; fi
      cat <<JSON
[{"Id":"%s","Name":"/%s","Config":{"Labels":{"shipmunk.profile-runtime":"true","shipmunk.profile-sandbox":"%s"}},"State":{"Running":$state}}]
JSON
      exit 0
    fi
    echo "Error: No such object: $2" >&2
    exit 1
    ;;
  stop) rm -f %q; exit 0 ;;
  kill) rm -f %q; exit 0 ;;
  rm) rm -f %q %q; exit 0 ;;
  *) exit 0 ;;
esac
`, present, running, testContainer, name, name, running, running, present, running)
	if err := os.WriteFile(docker, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"--docker", docker, "--name", name, "--profile-mode", "true",
		"--lease", fmt.Sprint(time.Now().Add(time.Minute).UnixNano()),
		"--deadline", fmt.Sprint(time.Now().Add(2 * time.Minute).UnixNano()),
		"--poll-interval", "1ms",
	}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- RunWatchdogCommand(arguments, inputReader, outputWriter) }()
	output := bufio.NewReader(outputReader)
	if ready, err := output.ReadString('\n'); err != nil || strings.TrimSpace(ready) != "READY" {
		t.Fatalf("watchdog readiness = %q, %v", ready, err)
	}
	if _, err := io.WriteString(inputWriter, "phase:1:started:\n"); err != nil {
		t.Fatal(err)
	}
	if ack, err := output.ReadString('\n'); err != nil || strings.TrimSpace(ack) != "ACK:1" {
		t.Fatalf("create-phase acknowledgement = %q, %v", ack, err)
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("profile watchdog treated temporary absence as proof after parent death: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	if err := os.WriteFile(present, []byte("late create"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(running, []byte("running"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("profile watchdog did not stop the delayed container")
	}
	if _, err := os.Stat(present); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile watchdog left the delayed container: %v", err)
	}
}

func TestArmProfileLaunchesWatchdogInExplicitProfileOwnershipMode(t *testing.T) {
	fixture := newFakeDocker(t, false, true)
	root := t.TempDir()
	watchdogBinary := filepath.Join(root, "watchdog-fixture")
	argumentsPath := filepath.Join(root, "watchdog-arguments")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" > %q\nprintf 'READY\\n'\ncat >/dev/null\n", argumentsPath)
	if err := os.WriteFile(watchdogBinary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	watchdog := NewWatchdog(Config{
		DockerExecutable:   fixture.executable,
		WatchdogExecutable: watchdogBinary,
		CommandTimeout:     time.Second,
	})
	name := "shipmunk-profile-01k4w000000000000000000001"
	lease, err := watchdog.ArmProfile(name, time.Now().Add(time.Minute), time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Disarm(); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(arguments), "--profile-mode true") || !strings.Contains(string(arguments), "--name "+name) {
		t.Fatalf("watchdog arguments omitted explicit profile ownership: %q", arguments)
	}
}

func TestWatchdogParsesExclusiveCodexOwnershipMode(t *testing.T) {
	name := "shipmunk-codex-01k4w000000000000000000001-7"
	arguments := []string{"--docker", "docker", "--name", name, "--profile-mode", "false", "--codex-mode", "true", "--lease", fmt.Sprint(time.Now().Add(time.Minute).UnixNano()), "--deadline", fmt.Sprint(time.Now().Add(2 * time.Minute).UnixNano()), "--poll-interval", "100ms"}
	options, err := parseWatchdogArguments(arguments)
	if err != nil || !options.codex || options.profile || options.name != name {
		t.Fatalf("Codex watchdog options = %+v, %v", options, err)
	}
	arguments[5] = "true"
	if _, err := parseWatchdogArguments(arguments); err == nil {
		t.Fatal("watchdog accepted overlapping profile and Codex ownership")
	}
}

func TestCodexWatchdogParentDeathRemovesOwnedSiblingTopology(t *testing.T) {
	root := t.TempDir()
	name := "shipmunk-codex-01k4w000000000000000000001-7"
	for _, suffix := range []string{"", "-repo", "-diff", "-workspace"} {
		if err := os.WriteFile(filepath.Join(root, name+suffix), []byte("present"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	docker := filepath.Join(root, "docker")
	log := filepath.Join(root, "docker.log")
	script := fmt.Sprintf(`#!/bin/sh
root=%q
owner=%q
printf '%%s\n' "$*" >> %q
if [ "$1" = volume ]; then
  target="$5"
  if [ "$2" = inspect ]; then
    [ -f "$root/$target" ] || { echo 'No such volume' >&2; exit 1; }
    printf '%%s\n' "$owner"; exit 0
  fi
  [ "$2" = rm ] && { rm -f "$root/$3"; exit 0; }
fi
target="$2"
if [ "$1" = inspect ]; then
  case "$target" in
    111111*) target="$owner" ;;
    222222*) target="$owner-repo" ;;
    333333*) target="$owner-diff" ;;
  esac
  [ -f "$root/$target" ] || { echo 'No such object' >&2; exit 1; }
  id=1111111111111111111111111111111111111111111111111111111111111111
  case "$target" in *-repo) id=2222222222222222222222222222222222222222222222222222222222222222 ;; *-diff) id=3333333333333333333333333333333333333333333333333333333333333333 ;; esac
  printf '[{"Id":"%%s","Name":"/%%s","Config":{"Labels":{"shipmunk.codex":"true","shipmunk.codex-owner":"%%s"}},"State":{"Running":false}}]\n' "$id" "$target" "$owner"
  exit 0
fi
[ "$1" = rm ] && { target="$3"; case "$target" in 111111*) target="$owner" ;; 222222*) target="$owner-repo" ;; 333333*) target="$owner-diff" ;; esac; rm -f "$root/$target"; exit 0; }
[ "$1" = stop ] && exit 0
exit 1
`, root, name, log)
	if err := os.WriteFile(docker, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"--docker", docker, "--name", name, "--profile-mode", "false", "--codex-mode", "true", "--lease", fmt.Sprint(time.Now().Add(time.Minute).UnixNano()), "--deadline", fmt.Sprint(time.Now().Add(2 * time.Minute).UnixNano()), "--poll-interval", "10ms"}
	if err := RunWatchdog(arguments, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-repo", "-diff", "-workspace"} {
		if _, err := os.Stat(filepath.Join(root, name+suffix)); !errors.Is(err, os.ErrNotExist) {
			calls, _ := os.ReadFile(log)
			t.Fatalf("watchdog left Codex resource %s present: %v\n%s", suffix, err, calls)
		}
	}
	for _, suffix := range []string{"", "-repo", "-diff", "-workspace"} {
		if err := os.WriteFile(filepath.Join(root, name+suffix), []byte("present"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- (&Docker{config: Config{DockerExecutable: docker, CommandTimeout: time.Second, PollInterval: 10 * time.Millisecond}}).cleanupCodexWatchdog(name, true)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Codex watchdog did not clear create-in-flight after removing the owned volume")
	}
}

func TestRunWatchdogWaitsThroughAbsentBeforeCreateWindow(t *testing.T) {
	fixture := newFakeDocker(t, false, true)
	arguments := watchdogTestArguments(fixture.executable, time.Now().Add(time.Minute), time.Now().Add(2*time.Minute))
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- RunWatchdog(arguments, reader)
	}()
	select {
	case err := <-done:
		t.Fatalf("watchdog exited while the supervisor was alive and the name was absent: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(fixture.marker, []byte("late create"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "phase:1:started:\nphase:2:finished:\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not remove the container created during the absent-before-create window")
	}
	if _, err := os.Stat(fixture.marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("watchdog left late-created sandbox present: %v", err)
	}
}

func TestRunWatchdogRejectsDisarmUntilSandboxAbsence(t *testing.T) {
	fixture := newFakeDocker(t, true, true)
	arguments := watchdogTestArguments(fixture.executable, time.Now().Add(time.Minute), time.Now().Add(2*time.Minute))
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- RunWatchdog(arguments, reader)
	}()
	if _, err := io.WriteString(writer, "disarm\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("watchdog exited while its sandbox was present: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := os.Stat(fixture.marker); err != nil {
		t.Fatalf("watchdog removed a sandbox before parent-death cleanup: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not clean up after the control pipe closed")
	}
}

func TestRunWatchdogRetainsUnconfirmedCreateAfterParentDeath(t *testing.T) {
	if os.Getenv("SHIPMUNK_WATCHDOG_UNCERTAIN_HELPER") == "1" {
		_ = RunWatchdog(watchdogTestArguments(os.Getenv("SHIPMUNK_WATCHDOG_DOCKER"), time.Now().Add(time.Minute), time.Now().Add(2*time.Minute)), os.Stdin)
		return
	}
	fixture := newFakeDocker(t, false, true)
	command := exec.Command(os.Args[0], "-test.run=^TestRunWatchdogRetainsUnconfirmedCreateAfterParentDeath$")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"SHIPMUNK_WATCHDOG_UNCERTAIN_HELPER=1",
		"SHIPMUNK_WATCHDOG_DOCKER=" + fixture.executable,
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, "phase:1:started:\n"); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("watchdog exited after parent death with an unconfirmed create: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("test watchdog process unexpectedly exited successfully after kill")
	}
}

func TestWatchdogRejectsExpiredLeaseBeforeReadinessAndBoundsFrames(t *testing.T) {
	arguments := watchdogTestArguments("unused", time.Now().Add(-time.Second), time.Now().Add(time.Minute))
	var output strings.Builder
	if err := RunWatchdogCommand(arguments, strings.NewReader(""), &output); err == nil {
		t.Fatal("RunWatchdogCommand accepted an expired lease")
	}
	if output.Len() != 0 {
		t.Fatalf("expired watchdog emitted readiness before validation: %q", output.String())
	}
	if _, err := readBoundedLine(bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 4096)), 32), 128); !errors.Is(err, errLineTooLong) {
		t.Fatalf("readBoundedLine error = %v, want frame-size rejection", err)
	}
}

func TestWatchdogControlWritesAndDisarmAreBounded(t *testing.T) {
	fixture := newFakeDocker(t, false, true)
	const timeout = time.Second
	docker, err := New(Config{Image: "fixture", DockerExecutable: fixture.executable, CommandTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	blocked := newTestWatchdogControl(true)
	lease := &Lease{docker: docker, name: testName, stdin: blocked, done: make(chan struct{})}
	started := time.Now()
	if err := lease.Renew(time.Now().Add(time.Minute)); err == nil {
		t.Fatal("Renew() succeeded while its control pipe was blocked")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("Renew() exceeded its bounded control-write timeout")
	}
	select {
	case <-blocked.closed:
	default:
		t.Fatal("timed-out renewal did not close the blocked parent pipe")
	}

	control := newTestWatchdogControl(false)
	lease = &Lease{docker: docker, name: testName, stdin: control, done: make(chan struct{})}
	started = time.Now()
	if err := lease.Disarm(); err == nil || !strings.Contains(err.Error(), "still pending") {
		t.Fatalf("Disarm() error = %v, want bounded pending-shutdown error", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("Disarm() waited unboundedly for watchdog exit")
	}
	select {
	case <-lease.done:
		t.Fatal("Disarm() closed/killed the independent cleanup watchdog")
	default:
	}
}

func TestLeasePhaseTransitionsWaitForAcknowledgment(t *testing.T) {
	fixture := newFakeDocker(t, false, true)
	watchdogBinary := filepath.Join(t.TempDir(), "watchdog-ack-fixture")
	script := `#!/bin/sh
printf 'READY\n'
while IFS= read -r line; do
  case "$line" in
    phase:*)
      sleep 0.05
      rest=${line#phase:}
      sequence=${rest%%:*}
      printf 'ACK:%s\n' "$sequence"
      ;;
    disarm) exit 0 ;;
  esac
done
`
	if err := os.WriteFile(watchdogBinary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	watchdog := NewWatchdog(Config{
		DockerExecutable:   fixture.executable,
		WatchdogExecutable: watchdogBinary,
		CommandTimeout:     time.Second,
		PollInterval:       10 * time.Millisecond,
	})
	lease, err := watchdog.Arm(testName, time.Now().Add(time.Minute), time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := lease.CreateStarted(); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 40*time.Millisecond {
		t.Fatal("CreateStarted() returned before the watchdog acknowledgment")
	}
	started = time.Now()
	if err := lease.CreateFinished(testContainer); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 40*time.Millisecond {
		t.Fatal("CreateFinished() returned before the watchdog acknowledgment")
	}
	if err := lease.Disarm(); err != nil {
		t.Fatal(err)
	}
}

func TestLeasePhaseTimeoutRetainsUncertaintyAndRefusesDisarm(t *testing.T) {
	fixture := newFakeDocker(t, false, true)
	watchdogBinary := filepath.Join(t.TempDir(), "watchdog-no-ack-fixture")
	script := "#!/bin/sh\nprintf 'READY\\n'\nIFS= read -r line\ncat >/dev/null\n"
	if err := os.WriteFile(watchdogBinary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	watchdog := NewWatchdog(Config{
		DockerExecutable:   fixture.executable,
		WatchdogExecutable: watchdogBinary,
		CommandTimeout:     time.Second,
	})
	lease, err := watchdog.Arm(testName, time.Now().Add(time.Minute), time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.CreateStarted(); err == nil || !strings.Contains(err.Error(), "acknowledgment timed out") {
		t.Fatalf("CreateStarted() error = %v, want acknowledgment timeout", err)
	}
	if err := lease.Disarm(); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("Disarm() error = %v, want create uncertainty refusal", err)
	}
	_ = lease.stdin.Close()
	if err := lease.waitForExitBounded(time.Second); err != nil {
		t.Fatalf("wait for fake watchdog after closing its control pipe: %v", err)
	}
}

func TestLeasePhaseRejectsWatchdogEOFBeforeAcknowledgment(t *testing.T) {
	fixture := newFakeDocker(t, false, true)
	watchdogBinary := filepath.Join(t.TempDir(), "watchdog-eof-fixture")
	script := "#!/bin/sh\nprintf 'READY\\n'\nIFS= read -r line\nsleep 0.1\nexit 0\n"
	if err := os.WriteFile(watchdogBinary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	watchdog := NewWatchdog(Config{
		DockerExecutable:   fixture.executable,
		WatchdogExecutable: watchdogBinary,
		CommandTimeout:     time.Second,
	})
	lease, err := watchdog.Arm(testName, time.Now().Add(time.Minute), time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.CreateStarted(); err == nil {
		t.Fatal("CreateStarted() succeeded without a watchdog acknowledgment")
	}
	if err := lease.waitForExitBounded(time.Second); err != nil {
		t.Fatalf("wait for EOF fixture: %v", err)
	}
	if err := lease.Disarm(); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("Disarm() error = %v, want create uncertainty refusal", err)
	}
}

type testWatchdogControl struct {
	closed chan struct{}
	once   sync.Once
	block  bool
}

func newTestWatchdogControl(block bool) *testWatchdogControl {
	return &testWatchdogControl{closed: make(chan struct{}), block: block}
}

func (control *testWatchdogControl) Write(value []byte) (int, error) {
	if control.block {
		<-control.closed
		return 0, errors.New("control channel closed")
	}
	return len(value), nil
}

func (control *testWatchdogControl) Close() error {
	control.once.Do(func() { close(control.closed) })
	return nil
}

func TestDockerSandboxLifecycleLive(t *testing.T) {
	if os.Getenv("SHIPMUNK_SANDBOX_DOCKER_TEST") != "1" {
		t.Skip("set SHIPMUNK_SANDBOX_DOCKER_TEST=1 to use the designated Linux Docker engine")
	}
	image := os.Getenv("SHIPMUNK_RUNNER_IMAGE")
	watchdogExecutable := os.Getenv("SHIPMUNK_WATCHDOG_BINARY")
	if image == "" || watchdogExecutable == "" {
		t.Fatal("SHIPMUNK_RUNNER_IMAGE and SHIPMUNK_WATCHDOG_BINARY are required for the live sandbox test")
	}
	claim := liveTestClaim(t)
	t.Setenv("OPENAI_API_KEY", "synthetic-test-secret")
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-test-secret")
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "fixture.txt"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	docker, err := New(Config{
		Image:              image,
		DockerExecutable:   os.Getenv("SHIPMUNK_DOCKER_EXECUTABLE"),
		WatchdogExecutable: watchdogExecutable,
	})
	if err != nil {
		t.Fatal(err)
	}
	name, err := docker.Name(claim)
	if err != nil {
		t.Fatal(err)
	}
	watchdog := NewWatchdog(docker.config)
	lease, err := watchdog.Arm(name, time.Now().Add(2*time.Minute), time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var process *Process
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if process != nil {
			_ = process.Remove(ctx)
		}
		_ = lease.Disarm()
	})
	if err := lease.CreateStarted(); err != nil {
		t.Fatal(err)
	}
	process, err = docker.Create(context.Background(), claim, map[string]any{
		"fake_require_fixture": true,
		"safe_marker":          "copied",
	}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.CreateFinished(process.ContainerID()); err != nil {
		t.Fatal(err)
	}
	if process.ContainerID() == name || !containerIDPattern.MatchString(process.ContainerID()) {
		t.Fatalf("ContainerID() = %q, want an immutable full Docker ID", process.ContainerID())
	}
	inspection, err := docker.inspect(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	labels := inspection.Config.Labels
	if labels["shipmunk.runner"] != "true" || labels["shipmunk.run"] != claim.RunID ||
		labels["shipmunk.attempt"] != claim.AttemptID || labels["shipmunk.fence"] != fmt.Sprint(claim.Fence) {
		t.Fatalf("live sandbox labels do not match claim: %#v", labels)
	}
	var liveInspection []struct {
		Config struct {
			User string   `json:"User"`
			Env  []string `json:"Env"`
		} `json:"Config"`
		HostConfig struct {
			ReadonlyRootfs bool     `json:"ReadonlyRootfs"`
			NetworkMode    string   `json:"NetworkMode"`
			CapDrop        []string `json:"CapDrop"`
			SecurityOpt    []string `json:"SecurityOpt"`
			Memory         int64    `json:"Memory"`
			MemorySwap     int64    `json:"MemorySwap"`
			PidsLimit      int64    `json:"PidsLimit"`
			NanoCPUs       int64    `json:"NanoCpus"`
			Binds          []string `json:"Binds"`
		} `json:"HostConfig"`
	}
	inspectionResult, err := docker.run(context.Background(), docker.config.CommandTimeout, maxCommandOutputBytes, "inspect", name)
	if err != nil {
		t.Fatalf("inspect live sandbox: %v", err)
	}
	if err := json.Unmarshal([]byte(inspectionResult.stdout), &liveInspection); err != nil || len(liveInspection) != 1 {
		t.Fatalf("decode live sandbox inspection: %v", err)
	}
	container := liveInspection[0]
	if container.Config.User != "65532:65532" || !container.HostConfig.ReadonlyRootfs ||
		container.HostConfig.NetworkMode != "none" || len(container.HostConfig.CapDrop) != 1 || container.HostConfig.CapDrop[0] != "ALL" ||
		!contains(container.HostConfig.SecurityOpt, "no-new-privileges:true") || container.HostConfig.Memory != defaultMemoryBytes ||
		container.HostConfig.MemorySwap != defaultMemoryBytes || container.HostConfig.PidsLimit != 64 ||
		container.HostConfig.NanoCPUs != 1_000_000_000 || len(container.HostConfig.Binds) != 0 {
		t.Fatalf("live sandbox isolation does not match the configured boundary: %#v", container)
	}
	if contains(container.Config.Env, "OPENAI_API_KEY=synthetic-test-secret") || contains(container.Config.Env, "ANTHROPIC_API_KEY=synthetic-test-secret") {
		t.Fatalf("live sandbox inherited a provider credential: %#v", container.Config.Env)
	}
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "FTP_PROXY", "ftp_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		if !contains(container.Config.Env, name+"=") {
			t.Errorf("live sandbox proxy default %q was not explicitly cleared: %#v", name, container.Config.Env)
		}
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exitCode, output, err := process.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 || !strings.Contains(string(output), `"events":[]`) {
		t.Fatalf("live fixture returned exit=%d output=%q", exitCode, output)
	}
	if err := process.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lease.Disarm(); err != nil {
		t.Fatal(err)
	}
}

func TestDockerSandboxWatchdogReapsKilledSupervisor(t *testing.T) {
	if os.Getenv("SHIPMUNK_SANDBOX_WATCHDOG_HELPER") == "1" {
		runKilledSupervisorFixture(t)
		return
	}
	if os.Getenv("SHIPMUNK_SANDBOX_DOCKER_TEST") != "1" {
		t.Skip("set SHIPMUNK_SANDBOX_DOCKER_TEST=1 to use the designated Linux Docker engine")
	}
	image := os.Getenv("SHIPMUNK_RUNNER_IMAGE")
	watchdogExecutable := os.Getenv("SHIPMUNK_WATCHDOG_BINARY")
	if image == "" || watchdogExecutable == "" {
		t.Fatal("SHIPMUNK_RUNNER_IMAGE and SHIPMUNK_WATCHDOG_BINARY are required for the live watchdog test")
	}
	readyFile := filepath.Join(t.TempDir(), "watchdog-ready")
	command := exec.Command(os.Args[0], "-test.run=^TestDockerSandboxWatchdogReapsKilledSupervisor$")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"SHIPMUNK_SANDBOX_DOCKER_TEST=1",
		"SHIPMUNK_SANDBOX_WATCHDOG_HELPER=1",
		"SHIPMUNK_RUNNER_IMAGE=" + image,
		"SHIPMUNK_WATCHDOG_BINARY=" + watchdogExecutable,
		"SHIPMUNK_SANDBOX_READY_FILE=" + readyFile,
	}
	if dockerExecutable := os.Getenv("SHIPMUNK_DOCKER_EXECUTABLE"); dockerExecutable != "" {
		command.Env = append(command.Env, "SHIPMUNK_DOCKER_EXECUTABLE="+dockerExecutable)
	}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	helperDone := make(chan error, 1)
	go func() {
		helperDone <- command.Wait()
	}()
	readyDeadline := time.Now().Add(60 * time.Second)
	var sandboxName string
	for time.Now().Before(readyDeadline) {
		contents, err := os.ReadFile(readyFile)
		if err == nil {
			sandboxName = strings.TrimSpace(string(contents))
			break
		}
		select {
		case err := <-helperDone:
			t.Fatalf("watchdog test helper exited before readiness: %v", err)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sandboxName == "" {
		_ = command.Process.Kill()
		<-helperDone
		t.Fatal("watchdog test helper did not start the sandbox before the deadline")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-helperDone
	docker, err := New(Config{Image: image, DockerExecutable: os.Getenv("SHIPMUNK_DOCKER_EXECUTABLE")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = docker.Reconcile(ctx, sandboxName)
	})
	absentDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(absentDeadline) {
		_, err := docker.inspect(context.Background(), sandboxName)
		if errors.Is(err, errContainerAbsent) {
			return
		}
		if err != nil {
			t.Logf("watchdog sandbox inspection is still uncertain: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("independent watchdog left sandbox %s present after supervisor SIGKILL", sandboxName)
}

func runKilledSupervisorFixture(t *testing.T) {
	t.Helper()
	image := os.Getenv("SHIPMUNK_RUNNER_IMAGE")
	watchdogExecutable := os.Getenv("SHIPMUNK_WATCHDOG_BINARY")
	readyFile := os.Getenv("SHIPMUNK_SANDBOX_READY_FILE")
	if image == "" || watchdogExecutable == "" || readyFile == "" {
		t.Fatal("live watchdog helper configuration is incomplete")
	}
	claim := liveTestClaim(t)
	workspace := filepath.Dir(readyFile)
	docker, err := New(Config{
		Image:              image,
		DockerExecutable:   os.Getenv("SHIPMUNK_DOCKER_EXECUTABLE"),
		WatchdogExecutable: watchdogExecutable,
	})
	if err != nil {
		t.Fatal(err)
	}
	name, err := docker.Name(claim)
	if err != nil {
		t.Fatal(err)
	}
	watchdog := NewWatchdog(docker.config)
	lease, err := watchdog.Arm(name, time.Now().Add(90*time.Second), time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.CreateStarted(); err != nil {
		t.Fatal(err)
	}
	process, err := docker.Create(context.Background(), claim, map[string]any{"fake_mode": "sleep"}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.CreateFinished(process.ContainerID()); err != nil {
		t.Fatal(err)
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readyFile, []byte(name), 0600); err != nil {
		t.Fatal(err)
	}
	_ = lease
	select {}
}

func testClaim() protocol.Claim {
	return protocol.Claim{RunID: testRunID, AttemptID: testAttemptID, Fence: testFence}
}

func watchdogTestArguments(docker string, lease, deadline time.Time) []string {
	return []string{
		"--docker", docker,
		"--name", testName,
		"--lease", fmt.Sprint(lease.UnixNano()),
		"--deadline", fmt.Sprint(deadline.UnixNano()),
		"--poll-interval", "1ms",
	}
}

func liveTestClaim(t *testing.T) protocol.Claim {
	t.Helper()
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal(err)
	}
	attemptID := "01"
	for _, value := range bytes {
		attemptID += string(alphabet[int(value)%len(alphabet)])
	}
	bytes = make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal(err)
	}
	runID := "01"
	for _, value := range bytes {
		runID += string(alphabet[int(value)%len(alphabet)])
	}
	fence := time.Now().UnixNano() % 1_000_000_000
	if fence < 1 {
		fence = 1
	}
	return protocol.Claim{RunID: runID, AttemptID: attemptID, Fence: fence}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func setSyntheticDockerEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", "/synthetic/docker-home")
	t.Setenv("DOCKER_HOST", "tcp://docker.example.test:2376")
	t.Setenv("DOCKER_CONTEXT", "synthetic-context")
	t.Setenv("DOCKER_CONFIG", "/synthetic/docker-config")
	t.Setenv("DOCKER_CERT_PATH", "/synthetic/docker-certs")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
}

type fakeDockerFixture struct {
	executable        string
	marker            string
	running           string
	arguments         string
	clientEnvironment string
	environmentLeak   string
	keepOnRemove      string
	ignoreStop        string
}

func newFakeDocker(t *testing.T, present, owned bool) fakeDockerFixture {
	t.Helper()
	root := t.TempDir()
	fixture := fakeDockerFixture{
		executable:        filepath.Join(root, "docker-fixture"),
		marker:            filepath.Join(root, "present"),
		running:           filepath.Join(root, "running"),
		arguments:         filepath.Join(root, "arguments"),
		clientEnvironment: filepath.Join(root, "client-environment"),
		environmentLeak:   filepath.Join(root, "environment-leak"),
		keepOnRemove:      filepath.Join(root, "keep-on-remove"),
		ignoreStop:        filepath.Join(root, "ignore-stop"),
	}
	runID, attemptID, fence := testRunID, testAttemptID, testFence
	runnerLabel := "true"
	if !owned {
		runID, attemptID, fence = "01arz3ndektsv4rrffq69g5fax", "01arz3ndektsv4rrffq69g5fay", 8
		runnerLabel = "false"
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
if [ -n "${OPENAI_API_KEY+x}" ] || [ -n "${ANTHROPIC_API_KEY+x}" ] || [ -n "${SHIPMUNK_RUNNER_TOKEN+x}" ] || [ -n "${SHIPMUNK_CONTROL_PLANE_TOKEN+x}" ] || [ -n "${GITHUB_TOKEN+x}" ]; then touch %q; fi
env | sort > %q
case "$1" in
  inspect)
    if [ -f %q ]; then
      running=false
      if [ -f %q ]; then running=true; fi
      cat <<JSON
[{"Id":"%s","Name":"/%s","Config":{"Image":"fixture","Labels":{"shipmunk.runner":"%s","shipmunk.run":"%s","shipmunk.attempt":"%s","shipmunk.fence":"%d"}},"State":{"Running":$running,"ExitCode":7}}]
JSON
      exit 0
    fi
    echo "Error: No such object: $2" >&2
    exit 1
    ;;
  create) touch %q; printf '%%s\n' '%s'; exit 0 ;;
  start) touch %q %q; exit 0 ;;
  exec) cat >/dev/null; exit 0 ;;
  wait) rm -f %q; printf '7\n'; exit 0 ;;
  logs) printf 'synthetic output'; exit 0 ;;
  stop) if [ ! -f %q ]; then rm -f %q; fi; exit 0 ;;
  kill) rm -f %q; exit 0 ;;
  rm) if [ ! -f %q ]; then rm -f %q %q; fi; exit 0 ;;
  *) exit 0 ;;
esac
`, fixture.arguments, fixture.environmentLeak, fixture.clientEnvironment, fixture.marker, fixture.running, testContainer, testName, runnerLabel, runID, attemptID, fence, fixture.marker, testContainer, fixture.marker, fixture.running, fixture.running, fixture.ignoreStop, fixture.running, fixture.running, fixture.keepOnRemove, fixture.marker, fixture.running)
	if err := os.WriteFile(fixture.executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if present {
		if err := os.WriteFile(fixture.marker, []byte("present"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}
