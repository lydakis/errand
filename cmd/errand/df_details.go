package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

func writeDfDetails(w io.Writer, rows []dfRow) {
	fmt.Fprintln(w, "\nSizes are logical file bytes; filesystem clones may share physical blocks.")
	for _, row := range rows {
		fmt.Fprintf(w, "\n%s:\n", terminalSafeField(row.Location))
		if row.Cache != nil {
			fmt.Fprintf(w, "  Snapshot cache: %d blobs, %s\n", row.Cache.Blobs, formatByteSize(row.Cache.Bytes))
		}
		if row.Changes != nil {
			fmt.Fprintf(w, "  Fetched changes: %d entries, %s\n", row.Changes.Items, formatByteSize(row.Changes.Bytes))
		}
		if row.Details == nil {
			if row.hasRunner {
				fmt.Fprintln(w, "  Detailed runner storage is unavailable; upgrade the runner.")
			}
			continue
		}
		details := row.Details
		if details.Incomplete {
			fmt.Fprintln(w, "  Details are incomplete: a local runner needs an upgrade; its totals are included above.")
		}
		fmt.Fprintf(w, "\n  Workspaces (%d):\n", len(details.Workspaces))
		tw := tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tID\tJOBS\tWORKING FILES\tCREATION BASE\tMETADATA\tTOTAL")
		for _, item := range details.Workspaces {
			job := strings.Join(item.JobIDs, ",")
			if job == "" {
				job = "-"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", terminalSafeField(item.Name), terminalSafeField(item.ID), terminalSafeField(job), formatByteSize(item.WorkingBytes), formatByteSize(item.BaseBytes), formatByteSize(item.MetadataBytes), formatByteSize(item.Bytes))
		}
		_ = tw.Flush()
		fmt.Fprintf(w, "\n  Named caches (%d; sizes from last job release):\n", len(details.NamedCaches))
		tw = tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tPROJECT ID\tWORKSPACE ID\tJOBS\tSIZE")
		for _, item := range details.NamedCaches {
			job := item.JobID
			workspace := item.WorkspaceID
			if workspace == "" {
				workspace = "-"
			}
			if len(item.JobIDs) > 0 {
				job = strings.Join(item.JobIDs, ",")
			}
			if job == "" {
				job = "-"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", terminalSafeField(item.Name), terminalSafeField(item.ProjectID), terminalSafeField(workspace), terminalSafeField(job), formatByteSize(item.Bytes))
		}
		_ = tw.Flush()
		fmt.Fprintf(w, "\n  Job storage (%d):\n", len(details.Jobs))
		tw = tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "  JOB\tWORKSPACE ID\tSIZE\tCLEANUP")
		for _, item := range details.Jobs {
			workspace := item.WorkspaceID
			if workspace == "" {
				workspace = "-"
			}
			cleanup := "-"
			if item.CleanupPending {
				cleanup = "pending"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", terminalSafeField(item.ID), terminalSafeField(workspace), formatByteSize(item.Bytes), cleanup)
		}
		_ = tw.Flush()
	}
}
