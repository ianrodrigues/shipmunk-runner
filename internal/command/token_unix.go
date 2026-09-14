//go:build linux || darwin

package command

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func readRunnerToken(path string) (string, error) {
	return readRunnerTokenWithHooks(path, runnerTokenReadHooks{})
}

type runnerTokenReadHooks struct {
	afterOpen  func()
	beforeRead func()
	afterRead  func()
}

func readRunnerTokenWithHooks(path string, hooks runnerTokenReadHooks) (string, error) {
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ".activation.json")); err == nil || !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("runner configuration activation is incomplete")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", errors.New("cannot open runner token")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	if hooks.afterOpen != nil {
		hooks.afterOpen()
	}
	if _, err := inspectRunnerToken(path, file); err != nil {
		return "", errors.New("runner token must be private regular owned file")
	}
	if hooks.beforeRead != nil {
		hooks.beforeRead()
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(raw) > 4096 {
		return "", errors.New("runner token is oversized or unreadable")
	}
	if hooks.afterRead != nil {
		hooks.afterRead()
	}
	info, err := inspectRunnerToken(path, file)
	if err != nil || int64(len(raw)) != info.Size() {
		return "", errors.New("runner token changed while being read")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, "\r\n\t \x00") {
		return "", errors.New("runner token is invalid")
	}
	return token, nil
}

func inspectRunnerToken(path string, file *os.File) (os.FileInfo, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(info, pathInfo) {
		return nil, errors.New("runner token path no longer identifies the opened file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink != 1 || int(stat.Uid) != os.Geteuid() || info.Size() > 4096 {
		return nil, errors.New("runner token must be private regular owned file")
	}
	return info, nil
}
