// Package install guards state-preserving runner renewal and configuration activation.
package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const maxConfigBytes = 16 << 10

type Identity struct {
	BaseURL   string
	RunnerID  string
	ProfileID string
}

type Guard struct {
	Root     string
	Identity Identity
	Rename   func(string, string) error
}

// Validate refuses renewal while work or recovery remains unresolved. It never
// removes or rewrites state, profiles, native homes, or journals.
func (guard Guard) Validate() error {
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
	if err := absent(filepath.Join(stateRoot, "active-attempt.json")); err != nil {
		return fmt.Errorf("runner attempt recovery must be resolved before renewal: %w", err)
	}
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

// Activate replaces config.json and both tokens as one failure-atomic group.
// If activation fails, the exact prior files are restored.
func (guard Guard) Activate(config, profileToken, executionToken []byte) error {
	if err := guard.Validate(); err != nil {
		return err
	}
	identity, err := decodeIdentity(config)
	if err != nil || identity != guard.Identity {
		return errors.New("new runner configuration identity does not match the installation")
	}
	if err := validToken(profileToken); err != nil {
		return fmt.Errorf("profile token is invalid: %w", err)
	}
	if err := validToken(executionToken); err != nil {
		return fmt.Errorf("execution token is invalid: %w", err)
	}
	values := map[string][]byte{"config.json": config, "profile.token": profileToken, "execution.token": executionToken}
	order := []string{"profile.token", "execution.token", "config.json"}
	if err := guard.Validate(); err != nil {
		return err
	}
	temporary := map[string]string{}
	backups := map[string]string{}
	cleanup := func() {
		for _, path := range temporary {
			_ = os.Remove(path)
		}
		for _, path := range backups {
			_ = os.Remove(path)
		}
	}
	defer cleanup()
	for _, name := range order {
		path := filepath.Join(guard.Root, name)
		if err := validateOptionalFile(path); err != nil {
			return fmt.Errorf("existing runner configuration is unsafe: %w", err)
		}
		file, err := os.CreateTemp(guard.Root, ".activate-*")
		if err != nil {
			return err
		}
		temporary[name] = file.Name()
		if err := file.Chmod(0600); err == nil {
			_, err = file.Write(values[name])
		}
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("persist runner configuration: %w", err)
		}
		if _, err := os.Lstat(path); err == nil {
			backup := filepath.Join(guard.Root, ".rollback-"+name)
			if err := os.Link(path, backup); err != nil {
				return fmt.Errorf("preserve runner configuration: %w", err)
			}
			backups[name] = backup
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	rename := guard.Rename
	if rename == nil {
		rename = os.Rename
	}
	committed := []string{}
	for _, name := range order {
		path := filepath.Join(guard.Root, name)
		if err := rename(temporary[name], path); err != nil {
			rollbackErr := rollback(guard.Root, committed, backups)
			return errors.Join(fmt.Errorf("activate runner configuration: %w", err), rollbackErr)
		}
		delete(temporary, name)
		committed = append(committed, name)
	}
	directory, err := os.Open(guard.Root)
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func rollback(root string, committed []string, backups map[string]string) error {
	var result error
	for index := len(committed) - 1; index >= 0; index-- {
		name := committed[index]
		path := filepath.Join(root, name)
		if backup, ok := backups[name]; ok {
			if err := os.Rename(backup, path); err != nil {
				result = errors.Join(result, err)
			}
			delete(backups, name)
		} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (guard Guard) validateExistingIdentity() error {
	path := filepath.Join(guard.Root, "config.json")
	raw, err := readPrivateFile(path, maxConfigBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("existing runner configuration is unsafe: %w", err)
	}
	identity, err := decodeIdentity(raw)
	if err != nil || identity != guard.Identity {
		return errors.New("setup identity differs from the existing runner")
	}
	return nil
}

func decodeIdentity(raw []byte) (Identity, error) {
	if len(raw) == 0 || len(raw) > maxConfigBytes {
		return Identity{}, errors.New("runner configuration is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var data map[string]any
	if err := decoder.Decode(&data); err != nil {
		return Identity{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Identity{}, errors.New("runner configuration has trailing data")
	}
	baseURL, baseOK := data["base_url"].(string)
	runnerID, runnerOK := data["runner_id"].(string)
	profileID, profileOK := data["profile_id"].(string)
	if !baseOK || !runnerOK || !profileOK || baseURL == "" {
		return Identity{}, errors.New("runner configuration identity is incomplete")
	}
	return Identity{BaseURL: baseURL, RunnerID: runnerID, ProfileID: profileID}, nil
}

func validToken(value []byte) error {
	if len(value) == 0 || len(value) > 4096 || strings.TrimSpace(string(value)) != string(value) || bytes.ContainsAny(value, "\r\n\x00") {
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

func readPrivateFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || info.Size() > limit {
		return nil, errors.New("file type, ownership, permissions, or size is unsafe")
	}
	return os.ReadFile(path)
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Geteuid()) {
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
