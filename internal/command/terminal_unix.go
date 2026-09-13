//go:build linux || darwin

package command

import "os"

func isTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	return terminalFD(file.Fd())
}
