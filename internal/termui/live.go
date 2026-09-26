package termui

import (
	"fmt"
	"strings"
	"time"
)

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const spinInterval = 80 * time.Millisecond

// liveLine is the single transient line at the bottom of a terminal.
type liveLine struct {
	owner  *Stream
	text   string
	frame  int
	bar    *barState
	ticker *time.Ticker
	stop   chan struct{}
	drawn  bool
}

type barState struct {
	done, total int64
	label       func(done, total int64) string
}

// Spinner is a transient status line. On a non-interactive stream every
// method is a no-op except Done, which prints its persistent line.
type Spinner struct {
	s    *Stream
	line *liveLine
}

// Spin starts a transient line on this stream, replacing any current one.
func (s *Stream) Spin(text string) *Spinner {
	sp := &Spinner{s: s}
	if !s.tty {
		return sp
	}
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
	l := &liveLine{owner: s, text: text, stop: make(chan struct{})}
	l.ticker = time.NewTicker(spinInterval)
	c.live = l
	sp.line = l
	c.drawLocked()
	go func() {
		for {
			select {
			case <-l.stop:
				return
			case <-l.ticker.C:
				c.mu.Lock()
				if c.live == l {
					l.frame++
					c.drawLocked()
				}
				c.mu.Unlock()
			}
		}
	}()
	return sp
}

// Set replaces the spinner's text.
func (sp *Spinner) Set(text string) {
	if sp.line == nil {
		return
	}
	c := sp.s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live == sp.line {
		sp.line.text = text
		sp.line.bar = nil
		c.drawLocked()
	}
}

// Progress turns the spinner into a progress bar. label renders the text after
// the bar from the current counts.
func (sp *Spinner) Progress(text string, done, total int64, label func(done, total int64) string) {
	if sp.line == nil {
		return
	}
	c := sp.s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live == sp.line {
		sp.line.text = text
		sp.line.bar = &barState{done: done, total: total, label: label}
		c.drawLocked()
	}
}

// Active reports whether the spinner is still on screen.
func (sp *Spinner) Active() bool {
	if sp.line == nil {
		return false
	}
	sp.s.c.mu.Lock()
	defer sp.s.c.mu.Unlock()
	return sp.s.c.live == sp.line
}

// Stop removes the transient line.
func (sp *Spinner) Stop() {
	if sp.line == nil {
		return
	}
	c := sp.s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live == sp.line {
		c.stopLocked()
	}
}

// Done replaces the transient line with a persistent narration line.
func (sp *Spinner) Done(g Glyph, text string) {
	sp.Stop()
	sp.s.Say(g, text)
}

// Replace replaces the transient line with a raw persistent line.
func (sp *Spinner) Replace(line string) {
	sp.Stop()
	sp.s.Print(line)
}

func (c *Console) stopLocked() {
	l := c.live
	if l == nil {
		return
	}
	l.ticker.Stop()
	close(l.stop)
	c.clearLocked()
	c.live = nil
}

// clearLocked erases the drawn transient line so ordinary output can take
// its row; redrawLocked puts it back underneath.
func (c *Console) clearLocked() {
	if l := c.live; l != nil && l.drawn {
		fmt.Fprint(l.owner.w, "\r\x1b[2K")
		l.drawn = false
	}
}

func (c *Console) redrawLocked() {
	if c.live != nil {
		c.drawLocked()
	}
}

func (c *Console) drawLocked() {
	l := c.live
	s := l.owner
	frame := spinFrames[l.frame%len(spinFrames)]
	if s.ascii {
		frame = []string{"-", "\\", "|", "/"}[l.frame%4]
	}
	line := s.Paint(frame, Cyan) + " " + l.text
	if b := l.bar; b != nil {
		line += " " + s.renderBar(b.done, b.total, 24)
		if b.label != nil {
			line += " " + b.label(b.done, b.total)
		}
	}
	if s.width > 0 {
		line = TruncateANSI(line, s.width-1)
	}
	fmt.Fprint(s.w, "\r\x1b[2K"+line)
	l.drawn = true
}

func (s *Stream) renderBar(done, total int64, width int) string {
	if total <= 0 {
		total = 1
	}
	if done > total {
		done = total
	}
	filled := int(int64(width) * done / total)
	fill, head, rest := "━", "╸", "─"
	if s.ascii {
		fill, head, rest = "=", ">", "-"
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(fill, filled))
	empty := width - filled
	if empty > 0 {
		b.WriteString(head)
		empty--
	}
	return s.Paint(b.String(), Cyan) + s.Paint(strings.Repeat(rest, max(0, empty)), Dim)
}
