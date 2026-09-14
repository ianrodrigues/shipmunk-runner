// Package installsmoke installs a real published (or freshly built) runner
// release archive inside a container that has neither PHP nor a Go
// toolchain, and drives it through the real shipmunk-setup binary.
//
// It proves: the archive extracts and verifies per docs/releases.md, guided
// setup completes end to end against a synthetic /up server and a faked
// docker client (reusing the technique from
// TestCompiledSetupInstallsIntoCleanHomeWithoutStartingWork), the installed
// run/connect launchers exist, are executable, and invoke the exact
// installed release by absolute path, and setup never starts queued work.
// It additionally invokes the installed run --once and connect probe
// launchers to prove the compiled binary executes natively in a
// PHP/Go-free environment (no missing interpreter, no missing shared
// library) and leaves no queued-work state behind, even though those
// commands cannot fully succeed without a live control plane and a real
// native profile login.
//
// It does not prove: real Docker container isolation or lifecycle (the
// docker client is faked, matching the existing unit test's approach), a
// live native login or control-plane compatibility (see docs/runtime/RT-02.md
// and RT-03.md), or darwin host behavior — this container only exercises the
// linux-amd64 and linux-arm64 release tuples. A macOS host still needs its
// own smoke run; this package does not claim that coverage.
package installsmoke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	shipmunkrelease "github.com/ianrodrigues/shipmunk-runner/internal/release"
)

const (
	envEnable     = "SHIPMUNK_INSTALL_SMOKE_TEST"
	envArch       = "SHIPMUNK_INSTALL_SMOKE_ARCH"
	envArchiveDir = "SHIPMUNK_INSTALL_SMOKE_ARCHIVE_DIR"
	envVersion    = "SHIPMUNK_INSTALL_SMOKE_VERSION"
	envImage      = "SHIPMUNK_INSTALL_SMOKE_IMAGE"
	envUpstubDir  = "SHIPMUNK_INSTALL_SMOKE_UPSTUB_DIR"

	defaultVersion = "v0.0.0-installsmoke"
	// A multi-arch manifest-list digest: docker resolves the matching
	// per-arch image for whichever --platform is requested.
	defaultImage = "debian@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171"

	smokeUser      = "smoke"
	smokeRunnerID  = "01k4w000000000000000000001"
	smokeProfileID = "01k4w000000000000000000002"
)

func TestCleanContainerInstallsReleaseWithoutPHPOrGo(t *testing.T) {
	if os.Getenv(envEnable) != "1" {
		t.Skip("set " + envEnable + "=1 for the clean-container release install smoke test (requires Docker and expect)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required for the clean-container install smoke test")
	}
	if _, err := exec.LookPath("expect"); err != nil {
		t.Skip("expect is required to provide an operator terminal for guided setup")
	}

	arch := os.Getenv(envArch)
	if arch == "" {
		arch = runtime.GOARCH
	}
	if arch != "amd64" && arch != "arm64" {
		t.Fatalf("%s=%q must be amd64 or arm64", envArch, arch)
	}
	platformKey := "linux-" + arch
	dockerPlatform := "linux/" + arch

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

	image := envDefault(envImage, defaultImage)
	container := uniqueName("shipmunk-install-smoke")
	dockerT(t, ctx, 3*time.Minute, "run", "-d", "--platform", dockerPlatform,
		"--name", container, "--label", "shipmunk-install-smoke=1", image, "sleep", "1800")
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanupCtx, "docker", "rm", "-f", container).Run()
	})

	assertCommandAbsent(t, ctx, container, "php")
	assertCommandAbsent(t, ctx, container, "go")

	home := "/home/" + smokeUser
	fixture := home + "/fixture"
	dockerT(t, ctx, 15*time.Second, "exec", container, "useradd", "-m", "-d", home, "-s", "/bin/sh", smokeUser)
	dockerT(t, ctx, 15*time.Second, "exec", container, "mkdir", "-p", fixture+"/bin")

	upstub := resolveUpstub(t, arch)
	fakeDocker := writeFakeDockerScript(t)
	bundlePath := writeSetupBundle(t, smokeRunnerID, smokeProfileID)

	for local, remote := range map[string]string{
		archivePath: fixture + "/" + platform.Archive.Name,
		filepath.Join(distDir, "runner-release.json"): fixture + "/runner-release.json",
		checksumsPath: fixture + "/SHA256SUMS",
		fakeDocker:    fixture + "/bin/docker",
		upstub:        fixture + "/upstub",
		bundlePath:    fixture + "/setup.json",
	} {
		dockerCP(t, ctx, local, container+":"+remote)
	}
	dockerT(t, ctx, 15*time.Second, "exec", container, "chown", "-R", smokeUser+":"+smokeUser, fixture)
	dockerT(t, ctx, 15*time.Second, "exec", container, "chmod", "0755", fixture+"/bin/docker", fixture+"/upstub")
	dockerT(t, ctx, 15*time.Second, "exec", container, "chmod", "0600", fixture+"/setup.json")

	// Extract only the setup binary, exactly as the published bootstrap
	// script does: the archive itself stays intact for --release-archive.
	extract := dockerExecAs(t, ctx, 30*time.Second, container, smokeUser,
		"tar", "-xf", fixture+"/"+platform.Archive.Name, "-C", fixture, "bin/shipmunk-setup")
	if extract.exitCode != 0 {
		t.Fatalf("extract shipmunk-setup: %s", extract.output)
	}

	verify := dockerExecAs(t, ctx, 30*time.Second, container, smokeUser,
		"sh", "-c", "cd "+fixture+" && sha256sum --ignore-missing -c SHA256SUMS")
	if verify.exitCode != 0 {
		t.Fatalf("archive/manifest checksum verification failed: %s", verify.output)
	}

	startUpstub(t, ctx, container, fixture)

	setupPath := fixture + "/bin/shipmunk-setup"
	pathEnv := "PATH=" + fixture + "/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	output, exitCode := runExpect(t, 90*time.Second,
		"docker", "exec", "-it", "-u", smokeUser, "-e", pathEnv, container,
		setupPath, fixture+"/setup.json",
		"--release-manifest", fixture+"/runner-release.json",
		"--release-archive", fixture+"/"+platform.Archive.Name)
	if exitCode != 0 || !strings.Contains(output, "Installed the verified runner release.") || !strings.Contains(output, "Setup did not start queued work.") {
		t.Fatalf("guided setup did not complete cleanly (exit %d): %s", exitCode, output)
	}

	root := home + "/.shipmunk/runners/" + smokeRunnerID
	assertInstalledConfiguration(t, ctx, container, root, manifest.Version, platformKey)
	assertLauncher(t, ctx, container, home, root, "run")
	assertLauncher(t, ctx, container, home, root, "connect")
	assertAbsent(t, ctx, container, root+"/state/active-attempt.json")

	// Setup only ever installs binaries under its own private release
	// store; it must not have made php or go reachable on PATH.
	assertCommandAbsent(t, ctx, container, "php")
	assertCommandAbsent(t, ctx, container, "go")

	runOnce := dockerExecAs(t, ctx, 60*time.Second, container, smokeUser, root+"/run", "--once")
	if runOnce.exitCode != 1 {
		t.Fatalf("run --once against a stub control plane should fail cleanly with exit 1, got %d: %s", runOnce.exitCode, runOnce.output)
	}
	if strings.Contains(runOnce.output, "no such file or directory") || strings.Contains(strings.ToLower(runOnce.output), "exec format error") {
		t.Fatalf("run --once failed for an environment reason rather than a control-plane reason: %s", runOnce.output)
	}
	assertAbsent(t, ctx, container, root+"/state/active-attempt.json")

	probeOutput, probeExit := runExpect(t, 30*time.Second, "docker", "exec", "-it", "-u", smokeUser, container, root+"/connect", "probe")
	if probeExit != 1 {
		t.Fatalf("connect probe against a stub control plane should fail cleanly with exit 1, got %d: %s", probeExit, probeOutput)
	}
	if strings.Contains(probeOutput, "no such file or directory") || strings.Contains(strings.ToLower(probeOutput), "exec format error") {
		t.Fatalf("connect probe failed for an environment reason rather than a control-plane reason: %s", probeOutput)
	}
	assertAbsent(t, ctx, container, root+"/state/active-attempt.json")

	t.Logf("clean-container install smoke passed for %s using image %s", platformKey, image)
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), time.Now().UnixNano())
}

func buildReleaseArchive(t *testing.T, ctx context.Context, output, version string) {
	t.Helper()
	root := repositoryRoot(t)
	buildCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if _, err := (shipmunkrelease.Builder{}).Build(buildCtx, root, output, version); err != nil {
		t.Fatalf("build release archive: %v", err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(resolved, "go.mod")); err != nil {
		t.Fatalf("resolved repository root %s has no go.mod: %v", resolved, err)
	}
	return resolved
}

func readManifest(t *testing.T, path string) shipmunkrelease.Manifest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read release manifest: %v", err)
	}
	var manifest shipmunkrelease.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode release manifest: %v", err)
	}
	return manifest
}

// resolveUpstub returns a linux/<arch> build of the loopback HTTP responder
// testdata/upstub/main.go uses for the installed release's /up check. A CI
// job that deliberately has no Go toolchain (see envUpstubDir) supplies one
// precompiled by a separate job that does; a local run with Go available
// compiles it on demand from the same source.
func resolveUpstub(t *testing.T, arch string) string {
	t.Helper()
	if dir := os.Getenv(envUpstubDir); dir != "" {
		path := filepath.Join(dir, "upstub-linux-"+arch)
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("precompiled up-stub binary %s is unavailable: %v", path, err)
		}
		return path
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("set " + envUpstubDir + " to a directory with a precompiled upstub-linux-" + arch + ", or run with a Go toolchain available")
	}
	binary := filepath.Join(t.TempDir(), "upstub")
	buildCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(buildCtx, "go", "build", "-o", binary, filepath.Join(repositoryRoot(t), "internal", "installsmoke", "testdata", "upstub"))
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build up-stub server: %v: %s", err, output)
	}
	return binary
}

// writeFakeDockerScript reuses the exact technique
// TestCompiledSetupInstallsIntoCleanHomeWithoutStartingWork uses on the
// host: a script that only answers "docker version" and "docker build
// --iidfile", enough for guided setup's Docker preflight and pinned image
// build without a real Docker-in-Docker daemon.
func writeFakeDockerScript(t *testing.T) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = version ]; then printf '26.0.0\\n26.0.0\\nlinux\\n'; exit 0; fi\n" +
		"while [ $# -gt 0 ]; do if [ \"$1\" = --iidfile ]; then shift; printf 'sha256:" + strings.Repeat("d", 64) + "\\n' > \"$1\"; exit 0; fi; shift; done\n" +
		"exit 1\n"
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSetupBundle(t *testing.T, runnerID, profileID string) string {
	t.Helper()
	bundle := map[string]any{
		"version": 1, "runtime_version": "0.154.0", "base_url": "http://127.0.0.1:8080",
		"runner_id": runnerID, "profile_id": profileID, "expires_at": "2099-01-01T00:00:00Z",
		"profile_token":   "1|AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"execution_token": "2|BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "setup.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startUpstub(t *testing.T, ctx context.Context, container, fixture string) {
	t.Helper()
	dockerT(t, ctx, 10*time.Second, "exec", "-d", "-u", smokeUser, container, fixture+"/upstub")
	ready := dockerExecAs(t, ctx, 15*time.Second, container, smokeUser, fixture+"/upstub", "-wait")
	if ready.exitCode != 0 {
		t.Fatalf("synthetic /up server did not become ready: %s", ready.output)
	}
}

func assertCommandAbsent(t *testing.T, ctx context.Context, container, name string) {
	t.Helper()
	result := dockerExecAs(t, ctx, 10*time.Second, container, "", "sh", "-c", "command -v "+name)
	if result.exitCode == 0 {
		t.Fatalf("expected %s to be absent from the clean container, found: %s", name, result.output)
	}
}

func assertInstalledConfiguration(t *testing.T, ctx context.Context, container, root, version, platformKey string) {
	t.Helper()
	result := dockerExecAs(t, ctx, 10*time.Second, container, smokeUser, "cat", root+"/config.json")
	if result.exitCode != 0 {
		t.Fatalf("read installed config.json: %s", result.output)
	}
	for _, want := range []string{
		`"release_version":"` + version + `"`,
		`"platform":"` + platformKey + `"`,
		`"runner_id":"` + smokeRunnerID + `"`,
		`"profile_id":"` + smokeProfileID + `"`,
	} {
		if !strings.Contains(result.output, want) {
			t.Fatalf("installed config.json missing %q: %s", want, result.output)
		}
	}
}

// assertLauncher checks that the installed run/connect launcher is private
// and owner-executable, and that it invokes the setup binary from its
// content-addressed release store (~/.shipmunk/releases/<sha256>/bin,
// not the transient bootstrap path setup was invoked from) against this
// exact installation root, matching setupLaunchers' fixed shape.
func assertLauncher(t *testing.T, ctx context.Context, container, home, root, name string) {
	t.Helper()
	permissions := dockerExecAs(t, ctx, 10*time.Second, container, smokeUser, "sh", "-c", "stat -c '%a' "+root+"/"+name)
	if permissions.exitCode != 0 || strings.TrimSpace(permissions.output) != "700" {
		t.Fatalf("launcher %s must be a private, owner-executable file: %q (%v)", name, permissions.output, permissions.exitCode)
	}
	content := dockerExecAs(t, ctx, 10*time.Second, container, smokeUser, "cat", root+"/"+name)
	if content.exitCode != 0 {
		t.Fatalf("read launcher %s: %s", name, content.output)
	}
	installedSetupSuffix := "/bin/shipmunk-setup' " + name + " '" + root + "' \"$@\""
	if !strings.Contains(content.output, home+"/.shipmunk/releases/") || !strings.Contains(content.output, installedSetupSuffix) {
		t.Fatalf("launcher %s does not target the installed release by absolute path: %s", name, content.output)
	}
}

func assertAbsent(t *testing.T, ctx context.Context, container, path string) {
	t.Helper()
	result := dockerExecAs(t, ctx, 10*time.Second, container, smokeUser, "test", "-e", path)
	if result.exitCode == 0 {
		t.Fatalf("expected %s to be absent (no implicitly started or leftover work)", path)
	}
}

type execResult struct {
	output   string
	exitCode int
}

func dockerT(t *testing.T, ctx context.Context, timeout time.Duration, args ...string) {
	t.Helper()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := exec.CommandContext(runCtx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func dockerCP(t *testing.T, ctx context.Context, source, destination string) {
	t.Helper()
	dockerT(t, ctx, 30*time.Second, "cp", source, destination)
}

func dockerExecAs(t *testing.T, ctx context.Context, timeout time.Duration, container, user string, args ...string) execResult {
	t.Helper()
	full := []string{"exec"}
	if user != "" {
		full = append(full, "-u", user)
	}
	full = append(full, container)
	full = append(full, args...)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := exec.CommandContext(runCtx, "docker", full...).CombinedOutput()
	result := execResult{output: string(output)}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		result.exitCode = 0
	case errors.As(err, &exitErr):
		result.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("docker exec %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return result
}

// expectDriver answers the one guided-setup confirmation prompt with "y" if
// it appears, then waits for the spawned command to exit and re-exits with
// its exact status. It provides the real operator pseudo-terminal that
// RunSetup and native profile operations require on stdin/stdout/stderr.
const expectDriver = `
log_user 1
set timeout %d
spawn {*}$argv
expect {
  -re {Continue with this server and runner\?} { send "y\r"; exp_continue }
  eof {}
  timeout { exit 97 }
}
catch wait result
exit [lindex $result 3]
`

func runExpect(t *testing.T, timeout time.Duration, argv ...string) (string, int) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "drive.expect")
	content := fmt.Sprintf(expectDriver, int(timeout.Seconds()))
	if err := os.WriteFile(script, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(context.Background(), timeout+15*time.Second)
	defer cancel()
	command := exec.CommandContext(runCtx, "expect", append([]string{script}, argv...)...)
	output, err := command.CombinedOutput()
	exitCode := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("expect driver: %v: %s", err, output)
	}
	return string(output), exitCode
}
