package termui

import (
	"io"
	"strings"
)

// Cell is one table value with the attributes it is drawn with.
type Cell struct {
	Text  string
	Attrs []Attr
}

// C builds a cell.
func C(text string, attrs ...Attr) Cell { return Cell{Text: text, Attrs: attrs} }

// Table lays out rows in aligned columns. The last column absorbs any
// overflow: on a terminal of known width it is cut with "…".
type Table struct {
	s       *Stream
	headers []string
	rows    [][]Cell
	indent  string
}

// Table starts a table on this stream.
func (s *Stream) Table(headers ...string) *Table {
	return &Table{s: s, headers: headers}
}

// Indent prefixes every line.
func (t *Table) Indent(prefix string) *Table { t.indent = prefix; return t }

// Row appends a row.
func (t *Table) Row(cells ...Cell) { t.rows = append(t.rows, cells) }

// Len is the number of rows.
func (t *Table) Len() int { return len(t.rows) }

// Lines renders the table without writing it.
func (t *Table) Lines() []string {
	cols := len(t.headers)
	for _, row := range t.rows {
		cols = max(cols, len(row))
	}
	widths := make([]int, cols)
	for i, h := range t.headers {
		widths[i] = CellWidth(h)
	}
	for _, row := range t.rows {
		for i, c := range row {
			widths[i] = max(widths[i], CellWidth(c.Text))
		}
	}
	var lines []string
	render := func(cells []Cell, header bool) string {
		var b strings.Builder
		b.WriteString(t.indent)
		for i, c := range cells {
			text := c.Text
			last := i == len(cells)-1
			if last && t.s.width > 0 {
				room := t.s.width - CellWidth(b.String()) - 1
				if room > 1 && CellWidth(text) > room {
					text = Truncate(text, room)
				}
			}
			attrs := c.Attrs
			if header {
				attrs = []Attr{Dim}
			}
			b.WriteString(t.s.Paint(text, attrs...))
			if !last {
				b.WriteString(strings.Repeat(" ", widths[i]-CellWidth(text)+2))
			}
		}
		return strings.TrimRight(b.String(), " ")
	}
	if len(t.headers) > 0 {
		hs := make([]Cell, len(t.headers))
		for i, h := range t.headers {
			hs[i] = Cell{Text: h}
		}
		lines = append(lines, render(hs, true))
	}
	for _, row := range t.rows {
		lines = append(lines, render(row, false))
	}
	return lines
}

// Print writes the table to its stream.
func (t *Table) Print() {
	for _, line := range t.Lines() {
		t.s.Print(line)
	}
}

// Fprint writes the table to w, bypassing the console's transient line.
func (t *Table) Fprint(w io.Writer) {
	for _, line := range t.Lines() {
		io.WriteString(w, line+"\n")
	}
}
