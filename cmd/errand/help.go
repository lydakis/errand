package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func cliHelpRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func cmdVersion(args []string) int {
	fs := flag.NewFlagSet("errand version", flag.ContinueOnError)
	setFlagUsage(fs, "errand version")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "errand: unexpected version arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	fmt.Println("errand", version)
	return 0
}

func setFlagUsage(fs *flag.FlagSet, synopsis string) {
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s\n", synopsis)
		hasFlags := false
		fs.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(fs.Output(), "\noptions:")
			fs.PrintDefaults()
		}
	}
}
