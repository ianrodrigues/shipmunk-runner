//go:build darwin || linux

package attemptstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func prepareDirectory(path string) (string, error) {
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	remaining := strings.TrimPrefix(path, current)
	for _, component := range strings.Split(remaining, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		candidate := filepath.Join(current, component)
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(candidate, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			info, err = os.Lstat(candidate)
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				return "", err
			}
			info, err = os.Stat(resolved)
			if err != nil {
				return "", err
			}
			if !info.IsDir() {
				return "", errors.New("path ancestor is not a directory")
			}
			current = resolved
			continue
		}
		if !info.IsDir() {
			return "", errors.New("path ancestor is not a directory")
		}
		current = candidate
	}
	return filepath.Clean(current), nil
}

func validateLeaf(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("state leaf must be a regular file")
	}
	return validatePrivateOwnedFile(info)
}

func openAndLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_EXCL|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	created := err == nil
	if errors.Is(err, syscall.EEXIST) {
		fd, err = syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	opened, statErr := file.Stat()
	current, lstatErr := os.Lstat(path)
	if statErr != nil || lstatErr != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, current) {
		_ = file.Close()
		return nil, errors.New("lock file changed during open")
	}
	if created {
		if err := file.Chmod(0600); err != nil {
			_ = file.Close()
			return nil, err
		}
		opened, statErr = file.Stat()
		if statErr != nil {
			_ = file.Close()
			return nil, statErr
		}
	}
	if err := validatePrivateOwnedFile(opened); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("supervisor is already using this state path: %w", err)
	}
	return file, nil
}

func lockDirectory(directory *os.File) error {
	if err := syscall.Flock(int(directory.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("supervisor is already using this state directory: %w", err)
	}
	return nil
}

func unlockDirectory(directory *os.File) error {
	return syscall.Flock(int(directory.Fd()), syscall.LOCK_UN)
}

func validatePrivateDirectory(info os.FileInfo) error {
	if !info.IsDir() {
		return errors.New("attempt state path is not a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("attempt state directory is not owned by the current user")
	}
	if info.Mode().Perm()&0077 != 0 || info.Mode().Perm()&0700 != 0700 {
		return errors.New("attempt state directory must be private and writable by its owner")
	}
	return nil
}

func validatePrivateOwnedFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("state leaf must be a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("state leaf is not owned by the current user")
	}
	if stat.Nlink != 1 {
		return errors.New("state leaf must not have multiple hard links")
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("state leaf must be private")
	}
	return nil
}

func unlockAndClose(file *os.File) error {
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func openReadNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	opened, statErr := file.Stat()
	current, lstatErr := os.Lstat(path)
	if statErr != nil || lstatErr != nil || !os.SameFile(opened, current) {
		_ = file.Close()
		return nil, errors.New("state file changed during open")
	}
	return file, nil
}

func createPrivateFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
