package main

import (
	"context"
	"fmt"
	"os"

	shipmunkrelease "github.com/ianrodrigues/shipmunk-runner/internal/release"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: shipmunk-package ROOT OUTPUT VERSION")
		os.Exit(2)
	}
	if _, err := (shipmunkrelease.Builder{}).Build(context.Background(), os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
