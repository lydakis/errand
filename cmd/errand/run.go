package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/telemetry"
)

func cmdRun(args []string, reporter *telemetry.Reporter) int {
	fs := flag.NewFlagSet("errand", flag.ContinueOnError)
	var settings runConfigFlags
	settings.bind(fs)
	workspace := fs.String("workspace", "", "run in an explicitly created persistent workspace; never upload local edits")
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
	workspaceSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "workspace" {
			workspaceSet = true
		}
	})
	if workspaceSet {
		if err := proto.ValidateWorkspaceName(*workspace); err != nil {
			fmt.Fprintln(os.Stderr, "errand:", err)
			return 2
		}
		if settings.noSnapshot || *includeAll {
			fmt.Fprintln(os.Stderr, "errand: --workspace cannot use --no-snapshot or --include-all")
			return 2
		}
	}
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
	if *workspace != "" && effective.Where != "" {
		fmt.Fprintln(os.Stderr, "errand: --workspace requires a pinned peer; use --on instead of where")
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
	if *workspace != "" {
		if !effective.CachesOverride {
			effective.Caches = nil
		}
		if !effective.ArtifactsOverride {
			effective.Artifacts = nil
		}
	}
	for _, cache := range effective.Caches {
		fmt.Fprintf(os.Stderr, "errand: using cache %q at %q\n", cache.Name, cache.Path)
	}
	for _, artifact := range effective.Artifacts {
		fmt.Fprintf(os.Stderr, "errand: retaining artifact %q\n", artifact)
	}
	if !effective.NoSnapshot && *workspace == "" {
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
	var chosen placementChoice
	opts := client.RunOptions{
		Where: effective.Where,
		OnAdmitted: func(admission client.Admission) {
			reporter.Admitted(telemetry.Run{Transport: telemetryTransport(chosen.URL), Workspace: admission.Workspace, Caches: admission.Caches, Artifacts: admission.Artifacts, Forwarding: admission.Forwarding, Apply: admission.Apply, Detached: admission.Detached})
		},
		Workspace: *workspace,
		Artifacts: effective.Artifacts, Caches: effective.Caches, Root: effective.Root,
		Argv: argv, Env: env, PassEnv: passenvs, Workdir: effective.Workdir,
		Project: effective.Project, IncludeAll: *includeAll, NoSnapshot: effective.NoSnapshot,
		Detach: *detach, ApplyOnSuccess: effective.ApplyOnSuccess, Forwards: forwards,
	}
	configurePlacement(&opts, choices, os.Stderr, func(c placementChoice) {
		chosen = c
		if effective.Where == "" {
			warnRunnerVersion(c.Target, c.Name)
		}
	})
	return client.Run(opts)
}
