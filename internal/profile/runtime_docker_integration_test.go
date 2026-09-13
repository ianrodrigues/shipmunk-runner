package profile

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDockerRuntimePreservesLinuxOwnershipAndNormalizesAfterStop(t *testing.T) {
	if os.Getenv("SHIPMUNK_PROFILE_DOCKER_TEST") != "1" {
		t.Skip("set SHIPMUNK_PROFILE_DOCKER_TEST=1 to run the Linux Docker profile fixture")
	}
	if runtime.GOOS != "linux" {
		t.Skip("host ownership assertions require a real Linux host")
	}
	image := os.Getenv("SHIPMUNK_PROFILE_IMAGE")
	if image == "" {
		t.Fatal("SHIPMUNK_PROFILE_IMAGE is required")
	}
	dockerContext, dockerCancel := context.WithTimeout(context.Background(), 15*time.Second)
	serverOS, err := exec.CommandContext(dockerContext, "docker", "version", "--format", "{{.Server.Os}}").Output()
	dockerCancel()
	if err != nil || strings.TrimSpace(string(serverOS)) != "linux" {
		t.Fatalf("a Linux Docker engine is required: %q %v", serverOS, err)
	}
	for _, variable := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "NODE_OPTIONS", "CODEX_HOME"} {
		t.Setenv(variable, "SYNTHETIC_SECRET_MUST_NOT_ENTER_CONTAINER")
	}

	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := initializeNativeHome(home); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".shipmunk-go-ownership-fixture"), []byte("synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sandboxName := "shipmunk-profile-01k4w000000000000000000009"
	profileRuntime, err := NewDockerRuntime(image)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if cleanupErr := profileRuntime.Stop(cleanupContext, sandboxName, false); cleanupErr != nil {
			t.Errorf("clean up profile fixture: %v", cleanupErr)
		}
	})

	checkpoint := func() error { return nil }
	if err := profileRuntime.Start(context.Background(), sandboxName, home, checkpoint, noOpCreatePhase{}); err != nil {
		t.Fatal(err)
	}
	result, err := profileRuntime.Run(context.Background(), sandboxName, AgentCodex, "probe", checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if got := StatusHealth(AgentCodex, result); got.Health != HealthReady {
		t.Fatalf("synthetic native probe health = %#v", got)
	}
	environment, err := os.ReadFile(filepath.Join(home, "environment"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(environment), "SYNTHETIC_SECRET_MUST_NOT_ENTER_CONTAINER") {
		t.Fatal("host credential or configuration environment entered the profile container")
	}
	dockerContext, dockerCancel = context.WithTimeout(context.Background(), 15*time.Second)
	inspectionOutput, err := exec.CommandContext(dockerContext, "docker", "inspect", sandboxName).Output()
	dockerCancel()
	if err != nil {
		t.Fatal(err)
	}
	var inspections []struct {
		Config     struct{ User string }
		HostConfig struct {
			ReadonlyRootfs bool
			CapDrop        []string
			SecurityOpt    []string
			NetworkMode    string
			PidsLimit      int64
		}
		Mounts []struct {
			Type, Source, Destination string
			RW                        bool
		}
	}
	if err := json.Unmarshal(inspectionOutput, &inspections); err != nil || len(inspections) != 1 {
		t.Fatalf("decode Docker inspection: %v", err)
	}
	inspection := inspections[0]
	if inspection.Config.User != strconv.Itoa(os.Geteuid())+":"+strconv.Itoa(os.Getegid()) ||
		!inspection.HostConfig.ReadonlyRootfs || !contains(inspection.HostConfig.CapDrop, "ALL") ||
		!hasDockerSecurityOption(inspection.HostConfig.SecurityOpt, "no-new-privileges") || inspection.HostConfig.NetworkMode != "bridge" ||
		inspection.HostConfig.PidsLimit != 64 {
		t.Fatalf("profile container hardening was not applied: %#v", inspection)
	}
	binds := 0
	for _, mount := range inspection.Mounts {
		if mount.Type == "bind" {
			binds++
			if mount.Source != home || mount.Destination != "/profile" || !mount.RW {
				t.Fatalf("unexpected profile bind mount: %#v", mount)
			}
		}
	}
	if binds != 1 {
		t.Fatalf("profile container bind mounts = %d, want 1", binds)
	}

	credential := filepath.Join(home, "native", "credential.json")
	for path, wantMode := range map[string]os.FileMode{
		filepath.Dir(credential): 0777,
		credential:               0666,
	} {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			t.Fatalf("inspect native-created path %s: %v", path, statErr)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
			t.Fatalf("native path %s ownership = %#v, want %d:%d", path, info.Sys(), os.Geteuid(), os.Getegid())
		}
		if info.Mode().Perm() != wantMode {
			t.Fatalf("native path %s mode = %#o, want fixture mode %#o", path, info.Mode().Perm(), wantMode)
		}
	}
	owner, err := os.ReadFile(filepath.Join(home, "native", "owner"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(owner)) != strconv.Itoa(os.Geteuid())+":"+strconv.Itoa(os.Getegid()) {
		t.Fatalf("native process identity = %q", owner)
	}

	if err := profileRuntime.Stop(context.Background(), sandboxName, false); err != nil {
		t.Fatal(err)
	}
	if err := normalizeNativeProfileTree(home, nativeTreeHooks{}); err != nil {
		t.Fatal(err)
	}
	for path, wantMode := range map[string]os.FileMode{
		filepath.Dir(credential):               0700,
		credential:                             0600,
		filepath.Join(home, "native", "owner"): 0600,
	} {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			t.Fatalf("inspect protected path %s: %v", path, statErr)
		}
		if info.Mode().Perm() != wantMode {
			t.Fatalf("protected path %s mode = %#o, want %#o", path, info.Mode().Perm(), wantMode)
		}
	}
	dockerContext, dockerCancel = context.WithTimeout(context.Background(), 15*time.Second)
	output, inspectErr := exec.CommandContext(dockerContext, "docker", "inspect", sandboxName).CombinedOutput()
	dockerCancel()
	if inspectErr == nil || !strings.Contains(string(output), "No such object") {
		t.Fatalf("profile container survived cleanup: %q %v", output, inspectErr)
	}
}

func hasDockerSecurityOption(options []string, expected string) bool {
	for _, option := range options {
		if option == expected || option == expected+":true" {
			return true
		}
	}
	return false
}

func TestDockerRuntimeRejectsForeignOwnedNativeEntryWithoutMutation(t *testing.T) {
	if os.Getenv("SHIPMUNK_PROFILE_DOCKER_TEST") != "1" || runtime.GOOS != "linux" {
		t.Skip("real Linux Docker ownership fixture is disabled")
	}
	if os.Geteuid() == 0 {
		t.Skip("foreign-owner fixture requires the dedicated runner to be non-root")
	}
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(home, "foreign")
	dockerContext, dockerCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dockerCancel()
	command := exec.CommandContext(dockerContext, "docker", "run", "--rm", "--network", "none", "--mount", "type=bind,src="+home+",dst=/profile", "alpine:3.20", "/bin/sh", "-c", "umask 000; printf foreign > /profile/foreign; chmod 0666 /profile/foreign")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create foreign-owned fixture: %q %v", output, err)
	}
	if err := normalizeNativeProfileTree(home, nativeTreeHooks{}); err == nil {
		t.Fatal("accepted a root-owned native entry")
	}
	info, err := os.Lstat(foreign)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm() != 0666 {
		t.Fatalf("rejected entry was mutated: ownership=%#v mode=%#o", info.Sys(), info.Mode().Perm())
	}
}
