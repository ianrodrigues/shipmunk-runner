package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	proxyControlCleanupBudget  = 20 * time.Second
	proxyControlCommandTimeout = 3 * time.Second
	proxyControlRetryDelay     = 250 * time.Millisecond
)

type proxyControlDockerCommand func(context.Context, ...string) (string, string, error)

type proxyControlInspection struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func TestDockerConfiguredProxyCredentialsDoNotEnterSandbox(t *testing.T) {
	if os.Getenv("SHIPMUNK_SANDBOX_DOCKER_TEST") != "1" {
		t.Skip("requires designated Linux Docker engine and synthetic runtime image")
	}
	executable := os.Getenv("SHIPMUNK_DOCKER_EXECUTABLE")
	if executable == "" {
		executable = "docker"
	}
	executable, err := exec.LookPath(executable)
	if err != nil {
		t.Fatal(err)
	}
	query := func(arguments ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, arguments...)
		command.Env = ClientEnvironment()
		output, err := command.Output()
		if err != nil {
			t.Fatalf("query designated Docker client: %v", err)
		}
		return strings.TrimSpace(string(output))
	}
	engineID := query("info", "--format", "{{.ID}}")
	activeContext := query("context", "show")
	if engineID == "" || activeContext == "" {
		t.Fatal("cannot identify designated Docker engine and context")
	}
	originalConfig := os.Getenv("DOCKER_CONFIG")
	if originalConfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		originalConfig = filepath.Join(home, ".docker")
	}
	originalConfig, err = filepath.Abs(originalConfig)
	if err != nil {
		t.Fatal(err)
	}
	configDirectory := t.TempDir()
	// Reuse the existing context store read-only, without copying credentials or
	// changing endpoint selection. The user's config.json is never read or edited.
	contexts := filepath.Join(originalConfig, "contexts")
	if info, err := os.Stat(contexts); err == nil && info.IsDir() {
		if err := os.Symlink(contexts, filepath.Join(configDirectory, "contexts")); err != nil {
			t.Fatal(err)
		}
	} else if activeContext != "default" {
		t.Fatal("named Docker context has no accessible context store")
	}
	proxy := "http://synthetic-user:synthetic-proxy-secret@proxy.invalid:3128"
	configuration, err := json.Marshal(map[string]any{
		"currentContext": activeContext,
		"proxies": map[string]any{"default": map[string]string{
			"httpProxy": proxy, "httpsProxy": proxy, "ftpProxy": proxy,
			"allProxy": proxy, "noProxy": "private.synthetic.internal",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDirectory, "config.json"), configuration, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", configDirectory)
	if selected := query("info", "--format", "{{.ID}}"); selected != engineID {
		t.Fatal("synthetic proxy config changed the designated engine; refusing container creation")
	}
	config := Config{Image: os.Getenv("SHIPMUNK_RUNNER_IMAGE"), DockerExecutable: executable, WatchdogExecutable: os.Getenv("SHIPMUNK_WATCHDOG_BINARY")}
	if config.Image == "" || config.WatchdogExecutable == "" {
		t.Fatal("synthetic image and watchdog executable are required")
	}
	if query("image", "inspect", "--format", "{{.Id}}", config.Image) == "" {
		t.Fatal("synthetic runtime image is not available locally; refusing any pull")
	}
	claim := liveTestClaim(t)
	controlName := fmt.Sprintf("shipmunk-proxy-control-%s-%d", claim.AttemptID, claim.Fence)
	var controlID string
	cleanupCommand := proxyControlDockerCommand(func(ctx context.Context, arguments ...string) (string, string, error) {
		command := exec.CommandContext(ctx, executable, arguments...)
		command.Env = ClientEnvironment()
		output, err := command.CombinedOutput()
		if err != nil {
			return "", string(output), err
		}
		return string(output), "", nil
	})
	t.Cleanup(func() {
		if err := cleanupProxyControlContainer(cleanupCommand, controlName, claim.AttemptID, controlID, proxyControlCleanupBudget, proxyControlCommandTimeout, proxyControlRetryDelay); err != nil {
			t.Errorf("clean up synthetic proxy control container: %v", err)
		}
	})
	controlID = query("create", "--pull", "never", "--name", controlName, "--label", "shipmunk.proxy-test=true", "--label", "shipmunk.proxy-test-attempt="+claim.AttemptID, "--network", "none", config.Image)
	if !isFullContainerID(controlID) {
		t.Fatal("Docker did not return an immutable ID for the positive-control container")
	}
	var positiveControl struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Config struct {
			Env    []string          `json:"Env"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal([]byte(query("inspect", "--format", "{{json .}}", controlID)), &positiveControl); err != nil {
		t.Fatal("decode positive-control inspection")
	}
	if positiveControl.ID != controlID || positiveControl.Name != "/"+controlName ||
		positiveControl.Config.Labels["shipmunk.proxy-test"] != "true" ||
		positiveControl.Config.Labels["shipmunk.proxy-test-attempt"] != claim.AttemptID {
		t.Fatal("positive-control container ownership could not be confirmed")
	}
	expectedProxy := map[string]string{
		"HTTP_PROXY": proxy, "http_proxy": proxy,
		"HTTPS_PROXY": proxy, "https_proxy": proxy,
		"FTP_PROXY": proxy, "ftp_proxy": proxy,
		"ALL_PROXY": proxy, "all_proxy": proxy,
		"NO_PROXY": "private.synthetic.internal", "no_proxy": "private.synthetic.internal",
	}
	for name, expected := range expectedProxy {
		value, found := lookupEnvironment(positiveControl.Config.Env, name)
		if !found || value != expected {
			t.Errorf("plain control container did not receive synthetic Docker proxy default %q", name)
		}
	}
	docker, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	name, err := docker.Name(claim)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := NewWatchdog(config).Arm(name, time.Now().Add(time.Minute), time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.CreateStarted(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	process, err := docker.Create(ctx, claim, map[string]any{}, t.TempDir())
	if err != nil {
		if !errors.Is(err, ErrCreateUncertain) {
			_ = lease.CreateFinished("")
			_ = lease.Disarm()
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if err := process.Remove(cleanupCtx); err != nil {
			t.Error(err)
		}
		_ = lease.Disarm()
	})
	if err := lease.CreateFinished(process.ContainerID()); err != nil {
		t.Fatal(err)
	}
	var environment []string
	if err := json.Unmarshal([]byte(query("inspect", "--format", "{{json .Config.Env}}", process.ContainerID())), &environment); err != nil {
		t.Fatal(err)
	}
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		if strings.Contains(value, "synthetic-proxy-secret") || strings.Contains(value, "private.synthetic.internal") {
			t.Fatal("Docker client proxy configuration leaked into sandbox")
		}
		if strings.HasSuffix(strings.ToUpper(key), "_PROXY") && value != "" {
			t.Errorf("sandbox retained nonempty proxy variable %s", key)
		}
	}
}

func lookupEnvironment(environment []string, name string) (string, bool) {
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if found && key == name {
			return value, true
		}
	}
	return "", false
}

func cleanupProxyControlContainer(
	run proxyControlDockerCommand,
	name string,
	attemptID string,
	knownID string,
	budget time.Duration,
	commandTimeout time.Duration,
	retryDelay time.Duration,
) error {
	if run == nil || name == "" || attemptID == "" || budget <= 0 || commandTimeout <= 0 || retryDelay <= 0 {
		return errors.New("proxy control cleanup configuration is invalid")
	}
	deadline := time.Now().Add(budget)
	containerID := ""
	if isFullContainerID(knownID) {
		containerID = knownID
	}
	var lastError error
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if lastError == nil {
				lastError = errors.New("removal was requested but absence was not confirmed")
			}
			if containerID == "" {
				return fmt.Errorf("cleanup remained uncertain: no owned container was observed by name %q before the deadline: %w", name, lastError)
			}
			return fmt.Errorf("cleanup remained unconfirmed for container %s before the deadline: %w", containerID, lastError)
		}
		identifier := containerID
		if identifier == "" {
			identifier = name
		}
		callTimeout := min(commandTimeout, remaining)
		callContext, cancel := context.WithTimeout(context.Background(), callTimeout)
		output, diagnostic, err := run(callContext, "inspect", "--format", "{{json .}}", identifier)
		cancel()
		if err != nil {
			if isDockerObjectAbsent(diagnostic) {
				if containerID != "" {
					return nil
				}
				lastError = fmt.Errorf("container name %q is currently absent but creation outcome is unresolved", name)
			} else {
				lastError = fmt.Errorf("inspect %s: %w", identifier, err)
			}
			waitProxyControlRetry(deadline, retryDelay)
			continue
		}
		var inspection proxyControlInspection
		if err := json.Unmarshal([]byte(output), &inspection); err != nil {
			lastError = errors.New("Docker returned an invalid container inspection")
			waitProxyControlRetry(deadline, retryDelay)
			continue
		}
		if !ownsProxyControl(inspection, name, attemptID) || !isFullContainerID(inspection.ID) || (containerID != "" && inspection.ID != containerID) {
			return fmt.Errorf("refusing cleanup because container identity or ownership could not be confirmed for %q", identifier)
		}
		containerID = inspection.ID
		callTimeout = min(commandTimeout, time.Until(deadline))
		if callTimeout <= 0 {
			continue
		}
		callContext, cancel = context.WithTimeout(context.Background(), callTimeout)
		_, _, err = run(callContext, "rm", "--force", containerID)
		cancel()
		if err != nil {
			lastError = fmt.Errorf("remove container %s: %w", containerID, err)
		} else {
			lastError = nil
		}
		waitProxyControlRetry(deadline, retryDelay)
	}
}

func ownsProxyControl(inspection proxyControlInspection, name string, attemptID string) bool {
	return inspection.Name == "/"+name &&
		inspection.Config.Labels["shipmunk.proxy-test"] == "true" &&
		inspection.Config.Labels["shipmunk.proxy-test-attempt"] == attemptID
}

func isFullContainerID(identifier string) bool {
	return len(identifier) == 64 && containerIDPattern.MatchString(identifier)
}

func isDockerObjectAbsent(diagnostic string) bool {
	message := strings.ToLower(diagnostic)
	return strings.Contains(message, "no such object:") || strings.Contains(message, "no such container:")
}

func waitProxyControlRetry(deadline time.Time, delay time.Duration) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return
	}
	if delay > remaining {
		delay = remaining
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	<-timer.C
}

func TestProxyControlCleanupFindsLostCreateAndRetriesByPinnedID(t *testing.T) {
	const name = "shipmunk-proxy-control-01arz3ndektsv4rrffq69g5faw-7"
	const attemptID = "01arz3ndektsv4rrffq69g5faw"
	const containerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	nameInspections := 0
	idInspections := 0
	removals := 0
	run := proxyControlDockerCommand(func(ctx context.Context, arguments ...string) (string, string, error) {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			t.Fatal("Docker cleanup command did not receive a bounded context")
		}
		switch arguments[0] {
		case "inspect":
			identifier := arguments[len(arguments)-1]
			if identifier == name {
				nameInspections++
				if nameInspections == 1 {
					return "", "Error: No such object: " + name, errors.New("inspect failed")
				}
				if nameInspections == 2 {
					return "", "temporary daemon failure", errors.New("inspect failed")
				}
				return proxyControlInspectionJSON(containerID, name, attemptID), "", nil
			}
			if identifier != containerID {
				t.Fatalf("cleanup inspected unexpected identifier %q", identifier)
			}
			idInspections++
			if idInspections == 1 {
				return proxyControlInspectionJSON(containerID, name, attemptID), "", nil
			}
			return "", "Error: No such object: " + containerID, errors.New("inspect failed")
		case "rm":
			if len(arguments) != 3 || arguments[2] != containerID {
				t.Fatalf("cleanup attempted to remove a non-pinned identifier: %q", arguments)
			}
			removals++
			if removals == 1 {
				return "", "temporary daemon failure", errors.New("remove failed")
			}
			return "", "", nil
		default:
			t.Fatalf("unexpected Docker cleanup command %q", arguments[0])
			return "", "", errors.New("unexpected command")
		}
	})

	if err := cleanupProxyControlContainer(run, name, attemptID, "", time.Second, 100*time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("cleanup did not reconcile the lost create response: %v", err)
	}
	if nameInspections != 3 || idInspections != 2 || removals != 2 {
		t.Fatalf("cleanup calls = name inspect %d, id inspect %d, remove %d; want 3, 2, 2", nameInspections, idInspections, removals)
	}
}

func TestProxyControlCleanupRefusesUnownedContainer(t *testing.T) {
	const name = "shipmunk-proxy-control-01arz3ndektsv4rrffq69g5faw-7"
	const attemptID = "01arz3ndektsv4rrffq69g5faw"
	const containerID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	run := proxyControlDockerCommand(func(_ context.Context, arguments ...string) (string, string, error) {
		if arguments[0] == "inspect" && arguments[len(arguments)-1] == name {
			return proxyControlInspectionJSON(containerID, name, "01arz3ndektsv4rrffq69g5fax"), "", nil
		}
		t.Fatalf("cleanup must not act on an unowned container: %q", arguments)
		return "", "", errors.New("unexpected command")
	})
	if err := cleanupProxyControlContainer(run, name, attemptID, "", time.Second, 100*time.Millisecond, time.Millisecond); err == nil {
		t.Fatal("cleanup accepted a container owned by a different attempt")
	}
}

func TestProxyControlCleanupReportsUnresolvedAbsentName(t *testing.T) {
	const name = "shipmunk-proxy-control-01arz3ndektsv4rrffq69g5faw-7"
	const attemptID = "01arz3ndektsv4rrffq69g5faw"
	inspections := 0
	run := proxyControlDockerCommand(func(_ context.Context, arguments ...string) (string, string, error) {
		if arguments[0] != "inspect" || arguments[len(arguments)-1] != name {
			t.Fatalf("cleanup acted before observing an owned container: %q", arguments)
		}
		inspections++
		return "", "Error: No such object: " + name, errors.New("inspect failed")
	})
	if err := cleanupProxyControlContainer(run, name, attemptID, "", 100*time.Millisecond, 10*time.Millisecond, 5*time.Millisecond); err == nil || !strings.Contains(err.Error(), "remained uncertain") {
		t.Fatalf("cleanup error = %v, want unresolved-creation uncertainty", err)
	}
	if inspections < 2 {
		t.Fatalf("cleanup inspected the reserved name only %d time(s), want bounded settling retries", inspections)
	}
}

func proxyControlInspectionJSON(identifier string, name string, attemptID string) string {
	inspection := proxyControlInspection{ID: identifier, Name: "/" + name}
	inspection.Config.Labels = map[string]string{
		"shipmunk.proxy-test":         "true",
		"shipmunk.proxy-test-attempt": attemptID,
	}
	raw, _ := json.Marshal(inspection)
	return string(raw)
}
