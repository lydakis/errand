package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/lydakis/errand/internal/proto"
)

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

func TestPsUsesCardsWhenRenderedTableDoesNotFit(t *testing.T) {
	rows := []psRow{{
		Peer: "cabal",
		JobListEntry: proto.JobListEntry{
			ID: proto.NewULID(), State: proto.StateRunning, Project: "atlas",
			Workdir: strings.Repeat("nested/", 50), Command: `"nix" "build"`,
		},
	}}
	var out bytes.Buffer
	t.Setenv("COLUMNS", "220")
	writePs(&out, rows)
	if got := out.String(); !strings.Contains(got, "  command ") || strings.HasPrefix(got, "PEER") {
		t.Fatalf("oversized table did not switch to cards: %q", got)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if len([]rune(line)) > 220 {
			t.Fatalf("ps line exceeds terminal width: %d runes in %q", len([]rune(line)), line)
		}
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
	t.Setenv("COLUMNS", "72")
	writePs(&out, rows)
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
		t.Setenv("COLUMNS", fmt.Sprint(width))
		writePs(&out, rows)
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
