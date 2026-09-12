package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/client"
)

func cmdFetch(args []string) int { return cmdFetchTo(args, os.Stdout, os.Stderr) }

func cmdFetchTo(args []string, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("errand fetch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	apply := fs.Bool("apply", false, "apply retained workspace changes with a clean-or-refuse three-way merge")
	conflicts := fs.Bool("conflicts", false, "materialize text conflicts and apply clean changes")
	output := fs.String("output", "", "export retained remote files into a new directory")
	fs.StringVar(output, "o", "", "export retained remote files into a new directory")
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	setFlagUsage(fs, "errand fetch [options] HANDLE [PATH]")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	invalidOutput := false
	fs.Visit(func(f *flag.Flag) {
		if (f.Name == "output" || f.Name == "o") && *output == "" {
			invalidOutput = true
		}
	})
	if invalidOutput || (*output != "" && (*apply || *conflicts)) {
		fmt.Fprintln(stderr, "errand fetch: --output requires a non-empty directory and cannot be combined with --apply or --conflicts")
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fmt.Fprintln(stderr, "errand fetch: HANDLE (peer/ULID) and at most one changed PATH are required")
		return 2
	}
	if *conflicts && !*apply {
		fmt.Fprintln(stderr, "errand fetch: --conflicts requires --apply")
		return 2
	}
	peerURL, label, jobID, err := resolveHandle(fs.Arg(0), *rawURL, *on)
	if err != nil {
		fmt.Fprintf(stderr, "errand: %v\n", err)
		return 2
	}
	changePath := ""
	if fs.NArg() == 2 {
		changePath = fs.Arg(1)
	}
	callerDir := ""
	if *apply {
		callerDir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "errand: resolving current workspace: %v\n", err)
			return client.ExitTransaction
		}
	}
	warnRunnerVersion(peerURL, label)
	var stats client.TransferStats
	staged, err := client.FetchChanges(client.ChangeFetchOptions{
		PeerURL: peerURL, JobID: jobID, Apply: *apply, MaterializeConflicts: *conflicts,
		Path: changePath, CallerDir: callerDir, OutputDir: *output, Stats: &stats,
	})
	action := "staged"
	if *apply {
		action = "applied"
	}
	if *output != "" {
		action = "exported"
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
			fmt.Fprintln(stderr, "errand:", writeErr)
			return client.ExitTransaction
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "errand: %v\n", err)
		if staged != "" {
			fmt.Fprintf(stderr, "errand: workspace changes remain staged at %s\n", staged)
		}
		return client.ExitTransaction
	}
	if !*jsonOutput {
		printTransferSummary(stderr, "fetch", action, "from "+cmpOr(label, peerURL)+"/"+jobID, stats)
		if !*apply {
			fmt.Fprintln(out, staged)
		}
		if action == "staged" {
			fmt.Fprintln(stderr, "errand: changes staged locally; repeat fetch with the same options and --apply to apply")
		}
	}
	return 0
}
