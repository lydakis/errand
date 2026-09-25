package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

func cmdWorkspaces(args []string) int { return cmdWorkspacesTo(args, os.Stdout, os.Stderr) }

func cmdWorkspacesTo(args []string, out, stderr io.Writer) int {
	verb := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb = args[0]
		args = args[1:]
	}
	if verb != "list" && verb != "create" && verb != "rm" {
		fmt.Fprintln(stderr, "errand workspaces: expected create, list, or rm")
		return 2
	}
	fs := flag.NewFlagSet("errand workspaces "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var settings runConfigFlags
	if verb == "list" {
		fs.StringVar(&settings.on, "on", "", "restrict to one peer name")
		fs.StringVar(&settings.url, "url", "", "restrict to one peer base URL")
	} else {
		fs.StringVar(&settings.on, "on", "", "peer name, or local")
		fs.StringVar(&settings.url, "url", "", "peer base URL")
	}
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	includeAll := false
	if verb == "create" {
		fs.StringVar(&settings.where, "where", "", "select a configured runner matching facts")
		fs.StringVar(&settings.profile, "profile", "", "use a named configuration profile")
		fs.StringVar(&settings.root, "workspace-root", "", "snapshot root containing the current directory")
		fs.BoolVar(&settings.noSnapshot, "no-snapshot", false, "create an empty persistent workspace")
		fs.BoolVar(&includeAll, "include-all", false, "allow an otherwise refused broad snapshot (never a filesystem root)")
		fs.Var(&settings.artifacts, "artifact", "retain an ignored output path (repeatable)")
		fs.BoolVar(&settings.noArtifacts, "no-artifacts", false, "clear configured artifacts")
		fs.Var(&settings.caches, "cache", "bind a named cache NAME=PATH (repeatable)")
		fs.BoolVar(&settings.noCaches, "no-caches", false, "clear configured caches")
	}
	synopsis := "errand workspaces [list] [options]"
	if verb != "list" {
		synopsis = "errand workspaces " + verb + " [options] NAME"
	}
	setFlagUsage(fs, synopsis)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	want := 1
	if verb == "list" {
		want = 0
	}
	if fs.NArg() != want {
		fmt.Fprintln(stderr, "usage: "+synopsis)
		return 2
	}
	if verb == "create" && includeAll && settings.noSnapshot {
		fmt.Fprintln(stderr, "errand: --include-all and --no-snapshot are mutually exclusive")
		return 2
	}
	var url, label string
	var opts client.RunOptions
	var creationChoices []placementChoice
	var err error
	if verb == "create" {
		if err := proto.ValidateWorkspaceName(fs.Arg(0)); err != nil {
			fmt.Fprintln(stderr, "errand:", err)
			return 2
		}
		overrides, overrideErr := settings.overrides(fs)
		if overrideErr != nil {
			fmt.Fprintln(stderr, "errand:", overrideErr)
			return 2
		}
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			fmt.Fprintln(stderr, "errand:", cwdErr)
			return 1
		}
		effective, resolveErr := config.ResolveWorkspaceCreation(cwd, overrides)
		if resolveErr != nil {
			fmt.Fprintln(stderr, "errand:", resolveErr)
			return 1
		}
		creationChoices, err = runChoices(effective, settings.url != "", stderr)
		if err != nil {
			fmt.Fprintln(stderr, "errand:", err)
			return 1
		}
		opts = client.RunOptions{Where: effective.Where, Root: effective.Root, Project: effective.Project, Caches: effective.Caches, Artifacts: effective.Artifacts, NoSnapshot: effective.NoSnapshot, IncludeAll: includeAll, Stderr: stderr}

	} else if verb == "rm" {
		url, label, err = resolvePeerTarget(settings.url, settings.on)
		if err != nil {
			fmt.Fprintln(stderr, "errand:", err)
			return 2
		}
	}
	switch verb {
	case "create":
		var chosen placementChoice
		configurePlacement(&opts, creationChoices, stderr, func(c placementChoice) { chosen = c })
		w, err := client.CreateWorkspace(opts, fs.Arg(0))
		url, label = chosen.URL, chosen.Name

		if err != nil {
			fmt.Fprintln(stderr, "errand:", err)
			return 1
		}
		if *jsonOutput {
			err = json.NewEncoder(out).Encode(struct {
				proto.Workspace
				Peer string `json:"peer"`
				URL  string `json:"url"`
			}{w, label, url})
		} else {
			_, err = fmt.Fprintf(out, "%s\n", w.Name)
			fmt.Fprintf(stderr, "errand: created workspace %s on %s\n", w.Name, cmpOr(label, url))
		}
		if err != nil {
			fmt.Fprintln(stderr, "errand:", err)
			return 1
		}
	case "rm":
		if err := client.RemoveWorkspace(url, fs.Arg(0)); err != nil {
			fmt.Fprintln(stderr, "errand:", err)
			return 1
		}
		if *jsonOutput {
			if err := json.NewEncoder(out).Encode(map[string]string{"removed": fs.Arg(0)}); err != nil {
				fmt.Fprintln(stderr, "errand:", err)
				return 1
			}
		}
	case "list":
		return listWorkspaces(settings.url, settings.on, *jsonOutput, out, stderr)
	}
	return 0
}

type workspaceRow struct {
	Peer string `json:"peer"`
	proto.WorkspaceSummary
}

// listWorkspaces follows the discovery contract: every configured peer unless
// --on or --url narrows it. Names are scoped per runner, so rows carry theirs.
func listWorkspaces(rawURL, on string, jsonOutput bool, out, stderr io.Writer) int {
	read, err := readFleet(rawURL, on, stderr, client.ListWorkspaces)
	if err != nil {
		fmt.Fprintln(stderr, "errand:", err)
		if errors.Is(err, errNoUsablePeers) {
			return 1
		}
		return 2
	}
	rows := make([]workspaceRow, 0)
	for _, result := range read.results {
		for _, summary := range result.value {
			rows = append(rows, workspaceRow{Peer: result.target.name, WorkspaceSummary: summary})
		}
	}
	if jsonOutput {
		if err := json.NewEncoder(out).Encode(rows); err != nil {
			fmt.Fprintln(stderr, "errand:", err)
			return 1
		}
		return read.exitCode()
	}
	tw := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "PEER\tNAME\tSTATE\tJOBS\tPROJECT")
	for _, r := range rows {
		state, job := "idle", "-"
		if len(r.JobIDs) != 0 {
			state, job = "busy", strings.Join(r.JobIDs, ",")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", terminalSafeField(r.Peer), terminalSafeField(r.Name), state, job, terminalSafeField(r.Project))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, "errand:", err)
		return 1
	}
	return read.exitCode()
}
