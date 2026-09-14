//go:build unix

package install

import (
	"errors"
	"io"
	"os"
	"syscall"
)

func withFileLock(path string, operation func() error) error {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !os.SameFile(info, current) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || fileUID(info) != os.Geteuid() || fileNlink(info) != 1 {
		return errors.New("lock file is unsafe")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return operation()
}

func fileUID(info os.FileInfo) int {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}

func fileNlink(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}

func fileOwner(info os.FileInfo) int { return fileUID(info) }

func readPrivateFile(path string, limit int64) ([]byte, error) {
	return readProtectedFile(path, limit, 0600)
}

func readExecutableFile(path string, limit int64) ([]byte, error) {
	return readProtectedFile(path, limit, 0700)
}

func readProtectedFile(path string, limit int64, mode os.FileMode) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	before, err := inspectProtectedFile(path, file, limit, mode)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("private file is oversized or unreadable")
	}
	after, err := inspectProtectedFile(path, file, limit, mode)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(raw)) != after.Size() {
		return nil, errors.New("private file changed while reading")
	}
	return raw, nil
}

func inspectProtectedFile(path string, file *os.File, limit int64, mode os.FileMode) (os.FileInfo, error) {
	opened, err := file.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !os.SameFile(opened, current) || !opened.Mode().IsRegular() || opened.Mode().Perm() != mode || fileUID(opened) != os.Geteuid() || fileNlink(opened) != 1 || opened.Size() > limit {
		return nil, errors.New("file type, ownership, permissions, links, or size is unsafe")
	}
	return opened, nil
}

func writePrivateExclusive(path string, raw []byte) error {
	return writeExclusive(path, raw, 0600)
}

func writeExecutableExclusive(path string, raw []byte) error {
	return writeExclusive(path, raw, 0700)
}

func writeExclusive(path string, raw []byte, mode uint32) error {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	_, writeErr := file.Write(raw)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func syncPrivateDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
