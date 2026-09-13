package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	flags := flag.NewFlagSet("shipmunk-setup", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.String("server-url", "", "override the setup bundle server URL")
	version := flags.Bool("version", false, "print version")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *version {
		fmt.Println("shipmunk-setup development")
		return
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: shipmunk-setup [--server-url URL] SETUP-FILE")
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "Go setup installation is not available in this compatibility foundation.")
	os.Exit(1)
}
