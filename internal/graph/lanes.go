package graph

import (
	"strings"
	"unicode/utf8"
)

// LaneRow is one row of the lane graph of repoctl graph, as git log
// --graph draws it (terminal.html#graph): the row of a node, or a separator
// row between two layers (Node nil).
type LaneRow struct {
	Node *Node
	// Lanes is the text left of the glyph: one column per lane and one gap
	// column after each lane. A separator row has only Lanes.
	Lanes string
}

// Width gives the width of the row in columns, with the glyph.
func (r LaneRow) Width() int {
	w := utf8.RuneCountInString(r.Lanes)
	if r.Node != nil {
		w++
	}
	return w
}

// Lane characters.
const (
	laneDown      = "│"
	laneBranch    = "├" // a dependency that more nodes need, nothing from the left
	laneLast      = "└" // the last node that needs the dependency, nothing from the left
	laneCross     = "┼" // a dependency that more nodes need, a line from the left
	laneJoin      = "┴" // the last node that needs the dependency, a line from the left
	laneLine      = "─"
	laneASCIIDown = "|"
)

// Lanes draws the lanes of the graph. Each node gets the lane right of the
// rightmost open lane; a lane stays open from a node to the last node that
// needs it. In the row of a node, a line runs from its leftmost dependency
// to its glyph. ascii draws | + - for the box characters.
func Lanes(g *Graph, ascii bool) []LaneRow {
	var rows []LaneRow
	var open []*Node // lane -> node, nil for a free lane
	left := map[*Node]int{}
	for _, n := range g.Nodes {
		left[n] = len(n.Users)
	}
	ch := func(s string) string {
		if !ascii {
			return s
		}
		switch s {
		case laneDown:
			return laneASCIIDown
		case laneLine:
			return "-"
		case laneBranch, laneLast, laneCross, laneJoin:
			return "+"
		}
		return s
	}
	prev := -1
	for _, n := range g.Nodes {
		if prev >= 0 && n.Depth != prev {
			rows = append(rows, LaneRow{Lanes: separator(open, ch)})
		}
		prev = n.Depth
		lane := len(open)
		deps := map[*Node]bool{}
		for _, e := range n.Deps {
			deps[e.From] = true
		}
		first := -1
		for j, x := range open {
			if x != nil && deps[x] {
				first = j
				break
			}
		}
		var b strings.Builder
		for j := 0; j < lane; j++ {
			x := open[j]
			line := first >= 0 && j > first
			switch {
			case x != nil && deps[x]:
				last := left[x] == 1
				switch {
				case last && line:
					b.WriteString(ch(laneJoin))
				case last:
					b.WriteString(ch(laneLast))
				case line:
					b.WriteString(ch(laneCross))
				default:
					b.WriteString(ch(laneBranch))
				}
			case x != nil:
				b.WriteString(ch(laneDown))
			case line:
				b.WriteString(ch(laneLine))
			default:
				b.WriteString(" ")
			}
			if first >= 0 && j >= first {
				b.WriteString(ch(laneLine))
			} else {
				b.WriteString(" ")
			}
		}
		rows = append(rows, LaneRow{Node: n, Lanes: b.String()})
		for j, x := range open {
			if x != nil && deps[x] {
				left[x]--
				if left[x] == 0 {
					open[j] = nil
				}
			}
		}
		if len(n.Users) > 0 {
			open = append(open, n)
		}
		for len(open) > 0 && open[len(open)-1] == nil {
			open = open[:len(open)-1]
		}
	}
	return rows
}

func separator(open []*Node, ch func(string) string) string {
	var b strings.Builder
	for _, x := range open {
		if x != nil {
			b.WriteString(ch(laneDown))
		} else {
			b.WriteString(" ")
		}
		b.WriteString(" ")
	}
	return strings.TrimRight(b.String(), " ")
}
