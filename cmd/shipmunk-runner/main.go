package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

func main() {
	flags := flag.NewFlagSet("shipmunk-runner", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	baseURL := flags.String("base-url", "", "control-plane URL")
	tokenFile := flags.String("token-file", "", "mode-0600 runner token file")
	stateDir := flags.String("state-dir", "", "mode-0700 state directory")
	image := flags.String("image", "", "repository image")
	once := flags.Bool("once", false, "handle at most one claim")
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
	client, err := protocol.NewHTTPClient(*baseURL, strings.TrimSpace(string(bytes)), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Runner configuration is invalid.")
		os.Exit(2)
	}
	for {
		claim, err := client.Claim(context.Background(), time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "Runner request failed. Check the runner connection and authorization.")
			os.Exit(1)
		}
		if claim != nil {
			// #55 owns the mandatory journal/cleanup lifecycle. This explicit
			// failure is safer than pretending that a claimed attempt completed.
			fmt.Fprintln(os.Stderr, "Runner stopped before a completed result could be reported. Check the application run and runner configuration.")
			os.Exit(1)
		}
		if *once {
			fmt.Println("No eligible queued work was returned for this runner.")
			return
		}
		time.Sleep(2 * time.Second)
	}
}
