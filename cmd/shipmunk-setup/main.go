package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/setup"
)

func main() {
	arguments := os.Args[1:]
	if len(arguments) == 1 && arguments[0] == "--version" {
		fmt.Println("shipmunk-setup development")
		return
	}
	if len(arguments) == 1 && (arguments[0] == "--help" || arguments[0] == "-h") {
		fmt.Println("Usage: shipmunk-setup SETUP-FILE [--server-url=https://runner-reachable-server]")
		return
	}
	if len(arguments) < 1 || len(arguments) > 2 {
		fmt.Fprintln(os.Stderr, "Usage: shipmunk-setup SETUP-FILE [--server-url=https://runner-reachable-server]")
		os.Exit(2)
	}
	serverURL := ""
	if len(arguments) == 2 {
		if !strings.HasPrefix(arguments[1], "--server-url=") {
			fmt.Fprintln(os.Stderr, "The only setup option is --server-url=https://runner-reachable-server.")
			os.Exit(2)
		}
		serverURL = strings.TrimPrefix(arguments[1], "--server-url=")
		if serverURL == "" {
			fmt.Fprintln(os.Stderr, "The only setup option is --server-url=https://runner-reachable-server.")
			os.Exit(2)
		}
	}
	if _, err := setup.Read(arguments[0], serverURL, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "Setup file is invalid or expired.")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "Go setup installation is not available in this compatibility foundation.")
	os.Exit(1)
}
