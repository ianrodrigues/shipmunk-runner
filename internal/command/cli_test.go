package command

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
	shipmunkrelease "github.com/ianrodrigues/shipmunk-runner/internal/release"
)

const (
	testRunnerID  = "01k4w000000000000000000001"
	testProfileID = "01k4w000000000000000000002"
	testOperation = "01k4w000000000000000000003"
)

func TestParseRunnerOptionsPreservesDefaultsAndCodexRequirements(t *testing.T) {
	options, err := ParseRunnerOptions([]string{
		"--base-url", "https://runner.example",
		"--token-file", "/private/runner.token",
		"--state-dir", "/private/state",
		"--image", "shipmunk:local",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Driver != "fixture" || options.RepositoryImage != options.Image || options.Once {
		t.Fatalf("unexpected defaults: %#v", options)
	}
	options, err = ParseRunnerOptions([]string{
		"--base-url=http://127.0.0.1:8000",
		"--token-file=/private/runner.token",
		"--state-dir=/private/state",
		"--image=shipmunk:local",
		"--driver=codex",
		"--profiles-dir=/private/profiles",
		"--repository-image=repo:local",
		"--once",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Driver != "codex" || options.RepositoryImage != "repo:local" || !options.Once {
		t.Fatalf("unexpected explicit options: %#v", options)
	}
	options, err = ParseRunnerOptions([]string{
		"--base-url=https://runner.example",
		"--token-file=/private/runner.token",
		"--state-dir=/private/state",
		"--image=shipmunk:local",
		"--discard-attempt",
		"--yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !options.DiscardAttempt || !options.Confirmed || options.Once {
		t.Fatalf("unexpected discard options: %#v", options)
	}
	for _, args := range [][]string{
		{"--base-url", "https://runner.example", "--token-file", "token", "--state-dir", "state", "--image", "image", "--driver", "codex"},
		{"--base-url", "http://remote.example", "--token-file", "token", "--state-dir", "state", "--image", "image"},
		{"--base-url", "https://runner.example", "--token-file", "token", "--state-dir", "state", "--image", "image", "--driver", "docker"},
		{"--base-url", "https://runner.example#", "--token-file", "token", "--state-dir", "state", "--image", "image"},
		{"--base-url", "https://runner.example", "--token-file", "token", "--state-dir", "state", "--image", "image", "--once", "--discard-attempt"},
		{"--base-url", "https://runner.example", "--token-file", "token", "--state-dir", "state", "--image", "image", "--yes"},
	} {
		if _, err := ParseRunnerOptions(args); err == nil {
			t.Errorf("accepted invalid runner options: %#v", args)
		}
	}
}

func TestInstalledRunnerPathsShareOneRoot(t *testing.T) {
	_, err := ParseRunnerOptions([]string{
		"--base-url", "https://runner.example",
		"--token-file", "/private/runner/execution.token",
		"--state-dir", "/other/state",
		"--image", "shipmunk:local",
	})
	if err == nil {
		t.Fatal("incoherent installed runner paths were accepted")
	}
}

func TestInstalledProfilePathsShareOneRoot(t *testing.T) {
	_, err := ParseProfileOptions([]string{
		"--base-url", "https://runner.example",
		"--token-file", "/private/runner/profile.token",
		"--profiles-dir", "/other/profiles",
		"--image", "shipmunk:local",
		"--profile", testProfileID,
		"--operation", "probe",
		"--operation-id", testOperation,
	})
	if err == nil {
		t.Fatal("incoherent installed profile paths were accepted")
	}
}

func TestParseProfileOptionsRequiresCanonicalIdentifiers(t *testing.T) {
	args := []string{
		"--base-url", "https://runner.example",
		"--token-file", "/private/profile.token",
		"--profiles-dir", "/private/profiles",
		"--image", "shipmunk:local",
		"--profile", testProfileID,
		"--operation", "login",
		"--operation-id", testOperation,
	}
	options, err := ParseProfileOptions(args)
	if err != nil {
		t.Fatal(err)
	}
	if options.Profile != testProfileID || options.Operation != "login" || options.OperationID != testOperation {
		t.Fatalf("unexpected profile options: %#v", options)
	}
	for _, replacement := range [][]string{
		{"--profile", strings.ToUpper(testProfileID)},
		{"--operation-id", "invalid"},
		{"--operation", "retry"},
	} {
		modified := append([]string(nil), args...)
		for index := 0; index < len(modified)-1; index++ {
			if modified[index] == replacement[0] {
				modified[index+1] = replacement[1]
				break
			}
		}
		if _, err := ParseProfileOptions(modified); err == nil {
			t.Errorf("accepted invalid profile options %v", replacement)
		}
	}
}

func TestRunnerAndProfileCommandsRemainFailClosedAndSafe(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := RunRunner([]string{"--help"}, &stdout, &stderr); exitCode != 0 || !strings.Contains(stdout.String(), "--repository-image") || stderr.Len() != 0 {
		t.Fatalf("runner help = %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if exitCode := RunRunner([]string{"--version"}, &stdout, &stderr); exitCode != 0 || stdout.String() != "shipmunk-runner development\n" {
		t.Fatalf("runner version = %d, %q", exitCode, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if exitCode := RunRunner([]string{
		"--base-url", "https://runner.example", "--token-file", "/secret/token",
		"--state-dir", "/private/state", "--image", "shipmunk:local",
	}, &stdout, &stderr); exitCode != 1 || stderr.Len() == 0 || strings.Contains(stderr.String(), "secret") {
		t.Fatalf("runner deferral = %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if exitCode := RunProfile([]string{"--help"}, &stdout, &stderr); exitCode != 0 || !strings.Contains(stdout.String(), "--operation-id") || stderr.Len() != 0 {
		t.Fatalf("profile help = %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if exitCode := RunProfile([]string{"--version"}, &stdout, &stderr); exitCode != 0 || stdout.String() != "shipmunk-profile development\n" {
		t.Fatalf("profile version = %d, %q", exitCode, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if exitCode := RunProfile([]string{
		"--base-url", "https://runner.example", "--token-file", "/secret/token",
		"--profiles-dir", "/private/profiles", "--image", "shipmunk:local",
		"--profile", testProfileID, "--operation", "login", "--operation-id", testOperation,
	}, &stdout, &stderr); exitCode != 1 || !strings.Contains(stderr.String(), "operator terminal") || strings.Contains(stderr.String(), "secret") {
		t.Fatalf("profile deferral = %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
}

func TestCommandsReportInjectedReleaseVersion(t *testing.T) {
	original := Version
	Version = "v1.2.3-alpha.4"
	t.Cleanup(func() { Version = original })

	for name, run := range map[string]func(*bytes.Buffer, *bytes.Buffer) int{
		"shipmunk-runner": func(stdout, stderr *bytes.Buffer) int {
			return RunRunner([]string{"--version"}, stdout, stderr)
		},
		"shipmunk-profile": func(stdout, stderr *bytes.Buffer) int {
			return RunProfile([]string{"--version"}, stdout, stderr)
		},
		"shipmunk-setup": func(stdout, stderr *bytes.Buffer) int {
			return RunSetup([]string{"--version"}, stdout, stderr)
		},
		"shipmunk-watchdog": func(stdout, stderr *bytes.Buffer) int {
			return RunWatchdog([]string{"--version"}, strings.NewReader(""), stdout, stderr)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(&stdout, &stderr); code != 0 || stdout.String() != name+" v1.2.3-alpha.4\n" || stderr.Len() != 0 {
				t.Fatalf("version = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestProfileCommandWritesSanitizedHealthAndExitStatus(t *testing.T) {
	original := runNativeProfile
	t.Cleanup(func() { runNativeProfile = original })
	arguments := []string{
		"--base-url", "https://runner.example", "--token-file", "/private/profile.token",
		"--profiles-dir", "/private/profiles", "--image", "shipmunk:local",
		"--profile", testProfileID, "--operation", "probe", "--operation-id", testOperation,
	}
	for _, test := range []struct {
		health     profile.Health
		code       int
		sentence   string
		jsonOutput string
	}{
		{health: profile.Health{Health: profile.HealthReady}, code: 0, sentence: "Runner is ready.\n", jsonOutput: `{"health":"ready","reason":null}` + "\n"},
		{health: profile.Health{Health: profile.HealthRateLimited, Reason: profile.ReasonRateLimited}, code: 1, sentence: "Runner is not ready: rate_limited.\n", jsonOutput: `{"health":"rate_limited","reason":"rate_limited"}` + "\n"},
	} {
		runNativeProfile = func(ProfileOptions, io.Writer) (profile.Health, error) { return test.health, nil }
		var stdout, stderr bytes.Buffer
		if code := RunProfile(arguments, &stdout, &stderr); code != test.code || stdout.String() != test.sentence || stderr.Len() != 0 {
			t.Fatalf("profile result = %d, %q, %q", code, stdout.String(), stderr.String())
		}
		stdout.Reset()
		stderr.Reset()
		if code := RunProfile(append(slices.Clone(arguments), "--json"), &stdout, &stderr); code != test.code || stdout.String() != test.jsonOutput || stderr.Len() != 0 {
			t.Fatalf("profile --json result = %d, %q, %q", code, stdout.String(), stderr.String())
		}
	}

	runNativeProfile = func(ProfileOptions, io.Writer) (profile.Health, error) {
		return profile.Health{}, errors.New("SYNTHETIC_PRIVATE_PROVIDER_TEXT")
	}
	var stdout, stderr bytes.Buffer
	if code := RunProfile(arguments, &stdout, &stderr); code != 1 || stdout.Len() != 0 || strings.Contains(stderr.String(), "SYNTHETIC") {
		t.Fatalf("unsafe profile error = %d, %q, %q", code, stdout.String(), stderr.String())
	}
}

func TestParseSetupOptionsAcceptsServerURLAfterFile(t *testing.T) {
	for _, args := range [][]string{
		{"setup.json", "--server-url", "https://runner.example", "--release-manifest=manifest.json", "--release-archive=archive.tar"},
		{"setup.json", "--server-url=https://runner.example", "--release-manifest", "manifest.json", "--release-archive", "archive.tar"},
		{"--server-url", "https://runner.example", "--release-manifest=manifest.json", "--release-archive=archive.tar", "setup.json"},
		{"--server-url=https://runner.example", "--release-manifest=manifest.json", "--release-archive=archive.tar", "setup.json"},
	} {
		options, err := ParseSetupOptions(args)
		if err != nil {
			t.Fatalf("parse %#v: %v", args, err)
		}
		if options.SetupFile != "setup.json" || options.ServerURL != "https://runner.example" || options.ReleaseManifest != "manifest.json" || options.ReleaseArchive != "archive.tar" {
			t.Fatalf("unexpected options for %#v: %#v", args, options)
		}
	}
	if options, err := ParseSetupOptions([]string{"--help"}); err != nil || !options.Help {
		t.Fatalf("help request = %#v, %v", options, err)
	}
}

func TestParseSetupBundleValidatesFieldsAndServerOverride(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	bundle, err := ParseSetupBundle([]byte(validSetupBundleJSON), "", now)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Version != 1 || bundle.RuntimeVersion != "0.154.0" || bundle.BaseURL != "https://runner.example" || bundle.RunnerID != testRunnerID || bundle.ProfileID != testProfileID || bundle.ExpiresAt.Location() != time.UTC {
		t.Fatalf("unexpected parsed bundle: %#v", bundle)
	}
	if bundle.ProfileToken == "" || bundle.ExecutionToken == "" {
		t.Fatal("setup tokens were not parsed")
	}
	if overridden, err := ParseSetupBundle([]byte(validSetupBundleJSON), "https://override.example", now); err != nil || overridden.BaseURL != "https://override.example" {
		t.Fatalf("server URL override = %#v, %v", overridden, err)
	}
	for _, invalid := range []string{
		strings.Replace(validSetupBundleJSON, `"version":1`, `"version":1.0`, 1),
		strings.Replace(validSetupBundleJSON, `"runtime_version":"0.154.0"`, `"runtime_version":"0.155.0"`, 1),
		strings.Replace(validSetupBundleJSON, `"base_url":"https://runner.example"`, `"base_url":"http://remote.example"`, 1),
		strings.Replace(validSetupBundleJSON, `"base_url":"https://runner.example"`, `"base_url":"https://runner.example#"`, 1),
		strings.Replace(validSetupBundleJSON, `"profile_id":"`+testProfileID+`"`, `"profile_id":"`+strings.ToUpper(testProfileID)+`"`, 1),
		strings.Replace(validSetupBundleJSON, `"execution_token":"2|BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"`, `"execution_token":null`, 1),
	} {
		if _, err := ParseSetupBundle([]byte(invalid), "", now); err == nil {
			t.Errorf("accepted invalid setup bundle: %s", invalid)
		}
	}
	if _, err := ParseSetupBundle([]byte(validSetupBundleJSON), "", time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("accepted expired setup tokens")
	}
	if _, err := ParseSetupBundle([]byte(`{"version":1,"version":1}`), "", now); err == nil {
		t.Fatal("accepted duplicate setup property")
	}
}

func TestRunSetupProtectsSummarizesAndRequiresConfirmation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("setup ownership fixture requires a non-root test account")
	}
	original := setupRuntime
	originalCheck, originalBuild := setupCheckServer, setupBuildImage
	t.Cleanup(func() { setupRuntime = original; setupCheckServer = originalCheck; setupBuildImage = originalBuild })
	setupCheckServer = func(string) error { return nil }
	setupBuildImage = func(string, string) (string, error) { return "sha256:" + strings.Repeat("c", 64), nil }
	home := ""
	setupRuntime = setupRuntimeHooks{
		effectiveUID: os.Geteuid,
		stdin:        strings.NewReader("\n"),
		isTerminal:   func(any) bool { return true },
		now:          func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) },
		homeDir:      func() (string, error) { return home, nil },
		goos:         runtime.GOOS,
		goarch:       runtime.GOARCH,
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := RunSetup([]string{"--help"}, &stdout, &stderr); exitCode != 0 || !strings.Contains(stdout.String(), "SETUP-FILE") || stderr.Len() != 0 {
		t.Fatalf("setup help = %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if exitCode := RunSetup([]string{"--version"}, &stdout, &stderr); exitCode != 0 || stdout.String() != "shipmunk-setup development\n" {
		t.Fatalf("setup version = %d, %q", exitCode, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if exitCode := RunSetup(nil, &stdout, &stderr); exitCode != 2 || !strings.Contains(stderr.String(), "Invalid setup options") {
		t.Fatalf("setup missing operand = %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home = root
	validSetupFile := filepath.Join(root, "valid.json")
	if err := os.WriteFile(validSetupFile, []byte(validSetupBundleJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, archive := setupReleaseFixture(t, root)
	args := setupCommandArgs(validSetupFile, manifest, archive)
	args = append(args, "--server-url", "https://override.example")
	if code := RunSetup(args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("declined setup = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "Server: https://override.example\n") || !strings.Contains(output, "Runner: "+testRunnerID+"\n") || !strings.Contains(output, "Profile: "+testProfileID+"\n") || !strings.Contains(output, "It does not start reviews.\n") || !strings.HasSuffix(output, "[y/N] ") {
		t.Fatalf("unsafe or incomplete setup summary: %q", output)
	}
	if strings.Contains(output, "AAAA") || strings.Contains(output, "BBBB") {
		t.Fatal("setup summary exposed a token")
	}
	if info, err := os.Stat(validSetupFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("setup file was not protected: %#v, %v", info, err)
	}

	stdout.Reset()
	setupRuntime.stdin = strings.NewReader("y\n")
	if code := RunSetup(setupCommandArgs(validSetupFile, manifest, archive), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "Installed the verified runner release") {
		t.Fatalf("confirmed setup handoff = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRunSetupRejectsUnsafeFilesBeforeDisplayingBundle(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("setup ownership fixture requires a non-root test account")
	}
	original := setupRuntime
	t.Cleanup(func() { setupRuntime = original })
	setupRuntime = setupRuntimeHooks{effectiveUID: os.Geteuid, stdin: strings.NewReader("\n"), isTerminal: func(any) bool { return true }, now: time.Now, goos: runtime.GOOS, goarch: runtime.GOARCH}

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(root, "valid.json")
	if err := os.WriteFile(valid, []byte(validSetupBundleJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "linked.json")
	if err := os.Symlink(valid, linked); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(root, "hard.json")
	if err := os.Link(valid, hard); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(root, "oversized.json")
	if err := os.WriteFile(oversized, bytes.Repeat([]byte("x"), setupBundleMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	redirect := filepath.Join(root, "redirect")
	if err := os.Symlink(realDirectory, redirect); err != nil {
		t.Fatal(err)
	}
	throughDirectoryLink := filepath.Join(redirect, "bundle.json")
	if err := os.WriteFile(filepath.Join(realDirectory, "bundle.json"), []byte(validSetupBundleJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, archive := setupReleaseFixture(t, root)

	for _, setupPath := range []string{filepath.Join(root, "missing.json"), linked, hard, directory, oversized, throughDirectoryLink} {
		var stdout, stderr bytes.Buffer
		if code := RunSetup(setupCommandArgs(setupPath, manifest, archive), &stdout, &stderr); code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Errorf("unsafe setup path %q = %d, stdout %q, stderr %q", setupPath, code, stdout.String(), stderr.String())
		}
	}
}

func TestRunSetupRequiresNonRootThreeFDTerminalBeforeFileAccess(t *testing.T) {
	original := setupRuntime
	t.Cleanup(func() { setupRuntime = original })

	for _, name := range []string{"root", "stdin", "stdout", "stderr"} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			stdin := strings.NewReader("")
			setupRuntime = setupRuntimeHooks{
				effectiveUID: func() int {
					if name == "root" {
						return 0
					}
					return 1000
				},
				stdin: stdin,
				isTerminal: func(value any) bool {
					return !(name == "stdin" && value == stdin) && !(name == "stdout" && value == &stdout) && !(name == "stderr" && value == &stderr)
				},
				now: time.Now,
			}
			if code := RunSetup(setupCommandArgs("/definitely/not/a/setup-file", "/missing/manifest", "/missing/archive"), &stdout, &stderr); code != 1 || stdout.Len() != 0 || stderr.Len() == 0 || strings.Contains(stderr.String(), "setup file") {
				t.Fatalf("host gate = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestCompiledSetupInstallsIntoCleanHomeWithoutStartingWork(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("guided setup refuses root")
	}
	if _, err := exec.LookPath("expect"); err != nil {
		t.Skip("expect is required to provide an operator terminal")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "shipmunk-runner")
	buildContext, cancelBuild := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelBuild()
	build := exec.CommandContext(buildContext, "go", "build", "-trimpath", "-buildvcs=false", "-o", binary, "./cmd/shipmunk-runner")
	build.Dir = repository
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build setup: %v: %s", err, output)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/up" {
			http.NotFound(response, request)
			return
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	bundle := filepath.Join(root, "setup.json")
	bundleJSON := strings.Replace(validSetupBundleJSON, "https://runner.example", server.URL, 1)
	if err := os.WriteFile(bundle, []byte(bundleJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, archive := setupReleaseFixture(t, root)
	dockerDirectory := filepath.Join(root, "bin")
	if err := os.Mkdir(dockerDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	docker := filepath.Join(dockerDirectory, "docker")
	dockerScript := "#!/bin/sh\nif [ \"$1\" = version ]; then printf '26.0.0\\n26.0.0\\nlinux\\n'; exit 0; fi\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --iidfile ]; then shift; printf 'sha256:" + strings.Repeat("d", 64) + "\\n' > \"$1\"; exit 0; fi; shift; done\nexit 1\n"
	if err := os.WriteFile(docker, []byte(dockerScript), 0700); err != nil {
		t.Fatal(err)
	}
	expectScript := "log_user 1\nset timeout 10\nspawn -noecho $env(SHIPMUNK_SETUP_BINARY) setup $env(SHIPMUNK_SETUP_BUNDLE) --release-manifest $env(SHIPMUNK_SETUP_MANIFEST) --release-archive $env(SHIPMUNK_SETUP_ARCHIVE) --skip-connect\nexpect \"Continue with this server and runner?\"\nsend \"y\\r\"\nexpect \"Setup did not start queued work.\"\nexit 0\n"
	process := exec.Command("expect", "-c", expectScript)
	process.Env = append(os.Environ(), "PATH="+dockerDirectory+":"+os.Getenv("PATH"), "HOME="+root, "SHIPMUNK_SETUP_BINARY="+binary, "SHIPMUNK_SETUP_BUNDLE="+bundle, "SHIPMUNK_SETUP_MANIFEST="+manifest, "SHIPMUNK_SETUP_ARCHIVE="+archive)
	output, err := process.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("Installed the verified runner release")) || !bytes.Contains(output, []byte("did not start queued work")) {
		t.Fatalf("compiled setup smoke: %v: %q", err, output)
	}
	configPath := filepath.Join(root, ".shipmunk", "runners", testRunnerID, "config.json")
	config, err := os.ReadFile(configPath)
	if err != nil || !bytes.Contains(config, []byte(`"release_version":"v1.2.3"`)) || !bytes.Contains(config, []byte(`"release_path":"`)) {
		t.Fatalf("installed config = %q, %v", config, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".shipmunk", "runners", testRunnerID, "state", "active-attempt.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("setup started or journaled queued work")
	}
}

func TestRunSetupBlocksRenewalBeforeInstallingRelease(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("setup ownership fixture requires a non-root test account")
	}
	original := setupRuntime
	t.Cleanup(func() { setupRuntime = original })

	for name, test := range map[string]struct {
		block            func(*testing.T, string)
		wantAlsoInStderr []string
	}{
		"changed identity": {block: func(t *testing.T, runnerRoot string) {
			raw := `{"base_url":"https://other.example","runner_id":"` + testRunnerID + `","profile_id":"` + testProfileID + `"}`
			if err := os.WriteFile(filepath.Join(runnerRoot, "config.json"), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		"unresolved attempt": {block: func(t *testing.T, runnerRoot string) {
			if err := os.WriteFile(filepath.Join(runnerRoot, "state", "active-attempt.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		"older release than installed": {
			block: func(t *testing.T, runnerRoot string) {
				releasePath := filepath.Join(filepath.Dir(filepath.Dir(runnerRoot)), "releases", strings.Repeat("a", 64))
				raw := `{"base_url":"https://runner.example","runner_id":"` + testRunnerID + `","profile_id":"` + testProfileID + `","expires_at":"2099-01-01T00:00:00Z","release_path":"` + releasePath + `","release_version":"v1.2.4","platform":"linux-amd64","image_id":"sha256:` + strings.Repeat("b", 64) + `"}`
				if err := os.WriteFile(filepath.Join(runnerRoot, "config.json"), []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			// setupReleaseFixture always builds manifest version v1.2.3; the
			// refusal must name both the incoming and the installed version.
			wantAlsoInStderr: []string{"v1.2.3", "v1.2.4"},
		},
	} {
		block := test.block
		wantAlsoInStderr := test.wantAlsoInStderr
		t.Run(name, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			bundle := filepath.Join(home, "setup.json")
			if err := os.WriteFile(bundle, []byte(validSetupBundleJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			manifest, archive := setupReleaseFixture(t, home)
			runnerRoot, _, err := prepareSetupDirectories(home, testRunnerID, os.Geteuid())
			if err != nil {
				t.Fatal(err)
			}
			block(t, runnerRoot)
			setupRuntime = setupRuntimeHooks{
				effectiveUID: os.Geteuid, stdin: strings.NewReader("y\n"), isTerminal: func(any) bool { return true },
				now:     func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) },
				homeDir: func() (string, error) { return home, nil }, goos: runtime.GOOS, goarch: runtime.GOARCH,
			}
			var stdout, stderr bytes.Buffer
			if code := RunSetup(setupCommandArgs(bundle, manifest, archive), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "renewal is blocked") {
				t.Fatalf("blocked renewal = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
			}
			for _, want := range wantAlsoInStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("blocked renewal stderr %q does not name %q", stderr.String(), want)
				}
			}
			if _, err := os.Lstat(filepath.Join(home, ".shipmunk", "releases")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("blocked renewal mutated the release store")
			}
		})
	}
}

func TestPublicReleaseFilesRejectLinksAndOversize(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(root, "release")
	if err := os.WriteFile(regular, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "linked")
	if err := os.Symlink(regular, linked); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(root, "hard")
	if err := os.Link(regular, hard); err != nil {
		t.Fatal(err)
	}
	oversize := filepath.Join(root, "oversize")
	if err := os.WriteFile(oversize, []byte("too large"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{regular, linked, hard, oversize} {
		if _, err := readPublicSetupFile(path, 7); err == nil {
			t.Fatalf("accepted unsafe release file %s", path)
		}
	}
}

func setupCommandArgs(bundle, manifest, archive string) []string {
	return []string{bundle, "--release-manifest", manifest, "--release-archive", archive}
}

func setupReleaseFixture(t *testing.T, root string) (string, string) {
	t.Helper()
	files := map[string]struct {
		raw  []byte
		mode int64
	}{
		"bin/shipmunk-runner":                 {[]byte("synthetic runner\n"), 0755},
		"bin/shipmunk-watchdog":               {[]byte("synthetic watchdog\n"), 0755},
		"containers/Dockerfile":               {[]byte("FROM scratch\nCOPY runner/containers/codex-mcp.mjs /tmp/\n"), 0644},
		"containers/Dockerfile.dockerignore":  {[]byte("**\n!runner/\n"), 0644},
		"containers/codex-mcp.mjs":            {[]byte("export {};\n"), 0644},
		"containers/codex-result.schema.json": {[]byte("{}\n"), 0644},
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	for _, name := range names {
		file := files[name]
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: file.mode, Size: int64(len(file.raw)), ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(file.raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	archiveDigest := sha256.Sum256(archive.Bytes())
	artifact := shipmunkrelease.Artifact{Name: "runner.tar", URL: "https://runner.example/runner.tar", SHA256: hex.EncodeToString(archiveDigest[:]), Size: int64(archive.Len())}
	inventory := make([]shipmunkrelease.File, 0, len(names))
	for _, name := range names {
		file := files[name]
		digest := sha256.Sum256(file.raw)
		inventory = append(inventory, shipmunkrelease.File{Path: name, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(file.raw)), Mode: fmt.Sprintf("%04o", file.mode)})
	}
	platforms := map[string]shipmunkrelease.PlatformRelease{}
	for _, target := range []struct{ os, arch string }{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		key := target.os + "-" + target.arch
		platforms[key] = shipmunkrelease.PlatformRelease{OS: target.os, Arch: target.arch, Archive: artifact, Files: inventory}
	}
	manifest := shipmunkrelease.Manifest{SchemaVersion: 1, Version: "v1.2.3", Platforms: platforms}
	manifest.NativeImage.Platforms = []string{"linux-amd64", "linux-arm64"}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath, archivePath := filepath.Join(root, "runner-release.json"), filepath.Join(root, "runner.tar")
	if err := os.WriteFile(manifestPath, manifestRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, archive.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return manifestPath, archivePath
}

const validSetupBundleJSON = `{"version":1,"runtime_version":"0.154.0","base_url":"https://runner.example","runner_id":"01k4w000000000000000000001","profile_id":"01k4w000000000000000000002","expires_at":"2099-01-01T00:00:00Z","profile_token":"1|AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","execution_token":"2|BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","future_field":{"ignored":true}}`
