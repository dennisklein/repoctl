// Package graph makes the dependency graph of one target from the SRPMs of
// its nodes (docs/design/build.html#graph): an edge from A to B means that
// B needs a package of A to build. Both nodes of an edge are on the same
// target. The requires of each SRPM are matched against the provides of
// the other set packages on the same target; a requirement that only a
// node of another target provides gives the warning dep.not-for-target.
package graph

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
)

// Info is what the graph knows of one node: the facts of its SRPM.
type Info struct {
	Node    api.NodeId
	Package string
	Target  string
	// Role is packages[].role: package or release.
	Role string
	// Name is the Name of the SRPM (the source package name).
	Name    string
	Version string
	EVR     string
	// Requires are the requires of the SRPM (its BuildRequires in the chroot
	// of the target).
	Requires []string
	// Provides are the capabilities of the binary packages, without version.
	Provides []string
	// RPMs are the names of the binary packages.
	RPMs []string
	// Dynamic reports %generate_buildrequires.
	Dynamic bool
	// Spec is the file name of the spec, for the hints.
	Spec string
	// Failed reports that the SRPM of the node failed at plan time: the node
	// has no known dependencies and provides nothing that the graph knows.
	Failed bool
}

// Node is one node of a graph.
type Node struct {
	ID      api.NodeId
	Package string
	Info    *Info
	// Depth is the layer: an L0 node needs no node of the set.
	Depth int
	// Deps are the edges from the dependencies of the node, in package order.
	Deps []*Edge
	// Users are the edges to the nodes that need this node, in graph order.
	Users []*Edge
	// Unprovided are the requires that no node of the set provides; the base
	// or EPEL repos of the target provide them (graph -v lists them).
	Unprovided []string

	order int
}

// DepIDs gives the ids of the dependencies of the node, in package order.
func (n *Node) DepIDs() []api.NodeId {
	out := make([]api.NodeId, len(n.Deps))
	for i, e := range n.Deps {
		out[i] = e.From.ID
	}
	return out
}

// DepPackages gives the packages of the dependencies, in package order.
func (n *Node) DepPackages() []string {
	out := make([]string, len(n.Deps))
	for i, e := range n.Deps {
		out[i] = e.From.Package
	}
	return out
}

// UserPackages gives the packages of the nodes that need this node, in
// package order.
func (n *Node) UserPackages() []string {
	us := append([]*Edge(nil), n.Users...)
	sort.SliceStable(us, func(i, j int) bool { return us[i].To.order < us[j].To.order })
	out := make([]string, len(us))
	for i, e := range us {
		out[i] = e.To.Package
	}
	return out
}

// Edge is one edge: To needs a package of From to build.
type Edge struct {
	From, To *Node
	// Via are the requires of To that a Provides of From matched.
	Via []string
}

// Graph is the graph of one target.
type Graph struct {
	Target string
	// Nodes are in graph order: layer, then the package order of
	// repoctl.yaml.
	Nodes []*Node
	Edges []*Edge
	// Layers is the number of layers.
	Layers int
	// Unresolved are the requires that only a node of another target
	// provides.
	Unresolved []api.GraphUnresolvedItem
	// Dynamic are the nodes with %generate_buildrequires.
	Dynamic []api.NodeId

	byID map[api.NodeId]*Node
}

// Node gives the node with the id, or nil.
func (g *Graph) Node(id api.NodeId) *Node { return g.byID[id] }

// ByPackage gives the node of a package, or nil.
func (g *Graph) ByPackage(pkg string) *Node { return g.byID[api.Node(pkg, g.Target)] }

// Input is what New needs for the graph of one target.
type Input struct {
	Target string
	// RHEL is the major release of the chroot (%rhel), for the hint of
	// dep.not-for-target.
	RHEL string
	// Nodes are the infos of the nodes of the target, in the package order
	// of repoctl.yaml.
	Nodes []*Info
	// Others are the infos of the nodes of the other targets, for
	// dep.not-for-target.
	Others []*Info
	// NotBuilt reports the packages that are not built for the target
	// (packages[].targets).
	NotBuilt map[string]bool
}

// New makes the graph of a target. It gives the warnings dep.not-for-target,
// graph.dynamic-buildrequires and graph.ambiguous-provider, and the error
// graph.cycle (exit 1) when the edges make a cycle.
func New(in Input) (*Graph, []api.Problem, error) {
	g := &Graph{Target: in.Target, byID: map[api.NodeId]*Node{}}
	var nodes []*Node
	for i, info := range in.Nodes {
		n := &Node{ID: info.Node, Package: info.Package, Info: info, order: i}
		nodes = append(nodes, n)
		g.byID[n.ID] = n
	}
	provides := map[string][]*Node{}
	for _, n := range nodes {
		if n.Info.Failed {
			continue
		}
		for _, p := range n.Info.Provides {
			provides[p] = append(provides[p], n)
		}
	}
	others := map[string][]*Info{}
	for _, o := range in.Others {
		if o.Failed || o.Target == in.Target {
			continue
		}
		for _, p := range o.Provides {
			others[p] = append(others[p], o)
		}
	}
	var warns []api.Problem
	edges := map[[2]api.NodeId]*Edge{}
	for _, n := range nodes {
		if n.Info.Failed {
			continue
		}
		if n.Info.Dynamic {
			g.Dynamic = append(g.Dynamic, n.ID)
			warns = append(warns, problem(api.PGraphDynamicBuildrequires, n.ID,
				fmt.Sprintf("%s has %%generate_buildrequires: its edges can be missing until the build", n.ID),
				"add the BuildRequires to "+specOf(n.Info)))
		}
		self := map[string]bool{}
		for _, p := range n.Info.Provides {
			self[p] = true
		}
		for _, req := range n.Info.Requires {
			var found []*Node
			selfOnly := false
			for _, name := range RequireNames(req) {
				for _, p := range provides[name] {
					if p != n {
						found = append(found, p)
					}
				}
				if self[name] {
					selfOnly = true
				}
			}
			found = uniqueNodes(found)
			if len(found) == 0 {
				if selfOnly {
					continue
				}
				var on []*Info
				for _, name := range RequireNames(req) {
					on = append(on, others[name]...)
				}
				on = uniqueInfos(on)
				if len(on) == 0 {
					n.Unprovided = append(n.Unprovided, req)
					continue
				}
				item := api.GraphUnresolvedItem{Node: n.ID, Require: req}
				for _, o := range on {
					item.ProvidedOn = append(item.ProvidedOn, o.Node)
				}
				g.Unresolved = append(g.Unresolved, item)
				warns = append(warns, notForTarget(in, n, req, on))
				continue
			}
			if pk := packagesOf(found); len(pk) > 1 {
				ids := make([]string, len(found))
				for i, f := range found {
					ids[i] = f.ID
				}
				warns = append(warns, problem(api.PGraphAmbiguousProvider, n.ID,
					fmt.Sprintf("%s needs %s, and %s provide it", n.ID, req, joinAnd(ids)),
					"make the Provides unique, or require a name that one package provides"))
			}
			for _, f := range found {
				k := [2]api.NodeId{f.ID, n.ID}
				e, ok := edges[k]
				if !ok {
					e = &Edge{From: f, To: n}
					edges[k] = e
					n.Deps = append(n.Deps, e)
					f.Users = append(f.Users, e)
				}
				e.Via = appendOnce(e.Via, req)
			}
		}
		sort.SliceStable(n.Deps, func(i, j int) bool { return n.Deps[i].From.order < n.Deps[j].From.order })
	}
	if err := layer(g, nodes); err != nil {
		return nil, warns, err
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Depth != nodes[j].Depth {
			return nodes[i].Depth < nodes[j].Depth
		}
		return nodes[i].order < nodes[j].order
	})
	g.Nodes = nodes
	pos := map[*Node]int{}
	for i, n := range nodes {
		pos[n] = i
	}
	for _, n := range nodes {
		g.Edges = append(g.Edges, n.Deps...)
		sort.SliceStable(n.Users, func(i, j int) bool { return pos[n.Users[i].To] < pos[n.Users[j].To] })
	}
	return g, warns, nil
}

// layer gives each node its depth: 0 without dependencies, else one more
// than its deepest dependency. A cycle gives graph.cycle.
func layer(g *Graph, nodes []*Node) error {
	indeg := map[*Node]int{}
	for _, n := range nodes {
		indeg[n] = len(n.Deps)
	}
	var queue []*Node
	for _, n := range nodes {
		if indeg[n] == 0 {
			queue = append(queue, n)
		}
	}
	done := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		done++
		if n.Depth+1 > g.Layers {
			g.Layers = n.Depth + 1
		}
		for _, e := range n.Users {
			if e.To.Depth < n.Depth+1 {
				e.To.Depth = n.Depth + 1
			}
			indeg[e.To]--
			if indeg[e.To] == 0 {
				queue = append(queue, e.To)
			}
		}
	}
	if done == len(nodes) {
		return nil
	}
	cycle := findCycle(nodes, indeg)
	ids := make([]string, len(cycle))
	for i, n := range cycle {
		ids[i] = n.ID
	}
	e := api.Errorf(api.PGraphCycle, "the BuildRequires make a cycle on %s: %s", g.Target, strings.Join(ids, " → ")).
		WithTarget(g.Target).WithHint("break the cycle in one spec", "")
	if len(cycle) > 0 {
		e = e.WithNode(cycle[0].ID)
	}
	return e
}

// findCycle gives one cycle among the nodes that Kahn's algorithm left,
// with its first node at the end again.
func findCycle(nodes []*Node, indeg map[*Node]int) []*Node {
	left := map[*Node]bool{}
	for _, n := range nodes {
		if indeg[n] > 0 {
			left[n] = true
		}
	}
	state := map[*Node]int{} // 1 on the stack, 2 done
	var stack []*Node
	var found []*Node
	var visit func(n *Node) bool
	visit = func(n *Node) bool {
		state[n] = 1
		stack = append(stack, n)
		for _, e := range n.Users {
			m := e.To
			if !left[m] {
				continue
			}
			if state[m] == 1 {
				for i, s := range stack {
					if s == m {
						found = append(append([]*Node{}, stack[i:]...), m)
						return true
					}
				}
			}
			if state[m] == 0 && visit(m) {
				return true
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = 2
		return false
	}
	for _, n := range nodes {
		if left[n] && state[n] == 0 && visit(n) {
			return found
		}
	}
	return found
}

func notForTarget(in Input, n *Node, req string, on []*Info) api.Problem {
	ids := make([]string, len(on))
	pkgs := map[string]bool{}
	var pkgList []string
	for i, o := range on {
		ids[i] = o.Node
		if !pkgs[o.Package] {
			pkgs[o.Package] = true
			pkgList = append(pkgList, o.Package)
		}
	}
	verb := "provides"
	if len(ids) > 1 {
		verb = "provide"
	}
	msg := fmt.Sprintf("%s needs %s; only %s %s it", n.ID, req, joinAnd(ids), verb)
	guard := "guard the BuildRequires in " + specOf(n.Info) + " with %if"
	if in.RHEL != "" {
		guard += " 0%{?rhel} < " + in.RHEL
	}
	var alt []string
	for _, p := range pkgList {
		if in.NotBuilt[p] {
			alt = append(alt, fmt.Sprintf("add %s to the targets of %s", in.Target, p))
		} else {
			alt = append(alt, fmt.Sprintf("make %s provide it on %s", p, in.Target))
		}
	}
	return problem(api.PDepNotForTarget, n.ID, msg, guard+", or "+strings.Join(alt, ", or "))
}

func problem(code api.ProblemCode, node api.NodeId, msg, hint string) api.Problem {
	p := api.Problem{Code: code, Message: msg, Node: node, Hint: hint}
	p.Package, p.Target = api.SplitNode(node)
	return p
}

func specOf(i *Info) string {
	if i.Spec != "" {
		return i.Spec
	}
	return i.Package + ".spec"
}

func joinAnd(s []string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

func appendOnce(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func uniqueNodes(in []*Node) []*Node {
	seen := map[*Node]bool{}
	var out []*Node
	for _, n := range in {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].order < out[j].order })
	return out
}

func uniqueInfos(in []*Info) []*Info {
	seen := map[api.NodeId]bool{}
	var out []*Info
	for _, n := range in {
		if !seen[n.Node] {
			seen[n.Node] = true
			out = append(out, n)
		}
	}
	return out
}

func packagesOf(ns []*Node) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range ns {
		if !seen[n.Package] {
			seen[n.Package] = true
			out = append(out, n.Package)
		}
	}
	return out
}

// RequireNames gives the capability names of one requirement: the name of
// a plain requirement, or each name of a rich dependency such as
// (foo-devel or bar-devel).
func RequireNames(req string) []string {
	r := strings.TrimSpace(req)
	if !strings.HasPrefix(r, "(") {
		name, _, _ := strings.Cut(r, " ")
		return []string{name}
	}
	var out []string
	ops := map[string]bool{"and": true, "or": true, "if": true, "else": true, "with": true, "without": true, "unless": true}
	skipVersion := false
	for _, tok := range strings.FieldsFunc(r, func(c rune) bool { return c == '(' || c == ')' || c == ' ' }) {
		switch {
		case skipVersion:
			skipVersion = false
		case ops[tok]:
		case tok == "=" || tok == "<" || tok == ">" || tok == "<=" || tok == ">=":
			skipVersion = true
		default:
			out = append(out, tok)
		}
	}
	return out
}

// Sub gives the graph of the nodes that keep reports, with the edges
// between them, for a selection. The depths stay those of the whole graph:
// an L0 node needs no node of the set.
func (g *Graph) Sub(keep func(api.NodeId) bool) *Graph {
	sub := &Graph{Target: g.Target, byID: map[api.NodeId]*Node{}}
	for _, n := range g.Nodes {
		if !keep(n.ID) {
			continue
		}
		c := &Node{ID: n.ID, Package: n.Package, Info: n.Info, Depth: n.Depth, Unprovided: n.Unprovided, order: n.order}
		sub.byID[c.ID] = c
		sub.Nodes = append(sub.Nodes, c)
		sub.Layers = max(sub.Layers, n.Depth+1)
	}
	for _, n := range g.Nodes {
		to := sub.byID[n.ID]
		if to == nil {
			continue
		}
		for _, e := range n.Deps {
			if from := sub.byID[e.From.ID]; from != nil {
				ne := &Edge{From: from, To: to, Via: e.Via}
				to.Deps = append(to.Deps, ne)
				from.Users = append(from.Users, ne)
				sub.Edges = append(sub.Edges, ne)
			}
		}
	}
	for _, u := range g.Unresolved {
		if keep(u.Node) {
			sub.Unresolved = append(sub.Unresolved, u)
		}
	}
	for _, d := range g.Dynamic {
		if keep(d) {
			sub.Dynamic = append(sub.Dynamic, d)
		}
	}
	return sub
}

// API gives the Graph item of --json.
func (g *Graph) API() api.Graph {
	out := api.Graph{Target: g.Target, Unresolved: g.Unresolved, Dynamic: g.Dynamic}
	for _, n := range g.Nodes {
		gn := api.GraphNode{ID: n.ID, Package: n.Package, Depth: n.Depth}
		if n.Info != nil {
			gn.EVR = n.Info.EVR
			gn.Role = n.Info.Role
		}
		out.Nodes = append(out.Nodes, gn)
	}
	for _, e := range g.Edges {
		out.Edges = append(out.Edges, api.GraphEdge{From: e.From.ID, To: e.To.ID, Via: e.Via})
	}
	return out
}
