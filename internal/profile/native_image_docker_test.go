package profile

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// nativeImageTestImage returns the pinned native/credential image to exercise.
// It is the same image `shipmunk-profile` runs for login, probe and native
// execution, built from runner/containers/Dockerfile.
func nativeImageTestImage(t *testing.T) string {
	t.Helper()
	if os.Getenv("SHIPMUNK_PROFILE_DOCKER_TEST") != "1" {
		t.Skip("set SHIPMUNK_PROFILE_DOCKER_TEST=1 to run the native image content regression")
	}
	image := os.Getenv("SHIPMUNK_PROFILE_IMAGE")
	if image == "" {
		t.Fatal("SHIPMUNK_PROFILE_IMAGE is required")
	}
	return image
}

func mustRunNativeImageCommand(t *testing.T, args []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("%v: %v: stdout=%q stderr=%q", args, err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// Exercises the image's default OpenSSL trust store and its pinned CLI
// versions without contacting an account or provider, isolated exactly as the
// runtime isolates a real native invocation.
func TestNativeImageTrustStoreAndPinnedVersions(t *testing.T) {
	image := nativeImageTestImage(t)
	isolated := []string{
		"docker", "run", "--rm", "--network", "none", "--read-only",
		"--user", "65532:65532", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--log-driver", "none",
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=64m,mode=1777",
	}
	trustScript := `test -s /etc/ssl/certs/ca-certificates.crt || {
    echo 'Native image is missing its system CA bundle.' >&2
    exit 1
}
root=/usr/share/ca-certificates/mozilla/ISRG_Root_X1.crt
openssl verify -no_check_time "$root"
if openssl verify -no_check_time -no-CAfile -no-CApath -no-CAstore "$root" >/dev/null 2>&1; then
    echo 'An untrusted root was unexpectedly accepted.' >&2
    exit 1
fi`
	mustRunNativeImageCommand(t, append(append([]string{}, isolated...), "--entrypoint", "/bin/sh", image, "-ec", trustScript))

	for _, agent := range []string{AgentCodex, AgentClaude} {
		version, ok := PinnedVersion(agent)
		if !ok {
			t.Fatalf("no pinned version recorded for %s", agent)
		}
		command, ok := Command(agent, "version")
		if !ok {
			t.Fatalf("no version command recorded for %s", agent)
		}
		args := append(append([]string{}, isolated...), "--entrypoint", "/usr/bin/env", image, "-i", "PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp")
		args = append(args, command...)
		output := mustRunNativeImageCommand(t, args)
		if !VersionMatches(agent, version, CommandResult{ExitCode: 0, Stdout: output}) {
			t.Fatalf("%s image reported %q, want pinned version %s", agent, output, version)
		}
	}
}

// The baked schema and MCP assets must keep the exact ownership and mode the
// Dockerfile assigns them, and remain readable to the unprivileged agent
// account the runtime actually executes as.
func TestNativeImageBakedAssetsAreReadableByUnprivilegedUser(t *testing.T) {
	image := nativeImageTestImage(t)
	modes := mustRunNativeImageCommand(t, []string{
		"docker", "run", "--rm", "--network", "none", "--read-only",
		"--entrypoint", "/usr/bin/stat", image, "-c", "%a:%u:%g",
		"/usr/local/lib/shipmunk", "/usr/local/lib/shipmunk/codex-mcp.mjs", "/usr/local/lib/shipmunk/codex-result.schema.json",
	})
	if strings.TrimSpace(modes) != "755:0:0\n644:0:0\n644:0:0" {
		t.Fatalf("baked asset ownership/modes = %q", modes)
	}

	readScript := `const fs = require('node:fs');
fs.accessSync('/usr/local/lib/shipmunk/codex-mcp.mjs', fs.constants.R_OK);
JSON.parse(fs.readFileSync('/usr/local/lib/shipmunk/codex-result.schema.json', 'utf8'));`
	mustRunNativeImageCommand(t, []string{
		"docker", "run", "--rm", "--network", "none", "--read-only",
		"--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--log-driver", "none",
		"--entrypoint", "/usr/bin/env", image, "-i", "PATH=/usr/local/bin:/usr/bin:/bin",
		"/usr/local/bin/node", "-e", readScript,
	})
}

// Dockerfile.dockerignore is the only thing keeping a live-checkout build of
// the native image (see docs/runtime/RT-02.md) from including a checkout's
// own environment and setup secrets. A trivial probe Dockerfile re-exports
// Docker's actual filtered build context via `--output type=local`, proving
// what the real ignore file admits without building the full multi-hundred
// megabyte runtime image just to inspect its context.
func TestNativeImageDockerfileContextExcludesCheckoutSecrets(t *testing.T) {
	if os.Getenv("SHIPMUNK_PROFILE_DOCKER_TEST") != "1" {
		t.Skip("set SHIPMUNK_PROFILE_DOCKER_TEST=1 to run the native image content regression")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ignore, err := os.ReadFile(filepath.Join(repository, "runner", "containers", "Dockerfile.dockerignore"))
	if err != nil {
		t.Fatal(err)
	}

	fixture := t.TempDir()
	mustWriteFixtureFile(t, filepath.Join(fixture, ".env"), "SYNTHETIC_CONTEXT_ENV_MUST_NOT_LEAVE_CHECKOUT")
	mustWriteFixtureFile(t, filepath.Join(fixture, "shipmunk-setup.json"), "SYNTHETIC_CONTEXT_TOKEN_MUST_NOT_LEAVE_CHECKOUT")
	mustWriteFixtureFile(t, filepath.Join(fixture, "runner", "containers", "nested-setup.json"), "SYNTHETIC_NESTED_SETUP_TOKEN")
	mustWriteFixtureFile(t, filepath.Join(fixture, "runner", "containers", "Dockerfile.dockerignore"), string(ignore))
	mustWriteFixtureFile(t, filepath.Join(fixture, "runner", "containers", "Dockerfile"), "FROM scratch\nCOPY . /\n")
	for _, asset := range []string{"codex-mcp.mjs", "codex-result.schema.json"} {
		copyFixtureFile(t, filepath.Join(repository, "runner", "containers", asset), filepath.Join(fixture, "runner", "containers", asset))
	}

	output := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", "build", "--file", filepath.Join(fixture, "runner", "containers", "Dockerfile"), "--output", "type=local,dest="+output, fixture)
	command.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v: %s", err, out)
	}

	var paths []string
	if err := filepath.WalkDir(output, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		relative, relErr := filepath.Rel(output, path)
		if relErr != nil {
			return relErr
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	want := []string{"runner/containers/codex-mcp.mjs", "runner/containers/codex-result.schema.json"}
	if !slices.Equal(paths, want) {
		t.Fatalf("native image build context = %v, want %v", paths, want)
	}
}

func mustWriteFixtureFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func copyFixtureFile(t *testing.T, source, destination string) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFixtureFile(t, destination, string(contents))
}
