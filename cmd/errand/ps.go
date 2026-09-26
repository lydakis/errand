package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

func cmdPs(args []string) int {
	return cmdPsTo(args, os.Stdout, os.Stderr)
}

func cmdPsTo(args []string, stdout, stderr io.Writer) int {
	con := newConsole(stdout, stderr)
	e := con.Err
	fs := flag.NewFlagSet("errand ps", flag.ContinueOnError)
	on := fs.String("on", "", "restrict to one peer name")
	rawURL := fs.String("url", "", "restrict to one peer base URL")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	workspace := fs.String("workspace", "", "restrict to jobs in a persistent workspace name or ID")
	all := false
	last := 0
	fs.BoolVar(&all, "all", false, "include terminal jobs")
	fs.BoolVar(&all, "a", false, "include terminal jobs")
	fs.IntVar(&last, "last", 0, "show only the latest N jobs across all states")
	fs.IntVar(&last, "n", 0, "show only the latest N jobs across all states")
	var output outputFlags
	output.bind(fs, "")
	if ok, code := parseFlags(fs, args, "ps", stdout, e); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageError(e, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if last < 0 {
		return usageError(e, "--last must be positive")
	}
	workspaceSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "workspace" {
			workspaceSet = true
		}
	})
	if workspaceSet && !proto.ValidULID(*workspace) {
		if err := proto.ValidateWorkspaceName(*workspace); err != nil {
			return usageError(e, "%v", err)
		}
	}
	if last > proto.MaxJobListEntries {
		return usageError(e, "--last can't be more than %d", proto.MaxJobListEntries)
	}
	local, inspectErr := client.InspectAutomaticApplies()
	byPeer := make(map[string][]client.AutomaticApplyInspection)
	for _, record := range local {
		byPeer[record.PeerURL] = append(byPeer[record.PeerURL], record)
	}
	targets, warnings, err := peerTargets(*rawURL, *on)
	if err != nil {
		return failWith(e, listErrorCode(err), err, errorScope{peer: *on})
	}
	read := fleetRead[[]psRow]{targets: targets, failed: len(warnings) != 0 || inspectErr != nil}
	for _, warning := range warnings {
		e.Warnf("%v", warning)
	}
	if inspectErr != nil {
		e.Warnf("couldn't read local apply state: %v", inspectErr)
	}
	if len(targets) == 0 {
		return failWith(e, 1, errNoUsablePeers, errorScope{})
	}
	activeOnly := !all && last == 0
	var spin *termui.Spinner
	if !*jsonOutput && !output.quiet {
		spin = e.Spin("Asking " + joinWords(targetNames(targets)) + "…")
	}
	rows := make([]psRow, 0)
	var unreachable []string
	for _, result := range queryPeerTargets(targets, func(url string) ([]psRow, error) {
		return psPeerRows(url, *workspace, activeOnly, byPeer[url])
	}) {
		if result.err != nil {
			unreachable = append(unreachable, result.target.name)
			msg, _ := describeError(result.err, errorScope{peer: result.target.name})
			if spin != nil {
				spin.Stop()
			}
			e.Warnf("%s: %s", result.target.name, msg)
			read.failed = true
		} else {
			read.results = append(read.results, result)
		}
		for _, row := range result.value {
			row.Peer = result.target.name
			rows = append(rows, row)
		}
	}
	if spin != nil {
		spin.Stop()
	}

	sort.SliceStable(rows, func(i, k int) bool {
		return rows[i].ID > rows[k].ID
	})
	if activeOnly {
		active := rows[:0]
		for _, row := range rows {
			if activeJobState(row.State) || applyNeedsAttention(row.AutomaticApply) {
				active = append(active, row)
			}
		}
		rows = active
	}
	if last > 0 && len(rows) > last {
		rows = rows[:last]
	}
	switch {
	case *jsonOutput:
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(rows); err != nil {
			e.Errorf("encoding job listing: %v", err)
			return 1
		}
	case output.quiet:
		for _, row := range rows {
			fmt.Fprintln(stdout, row.Peer+"/"+row.ID)
		}
	case len(rows) != 0:
		writePs(con.Out, rows, output.verbose)
	case len(read.results) != 0:
		fmt.Fprintln(stdout, psEmptyMessage(read.targets, activeOnly, read.failed))
		if activeOnly && con.Out.Interactive() {
			con.Out.Next("errand ps -a", "shows the latest finished jobs too")
		}
	}
	return read.exitCode()
}

func targetNames(targets []peerTarget) []string {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.name
	}
	return names
}

func psEmptyMessage(targets []peerTarget, activeOnly, partial bool) string {
	kind := "jobs"
	if activeOnly {
		kind = "active jobs"
	}
	if partial {
		return fmt.Sprintf("No %s on the runners that answered.", kind)
	}
	return fmt.Sprintf("No %s on %s.", kind, joinWordsOr(targetNames(targets)))
}

// joinWordsOr renders "a", "a or b", "a, b or c".
func joinWordsOr(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

func activeJobState(state string) bool {
	return state == proto.StateStaging || state == proto.StateQueued || state == proto.StateRunning
}

// writePs renders the listing: one row per job on a terminal, today's
// detailed cards with -v, and the stable full table when piped.
func writePs(s *termui.Stream, rows []psRow, verbose bool) {
	switch {
	case !s.Interactive():
		writePsTable(s.Writer(), rows)
	case verbose:
		writePsCards(s, rows, time.Now())
	default:
		writePsRows(s, rows, time.Now())
	}
}

// psState is a job's state as a word with a glyph and color.
func psState(row psRow) termui.Cell {
	switch row.State {
	case proto.StateRunning:
		return termui.C("● running", termui.Green)
	case proto.StateQueued:
		return termui.C("◌ queued", termui.Yellow)
	case proto.StateStaging:
		return termui.C("◌ staging", termui.Yellow)
	case proto.StateAmbiguous:
		return termui.C("! unknown", termui.Yellow)
	case "unknown":
		return termui.C("? unknown", termui.Dim)
	}
	switch {
	case row.ExitCode != nil && *row.ExitCode == 0:
		return termui.C("✓ exited 0", termui.Green)
	case row.ExitCode != nil:
		return termui.C(fmt.Sprintf("✗ exited %d", *row.ExitCode), termui.Red)
	case row.Signal != "":
		return termui.C("✗ killed", termui.Red)
	default:
		return termui.C("✗ didn't start", termui.Red)
	}
}

// psCommand renders the runner's quoted argv the way a person types it.
func psCommand(row psRow) string {
	if argv, ok := termui.ParseQuotedArgv(row.Command); ok {
		return terminalSafeField(termui.ShellQuote(argv))
	}
	return terminalSafeField(row.Command)
}

func writePsRows(s *termui.Stream, rows []psRow, now time.Time) {
	showChanged, showApply := false, false
	for _, row := range rows {
		showChanged = showChanged || row.ChangedPaths > 0
		showApply = showApply || applyNeedsAttention(row.AutomaticApply)
	}
	headers := []string{"JOB", "STATE", "AGE", "TIME", "PROJECT"}
	if showChanged {
		headers = append(headers, "CHANGED")
	}
	if showApply {
		headers = append(headers, "APPLY")
	}
	headers = append(headers, "COMMAND")
	t := s.Table(headers...)
	for _, row := range rows {
		timeCell := termui.C("-", termui.Dim)
		if row.StartedAt != nil {
			timeCell = termui.C(termui.Duration(time.Duration(row.DurationMS) * time.Millisecond))
		}
		project := termui.C(cmpOr(terminalSafeField(row.Project), "-"))
		if row.Project == "" {
			project.Attrs = []termui.Attr{termui.Dim}
		}
		cells := []termui.Cell{
			termui.C(terminalSafeField(row.Peer)+"/"+termui.ShortID(row.ID), termui.Cyan),
			psState(row),
			termui.C(termui.Age(row.AdmittedAt, now)),
			timeCell,
			project,
		}
		if showChanged {
			changed := termui.C("-", termui.Dim)
			if row.ChangedPaths > 0 {
				changed = termui.C(termui.Things(row.ChangedPaths, "file", "files"))
			}
			cells = append(cells, changed)
		}
		if showApply {
			apply := termui.C("-", termui.Dim)
			if applyNeedsAttention(row.AutomaticApply) {
				apply = termui.C(row.AutomaticApply.State, termui.Yellow)
			}
			cells = append(cells, apply)
		}
		cells = append(cells, termui.C(psCommand(row)))
		t.Row(cells...)
	}
	t.Print()
	if showApply {
		for _, row := range rows {
			if text := psApplyText(row); text != "" {
				s.Warnf("%s: %s", row.Peer+"/"+termui.ShortID(row.ID), terminalSafeField(text))
			}
		}
	}
}

func writePsCards(s *termui.Stream, rows []psRow, now time.Time) {
	for i, row := range rows {
		if i != 0 {
			s.Print("")
		}
		state := psState(row)
		head := s.Paint(terminalSafeField(row.Peer)+"/"+row.ID, termui.Bold, termui.Cyan) + "  " + s.Paint(state.Text, state.Attrs...)
		if row.StartedAt != nil {
			head += " " + termui.Duration(time.Duration(row.DurationMS)*time.Millisecond)
		}
		if row.Project != "" {
			head += "  " + s.D("·") + " " + terminalSafeField(row.Project)
		}
		s.Print(head)
		facts := []string{"admitted " + termui.Clock(row.AdmittedAt, now) + " (" + termui.Ago(row.AdmittedAt, now) + ")"}
		if row.StartedAt != nil {
			wait := row.StartedAt.Sub(row.AdmittedAt)
			if wait < time.Second {
				facts = append(facts, "started at once")
			} else {
				facts = append(facts, "started "+termui.Duration(wait)+" later")
			}
		}
		if source := jobSourceText(row.JobListEntry); source != "" {
			facts = append(facts, source)
		}
		facts = append(facts, "workdir "+cmpOr(terminalSafeField(psWorkdir(row.JobListEntry)), "."))
		if row.ChangedPaths > 0 {
			facts = append(facts, termui.Things(row.ChangedPaths, "changed file", "changed files"))
		}
		s.Print("  " + s.D(strings.Join(facts, " · ")))
		if row.Command != "" {
			s.Print("  " + psCommand(row))
		}
		if text := psApplyText(row); text != "" {
			s.Warnf("%s", terminalSafeField(text))
		}
	}
}

// jobSourceText names where a job's files came from: a commit or a snapshot.
func jobSourceText(entry proto.JobListEntry) string {
	switch {
	case entry.GitCommit != "":
		source := "source " + truncateHash(entry.GitCommit)
		if entry.GitDirty {
			source += " +dirty"
		}
		return source
	case entry.ManifestRoot != "":
		return "snapshot " + truncateHash(entry.ManifestRoot)
	default:
		return ""
	}
}

// listErrorCode is 2 for an unknown runner name, 1 for anything else.
func listErrorCode(err error) int {
	if runConfigErrorCode(err) == 2 {
		return 2
	}
	return 1
}
