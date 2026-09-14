//go:build unix

package command

import (
	"os"
	"syscall"
)

func setupFileUID(info os.FileInfo) uint32 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ^uint32(0)
	}
	return stat.Uid
}

func setupFileNlink(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}
