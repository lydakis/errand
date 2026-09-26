package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

// terminal is a console that behaves like an interactive terminal of the
// given width; Text strips styling for assertions.
func terminal(width int) (*termui.Console, *bytes.Buffer) {
	var out bytes.Buffer
	return termui.New(&out, &out, termui.Options{OutTTY: true, ErrTTY: true, Color: true, Width: width}), &out
}

func TestPsTableShowsProjectsAndTruthfulWorkdirs(t *testing.T) {
	rows := []psRow{
		{Peer: "cabal", JobListEntry: proto.JobListEntry{ID: proto.NewULID(), State: proto.StateRunning, Project: "atlas"}},
		{Peer: "mac-mini", JobListEntry: proto.JobListEntry{ID: proto.NewULID(), State: proto.StateRunning, Project: "errand"}},
	}
	var out bytes.Buffer
	writePsTable(&out, rows)
	if got := out.String(); !strings.Contains(got, "PROJECT") || !strings.Contains(got, "atlas") ||
		!strings.Contains(got, "errand") || strings.Contains(got, "WORKDIR") {
		t.Fatalf("root-level ps table = %q", got)
	}

	rows[0].Workdir = "."
	out.Reset()
	writePsTable(&out, rows)
	if got := out.String(); strings.Contains(got, "WORKDIR") {
		t.Fatalf("explicit root ps table = %q", got)
	}

	rows[0].Workdir = "atlas/docs"
	out.Reset()
	writePsTable(&out, rows)
	if got := out.String(); !strings.Contains(got, "WORKDIR") || !strings.Contains(got, "atlas/docs") {
		t.Fatalf("nested-workdir ps table = %q", got)
	}
}

func TestPsPipedOutputKeepsTheStableTable(t *testing.T) {
	rows := []psRow{{
		Peer: "cabal",
		JobListEntry: proto.JobListEntry{
			ID: proto.NewULID(), State: proto.StateRunning, Project: "atlas",
			Workdir: strings.Repeat("nested/", 50), Command: `"nix" "build"`,
		},
	}}
	var out bytes.Buffer
	writePs(termui.Plain(&out, &out).Out, rows, false)
	if got := out.String(); !strings.HasPrefix(got, "PEER") || strings.Contains(got, "\x1b[") || !strings.Contains(got, rows[0].ID) {
		t.Fatalf("piped ps was not the plain full-id table: %q", got)
	}
}

func TestPsTerminalShowsOneRowPerJob(t *testing.T) {
	now := time.Now()
	started := now.Add(-90 * time.Second)
	zero, three := 0, 3
	rows := []psRow{
		{Peer: "cabal", JobListEntry: proto.JobListEntry{ID: "01M3BF93Y7HS3VNGGZDB4M0GCK", State: proto.StateRunning, Project: "atlas",
			AdmittedAt: started, StartedAt: &started, DurationMS: 90000, Command: `"bash" "harbor-arms/run.sh"`}},
		{Peer: "mini", JobListEntry: proto.JobListEntry{ID: "01M3BFTQ6QD4GRXZKC3F4PTG15", State: proto.StateExited, ExitCode: &zero,
			AdmittedAt: now.Add(-6 * time.Minute), StartedAt: &started, DurationMS: 3, ChangedPaths: 2,
			Command: `"sh" "-c" "echo gen > gen.txt; echo done"`}},
		{Peer: "mini", JobListEntry: proto.JobListEntry{ID: "01M3BFTQX1WFQKGP1SP1RCDYW2", State: proto.StateExited, ExitCode: &three,
			AdmittedAt: now.Add(-6 * time.Minute), Command: `"sh" "-c" "exit 3"`}},
	}
	con, out := terminal(100)
	writePs(con.Out, rows, false)
	lines := strings.Split(strings.TrimSpace(termui.StripANSI(out.String())), "\n")
	if len(lines) != 4 {
		t.Fatalf("want a header and one line per job:\n%s", strings.Join(lines, "\n"))
	}
	for _, want := range []string{"JOB", "STATE", "AGE", "TIME", "PROJECT", "CHANGED", "COMMAND"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("header %q lacks %s", lines[0], want)
		}
	}
	if !strings.HasPrefix(lines[1], "cabal/01M3BF93Y7HS ") || !strings.Contains(lines[1], "● running") || !strings.Contains(lines[1], "1m30s") ||
		!strings.Contains(lines[1], "bash harbor-arms/run.sh") {
		t.Fatalf("running row = %q", lines[1])
	}
	if !strings.Contains(lines[2], "✓ exited 0") || !strings.Contains(lines[2], "6m") || !strings.Contains(lines[2], "2 files") ||
		!strings.Contains(lines[2], `sh -c 'echo gen > gen.txt; echo done'`) {
		t.Fatalf("finished row = %q", lines[2])
	}
	if !strings.Contains(lines[3], "✗ exited 3") || !strings.Contains(out.String(), "\x1b[31m✗ exited 3") {
		t.Fatalf("failed row isn't red: %q", lines[3])
	}
	for _, line := range lines {
		if termui.CellWidth(line) > 99 {
			t.Fatalf("row exceeds the terminal: %q", line)
		}
	}
}

func TestPsVerboseShowsCardsWithTimesAndSource(t *testing.T) {
	now := time.Now()
	started := now.Add(-2 * time.Minute)
	rows := []psRow{{Peer: "cabal", JobListEntry: proto.JobListEntry{
		ID: proto.NewULID(), State: proto.StateRunning, Project: "atlas", Workdir: "atlas/docs",
		AdmittedAt: started.Add(-3 * time.Second), StartedAt: &started, DurationMS: 120000,
		GitCommit: strings.Repeat("a", 40), GitDirty: true, Command: `"nix" "build"`,
	}}}
	con, out := terminal(120)
	writePs(con.Out, rows, true)
	got := termui.StripANSI(out.String())
	for _, want := range []string{"cabal/" + rows[0].ID, "● running", "started 3s later", "source aaaaaaaaaaaa +dirty", "workdir atlas/docs", "nix build"} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose ps lacks %q:\n%s", want, got)
		}
	}
}

func TestPsEmptyMessageNamesTheRunners(t *testing.T) {
	targets := []peerTarget{{name: "cabal"}, {name: "mini"}}
	if got := psEmptyMessage(targets, true, false); got != "No active jobs on cabal or mini." {
		t.Fatalf("empty message = %q", got)
	}
	if got := psEmptyMessage(targets, false, true); got != "No jobs on the runners that answered." {
		t.Fatalf("partial empty message = %q", got)
	}
}

func TestPsCommandFallsBackWhenTheRenderingIsTruncated(t *testing.T) {
	row := psRow{JobListEntry: proto.JobListEntry{Command: `"sh" "-c" "a very long comm…`}}
	if got := psCommand(row); got != row.Command {
		t.Fatalf("truncated command = %q", got)
	}
}
