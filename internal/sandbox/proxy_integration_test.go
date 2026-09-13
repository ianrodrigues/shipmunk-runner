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
	controlID := query("create", "--name", controlName, "--label", "shipmunk.proxy-test=true", "--label", "shipmunk.proxy-test-attempt="+claim.AttemptID, "--network", "none", config.Image)
	if !containerIDPattern.MatchString(controlID) {
		t.Fatal("Docker did not return an immutable ID for the positive-control container")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		inspect := exec.CommandContext(ctx, executable, "inspect", "--format", "{{json .}}", controlID)
		inspect.Env = ClientEnvironment()
		output, err := inspect.Output()
		if err != nil {
			t.Errorf("inspect positive-control container before cleanup: %v", err)
			return
		}
		var owned struct {
			ID     string `json:"Id"`
			Name   string `json:"Name"`
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
		}
		if err := json.Unmarshal(output, &owned); err != nil || owned.ID != controlID || owned.Name != "/"+controlName ||
			owned.Config.Labels["shipmunk.proxy-test"] != "true" ||
			owned.Config.Labels["shipmunk.proxy-test-attempt"] != claim.AttemptID {
			t.Error("refusing cleanup because positive-control ownership could not be confirmed")
			return
		}
		remove := exec.CommandContext(ctx, executable, "rm", "--force", controlID)
		remove.Env = ClientEnvironment()
		if err := remove.Run(); err != nil {
			t.Errorf("remove positive-control container: %v", err)
		}
	})
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
