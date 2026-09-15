package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/attemptstate"
	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const maxConfigBytes = 16 << 10

const activationJournalName = ".activation.json"

var releaseVersionPattern = regexp.MustCompile(`^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
var imageIDPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// compareReleaseVersions orders release tags by semver 2.0.0 precedence:
// negative means a precedes b, zero means equal, positive means a follows b.
func compareReleaseVersions(a, b string) (int, error) {
	aCore, aPrerelease, err := parseReleaseVersion(a)
	if err != nil {
		return 0, err
	}
	bCore, bPrerelease, err := parseReleaseVersion(b)
	if err != nil {
		return 0, err
	}
	for index := range aCore {
		if cmp := compareNumericIdentifier(aCore[index], bCore[index]); cmp != 0 {
			return cmp, nil
		}
	}
	return comparePrerelease(aPrerelease, bPrerelease), nil
}

func parseReleaseVersion(version string) ([3]string, []string, error) {
	if !releaseVersionPattern.MatchString(version) {
		return [3]string{}, nil, fmt.Errorf("release version %q is not a valid semantic version", version)
	}
	trimmed := strings.TrimPrefix(version, "v")
	if build := strings.IndexByte(trimmed, '+'); build != -1 {
		trimmed = trimmed[:build] // build metadata never affects precedence
	}
	core, prereleasePart, hasPrerelease := strings.Cut(trimmed, "-")
	parts := strings.SplitN(core, ".", 3)
	if len(parts) != 3 {
		return [3]string{}, nil, fmt.Errorf("release version %q is not a valid semantic version", version)
	}
	var prerelease []string
	if hasPrerelease {
		prerelease = strings.Split(prereleasePart, ".")
	}
	return [3]string{parts[0], parts[1], parts[2]}, prerelease, nil
}

// No leading zeros are possible here, so length then byte order equals
// numeric order without parsing.
func compareNumericIdentifier(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// comparePrerelease implements semver 2.0.0 precedence rule 11.
func comparePrerelease(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	for index := 0; index < limit; index++ {
		if cmp := comparePrereleaseIdentifier(a[index], b[index]); cmp != 0 {
			return cmp
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	default:
		return 0
	}
}

func comparePrereleaseIdentifier(a, b string) int {
	aNumeric, bNumeric := isNumericIdentifier(a), isNumericIdentifier(b)
	switch {
	case aNumeric && bNumeric:
		return compareNumericIdentifier(a, b)
	case aNumeric:
		return -1
	case bNumeric:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func isNumericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type Identity struct {
	BaseURL   string
	RunnerID  string
	ProfileID string
}

type Guard struct {
	Root     string
	Identity Identity
	// IncomingReleaseVersion enforces release order when set; empty skips it.
	IncomingReleaseVersion string
	// IncomingArchiveDigest is required at equal version precedence; see validateReleaseOrder.
	IncomingArchiveDigest string
	Rename                func(string, string) error
	Sync                  func(string) error
	Write                 func(string, []byte) error
}

type ActivationOutcome uint8

const (
	ActivationIndeterminate ActivationOutcome = iota
	ActivationPreserved
	ActivationCommitted
)

type ActivationError struct {
	Outcome ActivationOutcome
	Err     error
}

func (err *ActivationError) Error() string { return err.Err.Error() }
func (err *ActivationError) Unwrap() error { return err.Err }

func (guard Guard) Validate() error {
	return guard.withLocks(func(state *attemptstate.Store) error {
		if err := guard.recover(); err != nil {
			return err
		}
		return guard.validateLocked(state)
	})
}

func (guard Guard) Preflight() error {
	if protocol.ValidateProfileID(guard.Identity.RunnerID) != nil || protocol.ValidateProfileID(guard.Identity.ProfileID) != nil ||
		!filepath.IsAbs(guard.Root) || filepath.Clean(guard.Root) != guard.Root || filepath.Base(guard.Root) != guard.Identity.RunnerID {
		return errors.New("runner installation identity is invalid")
	}
	if _, err := os.Lstat(guard.Root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := rejectSymlinkComponents(guard.Root); err != nil {
		return err
	}
	if err := privateDirectory(guard.Root); err != nil {
		return fmt.Errorf("runner installation directory is unsafe: %w", err)
	}
	if err := guard.validateExistingIdentity(); err != nil {
		return err
	}
	if err := optionalPrivateDirectory(filepath.Join(guard.Root, "state")); err != nil {
		return fmt.Errorf("runner state layout is unsafe: %w", err)
	}
	if err := absent(filepath.Join(guard.Root, "state", "active-attempt.json")); err != nil {
		return fmt.Errorf("runner attempt recovery must be resolved before renewal: %w", err)
	}
	return guard.validateProfilesReadOnly()
}

func (guard Guard) validateLocked(state *attemptstate.Store) error {
	if protocol.ValidateProfileID(guard.Identity.RunnerID) != nil || protocol.ValidateProfileID(guard.Identity.ProfileID) != nil {
		return errors.New("runner installation identity is invalid")
	}
	if !filepath.IsAbs(guard.Root) || filepath.Clean(guard.Root) != guard.Root || filepath.Base(guard.Root) != guard.Identity.RunnerID {
		return errors.New("runner installation path is invalid")
	}
	if err := rejectSymlinkComponents(guard.Root); err != nil {
		return err
	}
	if err := privateDirectory(guard.Root); err != nil {
		return fmt.Errorf("runner installation directory is unsafe: %w", err)
	}
	if err := guard.validateExistingIdentity(); err != nil {
		return err
	}
	stateRoot := filepath.Join(guard.Root, "state")
	if err := optionalPrivateDirectory(stateRoot); err != nil {
		return fmt.Errorf("runner state layout is unsafe: %w", err)
	}
	active, err := state.Load()
	if err != nil || active != nil {
		return fmt.Errorf("runner attempt recovery must be resolved before renewal: %w", err)
	}
	return guard.validateProfilesReadOnly()
}

func (guard Guard) validateProfilesReadOnly() error {
	profiles := filepath.Join(guard.Root, "profiles")
	if err := optionalPrivateDirectory(profiles); err != nil {
		return fmt.Errorf("runner profile layout is unsafe: %w", err)
	}
	entries, err := os.ReadDir(profiles)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect runner profiles: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == ".lock" {
			if err := validateOptionalFile(filepath.Join(profiles, entry.Name())); err != nil {
				return errors.New("runner profile layout is unsafe")
			}
			continue
		}
		if protocol.ValidateProfileID(entry.Name()) != nil || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("runner profile layout is unsafe")
		}
		profileRoot := filepath.Join(profiles, entry.Name())
		if err := privateDirectory(profileRoot); err != nil {
			return errors.New("runner profile layout is unsafe")
		}
		for _, journal := range []string{"pending.json", "execution.json"} {
			if err := absent(filepath.Join(profileRoot, journal)); err != nil {
				return fmt.Errorf("profile recovery must be resolved before renewal: %w", err)
			}
		}
		profileEntries, err := os.ReadDir(profileRoot)
		if err != nil {
			return fmt.Errorf("inspect runner profile: %w", err)
		}
		for _, profileEntry := range profileEntries {
			if strings.HasSuffix(profileEntry.Name(), ".json") &&
				profileEntry.Name() != "active.json" && profileEntry.Name() != "completed.json" {
				return errors.New("unsupported profile recovery journal must be resolved before renewal")
			}
		}
	}
	return nil
}

func (guard Guard) Activate(config, profileToken, executionToken []byte) error {
	return guard.ActivateLaunchers(config, profileToken, executionToken, nil)
}

func (guard Guard) ActivateLaunchers(config, profileToken, executionToken []byte, launchers map[string][]byte) error {
	configuration, err := decodeConfiguration(config)
	if err != nil || configuration.Identity != guard.Identity || guard.validateReleaseBinding(configuration) != nil {
		return errors.New("new runner configuration identity does not match the installation")
	}
	if err := validToken(profileToken); err != nil {
		return fmt.Errorf("profile token is invalid: %w", err)
	}
	if err := validToken(executionToken); err != nil {
		return fmt.Errorf("execution token is invalid: %w", err)
	}
	values := map[string][]byte{"config.json": config, "profile.token": profileToken, "execution.token": executionToken}
	for _, name := range []string{"run", "connect"} {
		value, ok := launchers[name]
		if launchers != nil && (!ok || len(value) == 0 || len(value) > maxConfigBytes) {
			return errors.New("runner launcher is invalid")
		}
		if ok {
			values[name] = value
		}
	}
	return guard.withLocks(func(state *attemptstate.Store) error {
		if err := guard.recover(); err != nil {
			return err
		}
		if err := guard.validateLocked(state); err != nil {
			return err
		}
		return guard.activateLocked(values, activationNames(launchers != nil))
	})
}

type activationJournal struct {
	Version int             `json:"version"`
	Files   map[string]bool `json:"files"`
}

var activationOrder = []string{"profile.token", "execution.token", "config.json"}

func activationNames(withLaunchers bool) []string {
	if withLaunchers {
		return []string{"profile.token", "execution.token", "run", "connect", "config.json"}
	}
	return activationOrder
}

func (guard Guard) withLocks(operation func(*attemptstate.Store) error) error {
	if protocol.ValidateProfileID(guard.Identity.RunnerID) != nil || protocol.ValidateProfileID(guard.Identity.ProfileID) != nil {
		return errors.New("runner installation identity is invalid")
	}
	if !filepath.IsAbs(guard.Root) || filepath.Clean(guard.Root) != guard.Root || filepath.Base(guard.Root) != guard.Identity.RunnerID {
		return errors.New("runner installation path is invalid")
	}
	if err := rejectSymlinkComponents(guard.Root); err != nil {
		return err
	}
	if err := privateDirectory(guard.Root); err != nil {
		return fmt.Errorf("runner installation directory is unsafe: %w", err)
	}
	return WithLaunchLock(guard.Root, func() error { return guard.withRuntimeLocks(operation) })
}

func (guard Guard) withRuntimeLocks(operation func(*attemptstate.Store) error) error {
	state, err := attemptstate.Open(filepath.Join(guard.Root, "state", "active-attempt.json"))
	if err != nil {
		return fmt.Errorf("runner is active or its state is unsafe: %w", err)
	}
	defer state.Close()
	profilesRoot := filepath.Join(guard.Root, "profiles")
	return profile.WithRootExclusive(profilesRoot, func() error { return operation(state) })
}

func WithLaunchLock(root string, operation func() error) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || rejectSymlinkComponents(root) != nil || privateDirectory(root) != nil {
		return errors.New("runner installation path is unsafe")
	}
	return withFileLock(filepath.Join(root, ".launch.lock"), operation)
}

func ActivationPending(root string) bool {
	_, err := os.Lstat(filepath.Join(root, activationJournalName))
	return err == nil
}

func (guard Guard) activateLocked(values map[string][]byte, names []string) error {
	journal := activationJournal{Version: 1, Files: make(map[string]bool)}
	for _, name := range names {
		journal.Files[name] = false
		path := filepath.Join(guard.Root, name)
		old, err := readActivatedFile(path, name)
		if err == nil {
			journal.Files[name] = true
			if err := writeActivatedFile(guard.backup(name), name, old); err != nil {
				return guard.abortPreparation(err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return guard.abortPreparation(err)
		}
		if err := writeActivatedFile(guard.pending(name), name, values[name]); err != nil {
			return guard.abortPreparation(err)
		}
	}
	if err := guard.sync(); err != nil {
		return guard.preparationError(err)
	}
	raw, _ := json.Marshal(journal)
	journalPath := filepath.Join(guard.Root, activationJournalName)
	if err := guard.write(journalPath, raw); err != nil {
		return guard.abortJournal(err)
	}
	if err := guard.sync(); err != nil {
		return &ActivationError{Outcome: ActivationIndeterminate, Err: err}
	}
	rename := guard.Rename
	if rename == nil {
		rename = os.Rename
	}
	for _, name := range names {
		if err := rename(guard.pending(name), filepath.Join(guard.Root, name)); err != nil {
			cause := fmt.Errorf("activate runner configuration: %w", err)
			if recoveryErr := guard.recover(); recoveryErr != nil {
				return &ActivationError{Outcome: ActivationIndeterminate, Err: errors.Join(cause, recoveryErr)}
			}
			return &ActivationError{Outcome: ActivationPreserved, Err: cause}
		}
	}
	if err := guard.sync(); err != nil {
		return &ActivationError{Outcome: ActivationIndeterminate, Err: err}
	}
	committed, err := guard.finishActivation(names)
	if err != nil {
		outcome := ActivationIndeterminate
		if committed {
			outcome = ActivationCommitted
		}
		return &ActivationError{Outcome: outcome, Err: err}
	}
	return nil
}

func (guard Guard) recover() error {
	raw, err := readPrivateFile(filepath.Join(guard.Root, activationJournalName), maxConfigBytes)
	if errors.Is(err, os.ErrNotExist) {
		return guard.removePreparation()
	}
	if err != nil {
		return fmt.Errorf("activation recovery journal is unsafe: %w", err)
	}
	value, err := protocol.Decode(raw, maxConfigBytes)
	object, ok := value.(map[string]any)
	if err != nil || !ok || len(object) != 2 || object["version"] != json.Number("1") {
		return errors.New("activation recovery journal is invalid")
	}
	files, ok := object["files"].(map[string]any)
	if !ok || (len(files) != 3 && len(files) != 5) {
		return errors.New("activation recovery journal is invalid")
	}
	names := activationOrder
	if len(files) == 5 {
		names = activationNames(true)
	}
	for _, name := range names {
		existed, ok := files[name].(bool)
		if !ok {
			return errors.New("activation recovery journal is invalid")
		}
		path := filepath.Join(guard.Root, name)
		if existed {
			old, err := readActivatedFile(guard.backup(name), name)
			if err != nil {
				return errors.New("activation backup is unavailable")
			}
			_ = os.Remove(guard.pending(name))
			if err := writeActivatedFile(guard.pending(name), name, old); err != nil {
				return err
			}
			if err := os.Rename(guard.pending(name), path); err != nil {
				return err
			}
		} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := guard.sync(); err != nil {
		return err
	}
	_, err = guard.finishActivation(names)
	return err
}

func (guard Guard) finishActivation(names []string) (bool, error) {
	if err := os.Remove(filepath.Join(guard.Root, activationJournalName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := guard.sync(); err != nil {
		return false, err
	}
	for _, name := range names {
		for _, path := range []string{guard.backup(name), guard.pending(name)} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return true, err
			}
		}
	}
	return true, guard.sync()
}

func (guard Guard) abortPreparation(cause error) error {
	return guard.preparationError(cause)
}

func (guard Guard) abortJournal(cause error) error {
	if err := os.Remove(filepath.Join(guard.Root, activationJournalName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &ActivationError{Outcome: ActivationIndeterminate, Err: errors.Join(cause, err)}
	}
	if err := guard.sync(); err != nil {
		return &ActivationError{Outcome: ActivationIndeterminate, Err: errors.Join(cause, err)}
	}
	return guard.preparationError(cause)
}

func (guard Guard) preparationError(cause error) error {
	if cleanupErr := guard.removePreparation(); cleanupErr != nil {
		return &ActivationError{Outcome: ActivationIndeterminate, Err: errors.Join(cause, cleanupErr)}
	}
	if err := guard.sync(); err != nil {
		return &ActivationError{Outcome: ActivationIndeterminate, Err: errors.Join(cause, err)}
	}
	return &ActivationError{Outcome: ActivationPreserved, Err: cause}
}

func (guard Guard) sync() error {
	if guard.Sync != nil {
		return guard.Sync(guard.Root)
	}
	return syncPrivateDirectory(guard.Root)
}

func (guard Guard) write(path string, raw []byte) error {
	if guard.Write != nil {
		return guard.Write(path, raw)
	}
	return writePrivateExclusive(path, raw)
}

func (guard Guard) removePreparation() error {
	var result error
	for _, name := range activationNames(true) {
		for _, path := range []string{guard.backup(name), guard.pending(name)} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}

func readActivatedFile(path, name string) ([]byte, error) {
	if name != "run" && name != "connect" {
		return readPrivateFile(path, maxConfigBytes)
	}
	return readExecutableFile(path, maxConfigBytes)
}

func writeActivatedFile(path, name string, raw []byte) error {
	if name == "run" || name == "connect" {
		return writeExecutableExclusive(path, raw)
	}
	return writePrivateExclusive(path, raw)
}

func (guard Guard) backup(name string) string {
	return filepath.Join(guard.Root, ".activation-backup-"+name)
}
func (guard Guard) pending(name string) string {
	return filepath.Join(guard.Root, ".activation-new-"+name)
}

// validateExistingIdentity allows a missing configuration only for a new installation.
func (guard Guard) validateExistingIdentity() error {
	path := filepath.Join(guard.Root, "config.json")
	raw, err := readPrivateFile(path, maxConfigBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("existing runner configuration is unsafe: %w", err)
	}
	configuration, err := decodeConfiguration(raw)
	if err != nil || configuration.Identity != guard.Identity || guard.validateReleaseBinding(configuration) != nil {
		return errors.New("setup identity differs from the existing runner")
	}
	return guard.validateReleaseOrder(configuration.ReleaseVersion, configuration.ReleasePath)
}

// Downgrades are refused by design; a malformed installed version fails
// closed the same way. Build metadata is ignored at equal precedence (semver
// 2.0.0), so the incoming digest must match the installed one, or a
// differently built same-version archive could silently replace it.
func (guard Guard) validateReleaseOrder(installed, installedReleasePath string) error {
	if guard.IncomingReleaseVersion == "" {
		return nil
	}
	cmp, err := compareReleaseVersions(guard.IncomingReleaseVersion, installed)
	if err != nil {
		return errors.New("installed runner release version is unsafe")
	}
	if cmp < 0 {
		return &ReleaseOrderError{Incoming: guard.IncomingReleaseVersion, Installed: installed}
	}
	if cmp == 0 {
		if !digestPattern.MatchString(guard.IncomingArchiveDigest) {
			return errors.New("incoming runner release archive digest is unsafe")
		}
		if guard.IncomingArchiveDigest != filepath.Base(installedReleasePath) {
			return &ReleaseArchiveMismatchError{Version: guard.IncomingReleaseVersion}
		}
	}
	return nil
}

// ReleaseOrderError reports an incoming release older than the installed one.
type ReleaseOrderError struct {
	Incoming, Installed string
}

func (err *ReleaseOrderError) Error() string {
	return fmt.Sprintf("runner release %s is older than the installed release %s", err.Incoming, err.Installed)
}

// ReleaseArchiveMismatchError reports a same-version renewal whose archive digest differs from the installed release's.
type ReleaseArchiveMismatchError struct {
	Version string
}

func (err *ReleaseArchiveMismatchError) Error() string {
	return fmt.Sprintf("runner release %s has a different archive than the installed release", err.Version)
}

type installedConfiguration struct {
	Identity       Identity
	ExpiresAt      string
	ReleasePath    string
	ReleaseVersion string
	Platform       string
	ImageID        string
}

type Configuration = installedConfiguration

func LoadConfiguration(root string) (Configuration, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || rejectSymlinkComponents(root) != nil || privateDirectory(root) != nil {
		return Configuration{}, errors.New("runner installation path is unsafe")
	}
	raw, err := readPrivateFile(filepath.Join(root, "config.json"), maxConfigBytes)
	if err != nil {
		return Configuration{}, err
	}
	configuration, err := decodeConfiguration(raw)
	if err != nil || filepath.Base(root) != configuration.Identity.RunnerID || (Guard{Root: root}).validateReleaseBinding(configuration) != nil {
		return Configuration{}, errors.New("runner configuration is not bound to its installation")
	}
	return configuration, nil
}

func (guard Guard) Configuration() (Configuration, error) {
	var configuration Configuration
	err := guard.withLocks(func(state *attemptstate.Store) error {
		if err := guard.recover(); err != nil {
			return err
		}
		if err := guard.validateLocked(state); err != nil {
			return err
		}
		raw, err := readPrivateFile(filepath.Join(guard.Root, "config.json"), maxConfigBytes)
		if err != nil {
			return err
		}
		configuration, err = decodeConfiguration(raw)
		return err
	})
	return configuration, err
}

// decodeConfiguration accepts only the eight-field shape and rejects every
// other shape without migrating old data.
func decodeConfiguration(raw []byte) (installedConfiguration, error) {
	if len(raw) == 0 || len(raw) > maxConfigBytes {
		return installedConfiguration{}, errors.New("runner configuration is invalid")
	}
	value, err := protocol.Decode(raw, maxConfigBytes)
	data, ok := value.(map[string]any)
	if err != nil || !ok || len(data) != 8 {
		return installedConfiguration{}, errors.New("runner configuration is invalid")
	}
	keys := []string{"base_url", "runner_id", "profile_id", "expires_at", "release_path", "release_version", "platform", "image_id"}
	for _, key := range keys {
		if _, ok := data[key]; !ok {
			return installedConfiguration{}, errors.New("runner configuration is invalid")
		}
	}
	baseURL, baseOK := data["base_url"].(string)
	runnerID, runnerOK := data["runner_id"].(string)
	profileID, profileOK := data["profile_id"].(string)
	expiresAt, expiresOK := data["expires_at"].(string)
	releasePath, releasePathOK := data["release_path"].(string)
	releaseVersion, releaseVersionOK := data["release_version"].(string)
	platform, platformOK := data["platform"].(string)
	imageID, imageOK := data["image_id"].(string)
	if !baseOK || !runnerOK || !profileOK || !expiresOK || !releasePathOK || !releaseVersionOK || !platformOK ||
		baseURL == "" || len(baseURL) > 2048 || protocol.ValidateProfileID(runnerID) != nil || protocol.ValidateProfileID(profileID) != nil ||
		len(expiresAt) > 64 || len(releasePath) > 4096 || !filepath.IsAbs(releasePath) || filepath.Clean(releasePath) != releasePath ||
		!releaseVersionPattern.MatchString(releaseVersion) || !imageOK || !imageIDPattern.MatchString(imageID) || (platform != "linux-amd64" && platform != "linux-arm64" && platform != "darwin-amd64" && platform != "darwin-arm64") {
		return installedConfiguration{}, errors.New("runner configuration is invalid")
	}
	if parsed, err := time.Parse(time.RFC3339, expiresAt); err != nil || parsed.Format(time.RFC3339) != expiresAt {
		return installedConfiguration{}, errors.New("runner configuration is invalid")
	}
	return installedConfiguration{Identity: Identity{BaseURL: baseURL, RunnerID: runnerID, ProfileID: profileID}, ExpiresAt: expiresAt, ReleasePath: releasePath, ReleaseVersion: releaseVersion, Platform: platform, ImageID: imageID}, nil
}

func (guard Guard) validateReleaseBinding(configuration installedConfiguration) error {
	releases := filepath.Join(filepath.Dir(filepath.Dir(guard.Root)), "releases")
	if filepath.Dir(configuration.ReleasePath) != releases || !digestPattern.MatchString(filepath.Base(configuration.ReleasePath)) {
		return errors.New("runner release path is not bound to the installation")
	}
	return nil
}

func validToken(value []byte) error {
	if len(value) == 0 || len(value) > 4096 || strings.TrimSpace(string(value)) != string(value) || bytes.ContainsAny(value, "\r\n\t \x00") {
		return errors.New("token bytes are invalid")
	}
	return nil
}

func absent(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("journal exists")
}

func validateOptionalFile(path string) error {
	_, err := readPrivateFile(path, maxConfigBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || fileOwner(info) != os.Geteuid() {
		return errors.New("directory is not private and owned")
	}
	return nil
}

func optionalPrivateDirectory(path string) error {
	err := privateDirectory(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func rejectSymlinkComponents(path string) error {
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), current) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlinks are forbidden in the runner installation path")
		}
	}
	return nil
}
