package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/lydakis/errand/internal/proto"
)

func psApplyText(row psRow) string {
	if !applyNeedsAttention(row.AutomaticApply) {
		return ""
	}
	if row.applyNote != "" {
		return row.applyNote
	}
	state := "needs recovery"
	if row.AutomaticApply.State == "failed" {
		state = "failed"
	}
	return state + "; " + applyRecoveryHint(row.Peer+"/"+row.ID, row.AutomaticApply)
}

func writePsTable(w io.Writer, rows []psRow) {
	tw := tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
	showWorkdir := false
	showApply := false
	for _, row := range rows {
		showApply = showApply || applyNeedsAttention(row.AutomaticApply)
		if psWorkdir(row.JobListEntry) != "" {
			showWorkdir = true
		}
	}
	headerEnd := "COMMAND"
	if showApply {
		headerEnd += "\tAPPLY"
	}
	if showWorkdir {
		fmt.Fprintln(tw, "PEER\tPROJECT\tJOB\tSTATE\tEXIT\tADMITTED\tSTARTED\tDURATION\tSOURCE\tWORKDIR\t"+headerEnd)
	} else {
		fmt.Fprintln(tw, "PEER\tPROJECT\tJOB\tSTATE\tEXIT\tADMITTED\tSTARTED\tDURATION\tSOURCE\t"+headerEnd)
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
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
				row.Peer, project, row.ID, row.State, exit, admitted, started, duration, source, workdir, cmd)
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
				row.Peer, project, row.ID, row.State, exit, admitted, started, duration, source, cmd)
		}
		if showApply {
			fmt.Fprint(tw, "\t"+terminalSafeField(cmpOr(psApplyText(row), "-")))
		}
		fmt.Fprintln(tw)
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
