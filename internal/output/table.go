package output

import (
	"io"
	"strings"
	"unicode/utf8"
)

// Col is one table column. Numbers align right.
type Col struct {
	Header string
	Right  bool
}

// Table is a borderless table: a dim uppercase header, a two-space gutter
// and a leading marker column, where ● marks the rows of this device.
type Table struct {
	cols []Col
	rows []tableRow
}

type tableRow struct {
	marked bool
	cells  []Span
}

// NewTable starts a table.
func NewTable(cols ...Col) *Table { return &Table{cols: cols} }

// Row adds a row; marked rows get the ● marker.
func (t *Table) Row(marked bool, cells ...Span) {
	t.rows = append(t.rows, tableRow{marked, cells})
}

// Table prints the table on stdout. A table without rows prints nothing;
// callers print the empty state instead.
func (p *Printer) Table(t *Table) { p.table(p.out, t) }

// StatusTable prints a table that explains a status line, on stderr with it.
func (p *Printer) StatusTable(t *Table) { p.table(p.err, t) }

func (p *Printer) table(w io.Writer, t *Table) {
	if len(t.rows) == 0 {
		return
	}
	widths := make([]int, len(t.cols))
	for i, c := range t.cols {
		widths[i] = utf8.RuneCountInString(c.Header)
	}
	for _, r := range t.rows {
		for i := range t.cols {
			if i < len(r.cells) {
				widths[i] = max(widths[i], utf8.RuneCountInString(r.cells[i].text))
			}
		}
	}
	var b strings.Builder
	header := make([]Span, len(t.cols))
	for i, c := range t.cols {
		header[i] = Text(c.Header)
	}
	b.WriteString(p.paint(styleDim, "  "+strings.TrimRight(p.cells(t.cols, widths, header, false), " ")) + "\n")
	for _, r := range t.rows {
		lead := "  "
		if r.marked {
			lead = p.symbol(Live)
		}
		b.WriteString(lead + strings.TrimRight(p.cells(t.cols, widths, r.cells, true), " ") + "\n")
	}
	write(w, b.String())
}

// cells pads by visible width, then colours, so escapes never shift a column.
func (p *Printer) cells(cols []Col, widths []int, cells []Span, colour bool) string {
	var b strings.Builder
	for i, c := range cols {
		if i > 0 {
			b.WriteString("  ")
		}
		var cell Span
		if i < len(cells) {
			cell = cells[i]
		}
		pad := strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell.text))
		text := cell.text
		if colour {
			text = p.paint(cell.style, cell.text)
		}
		if c.Right {
			b.WriteString(pad + text)
		} else {
			b.WriteString(text + pad)
		}
	}
	return b.String()
}

// Pair is one row of a key/value block.
type Pair struct {
	Label string
	Value []Span
}

// KV builds a pair; bare strings are values at default brightness.
func KV(label string, value ...any) Pair { return Pair{label, spans(stylePlain, value)} }

// pairs renders dim labels padded to the longest one, a two-space gutter and
// the values. The padding is dim with the label, as one run.
func (p *Printer) pairs(w io.Writer, pairs []Pair) {
	width := 0
	for _, kv := range pairs {
		width = max(width, utf8.RuneCountInString(kv.Label))
	}
	var b strings.Builder
	for _, kv := range pairs {
		label := kv.Label + strings.Repeat(" ", width-utf8.RuneCountInString(kv.Label))
		b.WriteString(p.paint(styleDim, label) + "  " + p.join(kv.Value) + "\n")
	}
	write(w, b.String())
}
