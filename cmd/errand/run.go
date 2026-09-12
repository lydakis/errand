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
	verbose := fs.Bool("verbose", false, "show individual cache and artifact bindings")
	fs.BoolVar(verbose, "v", false, "show individual cache and artifact bindings")
	includeAll := fs.Bool("include-all", false, "allow an otherwise refused broad snapshot (never permits a filesystem root)")
	detach := fs.Bool("detach", false, "return after admission, printing the job handle on stdout")
	fs.BoolVar(detach, "d", false, "return after admission, printing the job handle on stdout")
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
	if *detach && len(settings.session.forwards) != 0 {
		fmt.Fprintln(os.Stderr, "errand: --detach and --forward are mutually exclusive")
		return 2
	}
	overrides, err := settings.overrides(fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 2
	}
	if settings.workspace != "" && (settings.noSnapshot || *includeAll) {
		fmt.Fprintln(os.Stderr, "errand: persistent workspace runs cannot use --no-snapshot or --include-all")
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
	if err := effective.PrepareExecution(*includeAll); err != nil {
		fmt.Fprintln(os.Stderr, "errand:", err)
		return 2
	}
	if *detach && len(effective.Forwards) != 0 {
		fmt.Fprintln(os.Stderr, "errand: --detach cannot use configured forwards; add --no-forward")
		return 2
	}
	forwards, err := sessionForwards(effective.Forwards)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 2
	}
	env, passenvs := effective.JobEnvironment()
	printRunBindings(os.Stderr, effective, *verbose)
	if !effective.NoSnapshot && effective.Workspace == "" {
		fmt.Fprintf(os.Stderr, "errand: workspace root %s (from %s)\n", terminalSafeField(effective.Root), terminalSafeField(effective.Sources["workspace_root"]))
		fmt.Fprintf(os.Stderr, "errand: command workdir %s\n", terminalSafeField(displaySourceWorkdir(effective)))
	}
	if effective.Where != "" && len(effective.MissingEnvironment()) != 0 {
		fmt.Fprintf(os.Stderr, "errand: required local variables are unset: %q\n", effective.MissingEnvironment())
		return client.ExitTransaction
	}
	choices, err := runChoices(effective, overrides.URL != "", os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "errand:", err)
		return client.ExitTransaction
	}
	opts := client.RunOptions{
		Where:     effective.Where,
		Workspace: effective.Workspace,
		Artifacts: effective.Artifacts, Caches: effective.Caches, Root: effective.Root,
		Argv: argv, Env: env, PassEnv: passenvs, Workdir: effective.Workdir,
		Project: effective.Project, IncludeAll: *includeAll, NoSnapshot: effective.NoSnapshot,
		Detach: *detach, ApplyOnSuccess: effective.ApplyOnSuccess, Forwards: forwards,
	}
	configurePlacement(&opts, choices, os.Stderr, func(c placementChoice) {
		if effective.Where == "" {
			warnRunnerVersion(c.Target, c.Name)
		}
	})
	return client.Run(opts)
}
