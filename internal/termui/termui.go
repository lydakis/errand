// Package termui renders errand's own output: styles, glyphs, transient
// progress lines, tables and human units. Commands decide what to say; this
// package decides how it looks on a terminal versus in a pipe or log.
//
// Rules that hold everywhere:
//   - Color only encodes state, and every glyph is paired with words.
//   - Transient lines (spinners, progress) exist only on an interactive
//     terminal and are erased before anything else is written.
//   - Off a terminal, narration keeps the "errand: " prefix and no styling,
//     so logs stay greppable.
package termui

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Attr is one SGR attribute.
type Attr string

const (
	Bold   Attr = "1"
	Dim    Attr = "2"
	Italic Attr = "3"
	Red    Attr = "31"
	Green  Attr = "32"
	Yellow Attr = "33"
	Cyan   Attr = "36"
)

// Glyph is a status mark with the color that goes with it.
type Glyph struct {
	Mark  string
	ASCII string
	Color Attr
}

var (
	OK   = Glyph{"✓", "ok", Green}
	Fail = Glyph{"✗", "x", Red}
	Run  = Glyph{"●", "*", Green}
	Wait = Glyph{"◌", "~", Yellow}
	Off  = Glyph{"○", "o", Red}
	Here = Glyph{"▸", ">", ""}
	Next = Glyph{"→", "->", ""}
	Skip = Glyph{"–", "-", ""}
	Warn = Glyph{"!", "!", Yellow}
	Up   = Glyph{"↑", "^", Green}
	Dot  = Glyph{"·", "-", ""}
)

// Options describes a console explicitly; Detect derives it from the process.
type Options struct {
	OutTTY, ErrTTY bool // interactive terminals: spinners, redraws, short ids
	Color          bool // styling allowed on terminal streams
	ForceColor     bool // styling also on non-terminal streams (CLICOLOR_FORCE)
	Italic         bool
	ASCII          bool // TERM=dumb and similar: no Unicode glyphs
	Width          int  // terminal columns; 0 when unknown
}

// Console owns stdout and stderr for one invocation. Both streams share one
// lock and at most one transient line, because on a terminal they share rows.
type Console struct {
	mu   sync.Mutex
	live *liveLine
	Out  *Stream
	Err  *Stream
}

// Stream is one side of a console.
type Stream struct {
	c      *Console
	w      io.Writer
	tty    bool
	color  bool
	italic bool
	ascii  bool
	width  int
}

// New builds a console with explicit options. Tests use it with buffers.
func New(stdout, stderr io.Writer, opts Options) *Console {
	c := &Console{}
	colorFor := func(tty bool) bool { return opts.Color && (tty || opts.ForceColor) }
	c.Out = &Stream{c: c, w: stdout, tty: opts.OutTTY, color: colorFor(opts.OutTTY), italic: opts.Italic, ascii: opts.ASCII, width: opts.Width}
	c.Err = &Stream{c: c, w: stderr, tty: opts.ErrTTY, color: colorFor(opts.ErrTTY), italic: opts.Italic, ascii: opts.ASCII, width: opts.Width}
	return c
}

// Plain is a console with no terminal features, for pipes and tests.
func Plain(stdout, stderr io.Writer) *Console { return New(stdout, stderr, Options{}) }

// Detect inspects the process: which streams are terminals, the terminal
// width, and the NO_COLOR / CLICOLOR_FORCE / FORCE_COLOR / TERM conventions.
func Detect(stdout, stderr io.Writer) *Console {
	return New(stdout, stderr, DetectOptions(stdout, stderr, os.Getenv))
}

// DetectOptions is Detect's policy with an injectable environment.
func DetectOptions(stdout, stderr io.Writer, getenv func(string) string) Options {
	term := getenv("TERM")
	dumb := term == "dumb"
	outTTY := isTerminalWriter(stdout) && !dumb
	errTTY := isTerminalWriter(stderr) && !dumb
	color := !dumb && getenv("NO_COLOR") == ""
	force := false
	if v := getenv("CLICOLOR_FORCE"); v != "" && v != "0" {
		force = true
	}
	if v := getenv("FORCE_COLOR"); v != "" && v != "0" && v != "false" {
		force = true
	}
	width := 0
	for _, w := range []io.Writer{stderr, stdout} {
		if f, ok := w.(*os.File); ok {
			if cols := terminalColumns(f.Fd()); cols > 0 {
				width = cols
				break
			}
		}
	}
	if cols, err := strconv.Atoi(getenv("COLUMNS")); err == nil && cols > 0 && (outTTY || errTTY) {
		width = cols
	}
	// tmux and screen advertise TERM=screen*, where italics are commonly
	// drawn as reverse video. The Linux console has no italics at all.
	italic := color && !strings.HasPrefix(term, "screen") && term != "linux"
	return Options{OutTTY: outTTY, ErrTTY: errTTY, Color: color, ForceColor: force && color, Italic: italic, ASCII: dumb, Width: width}
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTerminal(f.Fd())
}

// Interactive reports whether transient lines and terminal-only layouts apply.
func (s *Stream) Interactive() bool { return s.tty }

// Colored reports whether SGR styling is emitted.
func (s *Stream) Colored() bool { return s.color }

// Width is the terminal width in cells, or 0 when unknown.
func (s *Stream) Width() int { return s.width }

// Writer is the underlying writer.
func (s *Stream) Writer() io.Writer { return s.w }

// Paint applies attributes when styling is enabled. Italic falls back to
// nothing where terminals are known to render it badly.
func (s *Stream) Paint(text string, attrs ...Attr) string {
	if !s.color || text == "" || len(attrs) == 0 {
		return text
	}
	codes := make([]string, 0, len(attrs))
	for _, a := range attrs {
		if a == "" || a == Italic && !s.italic {
			continue
		}
		codes = append(codes, string(a))
	}
	if len(codes) == 0 {
		return text
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + text + "\x1b[0m"
}

func (s *Stream) B(text string) string    { return s.Paint(text, Bold) }
func (s *Stream) D(text string) string    { return s.Paint(text, Dim) }
func (s *Stream) Hint(text string) string { return s.Paint(text, Dim, Italic) }
func (s *Stream) ID(text string) string   { return s.Paint(text, Cyan) }

// G renders a glyph in its color.
func (s *Stream) G(g Glyph) string {
	mark := g.Mark
	if s.ascii {
		mark = g.ASCII
	}
	if g.Color == "" {
		return s.Paint(mark, Dim)
	}
	return s.Paint(mark, g.Color)
}

// Print writes raw text followed by a newline, keeping any transient line
// below it.
func (s *Stream) Print(line string) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.c.clearLocked()
	fmt.Fprintln(s.w, line)
	s.c.redrawLocked()
}

// Printf is Print with formatting.
func (s *Stream) Printf(format string, args ...any) { s.Print(fmt.Sprintf(format, args...)) }

// Say writes one line of errand's own narration: a glyph on a terminal, the
// errand: prefix everywhere else.
func (s *Stream) Say(g Glyph, text string) {
	if s.tty {
		s.Print(s.G(g) + " " + text)
		return
	}
	s.Print("errand: " + lowerFirst(text))
}

// lowerFirst turns a sentence into a clause after "errand: ", leaving
// acronyms and names like "PATH" alone.
func lowerFirst(text string) string {
	plain := StripANSI(text)
	if len(plain) < 2 || plain[0] < 'A' || plain[0] > 'Z' || plain[1] < 'a' || plain[1] > 'z' {
		return text
	}
	i := strings.IndexByte(text, plain[0])
	return text[:i] + string(plain[0]+'a'-'A') + text[i+1:]
}

// Detail writes a dim continuation line (used by -v): "· label  value".
func (s *Stream) Detail(label, value string) {
	if s.tty {
		s.Print(s.D("· " + padRight(label, 10) + " " + value))
		return
	}
	s.Print("errand: " + label + ": " + value)
}

// Errorf writes an "error:" line.
func (s *Stream) Errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if s.tty {
		s.Print(s.Paint("error:", Red, Bold) + " " + msg)
		return
	}
	s.Print("errand: error: " + msg)
}

// Hintf writes a "hint:" line that says how to fix the preceding error.
func (s *Stream) Hintf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if s.tty {
		s.Print(s.D("hint:") + " " + msg)
		return
	}
	s.Print("errand: hint: " + msg)
}

// Warnf writes a warning that needs the reader's attention.
func (s *Stream) Warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if s.tty {
		s.Print(s.G(Warn) + " " + msg)
		return
	}
	s.Print("errand: warning: " + msg)
}

// Next suggests the command the reader probably wants next.
func (s *Stream) Next(command, why string) {
	if s.tty {
		line := "  " + s.G(Next) + " " + s.Hint(command)
		if why != "" {
			line += "  " + s.D(why)
		}
		s.Print(line)
		return
	}
	if why != "" {
		s.Print("errand: next: " + command + " (" + why + ")")
		return
	}
	s.Print("errand: next: " + command)
}

// Write passes command output through. A transient line is removed for good
// before the first byte, because job output owns the terminal from then on.
func (s *Stream) Write(p []byte) (int, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if s.tty && s.c.live != nil {
		s.c.stopLocked()
	}
	return s.w.Write(p)
}

func padRight(s string, width int) string {
	if n := CellWidth(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}
