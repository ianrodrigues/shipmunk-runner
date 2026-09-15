package installsmoke

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// envHostMode, alongside envEnable, selects TestHostNativeInstallsReleaseWithoutPHPOrGo.
const envHostMode = "SHIPMUNK_INSTALL_SMOKE_HOST"

// This test cannot start from a clean machine image, so every subprocess
// runs with PATH scrubbed to the fixture's bin plus /usr/bin and /bin.
func TestHostNativeInstallsReleaseWithoutPHPOrGo(t *testing.T) {
	if os.Getenv(envEnable) != "1" || os.Getenv(envHostMode) != "1" {
		t.Skip("set " + envEnable + "=1 and " + envHostMode + "=1 for the host-native (non-container) install smoke test")
	}
	if _, err := exec.LookPath("expect"); err != nil {
		t.Skip("expect is required to provide an operator terminal for guided setup")
	}

	arch := envDefault(envArch, runtime.GOARCH)
	if arch != runtime.GOARCH {
		t.Fatalf("host-native smoke cannot exercise %s=%s on a %s host: it executes the archive directly, with no emulation", envArch, arch, runtime.GOARCH)
	}
	platformKey := runtime.GOOS + "-" + arch
	if platformKey != "darwin-amd64" && platformKey != "darwin-arm64" {
		t.Skipf("host-native smoke targets a darwin host; this host is %s", platformKey)
	}

	ctx := context.Background()
	distDir := os.Getenv(envArchiveDir)
	if distDir == "" {
		distDir = t.TempDir()
		buildReleaseArchive(t, ctx, distDir, envDefault(envVersion, defaultVersion))
	}
	manifest := readManifest(t, filepath.Join(distDir, "runner-release.json"))
	platform, ok := manifest.Platforms[platformKey]
	if !ok {
		t.Fatalf("release manifest does not declare platform %s", platformKey)
	}
	archivePath := filepath.Join(distDir, platform.Archive.Name)
	if info, err := os.Stat(archivePath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("release archive %s is unavailable: %v", archivePath, err)
	}
	checksumsPath := filepath.Join(distDir, "SHA256SUMS")
	if _, err := os.Stat(checksumsPath); err != nil {
		t.Fatalf("SHA256SUMS is unavailable: %v", err)
	}

	// prepareSetupDirectories requires filepath.EvalSymlinks(home) == home.
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(home, "fixture")
	if err := os.MkdirAll(filepath.Join(fixture, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	copyHostFile(t, archivePath, filepath.Join(fixture, platform.Archive.Name), 0644)
	copyHostFile(t, filepath.Join(distDir, "runner-release.json"), filepath.Join(fixture, "runner-release.json"), 0644)
	copyHostFile(t, checksumsPath, filepath.Join(fixture, "SHA256SUMS"), 0644)
	copyHostFile(t, writeFakeDockerScript(t), filepath.Join(fixture, "bin", "docker"), 0755)
	copyHostFile(t, resolveHostUpstub(t), filepath.Join(fixture, "upstub"), 0755)
	upstubAddr := freeLoopbackAddr(t)
	bundlePath := filepath.Join(fixture, "setup.json")
	copyHostFile(t, writeSetupBundle(t, smokeRunnerID, smokeProfileID, "http://"+upstubAddr), bundlePath, 0600)

	// Excludes /usr/local/bin and /opt/homebrew/bin, where php and go live.
	scrubbedPath := fixture + "/bin:/usr/bin:/bin"
	assertHostCommandAbsent(t, scrubbedPath, "php")
	assertHostCommandAbsent(t, scrubbedPath, "go")

	// Matches the published bootstrap script: only the setup binary is extracted.
	extract := runHostScrubbed(t, "", 30*time.Second, scrubbedPath, "tar", "-xf", filepath.Join(fixture, platform.Archive.Name), "-C", fixture, "bin/shipmunk-setup")
	if extract.exitCode != 0 {
		t.Fatalf("extract shipmunk-setup: %s", extract.output)
	}

	verify := runHostScrubbed(t, fixture, 30*time.Second, scrubbedPath, "sh", "-c", "shasum -a 256 --ignore-missing -c SHA256SUMS")
	if verify.exitCode != 0 {
		t.Fatalf("archive/manifest checksum verification failed: %s", verify.output)
	}

	setupPath := filepath.Join(fixture, "bin", "shipmunk-setup")
	versionResult := runHostScrubbed(t, "", 10*time.Second, scrubbedPath, setupPath, "--version")
	wantVersion := "shipmunk-setup " + manifest.Version + "\n"
	if versionResult.exitCode != 0 || versionResult.output != wantVersion {
		t.Fatalf("installed setup --version = %q (exit %d), want %q", versionResult.output, versionResult.exitCode, wantVersion)
	}

	startHostUpstub(t, filepath.Join(fixture, "upstub"), upstubAddr)

	output, exitCode := runExpect(t, 90*time.Second,
		"env", "-i", "HOME="+home, "PATH="+scrubbedPath,
		setupPath, bundlePath,
		"--release-manifest", filepath.Join(fixture, "runner-release.json"),
		"--release-archive", filepath.Join(fixture, platform.Archive.Name))
	if exitCode != 0 || !strings.Contains(output, "Installed the verified runner release.") || !strings.Contains(output, "Setup did not start queued work.") {
		t.Fatalf("guided setup did not complete cleanly (exit %d): %s", exitCode, output)
	}

	root := filepath.Join(home, ".shipmunk", "runners", smokeRunnerID)
	assertHostInstalledConfiguration(t, root, manifest.Version, platformKey)
	assertHostLauncher(t, home, root, "run")
	assertHostLauncher(t, home, root, "connect")
	assertHostAbsent(t, filepath.Join(root, "state", "active-attempt.json"))

	// Setup must not have made php or go reachable on the scrubbed PATH.
	assertHostCommandAbsent(t, scrubbedPath, "php")
	assertHostCommandAbsent(t, scrubbedPath, "go")

	runOnceEnv := []string{"HOME=" + home, "PATH=" + scrubbedPath}
	runOnce := runHostEnv(t, "", 60*time.Second, runOnceEnv, filepath.Join(root, "run"), "--once")
	if runOnce.exitCode != 1 {
		t.Fatalf("run --once against a stub control plane should fail cleanly with exit 1, got %d: %s", runOnce.exitCode, runOnce.output)
	}
	if strings.Contains(runOnce.output, "no such file or directory") || strings.Contains(strings.ToLower(runOnce.output), "exec format error") {
		t.Fatalf("run --once failed for an environment reason rather than a control-plane reason: %s", runOnce.output)
	}
	assertHostUpstubReceived(t, "POST /runner/v1/claims")
	assertHostAbsent(t, filepath.Join(root, "state", "active-attempt.json"))

	probeOutput, probeExit := runExpect(t, 30*time.Second, "env", "-i", "HOME="+home, "PATH="+scrubbedPath, filepath.Join(root, "connect"), "probe")
	if probeExit != 1 {
		t.Fatalf("connect probe against a stub control plane should fail cleanly with exit 1, got %d: %s", probeExit, probeOutput)
	}
	if strings.Contains(probeOutput, "no such file or directory") || strings.Contains(strings.ToLower(probeOutput), "exec format error") {
		t.Fatalf("connect probe failed for an environment reason rather than a control-plane reason: %s", probeOutput)
	}
	assertHostUpstubReceived(t, "POST /runner/v1/profiles/"+smokeProfileID+"/operations")
	assertHostAbsent(t, filepath.Join(root, "state", "active-attempt.json"))

	t.Logf("host-native install smoke passed for %s (PATH-scrubbed on this developer host, not a clean machine image)", platformKey)
}

// resolveHostUpstub builds the up-stub for this host's GOOS/GOARCH using the
// outer test process's own PATH: harness infrastructure, not under test.
func resolveHostUpstub(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("host-native install smoke needs a Go toolchain on this developer machine to build its up-stub fixture; the installed release itself still runs without php or go on its scrubbed PATH")
	}
	binary := filepath.Join(t.TempDir(), "upstub")
	buildCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(buildCtx, "go", "build", "-o", binary, filepath.Join(repositoryRoot(t), "internal", "installsmoke", "testdata", "upstub"))
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build up-stub server: %v: %s", err, output)
	}
	return binary
}

func copyHostFile(t *testing.T, source, destination string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if err := os.WriteFile(destination, data, mode); err != nil {
		t.Fatalf("write %s: %v", destination, err)
	}
	if err := os.Chmod(destination, mode); err != nil {
		t.Fatal(err)
	}
}

// freeLoopbackAddr reserves a free loopback port; the caller relies on the
// up-stub exiting non-zero on a failed bind, so a rebind race cannot fake readiness.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free loopback port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func startHostUpstub(t *testing.T, upstubPath, addr string) {
	t.Helper()
	_ = os.Remove(upstubRequestLog)
	env := append(os.Environ(), upstubAddrEnv+"="+addr)
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, upstubPath)
	command.Env = env
	if err := command.Start(); err != nil {
		cancel()
		t.Fatalf("start up-stub server: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = command.Wait()
		_ = os.Remove(upstubRequestLog)
	})
	ready := runHostEnv(t, "", 15*time.Second, env, upstubPath, "-wait")
	if ready.exitCode != 0 {
		t.Fatalf("synthetic /up server did not become ready: %s", ready.output)
	}
}

func assertHostCommandAbsent(t *testing.T, path, name string) {
	t.Helper()
	result := runHostScrubbed(t, "", 10*time.Second, path, "sh", "-c", "command -v "+name)
	if result.exitCode == 0 {
		t.Fatalf("expected %s to be absent from the scrubbed PATH %q, found: %s", name, path, result.output)
	}
}

func assertHostInstalledConfiguration(t *testing.T, root, version, platformKey string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatalf("read installed config.json: %v", err)
	}
	for _, want := range []string{
		`"release_version":"` + version + `"`,
		`"platform":"` + platformKey + `"`,
		`"runner_id":"` + smokeRunnerID + `"`,
		`"profile_id":"` + smokeProfileID + `"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("installed config.json missing %q: %s", want, raw)
		}
	}
}

// assertHostLauncher checks the launcher is private, owner-executable, and
// targets the installed release's content-addressed store by absolute path.
func assertHostLauncher(t *testing.T, home, root, name string) {
	t.Helper()
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0700 {
		t.Fatalf("launcher %s must be a private, owner-executable regular file: %v (%v)", name, info, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read launcher %s: %v", name, err)
	}
	installedSetupSuffix := "/bin/shipmunk-setup' " + name + " '" + root + "' \"$@\""
	if !strings.Contains(string(content), home+"/.shipmunk/releases/") || !strings.Contains(string(content), installedSetupSuffix) {
		t.Fatalf("launcher %s does not target the installed release by absolute path: %s", name, content)
	}
}

func assertHostAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("expected %s to be absent (no implicitly started or leftover work)", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
}

// assertHostUpstubReceived confirms the request reached the up-stub over the
// network rather than failing at an earlier local step.
func assertHostUpstubReceived(t *testing.T, want string) {
	t.Helper()
	raw, err := os.ReadFile(upstubRequestLog)
	if err != nil {
		t.Fatalf("read up-stub request log: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == want {
			return
		}
	}
	t.Fatalf("up-stub never received exactly %q; log:\n%s", want, raw)
}

func runHostScrubbed(t *testing.T, dir string, timeout time.Duration, path string, args ...string) execResult {
	t.Helper()
	return runHostEnv(t, dir, timeout, []string{"PATH=" + path}, args...)
}

// runHostEnv runs args[0] with exactly env (nil inherits this process's own).
func runHostEnv(t *testing.T, dir string, timeout time.Duration, env []string, args ...string) execResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Dir = dir
	command.Env = env
	output, err := command.CombinedOutput()
	result := execResult{output: string(output)}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		result.exitCode = 0
	case errors.As(err, &exitErr):
		result.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return result
}
