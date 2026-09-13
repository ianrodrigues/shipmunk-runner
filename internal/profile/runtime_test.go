package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	mu                  sync.Mutex
	commands            [][]string
	environments        [][]string
	imageID             string
	containerID         string
	containerName       string
	imageRef            string
	running             bool
	present             bool
	label               string
	nameLabel           string
	reservation         bool
	commandError        error
	commandOut          string
	commandErr          string
	blockExec           bool
	execDelay           time.Duration
	execStdin           string
	blockCreate         bool
	blockReservation    bool
	createNotDispatched bool
	homeMountSource     string
	afterImage          func() error
}

type fakeCreatePhase struct {
	started  int
	finished []string
}

func (phase *fakeCreatePhase) CreateStarted() error {
	phase.started++
	return nil
}

func (phase *fakeCreatePhase) CreateFinished(containerID string) error {
	phase.finished = append(phase.finished, containerID)
	return nil
}

type noOpCreatePhase struct{}

func (noOpCreatePhase) CreateStarted() error        { return nil }
func (noOpCreatePhase) CreateFinished(string) error { return nil }

func newFakeCommand() *fakeCommand {
	return &fakeCommand{imageID: testImageID, containerID: testContainerID, label: "true"}
}

func prepareRunningProfile(fake *fakeCommand) {
	fake.present = true
	fake.containerName = testProfileName
	fake.nameLabel = testProfileName
	fake.imageRef = testImageID
	fake.homeMountSource = "/private/profile/home"
	fake.running = true
}

func (fake *fakeCommand) Run(ctx context.Context, environment []string, stdin io.Reader, stdout, stderr io.Writer, args ...string) (bool, error) {
	fake.mu.Lock()
	fake.commands = append(fake.commands, append([]string(nil), args...))
	fake.environments = append(fake.environments, append([]string(nil), environment...))
	fake.mu.Unlock()
	if len(args) < 2 || args[0] != "docker" {
		return false, errors.New("unexpected executable")
	}
	args = args[1:]
	switch args[0] {
	case "image":
		_, _ = io.WriteString(stdout, fake.imageID+"\n")
		if fake.afterImage != nil {
			if err := fake.afterImage(); err != nil {
				return true, err
			}
		}
		return true, nil
	case "inspect":
		identifier := args[len(args)-1]
		fake.mu.Lock()
		present := fake.present && (identifier == fake.containerName || identifier == fake.containerID)
		inspection := fake.inspection()
		fake.mu.Unlock()
		if !present {
			_, _ = io.WriteString(stderr, "Error: No such object: "+identifier)
			return true, fakeExitError(1)
		}
		encoded, _ := json.Marshal(inspection)
		_, _ = stdout.Write(encoded)
		return true, nil
	case "create":
		isReservation := slices.Contains(args, profileReservationLabel+"=true")
		if fake.present {
			_, _ = io.WriteString(stderr, "Conflict. The container name is already in use")
			return true, fakeExitError(1)
		}
		if fake.createNotDispatched {
			return false, fakeExitError(1)
		}
		if (fake.blockCreate && !isReservation) || (fake.blockReservation && isReservation) {
			<-ctx.Done()
			return true, ctx.Err()
		}
		fake.mu.Lock()
		fake.present = true
		fake.running = false
		fake.reservation = isReservation
		for index := 0; index < len(args)-1; index++ {
			if args[index] == "--name" {
				fake.containerName = args[index+1]
			}
			if args[index] == "--label" && strings.HasPrefix(args[index+1], profileUIDLabel+"=") {
				fake.label = strings.TrimPrefix(args[index+1], profileUIDLabel+"=")
			}
			if args[index] == "--label" && strings.HasPrefix(args[index+1], profileNameLabel+"=") {
				fake.nameLabel = strings.TrimPrefix(args[index+1], profileNameLabel+"=")
			}
		}
		for index := 0; index+1 < len(args); index++ {
			if args[index] == "--mount" && strings.HasPrefix(args[index+1], "type=bind,") {
				fields := strings.Split(args[index+1], ",")
				fake.homeMountSource = strings.TrimPrefix(fields[1], "src=")
			}
			if args[index] == "--entrypoint" {
				fake.imageRef = args[index+2]
				break
			}
		}
		fake.mu.Unlock()
		_, _ = io.WriteString(stdout, fake.containerID+"\n")
		return true, nil
	case "start":
		fake.mu.Lock()
		fake.running = true
		fake.mu.Unlock()
		return true, nil
	case "exec":
		if fake.blockExec {
			<-ctx.Done()
			return true, ctx.Err()
		}
		if fake.execDelay > 0 {
			timer := time.NewTimer(fake.execDelay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return true, ctx.Err()
			case <-timer.C:
			}
		}
		if stdin != nil {
			input, _ := io.ReadAll(stdin)
			fake.execStdin = string(input)
		}
		_, _ = io.WriteString(stdout, fake.commandOut)
		_, _ = io.WriteString(stderr, fake.commandErr)
		return true, fake.commandError
	case "stop", "kill":
		fake.mu.Lock()
		fake.running = false
		fake.mu.Unlock()
		return true, nil
	case "rm":
		fake.mu.Lock()
		fake.present = false
		fake.reservation = false
		fake.mu.Unlock()
		return true, nil
	default:
		return true, errors.New("unexpected docker operation: " + args[0])
	}
}

func (fake *fakeCommand) inspection() map[string]any {
	var mounts []map[string]string
	if !fake.reservation {
		mounts = []map[string]string{{"Type": "bind", "Source": fake.homeMountSource, "Destination": "/profile"}}
	}
	return map[string]any{
		"Id":   fake.containerID,
		"Name": "/" + fake.containerName,
		"Config": map[string]any{
			"Image":  fake.imageRef,
			"Labels": map[string]string{profileUIDLabel: fake.label, profileNameLabel: fake.nameLabel, profileReservationLabel: map[bool]string{true: "true", false: ""}[fake.reservation]},
		},
		"State":  map[string]bool{"Running": fake.running},
		"Mounts": mounts,
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

func protectedHome(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartUsesResolvedImageAndCredentialOnlyIsolation(t *testing.T) {
	fake := newFakeCommand()
	runtime := newTestRuntime(t, fake)
	home := protectedHome(t)
	checkpoints := 0
	createPhase := &fakeCreatePhase{}
	if err := runtime.Start(context.Background(), testProfileName, home, func() error {
		checkpoints++
		return nil
	}, createPhase); err != nil {
		t.Fatal(err)
	}
	if createPhase.started != 1 || !slices.Equal(createPhase.finished, []string{testContainerID}) {
		t.Fatalf("watchdog create phases = started %d finished %v", createPhase.started, createPhase.finished)
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
	if strings.Contains(joined, "docker.sock") || strings.Contains(joined, "repository") {
		t.Fatalf("create command exposed an environment variable or unrelated mount: %v", create)
	}
	proxyVariables := []string{"HTTP_PROXY=", "http_proxy=", "HTTPS_PROXY=", "https_proxy=", "FTP_PROXY=", "ftp_proxy=", "ALL_PROXY=", "all_proxy=", "NO_PROXY=", "no_proxy="}
	environmentFlags := 0
	for index, argument := range create {
		if argument == "--env" {
			environmentFlags++
			if index+1 >= len(create) || !slices.Contains(proxyVariables, create[index+1]) {
				t.Errorf("container create contains an unexpected environment value near %d: %v", index, create)
			}
		}
	}
	if environmentFlags != len(proxyVariables) {
		t.Fatalf("proxy environment count = %d, want %d: %v", environmentFlags, len(proxyVariables), create)
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
	if err := runtime.Start(context.Background(), testProfileName, protectedHome(t), func() error { return nil }, noOpCreatePhase{}); err == nil {
		t.Fatal("accepted a mutable image inspect result")
	}
	if len(fake.commands) != 1 {
		t.Fatalf("commands = %v, want only image resolution", fake.commands)
	}
	if _, err := newDockerRuntime(runtimeConfig{image: "image", uid: 0, gid: 0}); err == nil {
		t.Fatal("accepted root as the profile runtime account")
	}
}

func TestStartRejectsInsecureProfileHomeAndPathReplacement(t *testing.T) {
	fake := newFakeCommand()
	runtime := newTestRuntime(t, fake)
	unsafeHome := protectedHome(t)
	if err := os.Chmod(unsafeHome, 0755); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background(), testProfileName, unsafeHome, func() error { return nil }, noOpCreatePhase{}); err == nil {
		t.Fatal("accepted a profile home with broad permissions")
	}
	if len(fake.commands) != 0 {
		t.Fatalf("Docker commands ran for an insecure home: %v", fake.commands)
	}

	home := protectedHome(t)
	target := protectedHome(t)
	backup := home + ".original"
	fake.afterImage = func() error {
		if err := os.Rename(home, backup); err != nil {
			return err
		}
		if err := os.Symlink(target, home); err != nil {
			return err
		}
		return nil
	}
	if err := runtime.Start(context.Background(), testProfileName, home, func() error { return nil }, noOpCreatePhase{}); err == nil {
		t.Fatal("accepted a profile home path replaced before container creation")
	}
	for _, command := range fake.commands {
		if len(command) > 1 && command[1] == "create" {
			t.Fatalf("Docker create ran after mount path replacement: %v", command)
		}
	}
}

func TestStartMarksOnlyDispatchedCreatesUncertain(t *testing.T) {
	t.Run("pre-dispatch failure", func(t *testing.T) {
		fake := newFakeCommand()
		fake.createNotDispatched = true
		runtime := newTestRuntime(t, fake)
		err := runtime.Start(context.Background(), testProfileName, protectedHome(t), func() error { return nil }, noOpCreatePhase{})
		if err == nil || errors.Is(err, ErrCreateUncertain) {
			t.Fatalf("Start error = %v, want ordinary pre-dispatch failure", err)
		}
		if err := runtime.Stop(context.Background(), testProfileName, false); err != nil {
			t.Fatalf("Stop after pre-dispatch failure = %v", err)
		}
	})

	t.Run("timed out after dispatch and late container", func(t *testing.T) {
		fake := newFakeCommand()
		fake.blockCreate = true
		runtime := newTestRuntime(t, fake)
		runtime.config.dockerTimeout = 20 * time.Millisecond
		err := runtime.Start(context.Background(), testProfileName, protectedHome(t), func() error { return nil }, noOpCreatePhase{})
		if !errors.Is(err, ErrCreateUncertain) {
			t.Fatalf("Start error = %v, want ErrCreateUncertain", err)
		}
		if err := runtime.Stop(context.Background(), testProfileName, true); !errors.Is(err, ErrCreateUncertain) {
			t.Fatalf("Stop while uncertain name is absent = %v", err)
		}
		fake.blockCreate = false
		prepareRunningProfile(fake)
		if err := runtime.Stop(context.Background(), testProfileName, true); err != nil {
			t.Fatalf("Stop after late owned container appeared = %v", err)
		}
		if fake.present {
			t.Fatal("late container remained after uncertainty reconciliation")
		}
	})
}

func TestRunUsesAllowlistedCommandAndBoundedCapturedOutput(t *testing.T) {
	fake := newFakeCommand()
	prepareRunningProfile(fake)
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
		"exec -i " + testContainerID + " /usr/bin/env -i",
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

func TestRunRejectsWrongImageStoppedOrUnownedContainerBeforeExec(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*fakeCommand)
	}{
		{name: "wrong image", mutate: func(fake *fakeCommand) { fake.imageRef = "sha256:" + strings.Repeat("b", 64) }},
		{name: "stopped", mutate: func(fake *fakeCommand) { fake.running = false }},
		{name: "unowned", mutate: func(fake *fakeCommand) { fake.nameLabel = "shipmunk-profile-01k4w000000000000000000002" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeCommand()
			prepareRunningProfile(fake)
			test.mutate(fake)
			runtime := newTestRuntime(t, fake)
			if _, err := runtime.Run(context.Background(), testProfileName, AgentCodex, "probe", func() error { return nil }); err == nil {
				t.Fatal("executed against an unexpected profile container")
			}
			for _, command := range fake.commands {
				if len(command) > 1 && command[1] == "exec" {
					t.Fatalf("native command executed before container validation: %v", command)
				}
			}
		})
	}
}

func TestRunRejectsOutputOverflowAndPreservesNativeExitCode(t *testing.T) {
	fake := newFakeCommand()
	prepareRunningProfile(fake)
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
	prepareRunningProfile(fake)
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
	prepareRunningProfile(fake)
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
	prepareRunningProfile(fake)
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
	prepareRunningProfile(fake)
	runtime := newTestRuntime(t, fake)
	if err := runtime.Stop(context.Background(), testProfileName, false); err != nil {
		t.Fatal(err)
	}
	if fake.present {
		t.Fatal("Stop returned before the container was removed")
	}
	fake.present = true
	fake.label = "false"
	if err := runtime.Stop(context.Background(), testProfileName, false); err == nil {
		t.Fatal("stopped a container without the runtime ownership label")
	}
}

func TestStopAcceptsAlreadyAbsentContainer(t *testing.T) {
	fake := newFakeCommand()
	runtime := newTestRuntime(t, fake)
	if err := runtime.Stop(context.Background(), testProfileName, false); err != nil {
		t.Fatal(err)
	}
}

func TestStopUsesIndependentCleanupContext(t *testing.T) {
	fake := newFakeCommand()
	prepareRunningProfile(fake)
	runtime := newTestRuntime(t, fake)

	operationContext, cancelOperation := context.WithCancel(context.Background())
	cancelOperation()
	if operationContext.Err() == nil {
		t.Fatal("operation context was not canceled")
	}

	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), time.Second)
	defer cancelCleanup()
	if err := runtime.Stop(cleanupContext, testProfileName, false); err != nil {
		t.Fatalf("Stop with an independent cleanup context = %v", err)
	}
	if fake.present {
		t.Fatal("Stop did not remove the profile container")
	}
}

func TestStopDoesNotAcknowledgeAbsenceForAnUncertainCreate(t *testing.T) {
	fake := newFakeCommand()
	runtime := newTestRuntime(t, fake)
	if err := runtime.Stop(context.Background(), testProfileName, true); !errors.Is(err, ErrCreateUncertain) {
		t.Fatalf("Stop error = %v, want ErrCreateUncertain", err)
	}
	if err := runtime.Stop(context.Background(), testProfileName, false); err != nil {
		t.Fatalf("idempotent Stop without create uncertainty = %v", err)
	}
}

func TestReconcileCreateReservesAbsentNameBeforeReleasingIt(t *testing.T) {
	fake := newFakeCommand()
	fake.blockCreate = true
	runtime := newTestRuntime(t, fake)
	runtime.config.dockerTimeout = 20 * time.Millisecond
	if err := runtime.Start(context.Background(), testProfileName, protectedHome(t), func() error { return nil }, noOpCreatePhase{}); !errors.Is(err, ErrCreateUncertain) {
		t.Fatalf("Start error = %v, want ErrCreateUncertain", err)
	}
	if err := runtime.ReconcileCreate(context.Background(), testProfileName); err != nil {
		t.Fatalf("ReconcileCreate() = %v", err)
	}
	if fake.present {
		t.Fatal("create reservation was not removed after reconciliation")
	}
	var reservationCreate, reservationRemove int
	for _, command := range fake.commands {
		if len(command) > 1 && command[1] == "create" && slices.Contains(command, profileReservationLabel+"=true") {
			reservationCreate++
		}
		if len(command) > 1 && command[1] == "rm" {
			reservationRemove++
		}
	}
	if reservationCreate != 1 || reservationRemove != 1 {
		t.Fatalf("reservation create/remove counts = %d/%d; commands=%v", reservationCreate, reservationRemove, fake.commands)
	}
}

func TestReconcileCreateRemovesLateOwnedContainerBeforeReservingName(t *testing.T) {
	fake := newFakeCommand()
	fake.blockCreate = true
	runtime := newTestRuntime(t, fake)
	runtime.config.dockerTimeout = 20 * time.Millisecond
	if err := runtime.Start(context.Background(), testProfileName, protectedHome(t), func() error { return nil }, noOpCreatePhase{}); !errors.Is(err, ErrCreateUncertain) {
		t.Fatalf("Start error = %v, want ErrCreateUncertain", err)
	}
	fake.blockCreate = false
	prepareRunningProfile(fake)
	if err := runtime.ReconcileCreate(context.Background(), testProfileName); err != nil {
		t.Fatalf("ReconcileCreate() = %v", err)
	}
	if fake.present {
		t.Fatal("late container or create reservation remained after reconciliation")
	}
	if fake.reservation {
		t.Fatal("reconciliation removed the late container but left the reservation behind")
	}
}

func TestReconcileCreateRetainsUncertaintyAfterDispatchedReservationTimeout(t *testing.T) {
	fake := newFakeCommand()
	fake.blockReservation = true
	runtime := newTestRuntime(t, fake)
	runtime.config.dockerTimeout = 20 * time.Millisecond
	if err := runtime.ReconcileCreate(context.Background(), testProfileName); !errors.Is(err, ErrCreateUncertain) {
		t.Fatalf("ReconcileCreate() = %v, want ErrCreateUncertain", err)
	}
	if fake.present {
		t.Fatal("timed-out reservation unexpectedly became visible")
	}
}
