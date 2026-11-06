package graphlayout

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// story is the graph of the story set on el9.
var story = [][2]string{
	{"munge", "pmix"}, {"munge", "pmix_v5"}, {"munge", "pmix_v4"}, {"pmix", "prrte"}, {"munge", "slurm"}, {"ucx", "slurm"},
	{"rocm-smi", "slurm"}, {"pmix_v5", "slurm"}, {"pmix_v4", "slurm"}, {"slurm", "slurm-singularity-exec"}, {"ucx", "openmpi"},
	{"pmix", "openmpi"}, {"prrte", "openmpi"}, {"munge", "slurm-singularity-exec"},
}

func nodesOf(edges [][2]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range edges {
		for _, n := range e {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

func depth(nodes []string, edges [][2]string) map[string]int {
	d := map[string]int{}
	for range nodes {
		for _, e := range edges {
			d[e[1]] = max(d[e[1]], d[e[0]]+1)
		}
	}
	return d
}

// crosses reports whether the axis-aligned segment a-b runs through the
// inside of the card n.
func crosses(a, b [2]float64, n Node) bool {
	x1, x2 := min(a[0], b[0]), max(a[0], b[0])
	y1, y2 := min(a[1], b[1]), max(a[1], b[1])
	ox := min(x2, n.X+n.W) - max(x1, n.X)
	oy := min(y2, n.Y+n.H) - max(y1, n.Y)
	switch {
	case y1 == y2: // horizontal
		return y1 > n.Y && y1 < n.Y+n.H && ox > 0
	case x1 == x2: // vertical
		return x1 > n.X && x1 < n.X+n.W && oy > 0
	}
	return ox > 0 && oy > 0
}

func check(t *testing.T, name string, nodes []string, edges [][2]string) {
	t.Helper()
	l := Compute(nodes, depth(nodes, edges), edges, Options{})
	if len(l.Nodes) != len(nodes) || len(l.Edges) != len(edges) {
		t.Fatalf("%s: %d nodes, %d edges", name, len(l.Nodes), len(l.Edges))
	}
	for i, a := range l.Nodes {
		if a.X < 0 || a.Y < 0 || a.X+a.W > l.Width || a.Y+a.H > l.Height {
			t.Errorf("%s: %s is outside the graph", name, a.ID)
		}
		for _, b := range l.Nodes[i+1:] {
			if a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H {
				t.Errorf("%s: %s and %s overlap", name, a.ID, b.ID)
			}
		}
	}
	for _, e := range l.Edges {
		if len(e.Points) < 2 {
			t.Errorf("%s: edge %s→%s has no route", name, e.From, e.To)
			continue
		}
		for k := 0; k+1 < len(e.Points); k++ {
			a, b := e.Points[k], e.Points[k+1]
			if a[0] != b[0] && a[1] != b[1] {
				t.Errorf("%s: edge %s→%s has a diagonal part", name, e.From, e.To)
			}
			for _, n := range l.Nodes {
				if crosses(a, b, n) {
					t.Errorf("%s: edge %s→%s crosses the card %s", name, e.From, e.To, n.ID)
				}
			}
		}
		if e.Path() == "" {
			t.Errorf("%s: no path", name)
		}
	}
}

func TestNoEdgeCrossesACard(t *testing.T) {
	check(t, "story", nodesOf(story), story)
	r := rand.New(rand.NewPCG(1, 2))
	for g := 0; g < 30; g++ {
		n := 3 + r.IntN(25)
		var nodes []string
		for i := 0; i < n; i++ {
			nodes = append(nodes, fmt.Sprintf("n%02d", i))
		}
		var edges [][2]string
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if r.IntN(6) == 0 {
					edges = append(edges, [2]string{nodes[i], nodes[j]})
				}
			}
		}
		check(t, fmt.Sprintf("random %d", g), nodes, edges)
	}
}

func TestClosure(t *testing.T) {
	up, down := Closure(nodesOf(story), story)
	if got := fmt.Sprint(up["openmpi"]); got != "[munge pmix prrte ucx]" {
		t.Errorf("up of openmpi: %s", got)
	}
	if got := fmt.Sprint(down["pmix"]); got != "[openmpi prrte]" {
		t.Errorf("down of pmix: %s", got)
	}
}
