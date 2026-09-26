package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/termui"
)

func cmdFetch(args []string) int { return cmdFetchTo(args, os.Stdout, os.Stderr) }

func cmdFetchTo(args []string, out, stderr io.Writer) int {
	con := newConsole(out, stderr)
	e := con.Err
	fs := flag.NewFlagSet("errand fetch", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	apply := fs.Bool("apply", false, "apply retained workspace changes with a clean-or-refuse three-way merge")
	conflicts := fs.Bool("conflicts", false, "materialize text conflicts and apply clean changes")
	output := fs.String("output", "", "export retained remote files into a new directory")
	fs.StringVar(output, "o", "", "export retained remote files into a new directory")
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	var verbosity outputFlags
	verbosity.bind(fs, "show transfer details and the staging directory")
	if ok, code := parseFlags(fs, args, "fetch", out, e); !ok {
		return code
	}
	invalidOutput := false
	fs.Visit(func(f *flag.Flag) {
		if (f.Name == "output" || f.Name == "o") && *output == "" {
			invalidOutput = true
		}
	})
	if invalidOutput {
		return usageError(e, "--output needs a directory")
	}
	if *output != "" && (*apply || *conflicts) {
		return usageError(e, "--output can't be combined with --apply or --conflicts")
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		if fs.NArg() == 0 {
			return needHandle(e, "fetch")
		}
		return usageError(e, "fetch takes a job handle and at most one path")
	}
	if *conflicts && !*apply {
		return usageError(e, "--conflicts only works with --apply")
	}
	peerURL, label, jobID, err := resolveHandle(fs.Arg(0), *rawURL, *on)
	if err != nil {
		return failWith(e, handleErrorCode(err), err, handleScope(fs.Arg(0), label, *on))
	}
	label = cmpOr(label, peerURL)
	shown := displayHandle(e, label, jobID)
	changePath := ""
	if fs.NArg() == 2 {
		changePath = fs.Arg(1)
	}
	callerDir := ""
	if *apply {
		callerDir, err = os.Getwd()
		if err != nil {
			return failWith(e, client.ExitTransaction, fmt.Errorf("resolving current workspace: %w", err), errorScope{})
		}
	}
	quiet := verbosity.quiet || *jsonOutput
	if !quiet {
		warnRunnerVersion(e, peerURL, label)
	}
	action := "staged"
	verb := "Downloading changes from " + e.B(label) + "…"
	switch {
	case *apply:
		action = "applied"
		verb = "Applying changes from " + e.B(label) + "…"
	case *output != "":
		action = "exported"
		verb = "Exporting changes from " + e.B(label) + "…"
	}
	var spin *termui.Spinner
	if !quiet {
		spin = e.Spin(verb)
	}
	var stats client.TransferStats
	var kinds []client.PathChange
	staged, err := client.FetchChanges(client.ChangeFetchOptions{
		PeerURL: peerURL, JobID: jobID, Apply: *apply, MaterializeConflicts: *conflicts,
		Path: changePath, CallerDir: callerDir, OutputDir: *output, Stats: &stats, Changes: &kinds,
	})
	if spin != nil {
		spin.Stop()
	}
	if errors.Is(err, client.ErrNoChanges) {
		if *jsonOutput {
			json.NewEncoder(out).Encode(newTransferReport("unchanged", stats, nil))
			return 0
		}
		if !verbosity.quiet {
			e.Say(termui.Dot, "Nothing to fetch: "+e.ID(shown)+" didn't change any files.")
		}
		return 0
	}
	if *jsonOutput {
		report := struct {
			transferReport
			Path         string   `json:"path"`
			Conflicts    []string `json:"conflicts,omitempty"`
			Materialized bool     `json:"materialized,omitempty"`
		}{transferReport: newTransferReport(action, stats, err), Path: staged}
		var conflict *changes.MergeConflictError
		if errors.As(err, &conflict) {
			report.Conflicts, report.Materialized = conflict.Paths, conflict.Materialized
		}
		if writeErr := json.NewEncoder(out).Encode(report); writeErr != nil {
			e.Errorf("%v", writeErr)
			return client.ExitTransaction
		}
	}
	if err != nil {
		// Errors go to stderr even with --json, which carries them on stdout too.
		var conflict *changes.MergeConflictError
		if errors.As(err, &conflict) {
			reportConflicts(e, conflict, "errand fetch --apply --conflicts "+shown)
		} else {
			failWith(e, client.ExitTransaction, err, errorScope{peer: label, job: jobID})
		}
		if staged != "" {
			e.Hintf("the files are downloaded at %s", homeRelative(staged))
		}
		return client.ExitTransaction
	}
	if *jsonOutput {
		return 0
	}
	// Scripts capture the staged or exported directory; people read the list.
	if !*apply && (!con.Out.Interactive() || verbosity.verbose || verbosity.quiet) {
		fmt.Fprintln(out, staged)
	}
	if verbosity.quiet {
		return 0
	}
	count := termui.Things(stats.ChangedPaths, "changed file", "changed files")
	transfer := termui.Bytes(stats.TransferredBytes) + " · " + termui.Duration(msDuration(stats.ElapsedMillis))
	switch action {
	case "applied":
		e.Say(termui.OK, "Applied "+e.B(termui.Things(stats.ChangedPaths, "file", "files"))+" from "+e.ID(shown)+detailSuffix(e, verbosity.verbose, transfer))
	case "exported":
		e.Say(termui.OK, "Exported "+e.B(termui.Things(stats.ChangedPaths, "file", "files"))+" to "+e.B(displayDir(staged))+detailSuffix(e, verbosity.verbose, transfer))
	default:
		e.Say(termui.OK, "Downloaded "+e.B(count)+" from "+e.ID(shown)+" "+e.D("· "+transfer))
	}
	writeChangeList(e, kinds, action == "applied")
	if action == "staged" {
		next := "errand fetch --apply " + shown
		if changePath != "" {
			next += " " + changePath
		}
		e.Next(next, "apply them here")
	}
	return 0
}

// writeChangeList prints changed paths, with A/M/D letters once they're
// applied. Long lists are cut after twenty.
func writeChangeList(e *termui.Stream, kinds []client.PathChange, letters bool) {
	const shown = 20
	for i, c := range kinds {
		if i == shown {
			e.Print("    " + e.D(fmt.Sprintf("… and %d more", len(kinds)-shown)))
			break
		}
		if letters {
			e.Print("    " + c.Letter(e) + " " + terminalSafeField(c.Path))
		} else {
			e.Print("    " + terminalSafeField(c.Path))
		}
	}
}

// reportConflicts explains a refused apply.
func reportConflicts(e *termui.Stream, conflict *changes.MergeConflictError, retry string) {
	if conflict.Materialized {
		e.Warnf("%s have conflict markers; clean changes were applied", termui.Things(len(conflict.Paths), "file", "files"))
	} else {
		e.Errorf("%s changed here too, so nothing was applied", termui.Things(len(conflict.Paths), "file", "files"))
	}
	for i, p := range conflict.Paths {
		if i == 20 {
			e.Print("    " + e.D(fmt.Sprintf("… and %d more", len(conflict.Paths)-20)))
			break
		}
		e.Print("    " + e.Paint("C", termui.Red) + " " + terminalSafeField(p))
	}
	if !conflict.Materialized {
		e.Hintf("resolve them with conflict markers: %s", retry)
	}
}

func detailSuffix(e *termui.Stream, verbose bool, detail string) string {
	if !verbose {
		return ""
	}
	return " " + e.D("· "+detail)
}

func displayDir(path string) string {
	if cwd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
			return "./" + rel
		}
	}
	return homeRelative(path)
}
