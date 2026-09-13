package main

import (
	"flag"
	"fmt"
	"os"
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
	fmt.Fprintln(os.Stderr, "Go profile lifecycle is not available in this compatibility foundation.")
	os.Exit(1)
}
