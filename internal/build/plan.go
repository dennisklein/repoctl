package build

import (
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/plan"
	"github.com/dennisklein/repoctl/internal/staging"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// Planner makes the plan of a build: the first step of a run
// (build.html#plan). Engine implements it with plan.Compute; SpecPlanner
// reads the BuildRequires of the specs without mock.
type Planner interface {
	Plan(ctx context.Context, r PlanRequest) (*Plan, error)
}

// PlanRequest is what a plan reads.
type PlanRequest struct {
	Set    *config.Set
	SetDir string
	Paths  config.Paths
	// Lock is repoctl.lock as the run read it.
	Lock *lock.File
	// Selection is the result of the selectors and --target.
	Selection *plan.Selection
	From      workspace.From
	Force     bool
	WithDeps  bool
	Rdeps     bool
	// Staged is the staged index of the set on this host.
	Staged *staging.Index
	// Mock runs mock for the plan root, when the planner needs one.
	Mock *mock.Mock
	// Selectors are the parsed selectors of the command line and Here the
	// package of the current directory, for planners that select again.
	Selectors []plan.Selector
	Here      string
	// Work is the work directory of the plan (wrapper configs, source
	// clones); the run keeps it until its end.
	Work string
	// Log gets the mock output of the SRPMs of the plan; nil discards it.
	Log io.Writer
}

// Plan is the plan of a run: the plan of the contract, the source of each
// package and the warnings of the plan.
type Plan struct {
	api.Plan
	// Sources are the sources of the packages of the plan, by package.
	Sources map[api.PackageId]workspace.Source
	// Warnings are the warnings of the plan, for example
	// dep.not-for-target. The run writes each as a warning event.
	Warnings []api.Problem
	// NotForTarget are the BuildRequires that only a package of other
	// targets provides (dep.not-for-target), for the %if hint of a failed
	// node. A planner may leave it empty.
	NotForTarget []NotForTarget
}

// NotForTarget is one BuildRequires of a node that only a package of other
// targets provides.
type NotForTarget struct {
	Node     api.NodeId
	Requires string
	// Package provides it on Targets.
	Package string
	Targets []api.TargetId
}

// Node gives the plan node of a node id.
func (p *Plan) Node(id api.NodeId) (*api.PlanNode, bool) {
	for i := range p.Nodes {
		if p.Nodes[i].Node == id {
			return &p.Nodes[i], true
		}
	}
	return nil, false
}

// Summarize sets the summary from the actions of the nodes.
func (p *Plan) Summarize() {
	s := api.PlanSummary{ByTarget: map[string]api.PlanSummaryByTargetValue{}}
	for _, n := range p.Nodes {
		_, t := api.SplitNode(n.Node)
		bt := s.ByTarget[t]
		switch n.Action {
		case api.PlanActionBuild:
			s.Build++
			bt.Build++
		case api.PlanActionCurrent:
			s.Current++
			bt.Current++
		case api.PlanActionStaged:
			s.Staged++
			bt.Staged++
		case api.PlanActionSkip:
			s.Skip++
			bt.Skip++
		}
		s.ByTarget[t] = bt
	}
	p.Summary = s
}

// SpecPlanner is a plan without mock: it reads the dependencies of each
// node from the BuildRequires of its spec at the commit, and the actions
// from the local copy of latest and the staged index. repoctl build uses
// Engine; SpecPlanner serves tests and hosts without a plan root.
type SpecPlanner struct {
	// Edges replaces the dependencies that the planner reads from the
	// specs: the dependencies of each node.
	Edges map[api.NodeId][]api.NodeId
}

// Plan makes the plan.
func (sp SpecPlanner) Plan(ctx context.Context, r PlanRequest) (*Plan, error) {
	srcs, err := workspace.Sources(ctx, r.Paths, r.Set, r.SetDir, r.Lock, r.From)
	if err != nil {
		return nil, err
	}
	targets := r.Selection.Targets
	inTarget := map[string]bool{}
	for _, t := range targets {
		inTarget[t] = true
	}
	var nodes []api.NodeId
	for _, n := range r.Set.Nodes() {
		if _, t := api.SplitNode(n); inTarget[t] {
			nodes = append(nodes, n)
		}
	}
	p := &Plan{Sources: map[api.PackageId]workspace.Source{}}
	p.Targets = targets
	edges := sp.Edges
	if edges == nil {
		edges, p.Warnings, p.NotForTarget = specEdges(ctx, r, srcs, nodes)
	}
	latest := map[string]*manifest.Manifest{}
	for _, t := range targets {
		m, err := manifest.ReadLocal(r.Paths, r.Set, t)
		if err != nil {
			return nil, err
		}
		latest[t] = m
	}
	selected := map[api.NodeId]bool{}
	for _, n := range r.Selection.Nodes {
		selected[n] = true
	}
	if r.Force && (r.WithDeps || r.Rdeps) {
		for _, n := range closure(nodes, edges, r.Selection.Nodes, r.WithDeps, r.Rdeps) {
			selected[n] = true
		}
	}
	// The first action of each node, without the selection.
	action := map[api.NodeId]*api.PlanNode{}
	for _, id := range nodes {
		pkg, t := api.SplitNode(id)
		pn := &api.PlanNode{Node: id, Deps: edges[id]}
		src, ok := srcs[pkg]
		switch {
		case !ok:
			pn.Action, pn.SkipBecause = api.PlanActionSkip, "unpinned"
		default:
			pn.Commit = src.Commit
			sp.action(r, pn, pkg, t, src, latest[t], selected[id])
		}
		action[id] = pn
	}
	// A selected build node needs its dependencies; a needed build node
	// needs its own.
	needed := map[api.NodeId]bool{}
	var walk func(id api.NodeId)
	walk = func(id api.NodeId) {
		if needed[id] {
			return
		}
		needed[id] = true
		if action[id] == nil || action[id].Action != api.PlanActionBuild {
			return
		}
		for _, d := range edges[id] {
			walk(d)
		}
	}
	for _, id := range nodes {
		if selected[id] {
			walk(id)
		}
	}
	for _, id := range nodes {
		pn := action[id]
		if !needed[id] && pn.Action != api.PlanActionSkip {
			pn.Action, pn.SkipBecause, pn.Reasons, pn.FromRun = api.PlanActionSkip, "not selected", nil, ""
		}
		if pn.Action == api.PlanActionBuild {
			pn.Checks = &api.NodeChecks{Soname: "pending"}
		}
		p.Nodes = append(p.Nodes, *pn)
		if pn.Action != api.PlanActionSkip {
			pkg, _ := api.SplitNode(id)
			p.Sources[pkg] = srcs[pkg]
		}
	}
	p.Selection = &api.Selection{Targets: targets, From: string(r.From), Force: r.Force, WithDeps: r.WithDeps, Rdeps: r.Rdeps}
	p.Baseline = "latest"
	p.Summarize()
	p.Warnings = append(p.Warnings, stagingWarnings(p)...)
	return p, nil
}

// action gives the action of one node: current when latest holds the
// commit, staged when an intact staging of this host holds it, else build.
// --force builds a selected node in each case.
func (sp SpecPlanner) action(r PlanRequest, pn *api.PlanNode, pkg, target string, src workspace.Source, m *manifest.Manifest, selected bool) {
	if m != nil {
		if b, ok := m.Find(src.Commit, ""); ok && b.Package == pkg {
			pn.Action, pn.Snapshot = api.PlanActionCurrent, m.Publish
			if r.Force && selected {
				pn.Action, pn.Snapshot = api.PlanActionBuild, ""
				pn.Reasons = []api.PlanReason{{Code: api.PlanReasonCodeForced, Message: "--force builds the current node"}}
			}
			return
		}
	}
	reasons := []api.PlanReason{}
	if m == nil {
		reasons = append(reasons, api.PlanReason{Code: api.PlanReasonCodeNewPackage, Message: "latest of " + target + " has no build of " + pkg})
	} else if prev, ok := m.Newest(pkg, ""); ok {
		pn.PreviousCommit = prev.Commit
		reasons = append(reasons, api.PlanReason{Code: api.PlanReasonCodeCommitChanged, Message: "the package commit changed",
			From: prev.Commit, To: src.Commit})
	} else {
		reasons = append(reasons, api.PlanReason{Code: api.PlanReasonCodeNewPackage, Message: "latest of " + target + " has no build of " + pkg})
	}
	if r.Staged != nil && r.From != workspace.FromWorktree {
		if e, ok := r.Staged.Lookup(target, pkg, src.Commit); ok {
			if e.Intact() {
				pn.Action, pn.FromRun = api.PlanActionStaged, e.Run
				if r.Force && selected {
					pn.Action, pn.FromRun = api.PlanActionBuild, ""
					pn.Reasons = append(reasons, api.PlanReason{Code: api.PlanReasonCodeForced, Message: "--force builds the staged node"})
				}
				return
			}
			reasons = append(reasons, api.PlanReason{Code: api.PlanReasonCodeStagingMissing,
				Message: "a file of the staging of " + e.Run + " is gone", Run: e.Run})
		}
	}
	pn.Action, pn.Reasons = api.PlanActionBuild, reasons
	if r.Force && selected {
		pn.Reasons = append(pn.Reasons, api.PlanReason{Code: api.PlanReasonCodeForced, Message: "--force"})
	}
}

func stagingWarnings(p *Plan) []api.Problem {
	var out []api.Problem
	for _, n := range p.Nodes {
		for _, r := range n.Reasons {
			if r.Code == api.PlanReasonCodeStagingMissing {
				w := api.Warning(api.PRunStagingMissing, "%s: a file of the staging of run %s is gone; the node builds again", n.Node, r.Run)
				w.Node, w.Run = n.Node, r.Run
				w.Package, w.Target = api.SplitNode(n.Node)
				out = append(out, w)
			}
		}
	}
	return out
}

// closure gives the nodes that --with-deps and --rdeps add.
func closure(nodes []api.NodeId, edges map[api.NodeId][]api.NodeId, roots []api.NodeId, deps, rdeps bool) []api.NodeId {
	rev := map[api.NodeId][]api.NodeId{}
	for _, n := range nodes {
		for _, d := range edges[n] {
			rev[d] = append(rev[d], n)
		}
	}
	seen := map[api.NodeId]bool{}
	var out []api.NodeId
	var walk func(n api.NodeId, next map[api.NodeId][]api.NodeId)
	walk = func(n api.NodeId, next map[api.NodeId][]api.NodeId) {
		for _, d := range next[n] {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
				walk(d, next)
			}
		}
	}
	for _, r := range roots {
		if deps {
			walk(r, edges)
		}
		if rdeps {
			walk(r, rev)
		}
	}
	return out
}

// specInfo is what the planner reads from a spec.
type specInfo struct {
	provides      []string
	buildRequires []string
}

// specEdges reads the dependencies of the nodes from the specs: a node
// depends on the node of the same target whose package provides one of
// its BuildRequires. A BuildRequires that only a package of another target
// provides gives the warning dep.not-for-target.
func specEdges(ctx context.Context, r PlanRequest, srcs map[api.PackageId]workspace.Source, nodes []api.NodeId) (map[api.NodeId][]api.NodeId, []api.Problem, []NotForTarget) {
	info := map[api.PackageId]specInfo{}
	for pkg, src := range srcs {
		text, err := readSpec(ctx, r.Paths, src)
		if err != nil {
			continue
		}
		info[pkg] = parseSpec(text)
	}
	byTarget := map[string]map[string]api.PackageId{}
	targetsOf := map[api.PackageId][]string{}
	for _, n := range nodes {
		pkg, t := api.SplitNode(n)
		if byTarget[t] == nil {
			byTarget[t] = map[string]api.PackageId{}
		}
		for _, pr := range info[pkg].provides {
			byTarget[t][pr] = pkg
		}
		targetsOf[pkg] = append(targetsOf[pkg], t)
	}
	allProviders := map[string]api.PackageId{}
	for _, pk := range r.Set.Packages {
		for _, pr := range info[pk.ID].provides {
			allProviders[pr] = pk.ID
		}
	}
	edges := map[api.NodeId][]api.NodeId{}
	var warnings []api.Problem
	var nft []NotForTarget
	for _, n := range nodes {
		pkg, t := api.SplitNode(n)
		for _, br := range info[pkg].buildRequires {
			if q, ok := byTarget[t][br]; ok {
				if q != pkg && !slices.Contains(edges[n], api.Node(q, t)) {
					edges[n] = append(edges[n], api.Node(q, t))
				}
				continue
			}
			if q, ok := allProviders[br]; ok && q != pkg {
				only := joinNodes(q, targetsOf[q])
				if only == "" {
					only = q
				}
				w := api.Warning(api.PDepNotForTarget, "%s requires %s. Only %s provides it; %s is not built for %s.", n, br, only, q, t)
				w.Node, w.Target, w.Package = n, t, pkg
				warnings = append(warnings, w)
				nft = append(nft, NotForTarget{Node: n, Requires: br, Package: q, Targets: targetsOf[q]})
			}
		}
	}
	return edges, warnings, nft
}

func joinNodes(pkg string, targets []string) string {
	var out []string
	for _, t := range targets {
		out = append(out, api.Node(pkg, t))
	}
	return strings.Join(out, ", ")
}

// readSpec reads the spec of a source at its commit.
func readSpec(ctx context.Context, p config.Paths, src workspace.Source) (string, error) {
	if err := ensureRepo(ctx, src); err != nil {
		return "", err
	}
	g := git.New(src.Repo)
	dir := src.Path
	out, err := g.Run(ctx, "ls-tree", "--name-only", src.Commit+":"+dir)
	if err != nil {
		return "", err
	}
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasSuffix(name, ".spec") {
			return g.Run(ctx, "show", src.Commit+":"+path.Join(dir, name))
		}
	}
	return "", os.ErrNotExist
}

// ensureRepo makes the bare clone of an external package when it is not
// there yet.
func ensureRepo(ctx context.Context, src workspace.Source) error {
	if _, err := os.Stat(src.Repo); err == nil || src.URL == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(src.Repo), 0o755); err != nil {
		return err
	}
	_, err := git.New(filepath.Dir(src.Repo)).Run(ctx, "clone", "--quiet", "--bare", src.URL, src.Repo)
	return err
}

// parseSpec reads the Name, the subpackages, the Provides and the
// BuildRequires of a spec, without macro expansion.
func parseSpec(text string) specInfo {
	var si specInfo
	name := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, ":")
		if ok && !strings.ContainsAny(key, " \t") {
			switch strings.ToLower(key) {
			case "name":
				name = strings.TrimSpace(val)
				si.provides = append(si.provides, name)
			case "provides":
				si.provides = append(si.provides, depNames(val)...)
			case "buildrequires":
				si.buildRequires = append(si.buildRequires, depNames(val)...)
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "%package"); ok {
			f := strings.Fields(rest)
			switch {
			case len(f) >= 2 && f[0] == "-n":
				si.provides = append(si.provides, f[1])
			case len(f) >= 1 && name != "":
				si.provides = append(si.provides, name+"-"+f[0])
			}
		}
	}
	return si
}

// depNames gives the names of a dependency list: "a, b >= 1.0 c" gives a,
// b and c.
func depNames(list string) []string {
	var out []string
	skip := false
	for _, f := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if skip {
			skip = false
			continue
		}
		if strings.ContainsAny(f[:1], "<>=") {
			skip = true
			continue
		}
		if strings.HasPrefix(f, "%") || strings.HasPrefix(f, "(") {
			continue
		}
		out = append(out, f)
	}
	return out
}
