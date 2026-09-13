package main

import (
	"os"

	"github.com/ianrodrigues/shipmunk-runner/internal/command"
)

func main() {
	os.Exit(command.RunRunner(os.Args[1:], os.Stdout, os.Stderr))
}
