package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	flags := flag.NewFlagSet("shipmunk-runner", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	baseURL := flags.String("base-url", "", "control-plane URL")
	tokenFile := flags.String("token-file", "", "mode-0600 runner token file")
	stateDir := flags.String("state-dir", "", "mode-0700 state directory")
	image := flags.String("image", "", "repository image")
	flags.Bool("once", false, "handle at most one claim")
	flags.String("driver", "fixture", "fixture or codex")
	flags.String("profiles-dir", "", "protected profile directory")
	flags.String("repository-image", "", "repository command image")
	version := flags.Bool("version", false, "print version")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *version {
		fmt.Println("shipmunk-runner development")
		return
	}
	if *baseURL == "" || *tokenFile == "" || *stateDir == "" || *image == "" {
		fmt.Fprintln(os.Stderr, "Missing required --base-url, --token-file, --state-dir or --image option.")
		os.Exit(2)
	}
	// The Go supervision/cleanup implementation is intentionally introduced by
	// issue #55. Do not claim work until it owns durable recovery.
	fmt.Fprintln(os.Stderr, "Go runner supervision is not available in this compatibility foundation.")
	os.Exit(1)
}
