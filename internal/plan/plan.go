package plan

import (
	"fmt"
	"sort"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/graph"
	"github.com/dennisklein/repoctl/internal/manifest"
)

// Staged finds the staged builds of the runs of this user on this host
// (build.html#staging). The run side implements it from the stagings and
// journals; a nil Staged finds nothing.
type Staged interface {
	// Find gives the newest build run from the lock that ended the node of
	// the package on the target ok with this commit, and that no publish on
	// the chain of latest took.
	Find(target, pkg, commit string) (StagedBuild, bool)
}

// StagedBuild is one staged node.
type StagedBuild struct {
	// Run is the run whose staging holds the node (fromRun).
	Run api.RunId
	// SRPM is the file name of the SRPM of the node in the staging.
	SRPM string
	// Missing reports that a file of the artifact.added events of the node
	// is gone from the staging or has another size: the node builds again
	// with the reason staging-missing.
	Missing bool
}

// NoStaged is a Staged without staged nodes.
type NoStaged struct{}

// Find finds nothing.
func (NoStaged) Find(string, string, string) (StagedBuild, bool) { return StagedBuild{}, false }

// Change is what the plan knows of the commits from the build in latest to
// the commit of a node.
type Change struct {
	// Subject is the subject line of the new commit.
	Subject string
	// Commits is the number of commits between the two; 0 when unknown.
	Commits int
	// Rebuild reports that the new commit is an empty commit "Rebuild:
	// TEXT" of repoctl rebuild; Text holds TEXT.
	Rebuild bool
	Text    string
	// RebuildOnly reports that every commit between the two is such a
	// rebuild commit: the reason is rebuild, not commit-changed.
	RebuildOnly bool
}

// Describer gives the Change of a package from one commit to another.
type Describer func(pkg, from, to string) Change

// Input is what Make needs.
type Input struct {
	Set *config.Set
	// Targets are the targets of the plan, after --target.
	Targets []string
	// Selection is the result of Select.
	Selection *Selection
	// Positive reports that the command line has a positive selector.
	Positive bool
	// From is lock or worktree.
	From                   string
	Force, WithDeps, Rdeps bool
	// Commits gives the commit of each pinned package; a package without
	// one is unpinned.
	Commits map[string]string
	// Graphs are the graphs of the targets of the plan.
	Graphs map[string]*graph.Graph
	// Manifests are the RepoManifests of latest of each active target; nil
	// for a target without a local copy.
	Manifests map[string]*manifest.Manifest
	Staged    Staged
	Describe  Describer
	// ABI reports that libabigail is installed (checks.abi).
	ABI bool
}

// Result is the plan.
type Result struct {
	Plan api.Plan
	// Warnings of the plan: lock.missing-package, run.staging-missing.
	Warnings []api.Problem
	// Unpinned are the nodes of packages without a lock line.
	Unpinned []api.NodeId
	// Selected are the nodes that the selectors and --with-deps or --rdeps
	// select; Needed are the dependencies that the selected build nodes
	// need.
	Selected, Needed map[api.NodeId]bool

	index map[api.NodeId]int
}

// Node gives the plan node with the id, or nil.
func (r *Result) Node(id api.NodeId) *api.PlanNode {
	i, ok := r.index[id]
	if !ok {
		return nil
	}
	return &r.Plan.Nodes[i]
}

// BuildCount gives the number of nodes with the action build.
func (r *Result) BuildCount() int { return r.Plan.Summary.Build }

// Make gives each node of the set its plan action and reasons
// (build.html#actions, #deps):
//
//   - current: the commit is in latest of the target, with the same source
//     package name (match key);
//   - staged: Staged finds an ok build of the commit in a run of this user;
//   - build: else, with the reasons commit-changed, rebuild, new-package,
//     new-target, forced and staging-missing;
//   - skip: not selected and not needed, not pinned, or removed by --target.
//
// A selected node with the action build needs its dependencies: they get
// their own action (a needed build node builds first). --force builds the
// selected nodes with the reason forced. --with-deps and --rdeps add the
// transitive dependencies or dependents to the selection.
func Make(in Input) *Result {
	if in.Staged == nil {
		in.Staged = NoStaged{}
	}
	if in.Describe == nil {
		in.Describe = func(string, string, string) Change { return Change{} }
	}
	inTargets := map[string]bool{}
	for _, t := range in.Targets {
		inTargets[t] = true
	}
	res := &Result{index: map[api.NodeId]int{}, Selected: map[api.NodeId]bool{}, Needed: map[api.NodeId]bool{}}
	p := &res.Plan
	p.Targets = append([]api.TargetId{}, in.Targets...)
	p.Baseline = "none"
	for _, t := range in.Targets {
		if in.Manifests[t] != nil {
			p.Baseline = "latest"
		}
	}
	sel := &api.Selection{Targets: p.Targets, From: in.From, Force: in.Force, WithDeps: in.WithDeps, Rdeps: in.Rdeps}
	if in.Positive && in.Selection != nil {
		sel.Packages = in.Selection.Packages()
	}
	if sel.Packages == nil {
		sel.Packages = []api.PackageId{}
	}
	p.Selection = sel

	// The nodes in target order, then package order.
	var order []api.NodeId
	for _, t := range in.Set.ActiveTargets() {
		for _, pk := range in.Set.Packages {
			for _, pt := range in.Set.PackageTargets(pk) {
				if pt == t.ID {
					order = append(order, api.Node(pk.ID, t.ID))
				}
			}
		}
	}
	nodes := map[api.NodeId]*api.PlanNode{}
	for _, id := range order {
		pkg, t := api.SplitNode(id)
		n := &api.PlanNode{Node: id}
		nodes[id] = n
		commit, pinned := in.Commits[pkg]
		switch {
		case !pinned || commit == "":
			n.Action, n.SkipBecause = api.PlanActionSkip, "unpinned"
			if inTargets[t] {
				res.Unpinned = append(res.Unpinned, id)
			}
			continue
		case !inTargets[t]:
			n.Action, n.SkipBecause = api.PlanActionSkip, "target-excluded"
			n.Commit = commit
			continue
		}
		n.Commit = commit
		var gn *graph.Node
		if g := in.Graphs[t]; g != nil {
			gn = g.Node(id)
		}
		name := ""
		if gn != nil && gn.Info != nil {
			n.EVR = gn.Info.EVR
			n.Deps = gn.DepIDs()
			if !gn.Info.Failed {
				name = gn.Info.Name
			}
		}
		natural(in, n, pkg, t, commit, name, res)
	}

	// The selection and its expansion.
	if in.Selection != nil {
		for _, id := range in.Selection.Nodes {
			res.Selected[id] = true
		}
	}
	if in.WithDeps || in.Rdeps {
		for _, id := range order {
			if !res.Selected[id] {
				continue
			}
			_, t := api.SplitNode(id)
			g := in.Graphs[t]
			if g == nil {
				continue
			}
			if in.WithDeps {
				walk(g, id, func(n *graph.Node) []*graph.Edge { return n.Deps }, true, res.Selected)
			}
			if in.Rdeps {
				walk(g, id, func(n *graph.Node) []*graph.Edge { return n.Users }, false, res.Selected)
			}
		}
	}
	if in.Force {
		for _, id := range order {
			n := nodes[id]
			if !res.Selected[id] || n.Action == api.PlanActionSkip {
				continue
			}
			if n.Action == api.PlanActionCurrent {
				// The commit is in latest: such a build is never staged
				// or published (build.html#force).
				n.PreviousCommit = n.Commit
			}
			if n.Action != api.PlanActionBuild {
				n.Action, n.Snapshot, n.FromRun = api.PlanActionBuild, "", ""
			}
			n.Reasons = append(n.Reasons, api.PlanReason{Code: api.PlanReasonCodeForced, To: n.Commit, Message: fmt.Sprintf("--force selected %s.", id)})
		}
	}
	// The dependencies that the selected build nodes need.
	var queue []api.NodeId
	for _, id := range order {
		if res.Selected[id] && nodes[id].Action == api.PlanActionBuild {
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, d := range nodes[id].Deps {
			if res.Selected[d] || res.Needed[d] {
				continue
			}
			res.Needed[d] = true
			if dn := nodes[d]; dn != nil && dn.Action == api.PlanActionBuild {
				queue = append(queue, d)
			}
		}
	}
	for _, id := range order {
		n := nodes[id]
		if n.Action == api.PlanActionSkip {
			continue
		}
		if !res.Selected[id] && !res.Needed[id] {
			n.Action, n.SkipBecause = api.PlanActionSkip, "not-selected"
			n.Reasons, n.Snapshot, n.FromRun, n.Checks = nil, "", "", nil
		}
	}
	for _, id := range order {
		n := nodes[id]
		if n.Action == api.PlanActionBuild {
			soname := "pending"
			if n.PreviousCommit == "" {
				soname = "none"
			}
			n.Checks = &api.NodeChecks{Soname: soname, ABI: "off"}
			if in.ABI {
				n.Checks.ABI = soname
			}
		}
	}

	// The warnings of the plan.
	seen := map[string]bool{}
	for _, id := range res.Unpinned {
		pkg, _ := api.SplitNode(id)
		if seen[pkg] {
			continue
		}
		seen[pkg] = true
		w := api.Problem{Code: api.PLockMissingPackage, Package: pkg,
			Message: fmt.Sprintf("%s has no line in repoctl.lock: the plan skips its nodes (not pinned)", pkg),
			Hint:    api.HintText("pin it", "repoctl pin "+pkg)}
		res.Warnings = append(res.Warnings, w)
	}

	p.Summary.ByTarget = map[string]api.PlanSummaryByTargetValue{}
	for i, id := range order {
		n := nodes[id]
		_, t := api.SplitNode(id)
		bt := p.Summary.ByTarget[t]
		switch n.Action {
		case api.PlanActionBuild:
			p.Summary.Build++
			bt.Build++
		case api.PlanActionCurrent:
			p.Summary.Current++
			bt.Current++
		case api.PlanActionStaged:
			p.Summary.Staged++
			bt.Staged++
		case api.PlanActionSkip:
			p.Summary.Skip++
			bt.Skip++
		}
		p.Summary.ByTarget[t] = bt
		p.Nodes = append(p.Nodes, *n)
		res.index[id] = i
	}
	for _, n := range p.Nodes {
		if n.Action == api.PlanActionBuild {
			for _, r := range n.Reasons {
				if r.Code == api.PlanReasonCodeStagingMissing {
					res.Warnings = append(res.Warnings, api.Problem{Code: api.PRunStagingMissing, Node: n.Node, Run: r.Run,
						Message: fmt.Sprintf("a file of %s is gone from the staging of %s: the plan builds it again", n.Node, r.Run)})
				}
			}
		}
	}
	return res
}

// natural gives a node the action of its own commit, without the
// selection.
func natural(in Input, n *api.PlanNode, pkg, target, commit, name string, res *Result) {
	m := in.Manifests[target]
	if m != nil {
		if _, ok := m.Find(commit, name); ok {
			n.Action, n.Snapshot = api.PlanActionCurrent, m.Publish
			return
		}
	}
	var prev *manifest.Build
	if m != nil {
		prev, _ = m.Newest(pkg, name)
	}
	if prev != nil {
		n.PreviousCommit = prev.Commit
	}
	sb, staged := in.Staged.Find(target, pkg, commit)
	if staged && !sb.Missing {
		n.Action, n.FromRun = api.PlanActionStaged, sb.Run
		return
	}
	n.Action = api.PlanActionBuild
	switch {
	case prev == nil:
		newTarget := false
		for t, om := range in.Manifests {
			if t == target || om == nil {
				continue
			}
			if b, ok := om.Newest(pkg, name); ok && b != nil {
				newTarget = true
			}
		}
		if newTarget {
			n.Reasons = append(n.Reasons, api.PlanReason{Code: api.PlanReasonCodeNewTarget, To: commit,
				Message: fmt.Sprintf("%s is new for %s.", pkg, target)})
		} else {
			n.Reasons = append(n.Reasons, api.PlanReason{Code: api.PlanReasonCodeNewPackage, To: commit,
				Message: fmt.Sprintf("%s is new in the set.", pkg)})
		}
	case prev.Commit == commit:
		// The same commit with another source package name.
		n.Reasons = append(n.Reasons, api.PlanReason{Code: api.PlanReasonCodeNewPackage, To: commit,
			Message: fmt.Sprintf("latest has no build of the source package %s of %s.", name, pkg)})
	default:
		ch := in.Describe(pkg, prev.Commit, commit)
		if ch.RebuildOnly {
			n.Reasons = append(n.Reasons, api.PlanReason{Code: api.PlanReasonCodeRebuild, From: prev.Commit, To: commit,
				Subject: ch.Subject, Text: ch.Text, Message: fmt.Sprintf("%s has a rebuild commit: %s.", pkg, ch.Text)})
			break
		}
		r := api.PlanReason{Code: api.PlanReasonCodeCommitChanged, From: prev.Commit, To: commit, Subject: ch.Subject, Commits: ch.Commits}
		switch {
		case ch.Commits == 1:
			r.Message = fmt.Sprintf("%s moved from %s to %s (1 commit: %s).", pkg, api.ShortCommit(prev.Commit), api.ShortCommit(commit), ch.Subject)
		case ch.Commits > 1:
			r.Message = fmt.Sprintf("%s moved from %s to %s (%d commits, the last: %s).", pkg, api.ShortCommit(prev.Commit), api.ShortCommit(commit), ch.Commits, ch.Subject)
		default:
			r.Message = fmt.Sprintf("%s moved from %s to %s.", pkg, api.ShortCommit(prev.Commit), api.ShortCommit(commit))
		}
		n.Reasons = append(n.Reasons, r)
		if ch.Rebuild {
			n.Reasons = append(n.Reasons, api.PlanReason{Code: api.PlanReasonCodeRebuild, From: prev.Commit, To: commit,
				Subject: ch.Subject, Text: ch.Text, Message: fmt.Sprintf("%s has a rebuild commit: %s.", pkg, ch.Text)})
		}
	}
	if staged && sb.Missing {
		n.Reasons = append(n.Reasons, api.PlanReason{Code: api.PlanReasonCodeStagingMissing, Run: sb.Run,
			Message: fmt.Sprintf("a file of the staging of %s is gone or has another size.", sb.Run)})
	}
}

// walk adds the nodes that next reaches from id, transitively, to set.
func walk(g *graph.Graph, id api.NodeId, next func(*graph.Node) []*graph.Edge, from bool, set map[api.NodeId]bool) {
	start := g.Node(id)
	if start == nil {
		return
	}
	stack := []*graph.Node{start}
	seen := map[*graph.Node]bool{start: true}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, e := range next(n) {
			m := e.To
			if from {
				m = e.From
			}
			if !seen[m] {
				seen[m] = true
				set[m.ID] = true
				stack = append(stack, m)
			}
		}
	}
}

// SortedNodes gives the plan nodes of a target in graph order: layer, then
// package order. Nodes without a graph node come last.
func SortedNodes(p *api.Plan, g *graph.Graph, target string) []api.PlanNode {
	pos := map[api.NodeId]int{}
	if g != nil {
		for i, n := range g.Nodes {
			pos[n.ID] = i
		}
	}
	var out []api.PlanNode
	for _, n := range p.Nodes {
		if _, t := api.SplitNode(n.Node); t == target {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		pi, oki := pos[out[i].Node]
		pj, okj := pos[out[j].Node]
		if oki != okj {
			return oki
		}
		return pi < pj
	})
	return out
}
