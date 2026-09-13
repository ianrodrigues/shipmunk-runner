package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
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
	info, err := os.Stat(*tokenFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintln(os.Stderr, "Runner token file must not be accessible by group or other users.")
		os.Exit(2)
	}
	bytes, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Runner token file could not be read.")
		os.Exit(2)
	}
	if _, err := protocol.NewHTTPClient(*baseURL, strings.TrimSpace(string(bytes)), nil); err != nil {
		fmt.Fprintln(os.Stderr, "Runner configuration is invalid.")
		os.Exit(2)
	}
	// #55 must persist a claim before any preparation begins. Until that
	// journal exists, polling here could reserve work that this process cannot
	// safely supervise or reconcile after a crash.
	fmt.Fprintln(os.Stderr, "Go runner supervision is not available in this compatibility foundation.")
	os.Exit(1)
}
