package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

func cmdWorkspaces(args []string) int { return cmdWorkspacesTo(args, os.Stdout, os.Stderr) }

func cmdWorkspacesTo(args []string, out, stderr io.Writer) int {
	con := newConsole(out, stderr)
	e := con.Err
	verb := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb = args[0]
		args = args[1:]
	}
	if verb == "remove" {
		verb = "rm"
	}
	if verb != "list" && verb != "create" && verb != "rm" {
		e.Errorf("unknown workspaces command '%s'", verb)
		if guess := termui.Suggest(verb, []string{"list", "create", "rm", "remove"}); guess != "" {
			e.Hintf("did you mean errand workspaces %s?", guess)
		} else {
			e.Hintf("use list, create or rm")
		}
		return 2
	}
	fs := flag.NewFlagSet("errand workspaces "+verb, flag.ContinueOnError)
	var settings runConfigFlags
	if verb == "list" {
		fs.StringVar(&settings.on, "on", "", "restrict to one peer name")
		fs.StringVar(&settings.url, "url", "", "restrict to one peer base URL")
	} else {
		fs.StringVar(&settings.on, "on", "", "peer name, or local")
		fs.StringVar(&settings.url, "url", "", "peer base URL")
	}
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	var output outputFlags
	output.bind(fs, "")
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
	if ok, code := parseFlags(fs, args, "workspaces", out, e); !ok {
		return code
	}
	want := 1
	if verb == "list" {
		want = 0
	}
	if fs.NArg() != want {
		if verb == "list" {
			return usageError(e, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
		}
		e.Errorf("errand workspaces %s needs a workspace name", verb)
		e.Hintf("for example errand workspaces %s --on mini dev", verb)
		return 2
	}
	if verb == "create" && includeAll && settings.noSnapshot {
		return usageError(e, "--include-all and --no-snapshot can't be combined")
	}
	quiet := output.quiet || *jsonOutput
	switch verb {
	case "list":
		return listWorkspaces(con, settings.url, settings.on, *jsonOutput, output.verbose)
	case "rm":
		url, label, err := resolvePeerTarget(settings.url, settings.on)
		if err != nil {
			return failWith(e, 2, err, errorScope{peer: settings.on})
		}
		name := fs.Arg(0)
		label = cmpOr(label, url)
		var spin *termui.Spinner
		if !quiet {
			spin = e.Spin("Removing " + e.B(name) + " from " + e.B(label) + "…")
		}
		removal, err := client.RemoveWorkspace(url, name)
		if spin != nil {
			spin.Stop()
		}
		if err != nil {
			return failWith(e, 1, err, errorScope{peer: label, workspace: name})
		}
		if *jsonOutput {
			if err := json.NewEncoder(out).Encode(struct {
				Removed    string `json:"removed"`
				FreedBytes int64  `json:"freed_bytes"`
			}{name, removal.FreedBytes}); err != nil {
				e.Errorf("%v", err)
				return 1
			}
		}
		if !quiet {
			freed := ""
			if removal.FreedBytes >= 0 {
				freed = " " + e.D("· freed "+termui.Bytes(removal.FreedBytes))
			}
			e.Say(termui.OK, "Removed "+e.B(name)+" from "+label+freed)
		}
		return 0
	}

	name := fs.Arg(0)
	if err := proto.ValidateWorkspaceName(name); err != nil {
		return usageError(e, "%v", err)
	}
	overrides, err := settings.overrides(fs)
	if err != nil {
		return usageError(e, "%v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return failWith(e, 1, err, errorScope{})
	}
	effective, err := config.ResolveWorkspaceCreation(cwd, overrides)
	if err != nil {
		return failWith(e, runConfigErrorCode(err), err, errorScope{peer: settings.on})
	}
	choices, err := runChoices(effective, settings.url != "", e, output.verbose)
	if err != nil {
		return failWith(e, 1, err, errorScope{})
	}
	opts := client.RunOptions{Where: effective.Where, Root: effective.Root, Project: effective.Project, Caches: effective.Caches, Artifacts: effective.Artifacts, NoSnapshot: effective.NoSnapshot, IncludeAll: includeAll, Stderr: stderr,
		Display: client.RunDisplay{UI: con, Quiet: quiet, Verbose: output.verbose}}
	var chosen placementChoice
	configurePlacement(&opts, choices, e, func(c placementChoice) { chosen = c })
	var spin *termui.Spinner
	if !quiet {
		what := e.B(cmpOr(effective.Project, "files"))
		if effective.NoSnapshot {
			what = "an empty workspace"
		}
		spin = e.Spin("Uploading " + what + "…")
	}
	w, err := client.CreateWorkspace(opts, name)
	if spin != nil {
		spin.Stop()
	}
	url, label := chosen.URL, cmpOr(chosen.Name, chosen.URL)
	if err != nil {
		return failWith(e, 1, err, errorScope{peer: label, workspace: name})
	}
	if *jsonOutput {
		if err := json.NewEncoder(out).Encode(struct {
			proto.Workspace
			Peer string `json:"peer"`
			URL  string `json:"url"`
		}{w, label, url}); err != nil {
			e.Errorf("%v", err)
			return 1
		}
		return 0
	}
	if !con.Out.Interactive() || quiet {
		fmt.Fprintln(out, w.Name)
	}
	if quiet {
		return 0
	}
	files, bytes := 0, int64(0)
	for _, entry := range w.Manifest.Entries {
		if entry.Type == proto.EntryFile {
			files++
			bytes += entry.Size
		}
	}
	from := "empty"
	if !effective.NoSnapshot {
		from = "from " + displayDir(effective.Root) + " · " + termui.Things(files, "file", "files") + " · " + termui.Bytes(bytes)
	}
	e.Say(termui.OK, "Created "+e.B(w.Name)+" on "+label+" "+e.D(from))
	e.Next("errand --workspace "+w.Name+" -- make test", "run in it")
	e.Next("errand push --watch --apply "+runnerFlag(label)+" --workspace "+w.Name, "keep it in sync")
	return 0
}

type workspaceRow struct {
	Peer string `json:"peer"`
	proto.WorkspaceSummary
}

// listWorkspaces follows the discovery contract: every configured peer unless
// --on or --url narrows it. Names are scoped per runner, so rows carry theirs.
func listWorkspaces(con *termui.Console, rawURL, on string, jsonOutput, verbose bool) int {
	e, o := con.Err, con.Out
	read, err := readFleet(rawURL, on, e, client.ListWorkspaces)
	if err != nil {
		code := 2
		if errors.Is(err, errNoUsablePeers) {
			code = 1
		}
		return failWith(e, code, err, errorScope{peer: on})
	}
	rows := make([]workspaceRow, 0)
	for _, result := range read.results {
		for _, summary := range result.value {
			rows = append(rows, workspaceRow{Peer: result.target.name, WorkspaceSummary: summary})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Peer != rows[j].Peer {
			return rows[i].Peer < rows[j].Peer
		}
		return rows[i].Name < rows[j].Name
	})
	if jsonOutput {
		if err := json.NewEncoder(o.Writer()).Encode(rows); err != nil {
			e.Errorf("%v", err)
			return 1
		}
		return read.exitCode()
	}
	if len(rows) == 0 {
		if len(read.results) != 0 {
			o.Print("No workspaces on " + joinWordsOr(targetNames(read.targets)) + ".")
			if o.Interactive() {
				o.Next("errand workspaces create NAME", "keep files on a runner between jobs")
			}
		}
		return read.exitCode()
	}
	if !o.Interactive() {
		tw := tabwriter.NewWriter(o.Writer(), 2, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "PEER\tNAME\tSTATE\tJOBS\tPROJECT")
		for _, r := range rows {
			state, job := "idle", "-"
			if len(r.JobIDs) != 0 {
				state, job = "busy", strings.Join(r.JobIDs, ",")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", terminalSafeField(r.Peer), terminalSafeField(r.Name), state, job, terminalSafeField(r.Project))
		}
		if err := tw.Flush(); err != nil {
			e.Errorf("%v", err)
			return 1
		}
		return read.exitCode()
	}
	headers := []string{"WORKSPACE", "PEER", "STATE", "PROJECT", "CREATED"}
	if verbose {
		headers = append(headers, "ID")
	}
	t := o.Table(headers...)
	now := time.Now()
	for _, r := range rows {
		state := termui.C("idle", termui.Dim)
		if len(r.JobIDs) != 0 {
			jobs := make([]string, len(r.JobIDs))
			for i, id := range r.JobIDs {
				jobs[i] = termui.ShortID(id)
			}
			state = termui.C("● busy "+strings.Join(jobs, ","), termui.Green)
		}
		cells := []termui.Cell{termui.C(terminalSafeField(r.Name), termui.Bold), termui.C(terminalSafeField(r.Peer)), state, termui.C(cmpOr(terminalSafeField(r.Project), "-")), termui.C(termui.Ago(r.CreatedAt, now))}
		if verbose {
			cells = append(cells, termui.C(r.ID, termui.Dim))
		}
		t.Row(cells...)
	}
	t.Print()
	return read.exitCode()
}
