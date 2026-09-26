package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

// peerTools lists a runner's tools, with kvm when it has it.
func peerTools(info *proto.Info) []string {
	names := map[string]bool{}
	for name := range info.Facts.Tools {
		names[name] = true
	}
	var tools []string
	for name := range names {
		tools = append(tools, terminalSafeField(name))
	}
	sort.Strings(tools)
	return tools
}

func peerSystem(info *proto.Info) string {
	var parts []string
	if system := strings.Trim(info.Facts.OS+"/"+info.Facts.Arch, "/"); system != "" {
		parts = append(parts, terminalSafeField(system))
	}
	if info.Facts.NumCPU > 0 {
		parts = append(parts, fmt.Sprintf("%d cpu", info.Facts.NumCPU))
	}
	if info.Facts.KVM {
		parts = append(parts, "kvm")
	}
	return strings.Join(parts, " · ")
}

// peerStatusCell is a runner's state as a colored word.
func peerStatusCell(row peerRow) termui.Cell {
	switch {
	case row.Info != nil && row.Status == "busy":
		return termui.C("◌ full", termui.Yellow)
	case row.Info != nil:
		return termui.C("● ready", termui.Green)
	case row.Status == "misconfigured":
		return termui.C("! misconfigured", termui.Yellow)
	case row.Status == string(client.ProbeForbidden):
		return termui.C("○ refused you", termui.Red)
	default:
		return termui.C("○ unreachable", termui.Red)
	}
}

// peerProblem is why a runner can't be used, in plain words.
func peerProblem(row peerRow) string {
	detail := row.Detail
	switch {
	case strings.Contains(detail, "no such host"):
		return "no such host"
	case strings.Contains(detail, "timeout") || strings.Contains(detail, "timed out") || strings.Contains(detail, "deadline exceeded"):
		return "timed out"
	case strings.Contains(detail, "connection refused"):
		return "connection refused"
	}
	if i := strings.LastIndex(detail, ": "); i > 0 && len(detail)-i < 60 {
		return detail[i+2:]
	}
	return detail
}

// writePeers renders the runner list. Version and staging are left out while
// every runner agrees with this CLI and nothing is staging.
func writePeers(s *termui.Stream, rows []peerRow, verbose bool) {
	if !s.Interactive() {
		writePeersTable(s.Writer(), rows)
		return
	}
	if verbose {
		writePeerBlocks(s, rows)
		return
	}
	t := s.Table("  PEER", "STATUS", "SLOTS", "QUEUE", "SYSTEM", "TOOLS")
	var mismatched []peerRow
	anyDefault := false
	for _, row := range rows {
		mark := "  "
		if row.Default {
			mark = "* "
			anyDefault = true
		}
		name := termui.C(mark+terminalSafeField(row.Name), termui.Bold)
		if row.Info == nil {
			problem := termui.C(terminalSafeField(peerProblem(row)), termui.Dim)
			t.Row(name, peerStatusCell(row), termui.C("-", termui.Dim), termui.C("-", termui.Dim), problem)
			continue
		}
		info := row.Info
		if info.Version != version {
			mismatched = append(mismatched, row)
		}
		t.Row(name, peerStatusCell(row),
			termui.C(fmt.Sprintf("%d/%d", info.StartingJobs+info.RunningJobs, info.MaxJobs)),
			termui.C(fmt.Sprintf("%d/%d", info.QueuedJobs, info.MaxQueued)),
			termui.C(peerSystem(info)),
			termui.C(strings.Join(peerTools(info), " ")))
	}
	lines := t.Lines()
	for i, line := range lines {
		// The default marker is part of the name column; color just the star.
		if i > 0 && strings.HasPrefix(termui.StripANSI(line), "* ") {
			line = strings.Replace(line, "* ", s.Paint("*", termui.Green)+" ", 1)
		}
		s.Print(line)
	}
	var notes []string
	if anyDefault {
		notes = append(notes, "* default")
	}
	if len(mismatched) == 0 && len(rows) > 0 {
		allReady := true
		for _, row := range rows {
			allReady = allReady && row.Info != nil
		}
		if allReady {
			what := "all on"
			if len(rows) == 2 {
				what = "both on"
			} else if len(rows) == 1 {
				what = "on"
			}
			notes = append(notes, what+" errand "+version+", same as this CLI")
		}
	}
	if len(notes) > 0 {
		s.Print(s.D(strings.Join(notes, " · ")))
	}
	for _, row := range mismatched {
		s.Print("")
		s.Warnf("%s runs errand %s; this CLI is %s.", row.Name, terminalSafeField(row.Info.Version), version)
		s.Next("errand setup", "on "+row.Name+" when it's idle")
	}
}

// writePeerBlocks is peers -v: one block per runner.
func writePeerBlocks(s *termui.Stream, rows []peerRow) {
	for i, row := range rows {
		if i > 0 {
			s.Print("")
		}
		mark := "  "
		if row.Default {
			mark = s.Paint("*", termui.Green) + " "
		}
		status := peerStatusCell(row)
		head := mark + s.B(terminalSafeField(row.Name)) + "  " + s.Paint(status.Text, status.Attrs...)
		if row.Info != nil {
			head += " " + s.D("· errand "+terminalSafeField(row.Info.Version))
		}
		s.Print(head)
		field := func(label, value string) { s.Print("    " + s.D(padRight(label, 8)) + " " + value) }
		transport := ""
		if row.Info != nil {
			switch {
			case row.Info.LocalOnly:
				transport = " · local only"
			case row.Info.SSHDisabled:
				transport = " · tailnet only"
			case strings.HasPrefix(row.Target, "ssh://"):
				transport = " · SSH"
			default:
				transport = " · tailnet, SSH enabled"
			}
		}
		field("url", terminalSafeField(row.Target)+s.D(transport))
		if row.Info == nil {
			field("problem", terminalSafeField(row.Detail))
			continue
		}
		info := row.Info
		field("system", peerSystem(info))
		field("slots", fmt.Sprintf("%d of %d running · %d of %d queued · %d staging", info.StartingJobs+info.RunningJobs, info.MaxJobs, info.QueuedJobs, info.MaxQueued, info.StagingJobs))
		field("tools", strings.Join(peerTools(info), " "))
	}
}

// writePeersTable is the stable piped form.
func writePeersTable(w io.Writer, rows []peerRow) {
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
			tools := peerTools(info)
			if info.Facts.KVM {
				tools = append(tools, "kvm")
				sort.Strings(tools)
			}
			capabilities = strings.Join(tools, ",")
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
