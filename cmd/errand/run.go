package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
)

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("errand", flag.ContinueOnError)
	var settings runConfigFlags
	settings.bind(fs)
	includeAll := fs.Bool("include-all", false, "allow an otherwise refused broad snapshot (never permits a filesystem root)")
	detach := fs.Bool("detach", false, "return after admission, printing the job handle on stdout")
	fs.BoolVar(detach, "d", false, "return after admission, printing the job handle on stdout")
	var forwards portForwardList
	fs.Var(&forwards, "forward", "forward local loopback [LOCAL:]REMOTE while attached (repeatable)")
	fs.Var(&forwards, "L", "forward local loopback [LOCAL:]REMOTE while attached (repeatable)")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }

	// Everything after "--" is the command; flags come before it.
	split := -1
	for i, a := range args {
		if a == "--" {
			split = i
			break
		}
	}
	if split < 0 {
		fmt.Fprintln(os.Stderr, "errand: missing \"--\" before the command\n\n"+usage)
		return 2
	}
	if err := fs.Parse(args[:split]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "errand: unexpected arguments before --: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	argv := args[split+1:]
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "errand: empty command after \"--\"")
		return 2
	}
	if *includeAll && settings.noSnapshot {
		fmt.Fprintln(os.Stderr, "errand: --include-all and --no-snapshot are mutually exclusive")
		return 2
	}
	if *detach && len(forwards) != 0 {
		fmt.Fprintln(os.Stderr, "errand: --detach and --forward are mutually exclusive")
		return 2
	}
	overrides, err := settings.overrides(fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return client.ExitTransaction
	}
	effective, err := config.ResolveRun(cwd, overrides)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return client.ExitTransaction
	}
	env, passenvs := effective.JobEnvironment()
	if !effective.NoSnapshot {
		shownWorkdir := effective.Workdir
		if shownWorkdir == "" {
			shownWorkdir = "."
		}
		fmt.Fprintf(os.Stderr, "errand: workspace root %s (from %s)\n", effective.Root, effective.Sources["workspace_root"])
		fmt.Fprintf(os.Stderr, "errand: command workdir %s\n", shownWorkdir)
	}
	peerURL := effective.URL
	// Raw URLs must remain the same identity used by handle resolution and
	// local change-state lookups. Only configured aliases need SSH registration.
	if overrides.URL == "" {
		peerURL = client.ConfigureSSHPeer(peerURL, effective.Peer, effective.RemoteCommand, effective.RemoteSocket)
	}
	return client.Run(client.RunOptions{
		PeerURL: peerURL, PeerName: effective.Peer, Root: effective.Root,
		Argv: argv, Env: env, PassEnv: passenvs, Workdir: effective.Workdir,
		Project: effective.Project, IncludeAll: *includeAll, NoSnapshot: effective.NoSnapshot,
		Detach: *detach, ApplyOnSuccess: effective.ApplyOnSuccess, Forwards: forwards,
	})
}
