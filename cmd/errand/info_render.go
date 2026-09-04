package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/lydakis/errand/internal/proto"
)

func writeInfo(w io.Writer, results []peerQueryResult[proto.Info]) {
	tw := tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "PEER\tSTATUS\tSLOTS\tQUEUE\tSTAGING\tSYSTEM\tCPU\tKVM\tVERSION\tTOOLS")
	for _, result := range results {
		info := result.value
		status := "ready"
		if info.Busy {
			status = "busy"
		}
		tools := make([]string, 0, len(info.Facts.Tools))
		for name := range info.Facts.Tools {
			tools = append(tools, name)
		}
		sort.Strings(tools)
		toolList := strings.Join(tools, ",")
		if toolList == "" {
			toolList = "-"
		}
		kvm := "no"
		if info.Facts.KVM {
			kvm = "yes"
		}
		system := terminalSafeField(info.Facts.OS + "/" + info.Facts.Arch)
		fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%d/%d\t%d\t%s\t%d\t%s\t%s\t%s\n",
			terminalSafeField(result.target.name), status,
			info.StartingJobs+info.RunningJobs, info.MaxJobs,
			info.QueuedJobs, info.MaxQueued, info.StagingJobs,
			system, info.Facts.NumCPU, kvm,
			terminalSafeField(info.Version), terminalSafeField(toolList))
	}
	_ = tw.Flush()
}
