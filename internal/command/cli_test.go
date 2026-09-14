package command

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
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
	for _, args := range [][]string{
		{"--base-url", "https://runner.example", "--token-file", "token", "--state-dir", "state", "--image", "image", "--driver", "codex"},
		{"--base-url", "http://remote.example", "--token-file", "token", "--state-dir", "state", "--image", "image"},
		{"--base-url", "https://runner.example", "--token-file", "token", "--state-dir", "state", "--image", "image", "--driver", "docker"},
		{"--base-url", "https://runner.example#", "--token-file", "token", "--state-dir", "state", "--image", "image"},
	} {
		if _, err := ParseRunnerOptions(args); err == nil {
			t.Errorf("accepted invalid runner options: %#v", args)
		}
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
		health profile.Health
		code   int
		output string
	}{
		{health: profile.Health{Health: profile.HealthReady}, code: 0, output: `{"health":"ready","reason":null}` + "\n"},
		{health: profile.Health{Health: profile.HealthRateLimited, Reason: profile.ReasonRateLimited}, code: 1, output: `{"health":"rate_limited","reason":"rate_limited"}` + "\n"},
	} {
		runNativeProfile = func(ProfileOptions, io.Writer) (profile.Health, error) { return test.health, nil }
		var stdout, stderr bytes.Buffer
		if code := RunProfile(arguments, &stdout, &stderr); code != test.code || stdout.String() != test.output || stderr.Len() != 0 {
			t.Fatalf("profile result = %d, %q, %q", code, stdout.String(), stderr.String())
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
		{"setup.json", "--server-url", "https://runner.example"},
		{"setup.json", "--server-url=https://runner.example"},
		{"--server-url", "https://runner.example", "setup.json"},
		{"--server-url=https://runner.example", "setup.json"},
	} {
		options, err := ParseSetupOptions(args)
		if err != nil {
			t.Fatalf("parse %#v: %v", args, err)
		}
		if options.SetupFile != "setup.json" || options.ServerURL != "https://runner.example" {
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
	t.Cleanup(func() { setupRuntime = original })
	setupRuntime = setupRuntimeHooks{
		effectiveUID: os.Geteuid,
		stdin:        strings.NewReader("\n"),
		isTerminal:   func(any) bool { return true },
		now:          func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) },
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
	validSetupFile := filepath.Join(root, "valid.json")
	if err := os.WriteFile(validSetupFile, []byte(validSetupBundleJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := RunSetup([]string{validSetupFile, "--server-url", "https://override.example"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
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
	if code := RunSetup([]string{validSetupFile}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "installation is not available") {
		t.Fatalf("confirmed setup handoff = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRunSetupRejectsUnsafeFilesBeforeDisplayingBundle(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("setup ownership fixture requires a non-root test account")
	}
	original := setupRuntime
	t.Cleanup(func() { setupRuntime = original })
	setupRuntime = setupRuntimeHooks{effectiveUID: os.Geteuid, stdin: strings.NewReader("\n"), isTerminal: func(any) bool { return true }, now: time.Now}

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

	for _, setupPath := range []string{filepath.Join(root, "missing.json"), linked, hard, directory, oversized, throughDirectoryLink} {
		var stdout, stderr bytes.Buffer
		if code := RunSetup([]string{setupPath}, &stdout, &stderr); code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
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
			if code := RunSetup([]string{"/definitely/not/a/setup-file"}, &stdout, &stderr); code != 1 || stdout.Len() != 0 || stderr.Len() == 0 || strings.Contains(stderr.String(), "setup file") {
				t.Fatalf("host gate = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

const validSetupBundleJSON = `{"version":1,"runtime_version":"0.154.0","base_url":"https://runner.example","runner_id":"01k4w000000000000000000001","profile_id":"01k4w000000000000000000002","expires_at":"2099-01-01T00:00:00Z","profile_token":"1|AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","execution_token":"2|BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","future_field":{"ignored":true}}`
