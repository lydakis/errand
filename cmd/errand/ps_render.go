package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/lydakis/errand/internal/proto"
)

func writePs(w io.Writer, rows []psRow) {
	width := terminalColumns(w)
	if width <= 0 {
		writePsTable(w, rows)
		return
	}
	var table bytes.Buffer
	writePsTable(&table, rows)
	if maxLineWidth(table.String()) > width {
		writePsCards(w, rows, width)
		return
	}
	_, _ = io.Copy(w, &table)
}

func maxLineWidth(value string) int {
	maxWidth := 0
	for _, line := range strings.Split(value, "\n") {
		if width := terminalCellWidth(line); width > maxWidth {
			maxWidth = width
		}
	}
	return maxWidth
}

func terminalColumns(w io.Writer) int {
	if configured, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && configured > 0 {
		return configured
	}
	file, ok := w.(*os.File)
	if !ok {
		return 0
	}
	return fileTerminalColumns(file.Fd())
}

func writePsCards(w io.Writer, rows []psRow, width int) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No jobs.")
		return
	}
	for i, row := range rows {
		if i != 0 {
			fmt.Fprintln(w)
		}
		header := []string{terminalSafeField(row.Peer) + "/" + row.ID, row.State}
		if row.Project != "" {
			header = append(header, terminalSafeField(row.Project))
		}
		if exit := psExit(row.JobListEntry); exit != "-" {
			header = append(header, "exit="+exit)
		}
		if row.StartedAt != nil {
			header = append(header, shortDuration(time.Duration(row.DurationMS)*time.Millisecond))
		}
		writeWrappedLine(w, "", strings.Join(header, "  "), width)

		metadata := []string{"admitted " + formatLocalTime(row.AdmittedAt)}
		if row.StartedAt != nil {
			metadata = append(metadata, "started "+formatLocalTime(*row.StartedAt))
		}
		if source := terminalSafeField(jobSource(row.JobListEntry)); source != "-" {
			metadata = append(metadata, "source "+source)
		}
		if workdir := psWorkdir(row.JobListEntry); workdir != "" {
			metadata = append(metadata, "workdir "+terminalSafeField(workdir))
		}
		writeWrappedLine(w, "  ", strings.Join(metadata, "  "), width)
		if row.Command != "" {
			writeWrappedLine(w, "  ", "command "+row.Command, width)
		}
	}
}

func writeWrappedLine(w io.Writer, indent, value string, width int) {
	if terminalCellWidth(indent) >= width {
		indent = ""
	}
	available := width - terminalCellWidth(indent)
	if available < 1 {
		available = 1
	}
	for _, line := range wrapText(value, available) {
		fmt.Fprintln(w, indent+line)
	}
}

func wrapText(value string, width int) []string {
	value = strings.TrimSpace(value)
	if value == "" || width <= 0 {
		return []string{value}
	}
	var lines []string
	for terminalCellWidth(value) > width {
		runes := []rune(value)
		fit := 0
		cells := 0
		lastSpace := -1
		for i, r := range runes {
			runeWidth := terminalRuneWidth(r)
			if cells+runeWidth > width {
				break
			}
			cells += runeWidth
			fit = i + 1
			if unicode.IsSpace(r) {
				lastSpace = i
			}
		}
		if fit == 0 {
			fit = 1
		}
		cut := fit
		if lastSpace > 0 {
			cut = lastSpace
		}
		lines = append(lines, strings.TrimSpace(string(runes[:cut])))
		value = strings.TrimSpace(string(runes[cut:]))
	}
	return append(lines, value)
}

func terminalCellWidth(value string) int {
	width := 0
	for _, r := range value {
		width += terminalRuneWidth(r)
	}
	return width
}

// terminalRuneWidth is deliberately conservative. ASCII occupies one cell,
// combining and joining runes occupy none, and other Unicode runes reserve two
// cells so output never exceeds the terminal under ambiguous width rules.
func terminalRuneWidth(r rune) int {
	switch {
	case unicode.IsControl(r), unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), r == '\u200d':
		return 0
	case r <= unicode.MaxASCII:
		return 1
	default:
		return 2
	}
}

func writePsTable(w io.Writer, rows []psRow) {
	tw := tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
	showWorkdir := false
	for _, row := range rows {
		if psWorkdir(row.JobListEntry) != "" {
			showWorkdir = true
			break
		}
	}
	if showWorkdir {
		fmt.Fprintln(tw, "PEER\tPROJECT\tJOB\tSTATE\tEXIT\tADMITTED\tSTARTED\tDURATION\tSOURCE\tWORKDIR\tCOMMAND")
	} else {
		fmt.Fprintln(tw, "PEER\tPROJECT\tJOB\tSTATE\tEXIT\tADMITTED\tSTARTED\tDURATION\tSOURCE\tCOMMAND")
	}
	for _, row := range rows {
		exit := psExit(row.JobListEntry)
		admitted := formatLocalTime(row.AdmittedAt)
		started := "-"
		if row.StartedAt != nil {
			started = formatLocalTime(*row.StartedAt)
		}
		duration := "-"
		if row.StartedAt != nil {
			duration = shortDuration(time.Duration(row.DurationMS) * time.Millisecond)
		}
		source := terminalSafeField(jobSource(row.JobListEntry))
		project := row.Project
		if project == "" {
			project = "-"
		}
		project = terminalSafeField(project)
		workdir := psWorkdir(row.JobListEntry)
		if workdir == "" {
			workdir = "-"
		}
		workdir = terminalSafeField(workdir)
		commandRunes := []rune(row.Command)
		cmd := row.Command
		if len(commandRunes) > 60 {
			cmd = string(commandRunes[:59]) + "…"
		}
		if showWorkdir {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				row.Peer, project, row.ID, row.State, exit, admitted, started, duration, source, workdir, cmd)
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				row.Peer, project, row.ID, row.State, exit, admitted, started, duration, source, cmd)
		}
	}
	_ = tw.Flush()
}

func psExit(entry proto.JobListEntry) string {
	switch {
	case entry.ExitCode != nil:
		return fmt.Sprintf("%d", *entry.ExitCode)
	case entry.Signal != "":
		return entry.Signal
	default:
		return "-"
	}
}

func psWorkdir(entry proto.JobListEntry) string {
	if entry.Workdir == "." {
		return ""
	}
	return entry.Workdir
}

func formatLocalTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Local().Format("2006-01-02 15:04:05")
}

func terminalSafeField(value string) string {
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return strconv.QuoteToGraphic(value)
	}
	return value
}

func jobSource(entry proto.JobListEntry) string {
	if entry.GitCommit != "" {
		source := truncateHash(entry.GitCommit)
		if entry.GitDirty {
			source += "+dirty"
		}
		return source
	}
	if entry.ManifestRoot != "" {
		return "snapshot:" + truncateHash(entry.ManifestRoot)
	}
	return "-"
}

func truncateHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Truncate(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	default:
		return fmt.Sprintf("%dd%dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
	}
}
