package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/client"
)

type transferReport struct {
	Status   string               `json:"status"`
	Transfer client.TransferStats `json:"transfer"`
	Error    string               `json:"error,omitempty"`
}

func newTransferReport(action string, stats client.TransferStats, err error) transferReport {
	report := transferReport{Status: action, Transfer: stats}
	if err == nil {
		return report
	}
	report.Error = err.Error()
	report.Status = "failed"
	var conflict *changes.MergeConflictError
	if errors.As(err, &conflict) {
		report.Status = "conflicted"
	}
	return report
}

func printTransferSummary(w io.Writer, command, action, target string, stats client.TransferStats) {
	fmt.Fprintf(w, "errand: %s: %s %d changed %s %s; %s transferred in %.2fs\n",
		command, action, stats.ChangedPaths, plural(stats.ChangedPaths, "path"), terminalSafeField(target),
		formatByteSize(stats.TransferredBytes), float64(stats.ElapsedMillis)/1000)
	if action == "applied" {
		fmt.Fprintln(w, "errand: file changes applied; application readiness not checked")
	}
}

func plural(count int, noun string) string {
	if count == 1 {
		return noun
	}
	return noun + "s"
}
