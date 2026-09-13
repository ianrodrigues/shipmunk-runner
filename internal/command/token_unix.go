//go:build linux || darwin

package command

import (
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
)

func readRunnerToken(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", errors.New("cannot open runner token")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", errors.New("cannot inspect runner token")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink != 1 || int(stat.Uid) != os.Geteuid() || info.Size() > 4096 {
		return "", errors.New("runner token must be private regular owned file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(raw) > 4096 {
		return "", errors.New("runner token is oversized or unreadable")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, "\r\n\t \x00") {
		return "", errors.New("runner token is invalid")
	}
	return token, nil
}
