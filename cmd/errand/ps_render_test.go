package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/lydakis/errand/internal/proto"
)

func TestPsEmptyStateDoesNotRenderTable(t *testing.T) {
	var out bytes.Buffer
	writePs(&out, nil)
	if got := out.String(); got != "No jobs.\n" {
		t.Fatalf("empty ps = %q", got)
	}
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

	rows[0].Workdir = "atlas"
	out.Reset()
	writePsTable(&out, rows)
	if got := out.String(); !strings.Contains(got, "WORKDIR") || !strings.Contains(got, "atlas") {
		t.Fatalf("project-root ps table = %q", got)
	}

	rows[0].Workdir = "atlas/docs"
	out.Reset()
	writePsTable(&out, rows)
	if got := out.String(); !strings.Contains(got, "WORKDIR") || !strings.Contains(got, "atlas/docs") {
		t.Fatalf("nested-workdir ps table = %q", got)
	}
}

func TestPsInteractiveOutputAlwaysUsesCards(t *testing.T) {
	rows := []psRow{{
		Peer: "cabal",
		JobListEntry: proto.JobListEntry{
			ID: proto.NewULID(), State: proto.StateRunning, Project: "atlas",
			Command: `"true"`,
		},
	}}
	var out bytes.Buffer
	writePsWithOptions(&out, rows, psRenderOptions{interactive: true, width: 220})
	if got := out.String(); !strings.Contains(got, "  command ") || strings.HasPrefix(got, "PEER") {
		t.Fatalf("interactive ps did not use cards: %q", got)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if len([]rune(line)) > 220 {
			t.Fatalf("ps line exceeds terminal width: %d runes in %q", len([]rune(line)), line)
		}
	}
}

func TestPsPipedOutputAlwaysUsesPlainTable(t *testing.T) {
	rows := []psRow{{
		Peer: "cabal",
		JobListEntry: proto.JobListEntry{
			ID: proto.NewULID(), State: proto.StateRunning, Project: "atlas",
			Workdir: strings.Repeat("nested/", 50), Command: `"nix" "build"`,
		},
	}}
	var out bytes.Buffer
	t.Setenv("COLUMNS", "60")
	writePs(&out, rows)
	if got := out.String(); !strings.HasPrefix(got, "PEER") || strings.Contains(got, "\x1b[") {
		t.Fatalf("piped ps was not a plain table: %q", got)
	}
}

func TestPsInteractiveOutputBoldsOnlyJobIdentity(t *testing.T) {
	jobID := proto.NewULID()
	rows := []psRow{{
		Peer: "cabal",
		JobListEntry: proto.JobListEntry{
			ID: jobID, State: proto.StateRunning, Project: "atlas", Command: `"nix" "build"`,
		},
	}}
	var out bytes.Buffer
	writePsWithOptions(&out, rows, psRenderOptions{interactive: true, width: 120, style: true})
	wantIdentity := "\x1b[1mcabal/" + jobID + "\x1b[0m"
	if got := out.String(); !strings.Contains(got, wantIdentity) {
		t.Fatalf("interactive ps did not emphasize identity: %q", got)
	} else if strings.Contains(got, "\x1b[1mcommand") || strings.Contains(got, "\x1b[1m\"nix\"") {
		t.Fatalf("interactive ps over-emphasized command: %q", got)
	}
}

func TestPsStylingRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if terminalStylingEnabled(true) {
		t.Fatal("NO_COLOR did not disable terminal styling")
	}
	t.Setenv("NO_COLOR", "")
	if terminalStylingEnabled(true) {
		t.Fatal("present but empty NO_COLOR did not disable terminal styling")
	}
	os.Unsetenv("NO_COLOR")
	if !terminalStylingEnabled(true) {
		t.Fatal("interactive terminal styling remained disabled without NO_COLOR")
	}
	if terminalStylingEnabled(false) {
		t.Fatal("non-interactive output enabled terminal styling")
	}
}

func TestPsCardsWrapToTerminalWidth(t *testing.T) {
	started := time.Date(2026, 8, 30, 14, 0, 0, 0, time.Local)
	rows := []psRow{{
		Peer: "cabal",
		JobListEntry: proto.JobListEntry{
			ID: proto.NewULID(), State: proto.StateRunning, Project: "atlas", Workdir: "atlas/docs",
			AdmittedAt: started.Add(-time.Minute), StartedAt: &started, DurationMS: 123000,
			GitCommit: strings.Repeat("a", 40), Command: `"nix" "build" "a-very-long-output-name-that-needs-to-wrap-cleanly"`,
		},
	}}
	var out bytes.Buffer
	writePsWithOptions(&out, rows, psRenderOptions{interactive: true, width: 72})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 4 {
		t.Fatalf("narrow ps did not wrap: %q", out.String())
	}
	for _, line := range lines {
		if len([]rune(line)) > 72 {
			t.Fatalf("ps line exceeds terminal width: %d runes in %q", len([]rune(line)), line)
		}
	}
	if got := out.String(); !strings.Contains(got, "atlas") || !strings.Contains(got, "workdir atlas/docs") ||
		!strings.Contains(got, "command") {
		t.Fatalf("narrow ps omitted context: %q", got)
	}
	if got := out.String(); !strings.Contains(got, "\n          \"") {
		t.Fatalf("wrapped command lacks hanging indent: %q", got)
	}
	if got := out.String(); strings.Contains(got, "source\n") || strings.Contains(got, "workdir\n") {
		t.Fatalf("wrapped metadata separated a label from its value: %q", got)
	}
}

func TestPsMeasuresAndWrapsUnicodeByTerminalCells(t *testing.T) {
	rows := []psRow{{
		Peer: "cabal",
		JobListEntry: proto.JobListEntry{
			ID: proto.NewULID(), State: proto.StateRunning,
			Project: strings.Repeat("界", 40), Command: `"true"`,
		},
	}}
	for _, width := range []int{160, 60} {
		var out bytes.Buffer
		writePsWithOptions(&out, rows, psRenderOptions{interactive: true, width: width})
		if got := out.String(); strings.HasPrefix(got, "PEER") {
			t.Fatalf("width %d selected an overflowing table: %q", width, got)
		}
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			if cells := testTerminalCells(line); cells > width {
				t.Fatalf("width %d rendered %d cells in %q", width, cells, line)
			}
		}
	}
}

func testTerminalCells(value string) int {
	width := 0
	for _, r := range value {
		switch {
		case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), r == '\u200d':
		case r <= unicode.MaxASCII:
			width++
		default:
			width += 2
		}
	}
	return width
}
