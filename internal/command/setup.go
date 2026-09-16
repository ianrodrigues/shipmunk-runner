package command

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/codex"
	"github.com/ianrodrigues/shipmunk-runner/internal/install"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
)

var setupCheckServer = checkSetupServer
var setupBuildImage = buildSetupImage
var setupExecConnect = execConnectLauncher
var setupImageIDPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// errUnsafeSetupFile marks a readPublicSetupFile guard failure. It carries no
// message of its own: fmt.Errorf wraps it with the guard's reason, so the
// wrapped error's text IS the reason, and errors.Is still classifies it.
var errUnsafeSetupFile = errors.New("")

func setupFileMessage(prefix string, err error) string {
	if !errors.Is(err, errUnsafeSetupFile) {
		return prefix
	}
	return prefix + " " + err.Error() + "."
}

const (
	setupBundleMaxBytes   = 16 * 1024
	setupManifestMaxBytes = 128 * 1024
)

var (
	setupTokenPattern  = regexp.MustCompile(`^[0-9]+\|[A-Za-z0-9_]{40,160}$`)
	setupExpiryPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:Z|[+-]\d{2}:\d{2})$`)
	setupRuntime       = setupRuntimeHooks{
		effectiveUID: os.Geteuid,
		stdin:        os.Stdin,
		isTerminal: func(value any) bool {
			file, ok := value.(*os.File)
			return ok && isTerminal(file)
		},
		now:     time.Now,
		homeDir: os.UserHomeDir,
		goos:    runtime.GOOS,
		goarch:  runtime.GOARCH,
	}
)

type setupRuntimeHooks struct {
	effectiveUID func() int
	stdin        io.Reader
	isTerminal   func(any) bool
	now          func() time.Time
	homeDir      func() (string, error)
	goos         string
	goarch       string
}

type SetupOptions struct {
	SetupFile       string
	ServerURL       string
	ReleaseManifest string
	ReleaseArchive  string
	SkipConnect     bool
	Help            bool
	Version         bool
}

type SetupBundle struct {
	Version        int
	RuntimeVersion string
	BaseURL        string
	RunnerID       string
	ProfileID      string
	ExpiresAt      time.Time
	ProfileToken   string
	ExecutionToken string
}

func ParseSetupOptions(args []string) (SetupOptions, error) {
	var options SetupOptions
	serverURLProvided := false
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--help" || argument == "-h" || argument == "-help":
			options.Help = true
		case argument == "--version" || argument == "-version":
			options.Version = true
		case argument == "--skip-connect" || argument == "-skip-connect":
			options.SkipConnect = true
		case argument == "--server-url" || argument == "-server-url":
			if index+1 >= len(args) {
				return SetupOptions{}, errors.New("missing --server-url value")
			}
			index++
			options.ServerURL = args[index]
			serverURLProvided = true
		case strings.HasPrefix(argument, "--server-url="):
			options.ServerURL = strings.TrimPrefix(argument, "--server-url=")
			serverURLProvided = true
		case argument == "--release-manifest" || argument == "-release-manifest":
			if index+1 >= len(args) {
				return SetupOptions{}, errors.New("missing --release-manifest value")
			}
			index++
			options.ReleaseManifest = args[index]
		case strings.HasPrefix(argument, "--release-manifest="):
			options.ReleaseManifest = strings.TrimPrefix(argument, "--release-manifest=")
		case argument == "--release-archive" || argument == "-release-archive":
			if index+1 >= len(args) {
				return SetupOptions{}, errors.New("missing --release-archive value")
			}
			index++
			options.ReleaseArchive = args[index]
		case strings.HasPrefix(argument, "--release-archive="):
			options.ReleaseArchive = strings.TrimPrefix(argument, "--release-archive=")
		case strings.HasPrefix(argument, "-"):
			return SetupOptions{}, errors.New("unknown setup option")
		default:
			if options.SetupFile != "" {
				return SetupOptions{}, errors.New("setup accepts exactly one setup file")
			}
			options.SetupFile = argument
		}
	}
	if serverURLProvided && options.ServerURL == "" {
		return SetupOptions{}, errors.New("--server-url cannot be empty")
	}
	if !options.Help && !options.Version && options.SetupFile == "" {
		return SetupOptions{}, errors.New("setup file is required")
	}
	if !options.Help && !options.Version && (options.ReleaseManifest == "" || options.ReleaseArchive == "") {
		return SetupOptions{}, errors.New("release manifest and archive are required")
	}
	return options, nil
}

func ParseSetupBundle(raw []byte, serverURLOverride string, now time.Time) (SetupBundle, error) {
	value, err := protocol.Decode(raw, setupBundleMaxBytes)
	if err != nil {
		return SetupBundle{}, errors.New("The setup file is not valid JSON or exceeds its size limit.")
	}
	data, err := setupObject(value)
	if err != nil {
		return SetupBundle{}, errors.New("The setup file must contain a JSON object.")
	}

	version, versionErr := parseSetupVersion(data["version"])
	if versionErr != nil || version != 1 {
		return SetupBundle{}, errors.New("Download a setup file for the supported Codex runtime.")
	}
	runtimeVersion, ok := data["runtime_version"].(string)
	if !ok || runtimeVersion != "0.154.0" {
		return SetupBundle{}, errors.New("Download a setup file for the supported Codex runtime.")
	}

	readString := func(key string) (string, bool) {
		value, exists := data[key].(string)
		return value, exists && value != ""
	}
	baseURL, baseURLOK := readString("base_url")
	runnerID, runnerIDOK := readString("runner_id")
	profileID, profileIDOK := readString("profile_id")
	expiresAtText, expiresAtOK := readString("expires_at")
	profileToken, profileTokenOK := readString("profile_token")
	executionToken, executionTokenOK := readString("execution_token")
	if !baseURLOK || !runnerIDOK || !profileIDOK || !expiresAtOK || !profileTokenOK || !executionTokenOK {
		return SetupBundle{}, errors.New("The setup file is incomplete. Download it again.")
	}
	if serverURLOverride != "" {
		baseURL = serverURLOverride
	}
	if err := protocol.ValidateProfileID(runnerID); err != nil {
		return SetupBundle{}, errors.New("Unsafe setup identifier, ownership, type or permissions.")
	}
	if err := protocol.ValidateProfileID(profileID); err != nil {
		return SetupBundle{}, errors.New("Unsafe setup identifier, ownership, type or permissions.")
	}
	if err := validateSetupURL(baseURL); err != nil {
		return SetupBundle{}, err
	}
	if !setupTokenPattern.MatchString(profileToken) || !setupTokenPattern.MatchString(executionToken) {
		return SetupBundle{}, errors.New("The setup file contains an invalid runner token.")
	}
	if !setupExpiryPattern.MatchString(expiresAtText) {
		return SetupBundle{}, errors.New("The setup expiry must be an ISO 8601 timestamp.")
	}
	expiresAt, err := time.Parse(time.RFC3339, expiresAtText)
	if err != nil {
		return SetupBundle{}, errors.New("The setup expiry must be an ISO 8601 timestamp.")
	}
	if !expiresAt.After(now) {
		return SetupBundle{}, errors.New("The setup tokens expired. Download setup again for this runner.")
	}
	return SetupBundle{
		Version:        1,
		RuntimeVersion: runtimeVersion,
		BaseURL:        baseURL,
		RunnerID:       runnerID,
		ProfileID:      profileID,
		ExpiresAt:      expiresAt,
		ProfileToken:   profileToken,
		ExecutionToken: executionToken,
	}, nil
}

func RunSetup(args []string, stdout, stderr io.Writer) int {
	options, err := ParseSetupOptions(args)
	if err != nil {
		fmt.Fprintln(stderr, "Invalid setup options. Run shipmunk-runner setup --help for usage.")
		return 2
	}
	if options.Help {
		writeUsage(stdout, "setup")
		return 0
	}
	if options.Version {
		fmt.Fprintln(stdout, "shipmunk-runner "+Version)
		return 0
	}
	if setupRuntime.effectiveUID() == 0 {
		fmt.Fprintln(stderr, "Run setup as a dedicated non-root account with Docker access.")
		return 1
	}
	if !setupRuntime.isTerminal(setupRuntime.stdin) || !setupRuntime.isTerminal(stdout) || !setupRuntime.isTerminal(stderr) {
		fmt.Fprintln(stderr, "Guided setup requires an operator terminal on stdin, stdout and stderr.")
		return 1
	}
	raw, err := readProtectedSetupFile(options.SetupFile, setupRuntime.effectiveUID())
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	bundle, err := ParseSetupBundle(raw, options.ServerURL, setupRuntime.now())
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	manifestRaw, err := readPublicSetupFile(options.ReleaseManifest, setupManifestMaxBytes)
	if err != nil {
		fmt.Fprintln(stderr, setupFileMessage("Runner release manifest is unsafe or invalid.", err))
		return 1
	}
	manifest, err := install.ParseManifest(manifestRaw)
	if err != nil {
		fmt.Fprintln(stderr, "Runner release manifest is unsafe or invalid.")
		return 1
	}
	platform, err := install.SelectPlatform(manifest, setupRuntime.goos, setupRuntime.goarch)
	if err != nil {
		fmt.Fprintln(stderr, "This runner release does not support the current platform.")
		return 1
	}
	fmt.Fprintf(stdout, "Server: %s\nRunner: %s\nProfile: %s\nTokens expire: %s\nRelease: %s (%s/%s)\n", bundle.BaseURL, bundle.RunnerID, bundle.ProfileID, bundle.ExpiresAt.Format(time.RFC3339), manifest.Version, platform.OS, platform.Arch)
	fmt.Fprintln(stdout, "Setup installs the verified release and private local configuration. It does not start reviews.")
	fmt.Fprint(stdout, "Continue with this server and runner? [y/N] ")
	answer, _ := bufio.NewReader(setupRuntime.stdin).ReadString('\n')
	if strings.ToLower(strings.TrimSpace(answer)) != "y" {
		return 0
	}
	home, err := setupRuntime.homeDir()
	if err != nil {
		fmt.Fprintln(stderr, "Runner installation failed. Check the private home and release files, then retry.")
		return 1
	}
	root := filepath.Join(home, ".shipmunk", "runners", bundle.RunnerID)
	guard := install.Guard{Root: root, Identity: install.Identity{BaseURL: bundle.BaseURL, RunnerID: bundle.RunnerID, ProfileID: bundle.ProfileID}, IncomingReleaseVersion: manifest.Version, IncomingArchiveDigest: platform.Archive.SHA256}
	if err := guard.Preflight(); err != nil {
		printRenewalRefusal(stderr, err)
		return 1
	}
	root, releases, err := prepareSetupDirectories(home, bundle.RunnerID, setupRuntime.effectiveUID())
	if err != nil {
		fmt.Fprintln(stderr, "Runner installation failed. Check the private home and release files, then retry.")
		return 1
	}
	if err := guard.Validate(); err != nil {
		printRenewalRefusal(stderr, err)
		return 1
	}
	archive, err := readPublicSetupFile(options.ReleaseArchive, int(platform.Archive.Size))
	if err != nil {
		fmt.Fprintln(stderr, setupFileMessage("Runner release archive is unsafe or invalid.", err))
		return 1
	}
	releasePath, err := install.Install(bytes.NewReader(archive), releases, platform)
	if err != nil {
		fmt.Fprintln(stderr, "Runner installation failed. Check the private home and release files, then retry.")
		return 1
	}
	if err := setupCheckServer(bundle.BaseURL); err != nil {
		fmt.Fprintln(stderr, "The server health check failed. Check its address and connectivity, then retry. No runner configuration was changed.")
		return 1
	}
	imageID, err := setupBuildImage(root, releasePath)
	if err != nil {
		if errors.Is(err, codex.ErrDockerRequirements) {
			fmt.Fprintln(stderr, codex.ErrDockerRequirements.Error()+". Previous configuration was preserved.")
		} else {
			fmt.Fprintln(stderr, "Runtime image preparation failed. Check the Linux Docker engine, then retry. Previous configuration was preserved.")
		}
		return 1
	}
	config, err := json.Marshal(map[string]any{
		"base_url": bundle.BaseURL, "runner_id": bundle.RunnerID, "profile_id": bundle.ProfileID,
		"expires_at": bundle.ExpiresAt.Format(time.RFC3339), "release_path": releasePath,
		"release_version": manifest.Version, "platform": platform.OS + "-" + platform.Arch, "image_id": imageID,
	})
	if err != nil {
		fmt.Fprintln(stderr, "Runner installation failed before configuration activation.")
		return 1
	}
	operatorBinary := filepath.Join(releasePath, "bin", "shipmunk-runner")
	if err := guard.ActivateLaunchers(config, []byte(bundle.ProfileToken), []byte(bundle.ExecutionToken), setupLaunchers(operatorBinary, root)); err != nil {
		var activationErr *install.ActivationError
		switch {
		case errors.As(err, &activationErr) && activationErr.Outcome == install.ActivationPreserved:
			fmt.Fprintln(stderr, "Runner installation failed. Previous configuration was preserved.")
		case errors.As(err, &activationErr) && activationErr.Outcome == install.ActivationCommitted:
			fmt.Fprintln(stderr, "Runner configuration was committed, but cleanup could not be confirmed. Resolve installation state before running.")
		default:
			fmt.Fprintln(stderr, "Runner configuration activation is indeterminate. Resolve recovery state before running or retrying setup.")
		}
		return 1
	}
	fmt.Fprintln(stdout, "Installed the verified runner release. Setup did not start queued work.")
	printNextSteps := func() {
		fmt.Fprintf(stdout, "\nCommands for this runner:\n  Connect: %s\n  Probe:   %s\n  Run:     %s\n", filepath.Join(root, "connect"), filepath.Join(root, "probe"), filepath.Join(root, "run"))
	}
	if options.SkipConnect {
		printNextSteps()
		return 0
	}
	if code := setupExecConnect(root, setupRuntime.stdin, stdout, stderr); code != 0 {
		fmt.Fprintln(stdout, "\nSetup finished, but connecting did not complete; run the commands below.")
		printNextSteps()
		return 1
	}
	fmt.Fprintf(stdout, "\nRun to work through the queue:\n  Run: %s\n", filepath.Join(root, "run"))
	return 0
}

// execConnectLauncher runs the just-activated connect launcher as a real
// child process with inherited stdio, so the connect step is a manual
// connect run against the installed binary: adjacent shipmunk-watchdog,
// installed Version, and its own real *os.File terminal gate, not the
// bootstrap binary's transient path or in-process state.
func execConnectLauncher(root string, stdin io.Reader, stdout, stderr io.Writer) int {
	command := exec.Command(filepath.Join(root, "connect"))
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return 1
	}
	return 0
}

// printRenewalRefusal reports both versions for a release-order refusal, one
// version for a same-version mismatch, and a generic refusal otherwise.
func printRenewalRefusal(stderr io.Writer, err error) {
	var releaseOrderErr *install.ReleaseOrderError
	var archiveMismatchErr *install.ReleaseArchiveMismatchError
	switch {
	case errors.As(err, &releaseOrderErr):
		fmt.Fprintln(stderr, "Runner renewal is blocked: "+releaseOrderErr.Error()+".")
	case errors.As(err, &archiveMismatchErr):
		fmt.Fprintln(stderr, "Runner renewal is blocked: "+archiveMismatchErr.Error()+".")
	default:
		fmt.Fprintln(stderr, "Runner renewal is blocked by changed identity or unresolved recovery state.")
	}
}

func checkSetupServer(baseURL string) error {
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	request, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/up", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	count, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 32*1024+1))
	if readErr != nil || count > 32*1024 || response.StatusCode != http.StatusOK {
		return errors.New("server is unavailable")
	}
	return nil
}

func buildSetupImage(root, releasePath string) (string, error) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return "", err
	}
	if err := codex.RequireDocker(context.Background(), docker); err != nil {
		return "", err
	}
	contextRoot, err := os.MkdirTemp(root, ".image-context-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(contextRoot)
	containers := filepath.Join(contextRoot, "runner", "containers")
	if err := os.MkdirAll(containers, 0700); err != nil {
		return "", err
	}
	for _, name := range []string{"Dockerfile", "Dockerfile.dockerignore", "codex-mcp.mjs", "codex-result.schema.json"} {
		raw, err := os.ReadFile(filepath.Join(releasePath, "containers", name))
		if err != nil || os.WriteFile(filepath.Join(containers, name), raw, 0600) != nil {
			return "", errors.New("runtime image input is unavailable")
		}
	}
	iid := filepath.Join(root, ".image-id-"+filepath.Base(contextRoot))
	defer os.Remove(iid)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, docker, "build", "--iidfile", iid, "--file", filepath.Join(containers, "Dockerfile"), contextRoot)
	command.Env = append(sandbox.ClientEnvironment(), "DOCKER_BUILDKIT=1")
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Run(); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(iid)
	imageID := strings.TrimSpace(string(raw))
	if err != nil || !setupImageIDPattern.MatchString(imageID) {
		return "", errors.New("Docker returned an invalid image identity")
	}
	return imageID, nil
}

func readProtectedSetupFile(path string, effectiveUID int) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil || setupPathHasSymlink(abs) {
		return nil, errors.New("Unsafe setup identifier, ownership, type or permissions.")
	}
	expected, err := os.Lstat(abs)
	if err != nil || !expected.Mode().IsRegular() || expected.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("The setup file must be a small regular file owned by this account, without links.")
	}
	file, err := os.Open(abs)
	if err != nil {
		return nil, errors.New("Cannot open the downloaded setup file safely.")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(expected, opened) || setupFileUID(opened) != uint32(effectiveUID) || setupFileNlink(opened) != 1 {
		return nil, errors.New("The setup file must be a small regular file owned by this account, without links.")
	}
	if opened.Mode().Perm() != 0o600 {
		if err := file.Chmod(0o600); err != nil {
			return nil, errors.New("Cannot protect the downloaded setup file.")
		}
	}
	reader := io.LimitReader(file, setupBundleMaxBytes+1)
	raw, err := io.ReadAll(reader)
	if err != nil || len(raw) > setupBundleMaxBytes {
		return nil, errors.New("The setup file is not valid JSON or exceeds its size limit.")
	}
	afterOpen, openErr := file.Stat()
	afterPath, pathErr := os.Lstat(abs)
	if openErr != nil || pathErr != nil || !os.SameFile(afterOpen, afterPath) || afterOpen.Mode().Perm() != 0o600 || setupFileUID(afterOpen) != uint32(effectiveUID) || setupFileNlink(afterOpen) != 1 {
		return nil, errors.New("The setup file changed while it was being read.")
	}
	return raw, nil
}

func readPublicSetupFile(path string, limit int) ([]byte, error) {
	if limit < 1 {
		return nil, errors.New("release file size is invalid")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("path cannot be resolved%w", errUnsafeSetupFile)
	}
	if component, isSymlink, unsafe := setupUnsafePathComponent(abs); unsafe {
		if isSymlink {
			return nil, fmt.Errorf("path component %s is a symlink%w", component, errUnsafeSetupFile)
		}
		return nil, fmt.Errorf("path component %s is missing or unreadable%w", component, errUnsafeSetupFile)
	}
	expected, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("is missing or unreadable%w", errUnsafeSetupFile)
	}
	if !expected.Mode().IsRegular() {
		return nil, fmt.Errorf("is not a regular file%w", errUnsafeSetupFile)
	}
	if setupFileNlink(expected) != 1 {
		return nil, fmt.Errorf("is hard-linked%w", errUnsafeSetupFile)
	}
	file, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("cannot be opened%w", errUnsafeSetupFile)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(expected, opened) || setupFileNlink(opened) != 1 {
		return nil, fmt.Errorf("changed while opening%w", errUnsafeSetupFile)
	}
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("cannot be read%w", errUnsafeSetupFile)
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("exceeds its limit%w", errUnsafeSetupFile)
	}
	afterOpen, openErr := file.Stat()
	afterPath, pathErr := os.Lstat(abs)
	if openErr != nil || pathErr != nil ||
		!os.SameFile(afterOpen, afterPath) ||
		opened.Size() != afterOpen.Size() ||
		!opened.ModTime().Equal(afterOpen.ModTime()) ||
		setupFileNlink(afterOpen) != 1 {
		return nil, fmt.Errorf("changed after reading%w", errUnsafeSetupFile)
	}
	return raw, nil
}

func prepareSetupDirectories(home, runnerID string, effectiveUID int) (string, string, error) {
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil || canonical != home || !filepath.IsAbs(home) || setupPathHasSymlink(home) {
		return "", "", errors.New("runner home is unsafe")
	}
	shipmunk := filepath.Join(home, ".shipmunk")
	paths := []string{shipmunk, filepath.Join(shipmunk, "runners"), filepath.Join(shipmunk, "runners", runnerID)}
	for _, path := range paths {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", "", err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || setupFileUID(info) != uint32(effectiveUID) {
			return "", "", errors.New("runner directory is unsafe")
		}
	}
	root := paths[len(paths)-1]
	for _, path := range []string{filepath.Join(root, "profiles"), filepath.Join(root, "state")} {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", "", err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || setupFileUID(info) != uint32(effectiveUID) {
			return "", "", errors.New("runner directory is unsafe")
		}
	}
	return root, filepath.Join(shipmunk, "releases"), nil
}

// setupUnsafePathComponent Lstats path component by component from the
// root. unsafe is true when a component is a symlink or cannot be Lstat'd;
// isSymlink distinguishes the two so callers don't blame a missing path on a
// symlink.
func setupUnsafePathComponent(path string) (component string, isSymlink, unsafe bool) {
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return current, false, true
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return current, true, true
		}
	}
	return "", false, false
}

func setupPathHasSymlink(path string) bool {
	_, _, unsafe := setupUnsafePathComponent(path)
	return unsafe
}

func validateSetupURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(value, "#") || parsed.Opaque != "" || hasForbiddenURLRune(value) {
		return errors.New("The server URL must be HTTP(S), without credentials, query, fragment or control characters.")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("The server URL must be HTTP(S), without credentials, query, fragment or control characters.")
	}
	if parsed.Scheme == "http" && !isSetupLoopbackHost(strings.ToLower(parsed.Hostname())) {
		return errors.New("Use HTTPS for a remote control plane, or HTTP on localhost/127.0.0.1 for local development.")
	}
	return nil
}

func hasForbiddenURLRune(value string) bool {
	for _, character := range value {
		if character <= 0x20 || character == 0x7f || character == '\\' {
			return true
		}
	}
	return false
}

func isSetupLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1"
}

func setupObject(value any) (map[string]any, error) {
	data, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("not an object")
	}
	return data, nil
}

func parseSetupVersion(value any) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("version must be an integer")
	}
	version, err := strconv.Atoi(number.String())
	if err != nil {
		return 0, err
	}
	return version, nil
}
