// Package graphlayout places the nodes of a dependency graph in layers,
// left to right, and routes the edges orthogonally in the gaps between the
// layers, so that no edge crosses a card.
//
// It is a small form of the layered (Sugiyama) layout of the design: layers
// from the dependency depth, virtual nodes for the edges that span more
// than one layer, barycenter sweeps for the order in each layer, and
// routes whose vertical parts run only in the gaps.
package graphlayout

import (
	"fmt"
	"slices"
	"strings"
)

// Options sets the sizes of the layout; a zero value takes the default.
type Options struct {
	CardW, CardH float64 // a card (default 168 × 52)
	GapX, GapY   float64 // between layers and between cards (default 64, 16)
	VirtualH     float64 // the slot of an edge that passes a layer (default 8)
	Margin       float64 // around the graph (default 16)
	Sweeps       int     // barycenter sweeps (default 8)
}

func (o Options) withDefaults() Options {
	def := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	def(&o.CardW, 168)
	def(&o.CardH, 52)
	def(&o.GapX, 64)
	def(&o.GapY, 16)
	def(&o.VirtualH, 8)
	def(&o.Margin, 16)
	if o.Sweeps == 0 {
		o.Sweeps = 8
	}
	return o
}

// Node is a placed card, or a virtual node of a long edge.
type Node struct {
	ID         string
	Layer      int
	X, Y, W, H float64
	Virtual    bool
}

// Edge is a routed edge from a dependency to its dependent.
type Edge struct {
	From, To string
	// Points are the corners of the route, from the right side of From
	// to the left side of To.
	Points [][2]float64
}

// Path gives the SVG path of the route.
func (e Edge) Path() string {
	var b strings.Builder
	for i, p := range e.Points {
		if i == 0 {
			fmt.Fprintf(&b, "M%g %g", p[0], p[1])
			continue
		}
		fmt.Fprintf(&b, "L%g %g", p[0], p[1])
	}
	return b.String()
}

// Layout is the placed graph.
type Layout struct {
	Width, Height float64
	Nodes         []Node // the cards, in the order of the input
	Edges         []Edge
	Layers        int
}

// Compute places the nodes, whose layers are given (0 for the nodes that
// need nothing), and routes the edges, each from a dependency (lower
// layer) to a dependent. An edge within one layer, or one that points
// back, is routed to the next layer of its source.
func Compute(nodes []string, layer map[string]int, edges [][2]string, o Options) Layout {
	o = o.withDefaults()
	type slot struct {
		id      string
		virtual bool
		pos     float64
	}
	maxL := 0
	for _, n := range nodes {
		maxL = max(maxL, layer[n])
	}
	layers := make([][]*slot, maxL+1)
	byID := map[string]*slot{}
	for _, n := range nodes {
		s := &slot{id: n}
		byID[n] = s
		layers[layer[n]] = append(layers[layer[n]], s)
	}
	for _, l := range layers {
		slices.SortFunc(l, func(a, b *slot) int { return strings.Compare(a.id, b.id) })
	}
	// Chains of slots, one per edge, with a virtual slot in each layer it passes.
	type chain struct {
		from, to string
		slots    []*slot
	}
	var chains []chain
	pred := map[*slot][]*slot{}
	succ := map[*slot][]*slot{}
	for i, e := range edges {
		a, b := byID[e[0]], byID[e[1]]
		if a == nil || b == nil {
			continue
		}
		la, lb := layer[e[0]], layer[e[1]]
		c := chain{from: e[0], to: e[1], slots: []*slot{a}}
		for l := la + 1; l < lb; l++ {
			v := &slot{id: fmt.Sprintf("\x00v%d.%d", i, l), virtual: true}
			layers[l] = append(layers[l], v)
			c.slots = append(c.slots, v)
		}
		c.slots = append(c.slots, b)
		for k := 1; k < len(c.slots); k++ {
			pred[c.slots[k]] = append(pred[c.slots[k]], c.slots[k-1])
			succ[c.slots[k-1]] = append(succ[c.slots[k-1]], c.slots[k])
		}
		chains = append(chains, c)
	}
	index := func() {
		for _, l := range layers {
			for i, s := range l {
				s.pos = float64(i)
			}
		}
	}
	index()
	bary := func(s *slot, nb []*slot) float64 {
		if len(nb) == 0 {
			return s.pos
		}
		sum := 0.0
		for _, n := range nb {
			sum += n.pos
		}
		return sum / float64(len(nb))
	}
	for sweep := 0; sweep < o.Sweeps; sweep++ {
		if sweep%2 == 0 {
			for l := 1; l < len(layers); l++ {
				keys := map[*slot]float64{}
				for _, s := range layers[l] {
					keys[s] = bary(s, pred[s])
				}
				slices.SortStableFunc(layers[l], func(a, b *slot) int { return cmpF(keys[a], keys[b]) })
				index()
			}
		} else {
			for l := len(layers) - 2; l >= 0; l-- {
				keys := map[*slot]float64{}
				for _, s := range layers[l] {
					keys[s] = bary(s, succ[s])
				}
				slices.SortStableFunc(layers[l], func(a, b *slot) int { return cmpF(keys[a], keys[b]) })
				index()
			}
		}
	}
	// Place: each layer a column, its slots stacked and centred.
	heights := make([]float64, len(layers))
	maxH := 0.0
	for l, sl := range layers {
		h := 0.0
		for i, s := range sl {
			if i > 0 {
				h += o.GapY
			}
			if s.virtual {
				h += o.VirtualH
			} else {
				h += o.CardH
			}
		}
		heights[l] = h
		maxH = max(maxH, h)
	}
	placed := map[*slot]Node{}
	for l, sl := range layers {
		x := o.Margin + float64(l)*(o.CardW+o.GapX)
		y := o.Margin + (maxH-heights[l])/2
		for _, s := range sl {
			h := o.CardH
			if s.virtual {
				h = o.VirtualH
			}
			placed[s] = Node{ID: s.id, Layer: l, X: x, Y: y, W: o.CardW, H: h, Virtual: s.virtual}
			y += h + o.GapY
		}
	}
	out := Layout{Width: 2*o.Margin + float64(len(layers))*o.CardW + float64(max(len(layers)-1, 0))*o.GapX, Height: 2*o.Margin + maxH, Layers: len(layers)}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, placed[byID[n]])
	}
	for ci, c := range chains {
		var pts [][2]float64
		for k := 0; k+1 < len(c.slots); k++ {
			a, b := placed[c.slots[k]], placed[c.slots[k+1]]
			y1, y2 := a.Y+a.H/2, b.Y+b.H/2
			x1, x2 := a.X+a.W, b.X
			if k == 0 {
				pts = append(pts, [2]float64{x1, y1})
			} else {
				// Through the virtual slot: across its column at its height.
				pts = append(pts, [2]float64{x1, y1})
			}
			if b.X <= a.X {
				// Same layer or back: go out to the gap after the source and come back.
				gx := a.X + a.W + o.GapX/2
				pts = append(pts, [2]float64{gx, y1}, [2]float64{gx, y2}, [2]float64{b.X + b.W, y2})
				continue
			}
			// Spread the vertical parts of the edges across the gap.
			mid := x1 + o.GapX*(0.3+0.4*float64(ci%5)/4)
			if y1 != y2 {
				pts = append(pts, [2]float64{mid, y1}, [2]float64{mid, y2})
			}
			pts = append(pts, [2]float64{x2, y2})
			if b.Virtual {
				pts = append(pts, [2]float64{b.X + b.W, y2})
			}
		}
		out.Edges = append(out.Edges, Edge{From: c.from, To: c.to, Points: dedupe(pts)})
	}
	return out
}

func dedupe(pts [][2]float64) [][2]float64 {
	out := pts[:0]
	for _, p := range pts {
		if len(out) > 0 && out[len(out)-1] == p {
			continue
		}
		out = append(out, p)
	}
	return out
}

func cmpF(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Closure gives, for each node, the nodes it depends on (up) and the
// nodes that depend on it (down), transitively.
func Closure(nodes []string, edges [][2]string) (up, down map[string][]string) {
	deps := map[string][]string{}
	rdeps := map[string][]string{}
	for _, e := range edges {
		rdeps[e[0]] = append(rdeps[e[0]], e[1])
		deps[e[1]] = append(deps[e[1]], e[0])
	}
	walk := func(start string, next map[string][]string) []string {
		seen := map[string]bool{start: true}
		stack := []string{start}
		var out []string
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, m := range next[n] {
				if !seen[m] {
					seen[m] = true
					out = append(out, m)
					stack = append(stack, m)
				}
			}
		}
		slices.Sort(out)
		return out
	}
	up, down = map[string][]string{}, map[string][]string{}
	for _, n := range nodes {
		up[n] = walk(n, deps)
		down[n] = walk(n, rdeps)
	}
	return up, down
}
