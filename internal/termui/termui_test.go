package termui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestUnits(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{Bytes(39), "39 B"},
		{Bytes(6246), "6.1 KiB"},
		{Bytes(119129462), "114 MiB"},
		{Bytes(1131449568), "1.1 GiB"},
		{Bytes(5 << 30), "5 GiB"},
		{Count(3545), "3,545"},
		{Count(1151), "1,151"},
		{Count(999), "999"},
		{Count(-1234567), "-1,234,567"},
		{Things(1, "file", "files"), "1 file"},
		{Things(3449, "file", "files"), "3,449 files"},
		{Duration(3 * time.Millisecond), "3ms"},
		{Duration(0), "0ms"},
		{Duration(6 * time.Second), "6s"},
		{Duration(12700 * time.Millisecond), "12.7s"},
		{Duration(758 * time.Second), "12m38s"},
		{Duration(3*time.Hour + 4*time.Minute), "3h04m"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
	now := time.Date(2026, 9, 25, 1, 20, 0, 0, time.Local)
	for _, tc := range []struct {
		t   time.Time
		age string
		ago string
	}{
		{now.Add(-5 * time.Second), "5s", "just now"},
		{now.Add(-6 * time.Minute), "6m", "6m ago"},
		{now.Add(-3 * time.Hour), "3h", "3h ago"},
		{now.Add(-50 * 24 * time.Hour), now.Add(-50 * 24 * time.Hour).Format("Jan 2"), "on " + now.Add(-50*24*time.Hour).Format("Jan 2")},
	} {
		if got := Age(tc.t, now); got != tc.age {
			t.Errorf("Age = %q, want %q", got, tc.age)
		}
		if got := Ago(tc.t, now); got != tc.ago {
			t.Errorf("Ago = %q, want %q", got, tc.ago)
		}
	}
}

func TestTextHelpers(t *testing.T) {
	if got := CellWidth("\x1b[32m✓\x1b[0m ok"); got != 4 {
		t.Fatalf("CellWidth ignores SGR and counts ✓ as one cell: %d", got)
	}
	if got := CellWidth("日本"); got != 4 {
		t.Fatalf("wide runes take two cells: %d", got)
	}
	cut := TruncateANSI("\x1b[36mabcdefghij\x1b[0m", 5)
	if StripANSI(cut) != "abcd…" || !strings.HasSuffix(cut, "\x1b[0m") {
		t.Fatalf("TruncateANSI = %q", cut)
	}
	if got := ShellQuote([]string{"sh", "-c", "echo \"FAIL\" >&2; exit 3"}); got != `sh -c 'echo "FAIL" >&2; exit 3'` {
		t.Fatalf("ShellQuote = %s", got)
	}
	if got := ShellQuote([]string{"echo", "it's", ""}); got != `echo 'it'\''s' ''` {
		t.Fatalf("ShellQuote quotes single quotes and empty args: %s", got)
	}
	argv, ok := ParseQuotedArgv(`"sh" "-c" "echo \"FAIL: TestFoo\" >&2; exit 3"`)
	if !ok || len(argv) != 3 || argv[2] != `echo "FAIL: TestFoo" >&2; exit 3` {
		t.Fatalf("ParseQuotedArgv = %q %v", argv, ok)
	}
	if _, ok := ParseQuotedArgv(`"sh" "-c" "unterminated…`); ok {
		t.Fatal("ParseQuotedArgv accepted a truncated rendering")
	}
	if ShortID("01M3BFTQ6QD4GRXZKC3F4PTG15") != "01M3BFTQ6QD4" {
		t.Fatal("ShortID")
	}
	commands := []string{"status", "attach", "fetch", "push", "peers", "ps", "kill"}
	for input, want := range map[string]string{"stauts": "status", "atach": "attach", "fetc": "fetch", "make": "", "go": ""} {
		if got := Suggest(input, commands); got != want {
			t.Errorf("Suggest(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestTableAlignsOnVisibleWidthAndCutsLastColumn(t *testing.T) {
	var out bytes.Buffer
	c := New(&out, &out, Options{OutTTY: true, Color: true, Width: 40})
	tb := c.Out.Table("JOB", "STATE", "COMMAND")
	tb.Row(C("mini/01M3BFTQ6QD4", Cyan), C("✓ exited 0", Green), C("sh -c 'echo gen > gen.txt; echo more'"))
	tb.Row(C("mini/01M3BF0HMTA3", Cyan), C("● running", Green), C("sh run_mini.sh"))
	lines := tb.Lines()
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = StripANSI(l)
	}
	if !strings.HasPrefix(plain[0], "JOB                STATE       COMMAND") {
		t.Fatalf("header = %q", plain[0])
	}
	if CellWidth(plain[1]) > 39 || !strings.HasSuffix(plain[1], "…") {
		t.Fatalf("last column not cut to width: %q", plain[1])
	}
	if CellWidth(plain[2][:strings.Index(plain[2], "sh run")]) != CellWidth(plain[0][:strings.Index(plain[0], "COMMAND")]) {
		t.Fatalf("columns misaligned:\n%s\n%s", plain[0], plain[2])
	}
}

func TestNarrationOnTerminalAndPipe(t *testing.T) {
	var tty, pipe bytes.Buffer
	term := New(&tty, &tty, Options{OutTTY: true, ErrTTY: true, Color: true, Italic: true})
	plain := Plain(&pipe, &pipe)
	for _, c := range []*Console{term, plain} {
		c.Err.Say(OK, "exited 0 in 3ms")
		c.Err.Errorf("no runner named %s", "nope")
		c.Err.Hintf("you have cabal and mini")
		c.Err.Next("errand fetch --apply mini/01M3BFTQ6QD4", "bring them here")
	}
	if got := pipe.String(); got != "errand: exited 0 in 3ms\nerrand: error: no runner named nope\nerrand: hint: you have cabal and mini\nerrand: next: errand fetch --apply mini/01M3BFTQ6QD4 (bring them here)\n" {
		t.Fatalf("pipe narration:\n%s", got)
	}
	got := StripANSI(tty.String())
	want := "✓ exited 0 in 3ms\nerror: no runner named nope\nhint: you have cabal and mini\n  → errand fetch --apply mini/01M3BFTQ6QD4  bring them here\n"
	if got != want {
		t.Fatalf("terminal narration:\n%q\nwant\n%q", got, want)
	}
	if !strings.Contains(tty.String(), "\x1b[2;3m") {
		t.Fatal("hints should be dim italic when italics are allowed")
	}
}

func TestSpinnerIsTransientAndYieldsToOutput(t *testing.T) {
	var buf bytes.Buffer
	c := New(&buf, &buf, Options{OutTTY: true, ErrTTY: true, Width: 80})
	sp := c.Err.Spin("Syncing with mini")
	c.Err.Print("row one")
	if !sp.Active() {
		t.Fatal("printing a line must keep the spinner")
	}
	c.Out.Write([]byte("job output\n"))
	if sp.Active() {
		t.Fatal("command output must remove the spinner for good")
	}
	sp.Done(OK, "never mind") // Done after output still prints the line
	lines := renderTerminal(buf.String())
	if strings.Join(lines, "\n") != "row one\njob output\n✓ never mind" {
		t.Fatalf("screen:\n%s", strings.Join(lines, "\n"))
	}

	buf.Reset()
	p := Plain(&buf, &buf)
	sp = p.Err.Spin("Syncing")
	sp.Progress("Uploading", 5, 10, nil)
	sp.Stop()
	p.Err.Spin("x").Done(OK, "synced")
	if buf.String() != "errand: synced\n" {
		t.Fatalf("non-terminal spinner printed %q", buf.String())
	}
}

func TestDetectOptions(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	var buf bytes.Buffer
	opts := DetectOptions(&buf, &buf, env(map[string]string{"CLICOLOR_FORCE": "1"}))
	if opts.OutTTY || !opts.Color || !opts.ForceColor {
		t.Fatalf("CLICOLOR_FORCE: %+v", opts)
	}
	opts = DetectOptions(&buf, &buf, env(map[string]string{"CLICOLOR_FORCE": "1", "NO_COLOR": "1"}))
	if opts.Color || opts.ForceColor {
		t.Fatalf("NO_COLOR wins: %+v", opts)
	}
	opts = DetectOptions(&buf, &buf, env(map[string]string{"TERM": "dumb", "FORCE_COLOR": "1"}))
	if opts.Color || !opts.ASCII {
		t.Fatalf("TERM=dumb: %+v", opts)
	}
	opts = DetectOptions(&buf, &buf, env(map[string]string{"TERM": "screen-256color"}))
	if opts.Italic {
		t.Fatal("italics are off under screen/tmux TERM")
	}
	c := New(&buf, &buf, Options{Color: true, ForceColor: true})
	if !c.Err.Colored() || c.Err.Interactive() {
		t.Fatal("forced color styles a pipe without making it interactive")
	}
}

// renderTerminal applies carriage returns and line clears to show what a
// terminal would display.
func renderTerminal(raw string) []string {
	raw = StripANSIExceptClear(raw)
	var lines []string
	cur := ""
	for len(raw) > 0 {
		switch {
		case strings.HasPrefix(raw, "\x1b[2K"):
			cur = ""
			raw = raw[len("\x1b[2K"):]
			continue
		case raw[0] == '\r':
			cur = ""
		case raw[0] == '\n':
			lines = append(lines, cur)
			cur = ""
		default:
			r := []rune(raw)[0]
			cur += string(r)
			raw = raw[len(string(r)):]
			continue
		}
		raw = raw[1:]
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func StripANSIExceptClear(s string) string {
	s = strings.ReplaceAll(s, "\x1b[2K", "\x00CLEAR\x00")
	s = StripANSI(s)
	return strings.ReplaceAll(s, "\x00CLEAR\x00", "\x1b[2K")
}

func TestRedirectedStreamsNeverTruncateToTheTerminalWidth(t *testing.T) {
	var out, errOut bytes.Buffer
	c := New(&out, &errOut, Options{OutTTY: false, ErrTTY: true, Width: 20})
	long := strings.Repeat("x", 60)
	tbl := c.Out.Table("NAME", "DETAIL")
	tbl.Row(C("a"), C(long))
	tbl.Print()
	if !strings.Contains(out.String(), long) {
		t.Fatalf("piped table was cut to stderr's width:\n%s", &out)
	}
}
