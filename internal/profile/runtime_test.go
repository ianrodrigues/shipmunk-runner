package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testProfileName = "shipmunk-profile-01k4w000000000000000000001"
	testImageID     = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testContainerID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

type fakeCommand struct {
	mu            sync.Mutex
	commands      [][]string
	environments  [][]string
	imageID       string
	containerID   string
	containerName string
	imageRef      string
	running       bool
	present       bool
	label         string
	commandError  error
	commandOut    string
	commandErr    string
	blockExec     bool
	execDelay     time.Duration
	execStdin     string
}

func newFakeCommand() *fakeCommand {
	return &fakeCommand{imageID: testImageID, containerID: testContainerID, label: "true"}
}

func (fake *fakeCommand) Run(ctx context.Context, environment []string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	fake.mu.Lock()
	fake.commands = append(fake.commands, append([]string(nil), args...))
	fake.environments = append(fake.environments, append([]string(nil), environment...))
	fake.mu.Unlock()
	if len(args) < 2 || args[0] != "docker" {
		return errors.New("unexpected executable")
	}
	args = args[1:]
	switch args[0] {
	case "image":
		_, _ = io.WriteString(stdout, fake.imageID+"\n")
		return nil
	case "inspect":
		identifier := args[len(args)-1]
		fake.mu.Lock()
		present := fake.present && (identifier == fake.containerName || identifier == fake.containerID)
		inspection := fake.inspection()
		fake.mu.Unlock()
		if !present {
			_, _ = io.WriteString(stderr, "Error: No such object: "+identifier)
			return fakeExitError(1)
		}
		encoded, _ := json.Marshal(inspection)
		_, _ = stdout.Write(encoded)
		return nil
	case "create":
		fake.mu.Lock()
		fake.present = true
		fake.running = false
		for index := 0; index < len(args)-1; index++ {
			if args[index] == "--name" {
				fake.containerName = args[index+1]
			}
			if args[index] == "--label" && strings.HasPrefix(args[index+1], profileUIDLabel+"=") {
				fake.label = strings.TrimPrefix(args[index+1], profileUIDLabel+"=")
			}
		}
		for index := 0; index+1 < len(args); index++ {
			if args[index] == "--entrypoint" {
				fake.imageRef = args[index+2]
				break
			}
		}
		fake.mu.Unlock()
		_, _ = io.WriteString(stdout, fake.containerID+"\n")
		return nil
	case "start":
		fake.mu.Lock()
		fake.running = true
		fake.mu.Unlock()
		return nil
	case "exec":
		if fake.blockExec {
			<-ctx.Done()
			return ctx.Err()
		}
		if fake.execDelay > 0 {
			timer := time.NewTimer(fake.execDelay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
		if stdin != nil {
			input, _ := io.ReadAll(stdin)
			fake.execStdin = string(input)
		}
		_, _ = io.WriteString(stdout, fake.commandOut)
		_, _ = io.WriteString(stderr, fake.commandErr)
		return fake.commandError
	case "stop", "kill":
		fake.mu.Lock()
		fake.running = false
		fake.mu.Unlock()
		return nil
	case "rm":
		fake.mu.Lock()
		fake.present = false
		fake.mu.Unlock()
		return nil
	default:
		return errors.New("unexpected docker operation: " + args[0])
	}
}

func (fake *fakeCommand) inspection() map[string]any {
	return map[string]any{
		"Id":   fake.containerID,
		"Name": "/" + fake.containerName,
		"Config": map[string]any{
			"Image":  fake.imageRef,
			"Labels": map[string]string{profileUIDLabel: fake.label},
		},
		"State": map[string]bool{"Running": fake.running},
	}
}

type fakeExit struct{ code int }

func (exit fakeExit) Error() string { return "fake command exit" }
func (exit fakeExit) ExitCode() int { return exit.code }

func fakeExitError(code int) error { return fakeExit{code: code} }

func newTestRuntime(t *testing.T, fake *fakeCommand) *dockerRuntime {
	t.Helper()
	runtime, err := newDockerRuntime(runtimeConfig{
		image: "shipmunk-profile-native:local", docker: "docker", uid: 1001, gid: 1001,
		commands: fake, stdin: strings.NewReader("login input\n"), stdout: io.Discard, stderr: io.Discard,
		dockerTimeout: time.Second, commandTimeout: time.Second, loginTimeout: time.Second,
		checkpointInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestStartUsesResolvedImageAndCredentialOnlyIsolation(t *testing.T) {
	fake := newFakeCommand()
	runtime := newTestRuntime(t, fake)
	home := t.TempDir()
	checkpoints := 0
	if err := runtime.Start(context.Background(), testProfileName, home, func() error {
		checkpoints++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if checkpoints < 4 {
		t.Fatalf("checkpoint calls = %d, want before image inspect, existing-name inspect, create, and start", checkpoints)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.imageRef != testImageID || !fake.running {
		t.Fatalf("created image=%q running=%t", fake.imageRef, fake.running)
	}
	var create []string
	for _, command := range fake.commands {
		if len(command) > 1 && command[1] == "create" {
			create = command
			break
		}
	}
	if len(create) == 0 {
		t.Fatal("Docker create was not called")
	}
	joined := strings.Join(create, " ")
	for _, expected := range []string{
		"--init", "--read-only", "--user 1001:1001", "--cap-drop ALL",
		"--security-opt no-new-privileges", "--pids-limit 64", "--memory 512m", "--cpus 1",
		"--ulimit nofile=1024:1024", "--log-driver none", "--network bridge",
		"--mount type=bind,src=" + home + ",dst=/profile",
		"--tmpfs /profile/.codex/tmp:rw,nosuid,nodev,size=16m,mode=0700,uid=1001,gid=1001",
		"--tmpfs /tmp:rw,nosuid,nodev,noexec,size=64m,mode=1777",
		"--entrypoint /usr/bin/env " + testImageID,
		"-i PATH=/usr/local/bin:/usr/bin:/bin",
		"test ! -e /etc/claude-code && test ! -e /etc/codex && exec sleep 1800",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("create command missing %q: %v", expected, create)
		}
	}
	if strings.Contains(joined, "--env") || strings.Contains(joined, "docker.sock") || strings.Contains(joined, "repository") {
		t.Fatalf("create command exposed an environment variable or unrelated mount: %v", create)
	}
	bindMounts := 0
	for _, argument := range create {
		if strings.HasPrefix(argument, "type=bind,") {
			bindMounts++
		}
	}
	if bindMounts != 1 {
		t.Fatalf("bind mount count = %d, want only the profile home: %v", bindMounts, create)
	}
	for _, environment := range fake.environments {
		for _, variable := range environment {
			name, _, _ := strings.Cut(variable, "=")
			switch name {
			case "PATH", "LANG", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY":
			default:
				t.Errorf("Docker client received non-allowlisted host variable %q", name)
			}
		}
	}
}

func TestStartRejectsMutableImageResponseAndRootAccount(t *testing.T) {
	fake := newFakeCommand()
	fake.imageID = "shipmunk-profile-native:local"
	runtime := newTestRuntime(t, fake)
	if err := runtime.Start(context.Background(), testProfileName, t.TempDir(), func() error { return nil }); err == nil {
		t.Fatal("accepted a mutable image inspect result")
	}
	if len(fake.commands) != 1 {
		t.Fatalf("commands = %v, want only image resolution", fake.commands)
	}
	if _, err := newDockerRuntime(runtimeConfig{image: "image", uid: 0, gid: 0}); err == nil {
		t.Fatal("accepted root as the profile runtime account")
	}
}

func TestRunUsesAllowlistedCommandAndBoundedCapturedOutput(t *testing.T) {
	fake := newFakeCommand()
	fake.present = true
	fake.containerName = testProfileName
	fake.imageRef = testImageID
	fake.commandOut = "version output"
	fake.commandErr = "diagnostic"
	runtime := newTestRuntime(t, fake)
	result, err := runtime.Run(context.Background(), testProfileName, "codex", "version", func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Stdout != "version output" || result.Stderr != "diagnostic" {
		t.Fatalf("result = %#v", result)
	}
	fake.mu.Lock()
	execArgs := append([]string(nil), fake.commands[len(fake.commands)-1]...)
	fake.mu.Unlock()
	joined := strings.Join(execArgs, " ")
	for _, expected := range []string{
		"exec -i " + testProfileName + " /usr/bin/env -i",
		"HOME=/profile CODEX_HOME=/profile/.codex CLAUDE_CONFIG_DIR=/profile/.claude",
		"XDG_CONFIG_HOME=/profile/.config XDG_CACHE_HOME=/profile/.cache",
		"PATH=/usr/local/bin:/usr/bin:/bin LANG=C.UTF-8 TERM=dumb DISABLE_AUTOUPDATER=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"/usr/local/bin/codex --version",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("exec command missing %q: %v", expected, execArgs)
		}
	}
	fake.mu.Lock()
	commands := fake.commands
	fake.mu.Unlock()
	if len(commands) < 2 || commands[len(commands)-2][1] != "inspect" || commands[len(commands)-1][1] != "exec" {
		t.Fatalf("profile ownership was not inspected before native execution: %v", commands)
	}
}

func TestRunRejectsOutputOverflowAndPreservesNativeExitCode(t *testing.T) {
	fake := newFakeCommand()
	fake.present = true
	fake.containerName = testProfileName
	fake.imageRef = testImageID
	fake.commandOut = strings.Repeat("x", nativeOutputLimit+1)
	runtime := newTestRuntime(t, fake)
	if _, err := runtime.Run(context.Background(), testProfileName, "codex", "probe", func() error { return nil }); err == nil {
		t.Fatal("accepted output above the native response bound")
	}
	fake.commandOut = "not logged in"
	fake.commandError = fakeExitError(9)
	result, err := runtime.Run(context.Background(), testProfileName, "codex", "probe", func() error { return nil })
	if err != nil || result.ExitCode != 9 || result.Stdout != "not logged in" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestInteractiveRunPassesStreamsThroughAndCheckpointsWhileRunning(t *testing.T) {
	fake := newFakeCommand()
	fake.present = true
	fake.containerName = testProfileName
	fake.imageRef = testImageID
	fake.commandOut = strings.Repeat("login prompt\n", 3000)
	fake.execDelay = 30 * time.Millisecond
	var terminalOut, terminalErr bytes.Buffer
	runtime, err := newDockerRuntime(runtimeConfig{
		image: "image", uid: 1001, gid: 1001, commands: fake,
		stdin: strings.NewReader("device code input"), stdout: &terminalOut, stderr: &terminalErr,
		loginTimeout: time.Second, checkpointInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := 0
	result, err := runtime.Run(context.Background(), testProfileName, "codex", "login", func() error {
		checkpoints++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "" || result.Stderr != "" || terminalOut.String() != fake.commandOut || fake.execStdin != "device code input" {
		t.Fatalf("interactive streams were not passed through (result=%#v bytes=%d stdin=%q)", result, terminalOut.Len(), fake.execStdin)
	}
	if checkpoints < 2 {
		t.Fatalf("checkpoint calls = %d, want an in-flight checkpoint", checkpoints)
	}
	fake.mu.Lock()
	args := strings.Join(fake.commands[len(fake.commands)-1], " ")
	fake.mu.Unlock()
	if !strings.Contains(args, "/usr/local/bin/codex login --device-auth") {
		t.Fatalf("unexpected interactive command: %s", args)
	}
}

func TestRunCheckpointFailureCancelsSubprocess(t *testing.T) {
	fake := newFakeCommand()
	fake.present = true
	fake.containerName = testProfileName
	fake.imageRef = testImageID
	fake.blockExec = true
	runtime := newTestRuntime(t, fake)
	checkpoints := 0
	want := errors.New("lease revoked")
	_, err := runtime.Run(context.Background(), testProfileName, "codex", "probe", func() error {
		checkpoints++
		if checkpoints >= 2 {
			return want
		}
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want checkpoint error", err)
	}
}

func TestRunAppliesNativeCommandTimeout(t *testing.T) {
	fake := newFakeCommand()
	fake.present = true
	fake.containerName = testProfileName
	fake.imageRef = testImageID
	fake.blockExec = true
	runtime := newTestRuntime(t, fake)
	runtime.config.commandTimeout = 20 * time.Millisecond
	_, err := runtime.Run(context.Background(), testProfileName, AgentCodex, "probe", func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("Run error = %v, want native command timeout", err)
	}
}

func TestStopConfirmsContainerAbsenceAndRefusesUnownedName(t *testing.T) {
	fake := newFakeCommand()
	fake.present = true
	fake.containerName = testProfileName
	fake.imageRef = testImageID
	runtime := newTestRuntime(t, fake)
	if err := runtime.Stop(context.Background(), testProfileName); err != nil {
		t.Fatal(err)
	}
	if fake.present {
		t.Fatal("Stop returned before the container was removed")
	}
	fake.present = true
	fake.label = "false"
	if err := runtime.Stop(context.Background(), testProfileName); err == nil {
		t.Fatal("stopped a container without the runtime ownership label")
	}
}

func TestStopAcceptsAlreadyAbsentContainer(t *testing.T) {
	fake := newFakeCommand()
	runtime := newTestRuntime(t, fake)
	if err := runtime.Stop(context.Background(), testProfileName); err != nil {
		t.Fatal(err)
	}
}
