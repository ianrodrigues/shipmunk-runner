package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

func main() {
	flags := flag.NewFlagSet("shipmunk-profile", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	for _, name := range []string{"base-url", "token-file", "profiles-dir", "image", "profile", "operation", "operation-id"} {
		flags.String(name, "", name)
	}
	version := flags.Bool("version", false, "print version")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *version {
		fmt.Println("shipmunk-profile development")
		return
	}
	for _, name := range []string{"base-url", "token-file", "profiles-dir", "image", "profile", "operation", "operation-id"} {
		if flags.Lookup(name).Value.String() == "" {
			fmt.Fprintf(os.Stderr, "Missing required --%s option.\n", name)
			os.Exit(2)
		}
	}
	if !protocol.IsULID(flags.Lookup("profile").Value.String()) || !protocol.IsULID(flags.Lookup("operation-id").Value.String()) {
		fmt.Fprintln(os.Stderr, "Profile and operation identifiers must be lowercase ULIDs.")
		os.Exit(2)
	}
	operation := flags.Lookup("operation").Value.String()
	if operation != "login" && operation != "probe" && operation != "disconnect" {
		fmt.Fprintln(os.Stderr, "Unsupported profile operation.")
		os.Exit(2)
	}
	info, err := os.Stat(flags.Lookup("token-file").Value.String())
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintln(os.Stderr, "Profile token file must not be accessible by group or other users.")
		os.Exit(2)
	}
	bytes, err := os.ReadFile(flags.Lookup("token-file").Value.String())
	if err != nil {
		fmt.Fprintln(os.Stderr, "Profile token file could not be read.")
		os.Exit(2)
	}
	if _, err := protocol.NewHTTPClient(flags.Lookup("base-url").Value.String(), strings.TrimSpace(string(bytes)), nil); err != nil {
		fmt.Fprintln(os.Stderr, "Profile configuration is invalid.")
		os.Exit(2)
	}
	if operation == "login" && (!isTerminal(os.Stdin) || !isTerminal(os.Stdout) || !isTerminal(os.Stderr)) {
		fmt.Fprintln(os.Stderr, "Native login requires an operator terminal.")
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "Go profile lifecycle is not available in this compatibility foundation.")
	os.Exit(1)
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
