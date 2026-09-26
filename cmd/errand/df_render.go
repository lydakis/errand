package main

import (
	"context"
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

type dfRow struct {
	Details     *proto.StorageDetails  `json:"details,omitempty"`
	Workspaces  *proto.StorageCategory `json:"workspaces,omitempty"`
	hasRunner   bool
	NamedCaches *proto.NamedCacheStats `json:"named_caches,omitempty"`
	Location    string                 `json:"location"`
	Cache       *proto.CacheStats      `json:"cache,omitempty"`
	Jobs        proto.StorageCategory  `json:"jobs"`
	Changes     *proto.StorageCategory `json:"changes,omitempty"`
	TotalBytes  int64                  `json:"total_bytes"`
}

func cmdDf(args []string) int {
	return cmdDfTo(args, os.Stdout, os.Stderr)
}

func cmdDfTo(args []string, stdout, stderr io.Writer) int {
	con := newConsole(stdout, stderr)
	e := con.Err
	fs := flag.NewFlagSet("errand df", flag.ContinueOnError)
	on := fs.String("on", "", "restrict to one peer name")
	rawURL := fs.String("url", "", "restrict to one peer base URL")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	var output outputFlags
	output.bind(fs, "show individual workspace, named-cache, and job storage")
	if ok, code := parseFlags(fs, args, "df", stdout, e); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageError(e, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	targets, warnings, err := peerTargets(*rawURL, *on)
	if err != nil {
		return failWith(e, listErrorCode(err), err, errorScope{peer: *on})
	}
	failed := len(warnings) != 0
	for _, warning := range warnings {
		e.Warnf("%v", warning)
	}
	// This machine's fetched changes belong in the answer unless the caller
	// narrowed it to one remote runner.
	measureLocal := (*on == "" && *rawURL == "") || *on == "local"
	query := client.StorageStats
	if output.verbose {
		query = client.StorageStatsDetailed
	}
	arrivals := make(chan peerQueryResult[proto.StorageStats], len(targets))
	for _, target := range targets {
		go func() {
			value, err := query(target.url)
			arrivals <- peerQueryResult[proto.StorageStats]{target: target, value: value, err: err}
		}()
	}
	type localResult struct {
		stats proto.ChangeStorageStats
		err   error
	}
	localDone := make(chan localResult, 1)
	if measureLocal {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			stats, err := client.ChangeStorageStats(ctx)
			localDone <- localResult{stats, err}
		}()
	}

	view := newDfView(con, targets, measureLocal, *jsonOutput || output.quiet)
	// A terminal gets each row as its runner answers; scripts and --json get
	// the configured order every time.
	streaming := con.Out.Interactive()
	var remote, localRunner []peerQueryResult[proto.StorageStats]
	for range targets {
		result := <-arrivals
		if result.err != nil {
			msg, _ := describeError(result.err, errorScope{peer: result.target.name})
			view.warn(result.target.name + ": " + msg)
			view.progress(result.target.name)
			failed = true
			continue
		}
		if strings.HasPrefix(result.target.url, "unix://") {
			localRunner = append(localRunner, result)
			continue
		}
		remote = append(remote, result)
		if streaming {
			for _, row := range storageRows([]peerQueryResult[proto.StorageStats]{result}, nil) {
				view.row(row)
			}
		}
	}
	order := map[peerTarget]int{}
	for i, target := range targets {
		order[target] = i
	}
	sort.SliceStable(remote, func(i, j int) bool { return order[remote[i].target] < order[remote[j].target] })
	sort.SliceStable(localRunner, func(i, j int) bool { return order[localRunner[i].target] < order[localRunner[j].target] })
	if !streaming {
		for _, row := range storageRows(remote, nil) {
			view.row(row)
		}
	}
	var localChanges *proto.ChangeStorageStats
	if measureLocal {
		view.measuringLocal()
		local := <-localDone
		if local.err != nil {
			view.warn("couldn't measure this machine's fetched changes: " + local.err.Error())
			failed = true
		} else {
			localChanges = &local.stats
		}
	}
	if len(localRunner) != 0 || localChanges != nil {
		for _, row := range storageRows(localRunner, localChanges) {
			view.row(row)
		}
	}
	view.done()
	rows := storageRows(append(remote, localRunner...), localChanges)

	switch {
	case *jsonOutput:
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(rows); err != nil {
			e.Errorf("encoding storage usage: %v", err)
			return 1
		}
	case output.quiet:
		var total int64
		for _, row := range rows {
			total += row.TotalBytes
		}
		fmt.Fprintln(stdout, termui.Bytes(total))
	default:
		view.footer(rows)
		if output.verbose {
			writeDfDetails(con.Out, rows)
		}
	}
	if failed {
		return 1
	}
	return 0
}

// dfView prints storage rows as runners answer. Columns have fixed widths
// so each row can go out before the others arrive.
type dfView struct {
	o, e      *termui.Stream
	quiet     bool
	whereW    int
	spin      *termui.Spinner
	waiting   []string
	headerOut bool
}

func newDfView(con *termui.Console, targets []peerTarget, local, quiet bool) *dfView {
	v := &dfView{o: con.Out, e: con.Err, quiet: quiet, whereW: len("WHERE")}
	for _, t := range targets {
		v.whereW = max(v.whereW, termui.CellWidth(t.name))
		v.waiting = append(v.waiting, t.name)
	}
	if local {
		v.whereW = max(v.whereW, len("local"))
	}
	if !quiet && len(v.waiting) > 0 {
		v.spin = v.e.Spin("Asking " + joinWords(v.waiting) + "…")
	}
	return v
}

// SNAPSHOT CACHE, JOBS, WORKSPACES, CHANGES; WHERE fits the longest name.
var dfWidths = []int{19, 10, 12, 10}

func (v *dfView) line(cells []termui.Cell) string {
	widths := append([]int{v.whereW + 2}, dfWidths...)
	var b strings.Builder
	for i, c := range cells {
		b.WriteString(v.o.Paint(c.Text, c.Attrs...))
		if i < len(cells)-1 {
			b.WriteString(strings.Repeat(" ", max(2, widths[i]-termui.CellWidth(c.Text))))
		}
	}
	return strings.TrimRight(b.String(), " ")
}

func (v *dfView) header() {
	if v.headerOut || v.quiet {
		return
	}
	v.headerOut = true
	d := []termui.Attr{termui.Dim}
	v.o.Print(v.line([]termui.Cell{{Text: "WHERE", Attrs: d}, {Text: "SNAPSHOT CACHE", Attrs: d}, {Text: "JOBS", Attrs: d}, {Text: "WORKSPACES", Attrs: d}, {Text: "CHANGES", Attrs: d}, {Text: "TOTAL", Attrs: d}}))
}

func (v *dfView) row(row dfRow) {
	if v.quiet {
		return
	}
	v.header()
	dash := termui.C("-", termui.Dim)
	cache, jobs, workspaces, changes := dash, dash, dash, dash
	if row.Cache != nil {
		text := termui.Bytes(row.Cache.Bytes)
		if row.Cache.MaxBytes > 0 {
			text += " of " + termui.Bytes(row.Cache.MaxBytes)
		}
		cache = termui.C(text)
	}
	if row.hasRunner {
		jobs = termui.C(termui.Bytes(row.Jobs.Bytes))
	}
	if row.Workspaces != nil {
		workspaces = termui.C(termui.Bytes(row.Workspaces.Bytes))
	}
	if row.Changes != nil {
		changes = termui.C(termui.Bytes(row.Changes.Bytes))
	}
	total := v.o.B(termui.Bytes(row.TotalBytes))
	if row.NamedCaches != nil && row.NamedCaches.Bytes > 0 {
		total += " " + v.o.D("incl. "+termui.Bytes(row.NamedCaches.Bytes)+" named caches")
	}
	if row.NamedCaches != nil && row.NamedCaches.Unmeasured > 0 {
		total += " " + v.o.Paint("+ "+termui.Things(row.NamedCaches.Unmeasured, "unmeasured cache", "unmeasured caches"), termui.Yellow)
	}
	v.o.Print(v.line([]termui.Cell{termui.C(terminalSafeField(row.Location), termui.Bold), cache, jobs, workspaces, changes, {Text: total}}))
	v.progress(row.Location)
}

func (v *dfView) progress(done string) {
	for i, name := range v.waiting {
		if name == done {
			v.waiting = append(v.waiting[:i], v.waiting[i+1:]...)
			break
		}
	}
	if v.spin != nil && len(v.waiting) > 0 {
		v.spin.Set("Asking " + joinWords(v.waiting) + "…")
	}
}

func (v *dfView) measuringLocal() {
	if v.quiet {
		return
	}
	text := v.e.B("local") + " " + v.e.D("measuring fetched changes…")
	if v.spin == nil || !v.spin.Active() {
		v.spin = v.e.Spin(text)
		return
	}
	v.spin.Set(text)
}

func (v *dfView) warn(msg string) { v.e.Warnf("%s", msg) }

func (v *dfView) done() {
	if v.spin != nil {
		v.spin.Stop()
	}
}

// footer notes what the totals leave out and the biggest thing to collect.
func (v *dfView) footer(rows []dfRow) {
	if v.quiet || len(rows) == 0 {
		return
	}
	var notes []string
	for _, row := range rows {
		if row.NamedCaches != nil && row.NamedCaches.Unmeasured > 0 {
			notes = append(notes, fmt.Sprintf("%s's %s haven't been measured yet, so its total is a floor.", row.Location, termui.Things(row.NamedCaches.Unmeasured, "named cache", "named caches")))
		}
	}
	type candidate struct {
		bytes   int64
		command string
		why     string
	}
	var best candidate
	for _, row := range rows {
		if row.Location == "local" && row.Changes != nil && row.Changes.Bytes > best.bytes {
			best = candidate{row.Changes.Bytes, "errand gc changes --older-than 30d", "fetched changes here hold " + termui.Bytes(row.Changes.Bytes)}
		}
		if row.hasRunner && row.Location != "local" && row.Jobs.Bytes > best.bytes {
			best = candidate{row.Jobs.Bytes, "errand gc jobs " + runnerFlag(row.Location) + " --older-than 7d", "jobs on " + row.Location + " hold " + termui.Bytes(row.Jobs.Bytes)}
		}
	}
	const worthIt = 1 << 30
	if len(notes) == 0 && best.bytes < worthIt {
		return
	}
	v.o.Print("")
	for _, note := range notes {
		v.o.Warnf("%s", note)
	}
	if best.bytes >= worthIt {
		v.o.Next(best.command, best.why)
	}
}

func formatByteSize(bytes int64) string { return termui.Bytes(bytes) }
