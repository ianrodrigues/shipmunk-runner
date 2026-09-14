package main

import (
	"os"

	"github.com/ianrodrigues/shipmunk-runner/internal/command"
)

func main() {
	os.Exit(command.RunWatchdog(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
