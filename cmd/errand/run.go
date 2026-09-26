package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/termui"
)

func cmdRun(args []string) int {
	con := newConsole(os.Stdout, os.Stderr)
	e := con.Err
	fs := flag.NewFlagSet("errand", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var settings runConfigFlags
	settings.bind(fs)
	var output outputFlags
	output.bind(fs, "show every step, with timings")
	includeAll := fs.Bool("include-all", false, "allow an otherwise refused broad snapshot (never permits a filesystem root)")
	detach := fs.Bool("detach", false, "return after admission, printing the job handle on stdout")
	fs.BoolVar(detach, "d", false, "return after admission, printing the job handle on stdout")
	fs.Usage = func() { printRootHelp(os.Stderr) }

	// Everything after "--" is the command; flags come before it.
	split := -1
	for i, a := range args {
		if a == "--" {
			split = i
			break
		}
	}
	if split < 0 {
		return unknownCommand(e, args)
	}
	if err := fs.Parse(args[:split]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return usageError(e, "unexpected arguments before --: %s", strings.Join(fs.Args(), " "))
	}
	argv := args[split+1:]
	if len(argv) == 0 {
		return usageError(e, "nothing to run after --")
	}
	if *includeAll && settings.noSnapshot {
		return usageError(e, "--include-all and --no-snapshot can't be combined")
	}
	if *detach && len(settings.session.forwards) != 0 {
		return usageError(e, "--detach and --forward can't be combined")
	}
	overrides, err := settings.overrides(fs)
	if err != nil {
		return usageError(e, "%v", err)
	}
	if settings.workspace != "" && (settings.noSnapshot || *includeAll) {
		return usageError(e, "persistent workspace runs can't use --no-snapshot or --include-all")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return failWith(e, client.ExitTransaction, err, errorScope{})
	}
	effective, err := config.ResolveRun(cwd, overrides)
	if err != nil {
		return failWith(e, runConfigErrorCode(err), err, errorScope{})
	}
	if err := effective.PrepareExecution(*includeAll); err != nil {
		return usageError(e, "%v", err)
	}
	if *detach && len(effective.Forwards) != 0 {
		e.Errorf("--detach can't use configured port forwards")
		e.Hintf("add --no-forward")
		return 2
	}
	forwards, err := sessionForwards(effective.Forwards)
	if err != nil {
		return usageError(e, "%v", err)
	}
	env, passenvs := effective.JobEnvironment()
	if effective.Where != "" && len(effective.MissingEnvironment()) != 0 {
		e.Errorf("required local variables aren't set: %s", strings.Join(effective.MissingEnvironment(), ", "))
		return client.ExitTransaction
	}
	display := runDisplay(con, effective, output)
	choices, err := runChoices(effective, overrides.URL != "", e, display.Verbose)
	if err != nil {
		return failWith(e, client.ExitTransaction, err, errorScope{})
	}
	opts := client.RunOptions{
		Where:     effective.Where,
		Workspace: effective.Workspace,
		Artifacts: effective.Artifacts, Caches: effective.Caches, Root: effective.Root,
		Argv: argv, Env: env, PassEnv: passenvs, Workdir: effective.Workdir,
		Project: effective.Project, IncludeAll: *includeAll, NoSnapshot: effective.NoSnapshot,
		Detach: *detach, ApplyOnSuccess: effective.ApplyOnSuccess, Forwards: forwards,
		Display: display, Stdout: con.Out, Stderr: con.Err,
	}
	configurePlacement(&opts, choices, e, func(c placementChoice) {
		if effective.Where == "" && !output.quiet {
			warnRunnerVersion(e, c.Target, c.Name)
		}
	})
	return client.Run(opts)
}

// runDisplay resolves what the run header and -v lines say.
func runDisplay(con *termui.Console, effective config.EffectiveRun, output outputFlags) client.RunDisplay {
	d := client.RunDisplay{UI: con, Quiet: output.quiet, Verbose: output.verbose && !output.quiet, Project: effective.Project}
	if !effective.NoSnapshot && effective.Workspace == "" {
		d.Workdir = filepath.ToSlash(effective.Workdir)
		d.Details = append(d.Details,
			[2]string{"workspace", homeRelative(effective.Root) + " (" + effective.Sources["workspace_root"] + ")"},
			[2]string{"workdir", workdirLabel(effective.Workdir)},
		)
	}
	var bindings []string
	if n := len(effective.Caches); n != 0 {
		bindings = append(bindings, termui.Things(n, "cache", "caches"))
	}
	if n := len(effective.Artifacts); n != 0 {
		bindings = append(bindings, termui.Things(n, "artifact", "artifacts"))
	}
	d.Bindings = strings.Join(bindings, ", ")
	var bound []string
	for _, cache := range effective.Caches {
		bound = append(bound, "cache "+cache.Name+" at "+cache.Path)
	}
	for _, artifact := range effective.Artifacts {
		bound = append(bound, "artifact "+artifact)
	}
	if len(bound) == 0 {
		bound = append(bound, "no caches, no artifacts")
	}
	d.Details = append(d.Details, [2]string{"bindings", strings.Join(bound, "; ")})
	return d
}

func workdirLabel(workdir string) string {
	if workdir == "" || workdir == "." {
		return ". (workspace root)"
	}
	return filepath.ToSlash(workdir)
}

// homeRelative shortens paths under the home directory to ~/…
func homeRelative(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

// runConfigErrorCode is 2 for a mistake on the command line, 120 otherwise.
func runConfigErrorCode(err error) int {
	var unknown *config.UnknownPeerError
	if errors.As(err, &unknown) {
		return 2
	}
	return client.ExitTransaction
}
