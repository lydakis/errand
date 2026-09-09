package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

func writePeers(w io.Writer, rows []peerRow) {
	var values [][]string
	for _, row := range rows {
		isDefault := ""
		if row.Default {
			isDefault = "yes"
		}
		var slots, queue, staging, system, capabilities, runnerVersion string
		if info := row.Info; info != nil {
			runnerVersion = info.Version
			slots = fmt.Sprintf("%d/%d", info.StartingJobs+info.RunningJobs, info.MaxJobs)
			queue = fmt.Sprintf("%d/%d", info.QueuedJobs, info.MaxQueued)
			staging = fmt.Sprint(info.StagingJobs)
			system = strings.Trim(info.Facts.OS+"/"+info.Facts.Arch, "/")
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
		values = append(values, []string{row.Name, isDefault, row.Status, runnerVersion, slots, queue, staging, system, capabilities, row.Detail})
	}
	writeNonemptyColumns(w, []string{"NAME", "DEFAULT", "STATUS", "VERSION", "SLOTS", "QUEUE", "STAGING", "SYSTEM", "CAPABILITIES", "DETAIL"}, values)
}

func writeNonemptyColumns(w io.Writer, headers []string, rows [][]string) {
	visible := make([]bool, len(headers))
	for _, row := range rows {
		for i, value := range row {
			visible[i] = visible[i] || value != ""
		}
	}
	tw := tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
	for _, row := range append([][]string{headers}, rows...) {
		var cells []string
		for i, value := range row {
			if visible[i] {
				cells = append(cells, terminalSafeField(value))
			}
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
}
