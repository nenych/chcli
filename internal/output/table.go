package output

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"
)

// tableChunkRows bounds how many rows are held to compute column widths.
// Larger results are rendered as consecutive tables of this many rows, the
// same way ClickHouse's own Pretty formats render one table per block, so
// memory use stays flat no matter how large the result is.
const tableChunkRows = 10000

// tableWriter renders results in the style of ClickHouse's PrettyCompact:
//
//	┌─name────┬─value─┐
//	│ answer  │    42 │
//	└─────────┴───────┘
type tableWriter struct {
	w     *bufio.Writer
	cols  []Column
	fmts  []colFormat
	rows  [][]string
	wrote bool
}

func (t *tableWriter) Begin(cols []Column) error {
	t.cols, t.fmts = cols, colFormats(cols)
	return nil
}

func (t *tableWriter) Row(values []any) error {
	cells := make([]string, len(values))
	for i, v := range values {
		s, ok := t.fmts[i].text(v)
		if !ok {
			s = nullText
		}
		cells[i] = escapeControl(s)
	}
	t.rows = append(t.rows, cells)
	if len(t.rows) >= tableChunkRows {
		return t.flush()
	}
	return nil
}

// Totals renders the totals row as a table of its own, as ClickHouse does.
func (t *tableWriter) Totals(values []any) error {
	if err := t.flush(); err != nil {
		return err
	}
	if _, err := t.w.WriteString("\nTotals:\n"); err != nil {
		return err
	}
	t.wrote = false
	return t.Row(values)
}

func (t *tableWriter) End() error {
	if err := t.flush(); err != nil {
		return err
	}
	return t.w.Flush()
}

func (t *tableWriter) flush() error {
	if len(t.rows) == 0 {
		return nil
	}
	names := make([]string, len(t.cols))
	widths := make([]int, len(t.cols))
	for i, c := range t.cols {
		names[i] = escapeControl(c.Name)
		widths[i] = runewidth.StringWidth(names[i])
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if w := runewidth.StringWidth(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}

	var b strings.Builder
	if t.wrote {
		b.WriteByte('\n')
	}
	t.wrote = true

	b.WriteString("┌─")
	for i, name := range names {
		if i > 0 {
			b.WriteString("─┬─")
		}
		b.WriteString(name)
		b.WriteString(strings.Repeat("─", widths[i]-runewidth.StringWidth(name)))
	}
	b.WriteString("─┐\n")

	for _, row := range t.rows {
		b.WriteString("│ ")
		for i, cell := range row {
			if i > 0 {
				b.WriteString(" │ ")
			}
			pad := strings.Repeat(" ", widths[i]-runewidth.StringWidth(cell))
			if t.fmts[i].numeric {
				b.WriteString(pad + cell)
			} else {
				b.WriteString(cell + pad)
			}
		}
		b.WriteString(" │\n")
	}

	b.WriteString("└─")
	for i, w := range widths {
		if i > 0 {
			b.WriteString("─┴─")
		}
		b.WriteString(strings.Repeat("─", w))
	}
	b.WriteString("─┘\n")

	t.rows = t.rows[:0]
	_, err := t.w.WriteString(b.String())
	return err
}

// escapeControl makes control characters visible. Besides keeping the table
// aligned, this stops data from smuggling terminal escape sequences into the
// user's terminal.
func escapeControl(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// verticalWriter prints each row as a block of "column: value" lines.
type verticalWriter struct {
	w     *bufio.Writer
	cols  []Column
	fmts  []colFormat
	names []string // padded to equal width
	n     int
	wrote bool
}

func (v *verticalWriter) Begin(cols []Column) error {
	v.cols, v.fmts = cols, colFormats(cols)
	width := 0
	for _, c := range cols {
		if w := runewidth.StringWidth(escapeControl(c.Name)); w > width {
			width = w
		}
	}
	v.names = make([]string, len(cols))
	for i, c := range cols {
		name := escapeControl(c.Name)
		v.names[i] = name + ":" + strings.Repeat(" ", width-runewidth.StringWidth(name)+1)
	}
	return nil
}

func (v *verticalWriter) Row(values []any) error {
	v.n++
	return v.block(fmt.Sprintf("Row %d:", v.n), values)
}

// Totals prints the totals row as one more block.
func (v *verticalWriter) Totals(values []any) error {
	return v.block("Totals:", values)
}

func (v *verticalWriter) block(header string, values []any) error {
	if v.wrote {
		v.w.WriteByte('\n')
	}
	v.wrote = true
	fmt.Fprintf(v.w, "%s\n%s\n", header, strings.Repeat("─", len(header)))
	for i, val := range values {
		s, ok := v.fmts[i].text(val)
		if !ok {
			s = nullText
		}
		v.w.WriteString(v.names[i])
		v.w.WriteString(escapeControl(s))
		v.w.WriteByte('\n')
	}
	// Each row is complete on its own; let slow queries show progress.
	return v.w.Flush()
}

func (v *verticalWriter) End() error { return v.w.Flush() }
