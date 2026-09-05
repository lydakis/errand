package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

func writePeers(w io.Writer, rows []peerRow) {
	hasDetails := false
	for _, row := range rows {
		hasDetails = hasDetails || row.Detail != ""
	}
	tw := tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
	fmt.Fprint(tw, "NAME\tDEFAULT\tSTATUS\tSLOTS\tQUEUE\tSTAGING\tSYSTEM\tCAPABILITIES")
	if hasDetails {
		fmt.Fprint(tw, "\tDETAIL")
	}
	fmt.Fprintln(tw)
	for _, row := range rows {
		isDefault := ""
		if row.Default {
			isDefault = "yes"
		}
		slots, queue, staging, system, capabilities := "-", "-", "-", "-", "-"
		if info := row.Info; info != nil {
			slots = fmt.Sprintf("%d/%d", info.StartingJobs+info.RunningJobs, info.MaxJobs)
			queue = fmt.Sprintf("%d/%d", info.QueuedJobs, info.MaxQueued)
			staging = fmt.Sprint(info.StagingJobs)
			system = info.Facts.OS + "/" + info.Facts.Arch
			names := make(map[string]bool, len(info.Facts.Tools)+1)
			for name := range info.Facts.Tools {
				names[name] = true
			}
			if info.Facts.KVM {
				names["kvm"] = true
			}
			var tools []string
			for name := range names {
				tools = append(tools, name)
			}
			sort.Strings(tools)
			if len(tools) != 0 {
				capabilities = strings.Join(tools, ",")
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
			terminalSafeField(row.Name), isDefault, terminalSafeField(row.Status),
			slots, queue, staging, terminalSafeField(system), terminalSafeField(capabilities))
		if hasDetails {
			fmt.Fprintf(tw, "\t%s", terminalSafeField(row.Detail))
		}
		fmt.Fprintln(tw)
	}
	_ = tw.Flush()
}
