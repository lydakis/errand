package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
)

// Both submission and inspection use these flags and the same resolver.
type runConfigFlags struct {
	on, url, workdir, root     string
	apply, noApply, noSnapshot bool
}

func (f *runConfigFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.on, "on", "", "peer name from personal configuration")
	fs.StringVar(&f.url, "url", "", "peer base URL (mutually exclusive with --on)")
	fs.StringVar(&f.workdir, "workdir", "", "working directory, relative to the workspace root")
	fs.StringVar(&f.workdir, "w", "", "working directory, relative to the workspace root")
	fs.StringVar(&f.root, "workspace-root", "", "snapshot root containing the current directory")
	fs.BoolVar(&f.apply, "apply", false, "apply retained workspace changes after successful completion")
	fs.BoolVar(&f.noApply, "no-apply", false, "do not apply retained workspace changes after the run")
	fs.BoolVar(&f.noSnapshot, "no-snapshot", false, "run in an empty remote workspace")
}

func (f runConfigFlags) overrides(fs *flag.FlagSet) (config.RunOverrides, error) {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	result := config.RunOverrides{Peer: f.on, URL: f.url, WorkspaceRoot: f.root, NoSnapshot: f.noSnapshot}
	if set["on"] && f.on == "" || set["url"] && f.url == "" {
		return result, fmt.Errorf("--on and --url require non-empty values")
	}
	if f.on != "" && f.url != "" {
		return result, fmt.Errorf("--on and --url are mutually exclusive")
	}
	if set["apply"] && set["no-apply"] {
		return result, fmt.Errorf("--apply and --no-apply are mutually exclusive")
	}
	if set["apply"] {
		result.ApplyOnSuccess = &f.apply
	}
	if set["no-apply"] {
		value := !f.noApply
		result.ApplyOnSuccess = &value
	}
	if set["workdir"] || set["w"] {
		result.Workdir = &f.workdir
	}
	if f.noSnapshot && f.root != "" {
		return result, fmt.Errorf("--workspace-root and --no-snapshot are mutually exclusive")
	}
	if f.noSnapshot && f.workdir != "" && f.workdir != "." {
		return result, fmt.Errorf("--workdir must be the workspace root when using --no-snapshot")
	}
	return result, nil
}

func cmdConfig(args []string) int { return cmdConfigTo(args, os.Stdout, os.Stderr) }

func cmdConfigTo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("errand config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var flags runConfigFlags
	flags.bind(fs)
	asJSON := fs.Bool("json", false, "print effective run configuration and sources as JSON")
	setFlagUsage(fs, "errand config [options]")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "errand: unexpected config arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	overrides, err := flags.overrides(fs)
	if err != nil {
		fmt.Fprintf(stderr, "errand: %v\n", err)
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "errand: %v\n", err)
		return client.ExitTransaction
	}
	effective, err := config.ResolveRun(cwd, overrides)
	if err != nil {
		fmt.Fprintf(stderr, "errand: %v\n", err)
		return client.ExitTransaction
	}
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(effective)
	} else {
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "SETTING\tVALUE\tSOURCE")
		for _, row := range []struct {
			key   string
			value any
		}{
			{"peer", effective.Peer}, {"url", effective.URL},
			{"remote_command", effective.RemoteCommand}, {"remote_socket", effective.RemoteSocket},
			{"workspace_root", effective.Root}, {"workdir", effective.Workdir},
			{"project", effective.Project}, {"apply_on_success", effective.ApplyOnSuccess},
			{"no_snapshot", effective.NoSnapshot},
		} {
			if _, exists := effective.Sources[row.key]; !exists {
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", row.key, terminalSafeField(fmt.Sprint(row.value)), terminalSafeField(effective.Sources[row.key]))
		}
		err = w.Flush()
	}
	if err != nil {
		fmt.Fprintf(stderr, "errand: writing config: %v\n", err)
		return client.ExitTransaction
	}
	return 0
}
