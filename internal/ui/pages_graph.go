package ui

import (
	"slices"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/ui/graphlayout"
)

// graphPage is the dependency graph of one target, as a DAG or as the
// matrix of packages and targets, with the panel of a selected node.
type graphPage struct {
	Set     string
	Target  string
	Targets []string
	View    string
	NA      *notAvailable
	Graph   *api.Graph
	Width   float64
	Height  float64
	Nodes   []graphNode
	Edges   []graphEdge
	Sel     *graphNode
	Matrix  []matrixRow
	Run     string
	Unres   []api.GraphUnresolvedItem
	Lanes   []graphLane
}

// graphLane is the band of one level of the DAG.
type graphLane struct {
	X, Y, W, H float64
	Level, N   int
}

type graphNode struct {
	ID, Package, EVR, State, Role string
	X, Y, W, H                    float64
	Up, Down                      []string
	Deps, Users                   []string
	Href                          string
	NVR                           string
	Line                          string
	Selected                      bool
}

type graphEdge struct {
	From, To, Path, SrcState string
	Via                      []string
}

func buildGraph(pr *pageReq) *built {
	targets := pr.set.Targets()
	p := graphPage{Set: pr.set.Name, Targets: targets, Target: pr.p("target"), View: pr.p("view")}
	if p.Target == "" && len(targets) > 0 {
		p.Target = targets[0]
	}
	if p.View != "matrix" {
		p.View = "dag"
	}
	runs := pr.s.runs.list(pr.s.o.Paths.Runs(pr.set.Name), 6)
	cli := pr.s.cliPrefix(pr.set.Dir) + " graph --target " + p.Target
	b := &built{Title: "Graph " + p.Target + " · " + pr.set.Name, CLI: []string{cli}}
	if p.View == "matrix" {
		p.Matrix, _ = pr.matrixAll(runs)
		b.Data = p
		b.Live = []liveRegion{{Event: "graph:matrix", Block: "graph-matrix", Data: p}}
		return b
	}
	res := pr.s.run(pr.ctx, pr.set, "graph", "--target", p.Target)
	var g *api.Graph
	for _, it := range Items[api.Graph](res) {
		if it.Target == p.Target || g == nil {
			it := it
			g = &it
		}
	}
	if g == nil {
		p.NA = na("The dependency graph", "repoctl graph", res)
		if p.NA == nil {
			p.NA = &notAvailable{What: "The dependency graph", Command: "repoctl graph"}
		}
		b.Data = p
		return b
	}
	p.Graph, p.Unres = g, g.Unresolved
	state, href, nvr, line := nodeStates(pr.set.Name, runs)
	ids := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		ids = append(ids, n.ID)
	}
	var edges [][2]string
	for _, e := range g.Edges {
		edges = append(edges, [2]string{e.From, e.To}) // from is built before to
	}
	lay := graphlayout.Compute(ids, layers(ids, edges), edges, graphlayout.Options{Margin: 32})
	up, down := graphlayout.Closure(ids, edges)
	p.Width, p.Height = lay.Width, lay.Height
	sel := pr.p("node")
	for i, n := range g.Nodes {
		ln := lay.Nodes[i]
		gn := graphNode{ID: n.ID, Package: n.Package, EVR: n.EVR, Role: n.Role, X: ln.X, Y: ln.Y, W: ln.W, H: ln.H,
			Up: up[n.ID], Down: down[n.ID], State: state[n.ID], Href: href[n.ID], NVR: nvr[n.ID], Line: line[n.ID], Selected: n.ID == sel}
		if gn.State == "" {
			gn.State = "none"
		}
		for _, e := range edges {
			if e[1] == n.ID {
				gn.Deps = append(gn.Deps, e[0])
			}
			if e[0] == n.ID {
				gn.Users = append(gn.Users, e[1])
			}
		}
		p.Nodes = append(p.Nodes, gn)
		if gn.Selected {
			gn := gn
			p.Sel = &gn
		}
	}
	lanes := map[int]*graphLane{}
	for _, n := range lay.Nodes {
		l, ok := lanes[n.Layer]
		if !ok {
			l = &graphLane{X: n.X - 6, Y: 4, W: n.W + 12, H: lay.Height - 8, Level: n.Layer}
			lanes[n.Layer] = l
		}
		l.N++
	}
	for i := 0; i < lay.Layers; i++ {
		if l, ok := lanes[i]; ok {
			p.Lanes = append(p.Lanes, *l)
		}
	}
	for i, e := range lay.Edges {
		ge := graphEdge{From: e.From, To: e.To, Path: e.Path(), SrcState: state[e.From]}
		if i < len(g.Edges) {
			ge.Via = g.Edges[i].Via
		}
		p.Edges = append(p.Edges, ge)
	}
	b.Data = p
	b.Live = []liveRegion{{Event: "graph:" + p.Target, Block: "graph-nodes", Data: p}}
	return b
}

// layers gives each node the length of its longest chain of dependencies.
func layers(ids []string, edges [][2]string) map[string]int {
	deps := map[string][]string{}
	for _, e := range edges {
		deps[e[1]] = append(deps[e[1]], e[0])
	}
	out := map[string]int{}
	busy := map[string]bool{}
	var depth func(n string) int
	depth = func(n string) int {
		if d, ok := out[n]; ok {
			return d
		}
		if busy[n] { // a cycle: cut it here
			return 0
		}
		busy[n] = true
		d := 0
		for _, m := range deps[n] {
			d = max(d, depth(m)+1)
		}
		busy[n] = false
		out[n] = d
		return d
	}
	for _, n := range ids {
		depth(n)
	}
	return out
}

// nodeStates gives the state of each node in the newest run that has it,
// a link to its log there, its first NVR and its last log line.
func nodeStates(set string, runs []*foldedRun) (state, href, nvr, line map[string]string) {
	state, href, nvr, line = map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}
	for _, f := range runs {
		if f.Publish() {
			continue
		}
		for _, n := range f.Run.Nodes {
			if _, ok := state[n.Node]; ok {
				continue
			}
			state[n.Node] = string(n.State)
			href[n.Node] = nodeHref(set, f.Run.ID, n.Node)
			if len(n.NVRs) > 0 {
				nvr[n.Node] = n.NVRs[0]
			}
			if n.State == api.NodeStateRunning || n.State == api.NodeStateFailed {
				line[n.Node] = f.LastLine[n.Node]
			}
		}
	}
	return state, href, nvr, line
}

// matrixAll gives the matrix with every package.
func (pr *pageReq) matrixAll(runs []*foldedRun) ([]matrixRow, int) {
	rows, _ := pr.matrix(nil, runs)
	if pr.set.Conf == nil {
		return rows, 0
	}
	// matrix leaves out the packages current everywhere; the graph shows all.
	have := map[string]bool{}
	for _, r := range rows {
		have[r.Package] = true
	}
	state, href, nvr, _ := nodeStates(pr.set.Name, runs)
	for _, p := range pr.set.Conf.Packages {
		if have[p.ID] {
			continue
		}
		row := matrixRow{Package: p.ID}
		pts := pr.set.Conf.PackageTargets(p)
		for _, t := range pr.set.Conf.ActiveTargets() {
			n := api.Node(p.ID, t.ID)
			row.Cells = append(row.Cells, matrixCell{Node: n, Target: t.ID, State: state[n], Href: href[n], NVR: nvr[n], Absent: !slices.Contains(pts, t.ID)})
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b matrixRow) int { return strings.Compare(a.Package, b.Package) })
	return rows, 0
}
