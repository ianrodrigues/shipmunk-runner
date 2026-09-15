package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/codex"
	"github.com/ianrodrigues/shipmunk-runner/internal/install"
)

func TestInstalledLaunchersDispatchOnlyBoundArguments(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("installed commands refuse root")
	}
	root, _ := installedLauncherFixture(t)
	originalRuntime, originalRunner, originalProfile, originalVersion := setupRuntime, setupRunRunner, setupRunProfile, Version
	t.Cleanup(func() {
		setupRuntime, setupRunRunner, setupRunProfile, Version = originalRuntime, originalRunner, originalProfile, originalVersion
	})
	Version = "v1.2.3"
	setupRuntime.effectiveUID = os.Geteuid
	setupRuntime.stdin = strings.NewReader("")
	setupRuntime.isTerminal = func(any) bool { return true }
	setupRuntime.now = func() time.Time { return time.UnixMilli(1_700_000_000_000) }
	var observed [][]string
	setupRunRunner = func(args []string, _, _ io.Writer) int { observed = append(observed, slices.Clone(args)); return 0 }
	setupRunProfile = func(args []string, _, _ io.Writer) int { observed = append(observed, slices.Clone(args)); return 0 }
	for _, test := range []struct {
		args     []string
		binary   string
		contains []string
	}{
		{[]string{"run", root, "--once"}, "shipmunk-runner", []string{"--driver=codex", "--once", "--token-file=" + filepath.Join(root, "execution.token"), "--image=sha256:" + strings.Repeat("b", 64), "--repository-image=sha256:" + strings.Repeat("b", 64)}},
		{[]string{"connect", root}, "shipmunk-profile", []string{"--operation=login", "--token-file=" + filepath.Join(root, "profile.token"), "--image=sha256:" + strings.Repeat("b", 64)}},
		{[]string{"connect", root, "probe"}, "shipmunk-profile", []string{"--operation=probe"}},
	} {
		var stdout, stderr bytes.Buffer
		if code := runInstalledSetup(test.args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("%v = %d, %q", test.args, code, stderr.String())
		}
		call := observed[len(observed)-1]
		for _, argument := range test.contains {
			if !slices.Contains(call, argument) {
				t.Fatalf("args %v lack %s", call, argument)
			}
		}
		operation := ""
		for _, argument := range call {
			if strings.HasPrefix(argument, "--operation-id=") {
				operation = strings.TrimPrefix(argument, "--operation-id=")
			}
		}
		if test.binary == "shipmunk-profile" && len(operation) != 26 {
			t.Fatalf("operation id = %q", operation)
		}
	}
	for _, args := range [][]string{{"run", root, "--other"}, {"connect", root, "disconnect"}, {"connect", root, "probe", "extra"}} {
		if code := runInstalledSetup(args, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
			t.Fatalf("unsafe args %v = %d", args, code)
		}
	}
}

// An interrupted attempt is the supervisor's to recover, so only the run path may start with one.
func TestInstalledRunRecoversInterruptedAttemptWhileOtherCommandsRefuse(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("installed commands refuse root")
	}
	for name, journal := range map[string]func(*testing.T, string){
		"attempt":   writeInterruptedAttempt,
		"execution": writeProfileExecutionJournal,
	} {
		t.Run(name, func(t *testing.T) {
			root, _ := installedLauncherFixture(t)
			journal(t, root)
			originalRuntime, originalRunner, originalVersion := setupRuntime, setupRunRunner, Version
			t.Cleanup(func() { setupRuntime, setupRunRunner, Version = originalRuntime, originalRunner, originalVersion })
			Version = "v1.2.3"
			setupRuntime.effectiveUID, setupRuntime.stdin = os.Geteuid, strings.NewReader("")
			setupRuntime.isTerminal = func(any) bool { return true }
			setupRuntime.now = func() time.Time { return time.UnixMilli(1_700_000_000_000) }
			runs := 0
			setupRunRunner = func([]string, io.Writer, io.Writer) int { runs++; return 0 }

			var stdout, stderr bytes.Buffer
			if code := runInstalledSetup([]string{"run", root, "--once"}, &stdout, &stderr); code != 0 || runs != 1 || stderr.Len() != 0 {
				t.Fatalf("run refused recovery: code=%d runs=%d stderr=%q", code, runs, stderr.String())
			}
			stderr.Reset()
			if code := runInstalledSetup([]string{"connect", root}, &stdout, &stderr); code != 1 ||
				!strings.Contains(stderr.String(), "interrupted runner attempt") {
				t.Fatalf("connect refusal did not name the condition: code=%d stderr=%q", code, stderr.String())
			}
		})
	}
}

func writeInterruptedAttempt(t *testing.T, root string) {
	t.Helper()
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"run_id": testRunnerID, "attempt_id": testOperation, "fence": 1, "profile_id": testProfileID,
		"sandbox_id": nil, "lease_expires_at": "2026-09-15T16:29:17+00:00", "deadline": "2026-09-15T16:43:14+00:00",
		"workspace": filepath.Join(state, "workspaces", testOperation+"-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "active-attempt.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func writeProfileExecutionJournal(t *testing.T, root string) {
	t.Helper()
	profileRoot := filepath.Join(root, "profiles", testProfileID)
	if err := os.MkdirAll(profileRoot, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"run_id": testRunnerID, "attempt_id": testOperation, "fence": 1, "profile_id": testProfileID,
		"sandbox_id": "shipmunk-codex-" + testOperation + "-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileRoot, "execution.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledConnectionRequiresThreeTerminalDescriptors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("installed commands refuse root")
	}
	root, _ := installedLauncherFixture(t)
	originalRuntime, originalVersion := setupRuntime, Version
	t.Cleanup(func() { setupRuntime, Version = originalRuntime, originalVersion })
	Version = "v1.2.3"
	var stdout, stderr bytes.Buffer
	setupRuntime.effectiveUID, setupRuntime.stdin = os.Geteuid, strings.NewReader("")
	setupRuntime.isTerminal = func(value any) bool { return value != &stdout }
	if code := runInstalledSetup([]string{"connect", root, "probe"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "operator terminal") {
		t.Fatalf("terminal gate = %d, %q", code, stderr.String())
	}
}

func TestSetupServerHealthRefusesRedirectsAndFailures(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			if status == http.StatusFound {
				response.Header().Set("Location", "/up")
			}
			response.WriteHeader(status)
		}))
		if err := checkSetupServer(server.URL); err == nil {
			server.Close()
			t.Fatalf("accepted HTTP %d", status)
		}
		server.Close()
	}
}

func TestSetupServerHealthRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(bytes.Repeat([]byte("x"), 32*1024+1))
	}))
	defer server.Close()
	if err := checkSetupServer(server.URL); err == nil {
		t.Fatal("accepted oversized health response")
	}
}

func TestInstalledDispatchExcludesConcurrentActivation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("installed commands refuse root")
	}
	root, _ := installedLauncherFixture(t)
	originalRuntime, originalRunner, originalVersion := setupRuntime, setupRunRunner, Version
	t.Cleanup(func() { setupRuntime, setupRunRunner, Version = originalRuntime, originalRunner, originalVersion })
	Version = "v1.2.3"
	setupRuntime.effectiveUID, setupRuntime.stdin = os.Geteuid, strings.NewReader("")
	started, release := make(chan struct{}), make(chan struct{})
	setupRunRunner = func([]string, io.Writer, io.Writer) int { close(started); <-release; return 0 }
	done := make(chan int, 1)
	go func() { done <- runInstalledSetup([]string{"run", root, "--once"}, io.Discard, io.Discard) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("installed dispatch did not acquire its generation lock")
	}
	configuration, err := install.LoadConfiguration(root)
	if err != nil {
		t.Fatal(err)
	}
	guard := install.Guard{Root: root, Identity: configuration.Identity}
	config := mustConfigurationJSON(t, configuration)
	launchers := setupLaunchers(filepath.Join(configuration.ReleasePath, "bin", "shipmunk-setup"), root)
	if err := guard.ActivateLaunchers(config, []byte("new-profile"), []byte("new-execution"), launchers); err == nil {
		t.Fatal("activation entered while installed command owned its generation")
	}
	if raw, err := os.ReadFile(filepath.Join(root, "execution.token")); err != nil || string(raw) != "execution" {
		t.Fatal("failed activation changed the active generation")
	}
	close(release)
	if code := <-done; code != 0 {
		t.Fatalf("dispatch = %d", code)
	}
	if err := guard.ActivateLaunchers(config, []byte("new-profile"), []byte("new-execution"), launchers); err != nil {
		t.Fatalf("activation remained blocked: %v", err)
	}
}

func mustConfigurationJSON(t *testing.T, configuration install.Configuration) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"base_url": configuration.Identity.BaseURL, "runner_id": configuration.Identity.RunnerID, "profile_id": configuration.Identity.ProfileID,
		"expires_at": configuration.ExpiresAt, "release_path": configuration.ReleasePath, "release_version": configuration.ReleaseVersion,
		"platform": configuration.Platform, "image_id": configuration.ImageID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func installedLauncherFixture(t *testing.T) (string, string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, releases, err := prepareSetupDirectories(home, testRunnerID, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(releases, 0700); err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(releases, strings.Repeat("a", 64))
	if err := os.MkdirAll(filepath.Join(release, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shipmunk-runner", "shipmunk-profile", "shipmunk-setup"} {
		if err := os.WriteFile(filepath.Join(release, "bin", name), []byte("synthetic"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	config, err := json.Marshal(map[string]any{
		"base_url": "https://runner.example", "runner_id": testRunnerID, "profile_id": testProfileID,
		"expires_at": "2099-01-01T00:00:00Z", "release_path": release, "release_version": "v1.2.3",
		"platform": "linux-amd64", "image_id": "sha256:" + strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	guard := install.Guard{Root: root, Identity: install.Identity{BaseURL: "https://runner.example", RunnerID: testRunnerID, ProfileID: testProfileID}}
	if err := guard.ActivateLaunchers(config, []byte("profile"), []byte("execution"), setupLaunchers(filepath.Join(release, "bin", "shipmunk-setup"), root)); err != nil {
		t.Fatal(err)
	}
	return root, release
}

func TestSetupPreflightFailuresPreserveConfiguration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("guided setup refuses root")
	}
	for _, failure := range []string{"health", "image", "docker version"} {
		t.Run(failure, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			bundle := filepath.Join(home, "setup.json")
			if err := os.WriteFile(bundle, []byte(validSetupBundleJSON), 0600); err != nil {
				t.Fatal(err)
			}
			manifest, archive := setupReleaseFixture(t, home)
			originalRuntime, originalCheck, originalBuild := setupRuntime, setupCheckServer, setupBuildImage
			t.Cleanup(func() {
				setupRuntime, setupCheckServer, setupBuildImage = originalRuntime, originalCheck, originalBuild
			})
			setupRuntime = setupRuntimeHooks{effectiveUID: os.Geteuid, stdin: strings.NewReader("y\n"), isTerminal: func(any) bool { return true }, now: func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }, homeDir: func() (string, error) { return home, nil }, goos: runtime.GOOS, goarch: runtime.GOARCH}
			setupCheckServer = func(string) error {
				if failure == "health" {
					return errors.New("failed")
				}
				return nil
			}
			setupBuildImage = func(string, string) (string, error) {
				if failure == "docker version" {
					return "", codex.ErrDockerRequirements
				}
				if failure == "image" {
					return "", errors.New("failed")
				}
				return "sha256:" + strings.Repeat("c", 64), nil
			}
			var stderr bytes.Buffer
			if code := RunSetup(setupCommandArgs(bundle, manifest, archive), &bytes.Buffer{}, &stderr); code != 1 {
				t.Fatalf("failure code = %d", code)
			}
			if failure == "docker version" && !strings.Contains(stderr.String(), "Docker client and Linux engine 26.0 or newer") {
				t.Fatalf("unsupported Docker diagnostic missing: %s", stderr.String())
			}
			if _, err := os.Stat(filepath.Join(home, ".shipmunk", "runners", testRunnerID, "config.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("preflight failure activated configuration")
			}
		})
	}
}

func TestSetupRejectsOldDockerBeforeBuildingImage(t *testing.T) {
	for _, versions := range []string{"25.0.5\n29.0.0\nlinux", "29.0.0\n25.0.5\nlinux"} {
		t.Run(strings.ReplaceAll(versions, "\n", "_"), func(t *testing.T) {
			root := t.TempDir()
			bin := t.TempDir()
			script := "#!/bin/sh\n[ \"$1\" = version ] || exit 99\nprintf '%s\\n' '" + versions + "'\n"
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if _, err := buildSetupImage(root, filepath.Join(root, "missing-release")); !errors.Is(err, codex.ErrDockerRequirements) {
				t.Fatalf("preflight did not reject old Docker: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("unsupported Docker reached image preparation: %v %v", entries, err)
			}
		})
	}
}
