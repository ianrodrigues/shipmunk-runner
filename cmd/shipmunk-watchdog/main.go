package main

import (
	"fmt"
	"os"

	"github.com/ianrodrigues/shipmunk-runner/internal/sandbox"
)

func main() {
	if err := sandbox.RunWatchdogCommand(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "shipmunk-watchdog:", err)
		os.Exit(1)
	}
}
