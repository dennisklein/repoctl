package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/GSI-HPC/go-clikit/termtext"
)

// Table is a table of the human output, as cliout.Table writes one, but a
// cell can carry a colour: the width comes from the plain text, and the
// colour goes around the padded cell. Each cell is escaped.
type Table struct {
	Heads []string
	// Right marks the columns that align right (counts).
	Right map[int]bool
	// Wide marks columns that only -v shows.
	Wide map[int]bool
	Rows [][]Cell
	// Caption goes under the table after an empty line.
	Caption string
}

// Cell is one cell of a Table.
type Cell struct {
	Text  string
	Color func(string) string
}

// NewTable makes a table with these headings.
func NewTable(heads ...string) *Table {
	return &Table{Heads: heads, Right: map[int]bool{}, Wide: map[int]bool{}}
}

// Row adds a row of plain cells.
func (t *Table) Row(cells ...string) {
	row := make([]Cell, len(cells))
	for i, c := range cells {
		row[i] = Cell{Text: c}
	}
	t.Rows = append(t.Rows, row)
}

// RowCells adds a row of cells with colours.
func (t *Table) RowCells(cells ...Cell) { t.Rows = append(t.Rows, cells) }

// C is a cell with a colour.
func C(text string, color func(string) string) Cell { return Cell{Text: text, Color: color} }

// Write writes the table. The last column has no padding.
func (t *Table) Write(w io.Writer, wide bool) error {
	var shown []int
	for i := range t.Heads {
		if !t.Wide[i] || wide {
			shown = append(shown, i)
		}
	}
	if len(shown) == 0 {
		return nil
	}
	widths := make([]int, len(shown))
	for j, i := range shown {
		widths[j] = termtext.Width(termtext.Escape(t.Heads[i]))
	}
	for _, r := range t.Rows {
		for j, i := range shown {
			if i < len(r) {
				widths[j] = max(widths[j], termtext.Width(termtext.Escape(r[i].Text)))
			}
		}
	}
	var b strings.Builder
	line := func(cells []Cell) {
		var l strings.Builder
		for j, i := range shown {
			c := Cell{}
			if i < len(cells) {
				c = cells[i]
			}
			text := termtext.Escape(c.Text)
			pad := widths[j] - termtext.Width(text)
			if j > 0 {
				l.WriteString("  ")
			}
			last := j == len(shown)-1
			styled := text
			if c.Color != nil {
				styled = c.Color(text)
			}
			if t.Right[i] {
				l.WriteString(strings.Repeat(" ", pad) + styled)
			} else if last {
				l.WriteString(styled)
			} else {
				l.WriteString(styled + strings.Repeat(" ", pad))
			}
		}
		b.WriteString(strings.TrimRight(l.String(), " ") + "\n")
	}
	heads := make([]Cell, len(t.Heads))
	for i, h := range t.Heads {
		heads[i] = Cell{Text: h}
	}
	line(heads)
	for _, r := range t.Rows {
		line(r)
	}
	if t.Caption != "" {
		b.WriteString("\n" + termtext.EscapeLines(t.Caption) + "\n")
	}
	_, err := fmt.Fprint(w, b.String())
	return err
}
