package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/GSI-HPC/go-clikit/cobratree"
	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/graph"
	"github.com/dennisklein/repoctl/internal/journal"
	"github.com/dennisklein/repoctl/internal/localcopy"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/plan"
	"github.com/dennisklein/repoctl/internal/srpm"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// The handlers of graph and plan: graph, plan, list, show. They read only:
// no set lock, the host mock lock shared while they run mock, the plan root
// (mock.PlanExt), and the SRPM and source caches (build.html#graph, #mock).

// StagedOf gives the staged nodes of the runs of this user on this host for
// the plan commands (build.html#staging). The run side sets it; nil finds
// no staged node.
var StagedOf func(a *App, set *config.Set) plan.Staged

// planOpts are the options of a plan computation.
type planOpts struct {
	selectors       []plan.Selector
	targets         []string
	from            workspace.From
	force           bool
	withDeps, rdeps bool
	// fresh are the results of the freshness check by target; empty
	// without store.oci.
	fresh map[string]localcopy.Freshness
}

// computePlan builds the SRPMs, the graphs and the plan of the set under
// the host mock lock, in a work directory that it removes at the end.
func (a *App) computePlan(ctx context.Context, set *config.Set, setDir string, o planOpts) (*plan.Computed, error) {
	eff, err := a.Settings()
	if err != nil {
		return nil, err
	}
	p := a.Paths()
	hl, err := lock.Acquire(ctx, p.HostMockLock(), lock.Holder{Command: a.cmdName(), PID: os.Getpid(), Host: hostname(), User: username(a.Getenv), Started: a.Now()},
		a.LockWait(), true, lock.What{Code: api.PMockBusy, Name: "mock on this host", Lock: "the host mock lock"})
	if err != nil {
		return nil, err
	}
	defer hl.Release()
	work := p.MockWork(set.Name, "plan-"+strconv.Itoa(os.Getpid()))
	defer os.RemoveAll(work)
	m := mock.New(p, mock.Options{NoSync: eff.Bool("mock.nosync"), TmpFS: eff.String("mock.tmpfs")})
	env := plan.Env{Paths: p, Set: set, SetDir: setDir, Host: srpm.NewHost(m, p), Work: work, Uniqueext: mock.PlanExt()}
	if StagedOf != nil {
		env.Staged = StagedOf(a, set)
	}
	var shown bool
	if !a.quiet && cliprogress.IsTerminal(a.Stderr) && a.Getenv("TERM") != "dumb" {
		env.Progress = func(node api.NodeId, done bool, _ error) {
			if !done {
				fmt.Fprintf(a.Stderr, "\r\x1b[K%s %s", a.style.Copper(a.style.Mark("running")), a.style.Dim("srpm "+node+" in mock"))
				shown = true
			}
		}
	}
	c, err := plan.Compute(ctx, env, plan.Request{
		Selectors: o.selectors, Here: a.Here(set, setDir), Targets: o.targets, From: o.from,
		Force: o.force, WithDeps: o.withDeps, Rdeps: o.rdeps,
	})
	if shown {
		fmt.Fprint(a.Stderr, "\r\x1b[K")
	}
	return c, err
}

// planFlags reads the flags of graph and plan.
func planFlags(cmd *cobra.Command) (planOpts, error) {
	var o planOpts
	fl := cmd.Flags()
	o.withDeps, _ = fl.GetBool("with-deps")
	o.rdeps, _ = fl.GetBool("rdeps")
	if f := fl.Lookup("force"); f != nil {
		o.force, _ = fl.GetBool("force")
	}
	o.from = workspace.FromLock
	if f := fl.Lookup("from"); f != nil {
		switch v := f.Value.String(); v {
		case "lock", "":
		case "worktree":
			o.from = workspace.FromWorktree
		default:
			return o, usagef("--from %s: want lock or worktree", v)
		}
	}
	return o, nil
}

func (a *App) warnAll(ws []api.Problem) {
	for _, w := range ws {
		a.Warn(w)
	}
}

// --- graph -----------------------------------------------------------------

func hGraph(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	format, _ := cmd.Flags().GetString("format")
	switch format {
	case "text", "matrix", "dot":
	case "json":
		a.jsonOut = true
	default:
		return nil, usagef("--format %s: want text, matrix, dot or json", format)
	}
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	targets, err := a.Targets(set, true)
	if err != nil {
		return nil, err
	}
	sels, err := plan.ParseSelectors(args)
	if err != nil {
		return nil, err
	}
	o, err := planFlags(cmd)
	if err != nil {
		return nil, err
	}
	o.selectors, o.targets = sels, targets
	runID, _ := cmd.Flags().GetString("run")
	var states map[api.NodeId]string
	var runNodes map[api.NodeId]*api.NodeResult
	if runID != "" {
		runID, states, runNodes, err = readRun(a.Paths(), set.Name, runID)
		if err != nil {
			return nil, err
		}
	}
	c, err := a.computePlan(ctx, set, setDir, o)
	if c != nil {
		a.warnAll(c.GraphWarnings)
	}
	if err != nil {
		return nil, err
	}
	a.warnAll(c.Result.Warnings)
	positive := false
	for _, s := range sels {
		positive = positive || !s.Not
	}
	view := &graphView{c: c, targets: targets, states: states, runNodes: runNodes, run: runID, set: set}
	for _, t := range targets {
		g := c.Graphs[t]
		if g == nil {
			continue
		}
		if positive || o.withDeps || o.rdeps {
			g = g.Sub(func(id api.NodeId) bool { return c.Selected[id] || c.Needed[id] })
		}
		view.graphs = append(view.graphs, g)
	}
	res := &Result{Kind: api.KindGraph, Targets: targets, Run: runID}
	for _, g := range view.graphs {
		res.Items = append(res.Items, g.API())
	}
	res.Human = func(out *Out) error {
		switch format {
		case "matrix":
			return view.matrix(out)
		case "dot":
			return view.dot(out)
		}
		return view.text(out)
	}
	return res, nil
}

// graphView draws the graphs of a command.
type graphView struct {
	c       *plan.Computed
	set     *config.Set
	targets []string
	graphs  []*graph.Graph
	states  map[api.NodeId]string
	// runNodes are the nodes of the run of --run.
	runNodes map[api.NodeId]*api.NodeResult
	run      string
}

// state gives the plan action of a node, or its state in the run.
func (v *graphView) state(id api.NodeId) string {
	if v.states != nil {
		if s, ok := v.states[id]; ok {
			return s
		}
		return "skip"
	}
	if n := v.c.Node(id); n != nil {
		return string(n.Action)
	}
	return "skip"
}

// stateOrder is the order of the counts of a header or a totals line.
var stateOrder = []string{"failed", "blocked", "canceled", "running", "queued", "waiting", "ok", "build", "staged", "current", "skip"}

func (v *graphView) counts(ids []api.NodeId) map[string]int {
	out := map[string]int{}
	for _, id := range ids {
		out[v.state(id)]++
	}
	return out
}

func countText(s Style, counts map[string]int, sep string) string {
	var parts []string
	for _, st := range stateOrder {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d %s", s.Mark(st), counts[st], st))
		}
	}
	return strings.Join(parts, sep)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func (v *graphView) text(out *Out) error {
	st := out.StdoutStyle
	for i, g := range v.graphs {
		if i > 0 {
			out.Printf("\n")
		}
		var ids []api.NodeId
		for _, n := range g.Nodes {
			ids = append(ids, n.ID)
		}
		cfg := ""
		if t, ok := v.set.Target(g.Target); ok {
			cfg = t.Mock
		}
		head := fmt.Sprintf("%s  %s  %s, %s, %s", st.Bold(g.Target), st.Dim(cfg), plural(len(g.Nodes), "node"), plural(len(g.Edges), "edge"), plural(g.Layers, "layer"))
		if c := countText(st, v.counts(ids), "  "+st.Dim("·")+"  "); c != "" {
			head += "  " + st.Dim("·") + "  " + c
		}
		if v.run != "" {
			head += "  " + st.Dim("run "+v.run)
		}
		out.Printf("%s\n\n", head)
		rows := graph.Lanes(g, st.ASCII())
		laneW, nameW, verW, actW := 0, 0, 0, 0
		for _, r := range rows {
			laneW = max(laneW, r.Width())
			if r.Node != nil {
				nameW = max(nameW, termtext.Width(r.Node.Package))
				verW = max(verW, termtext.Width(v.version(r.Node)))
				actW = max(actW, len(v.state(r.Node.ID)))
			}
		}
		for _, r := range rows {
			if r.Node == nil {
				out.Printf("  %s\n", r.Lanes)
				continue
			}
			n := r.Node
			state := v.state(n.ID)
			name := pad(n.Package, nameW)
			if state == "build" || state == "failed" || state == "running" {
				name = st.Bold(n.Package) + strings.Repeat(" ", nameW-termtext.Width(n.Package))
			}
			tail := fmt.Sprintf("L%d", n.Depth)
			if deps := n.DepPackages(); len(deps) > 0 {
				tail += "  ← " + strings.Join(deps, " ")
			}
			line := fmt.Sprintf("  %s%s%s  %s  %s  %s  %s", r.Lanes, st.Mark(state), strings.Repeat(" ", laneW-r.Width()),
				name, pad(v.version(n), verW), pad(st.State(state), actW+len(st.State(state))-len(state)), st.Dim(tail))
			out.Printf("%s\n", strings.TrimRight(line, " "))
			if out.Verbose > 0 && len(n.Unprovided) > 0 {
				out.Printf("  %s  %s\n", strings.Repeat(" ", laneW), st.Dim("from the base repos: "+strings.Join(n.Unprovided, " ")))
			}
		}
	}
	return nil
}

// version gives the version of a node: of the lock, or with --run of the
// run. A node of the run shows the version of its SRPM, or the version of
// the lock when the run had the same commit; else "-", so that the states
// of an old run never stand next to the versions of the lock.
func (v *graphView) version(n *graph.Node) string {
	if v.runNodes == nil {
		return nodeVersion(n)
	}
	rn := v.runNodes[n.ID]
	if rn == nil {
		return "-"
	}
	if ver := srpmVersion(rn.SRPM); ver != "" {
		return ver
	}
	if pn := v.c.Node(n.ID); pn != nil && rn.Commit != "" && pn.Commit == rn.Commit {
		return nodeVersion(n)
	}
	return "-"
}

// srpmVersion gives the version of an SRPM file name:
// munge-0.5.18-1.el9.src.rpm gives 0.5.18.
func srpmVersion(file string) string {
	nvr := strings.TrimSuffix(file, ".src.rpm")
	if nvr == file || nvr == "" {
		return ""
	}
	i := strings.LastIndex(nvr, "-")
	if i <= 0 {
		return ""
	}
	j := strings.LastIndex(nvr[:i], "-")
	if j < 0 {
		return ""
	}
	return nvr[j+1 : i]
}

func nodeVersion(n *graph.Node) string {
	if n.Info == nil || n.Info.Version == "" {
		return "-"
	}
	return n.Info.Version
}

func pad(s string, w int) string {
	return s + strings.Repeat(" ", max(0, w-termtext.Width(s)))
}

// matrix draws one row per package and one column per target.
func (v *graphView) matrix(out *Out) error {
	st := out.StdoutStyle
	heads := []string{"PACKAGE", "VERSION"}
	for _, t := range v.targets {
		heads = append(heads, strings.ToUpper(t))
	}
	heads = append(heads, "NEEDS")
	tb := NewTable(heads...)
	byTarget := map[string]*graph.Graph{}
	for _, g := range v.graphs {
		byTarget[g.Target] = g
	}
	var all []api.NodeId
	notBuilt := map[string][]string{}
	for _, pkg := range packageOrder(v.set, v.graphs) {
		version := ""
		row := []Cell{{Text: pkg}, {}}
		shown := false
		var needs []string
		needOn := map[string][]string{}
		inAll := 0
		for _, t := range v.targets {
			g := byTarget[t]
			var n *graph.Node
			if g != nil {
				n = g.ByPackage(pkg)
			}
			if n == nil {
				row = append(row, Cell{Text: markText(st, "")})
				if g != nil && !v.builtOn(pkg, t) {
					notBuilt[t] = append(notBuilt[t], pkg)
				}
				continue
			}
			shown = true
			inAll++
			all = append(all, n.ID)
			if version == "" || version == "-" {
				version = v.version(n)
			}
			row = append(row, Cell{Text: st.Mark(v.state(n.ID))})
			for _, d := range n.DepPackages() {
				if _, ok := needOn[d]; !ok {
					needs = append(needs, d)
				}
				needOn[d] = append(needOn[d], t)
			}
		}
		if !shown {
			continue
		}
		row[1] = Cell{Text: version}
		var parts []string
		for _, d := range orderPackages(v.set, needs) {
			if len(needOn[d]) < inAll {
				parts = append(parts, d+"("+strings.Join(needOn[d], ",")+")")
			} else {
				parts = append(parts, d)
			}
		}
		needText := "-"
		if len(parts) > 0 {
			needText = strings.Join(parts, " ")
		}
		row = append(row, Cell{Text: needText})
		tb.RowCells(row...)
	}
	if err := out.Table(tb); err != nil {
		return err
	}
	total := fmt.Sprintf("%s: %s", plural(len(all), "node"), countText(st, v.counts(all), ", "))
	var nb []string
	for _, t := range v.targets {
		if len(notBuilt[t]) > 0 {
			nb = append(nb, strings.Join(notBuilt[t], ", ")+" "+isAre(len(notBuilt[t]))+" not built for "+t)
		}
	}
	if len(nb) > 0 {
		total += "; " + strings.Join(nb, "; ")
	}
	out.Printf("%s\n", total)
	return nil
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func markText(st Style, state string) string {
	if state == "" {
		return "·"
	}
	return st.Mark(state)
}

func (v *graphView) builtOn(pkg, target string) bool {
	pk, ok := v.set.Package(pkg)
	if !ok {
		return false
	}
	for _, t := range v.set.PackageTargets(*pk) {
		if t == target {
			return true
		}
	}
	return false
}

// dot writes Graphviz input: one cluster per target.
func (v *graphView) dot(out *Out) error {
	var b strings.Builder
	fmt.Fprintf(&b, "digraph %q {\n  rankdir=LR;\n  node [shape=box, fontname=\"sans-serif\"];\n", v.set.Name)
	for _, g := range v.graphs {
		fmt.Fprintf(&b, "  subgraph %q {\n    label=%q;\n", "cluster_"+g.Target, g.Target)
		for _, n := range g.Nodes {
			label := n.Package + "\\n" + v.version(n) + "\\n" + v.state(n.ID)
			style := ""
			switch v.state(n.ID) {
			case "build":
				style = ", style=bold"
			case "failed":
				style = ", color=red"
			case "blocked":
				style = ", color=orange"
			}
			fmt.Fprintf(&b, "    %q [label=\"%s\"%s];\n", n.ID, label, style)
		}
		for _, e := range g.Edges {
			fmt.Fprintf(&b, "    %q -> %q [tooltip=%q];\n", e.From.ID, e.To.ID, strings.Join(e.Via, " "))
		}
		b.WriteString("  }\n")
	}
	b.WriteString("}\n")
	out.Printf("%s", b.String())
	return nil
}

// packageOrder gives the packages of the graphs in graph order: the
// deepest layer of the package, then the order of repoctl.yaml.
func packageOrder(set *config.Set, gs []*graph.Graph) []string {
	depth := map[string]int{}
	for _, g := range gs {
		for _, n := range g.Nodes {
			if d, ok := depth[n.Package]; !ok || n.Depth > d {
				depth[n.Package] = n.Depth
			}
		}
	}
	var out []string
	for _, pk := range set.Packages {
		if _, ok := depth[pk.ID]; ok {
			out = append(out, pk.ID)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return depth[out[i]] < depth[out[j]] })
	return out
}

// orderPackages sorts package ids in the order of repoctl.yaml.
func orderPackages(set *config.Set, ids []string) []string {
	pos := map[string]int{}
	for i, pk := range set.Packages {
		pos[pk.ID] = i
	}
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool { return pos[out[i]] < pos[out[j]] })
	return out
}

// readRun folds the journal of a run as runs and logs do (journal.Find),
// and gives the state of each node and the node. RUN latest names the
// newest build run of the set on this host: only a build run has node
// states (cli.html#runs).
func readRun(p config.Paths, set, runID string) (string, map[api.NodeId]string, map[api.NodeId]*api.NodeResult, error) {
	kind := ""
	if runID == "latest" {
		kind = journal.KindBuild
	}
	e, err := journal.Find(journal.Readers(p, set, hostname()), set, runID, kind)
	if err != nil {
		return "", nil, nil, err
	}
	r := e.Run()
	states := map[api.NodeId]string{}
	nodes := map[api.NodeId]*api.NodeResult{}
	for i := range r.Nodes {
		n := &r.Nodes[i]
		states[n.Node] = string(n.State)
		nodes[n.Node] = n
	}
	return r.ID, states, nodes, nil
}

// --- plan ------------------------------------------------------------------

func hPlan(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	targets, err := a.Targets(set, true)
	if err != nil {
		return nil, err
	}
	sels, err := plan.ParseSelectors(args)
	if err != nil {
		return nil, err
	}
	o, err := planFlags(cmd)
	if err != nil {
		return nil, err
	}
	o.selectors, o.targets = sels, targets
	check, _ := cmd.Flags().GetBool("check")
	// First the freshness check: latest against the local copy
	// (build.html#plan).
	fresh, warns, err := rbFreshPlan(ctx, a, set, setDir, targets)
	a.warnAll(warns)
	if err != nil {
		return nil, err
	}
	o.fresh = fresh
	c, err := a.computePlan(ctx, set, setDir, o)
	if c != nil {
		a.warnAll(c.GraphWarnings)
	}
	if err != nil {
		return nil, err
	}
	a.warnAll(c.Result.Warnings)
	var failure error
	if o.from == workspace.FromLock {
		dirty, warns := plan.CheckWorktrees(ctx, set, setDir, c.Lock, c.Result)
		a.warnAll(warns)
		if dirty != nil {
			failure = dirty
		}
	}
	res := &Result{Kind: api.KindPlan, Targets: targets, Items: []any{c.Plan}, ShownWarnings: true}
	res.Human = func(out *Out) error { return a.planHuman(out, c, o) }
	if failure == nil && check && c.Plan.Summary.Build > 0 {
		failure = exitOnly(api.ExitDifferences)
	}
	return res, failure
}

// reasonText gives the REASON cell of a plan row.
func reasonText(st Style, n api.PlanNode) string {
	switch n.Action {
	case api.PlanActionStaged:
		return "from " + n.FromRun
	case api.PlanActionSkip:
		switch n.SkipBecause {
		case "unpinned":
			return "not pinned"
		case "target-excluded":
			return "not in --target"
		}
		return "not selected"
	case api.PlanActionCurrent:
		return n.Snapshot
	}
	var parts []string
	for _, r := range n.Reasons {
		switch r.Code {
		case api.PlanReasonCodeCommitChanged:
			parts = append(parts, fmt.Sprintf("commit-changed %s %s %s", api.ShortCommit(r.From), st.Arrow(), api.ShortCommit(r.To)))
		case api.PlanReasonCodeRebuild:
			parts = append(parts, fmt.Sprintf("rebuild %q", r.Text))
		case api.PlanReasonCodeStagingMissing:
			parts = append(parts, "staging-missing "+r.Run)
		default:
			parts = append(parts, string(r.Code))
		}
	}
	return strings.Join(parts, ", ")
}

// planRow is one row of the plan table: a package with the same action and
// reason on some targets.
type planRow struct {
	pkg, action, reason string
	targets             []string
	nodes               []api.NodeId
}

func (a *App) planHuman(out *Out, c *plan.Computed, o planOpts) error {
	st := out.StdoutStyle
	set := a.set
	p := c.Plan
	from := "repoctl.lock"
	if o.from == workspace.FromWorktree {
		from = "the worktrees (unpinned)"
	}
	head := fmt.Sprintf("plan  %s  from %s  base %s", set.Name, from, baseText(c.Manifests, p.Targets, o.fresh))
	out.Printf("%s\n\n", head)

	gs := make([]*graph.Graph, 0, len(p.Targets))
	for _, t := range p.Targets {
		if g := c.Graphs[t]; g != nil {
			gs = append(gs, g)
		}
	}
	pkgs := packageOrder(set, gs)
	for _, pk := range set.Packages {
		if !contains(pkgs, pk.ID) {
			pkgs = append(pkgs, pk.ID)
		}
	}
	var rows []*planRow
	folded := map[string][]string{} // target -> current packages
	var total []api.NodeId
	for _, pkg := range pkgs {
		var mine []*planRow
		for _, t := range p.Targets {
			n := c.Node(api.Node(pkg, t))
			if n == nil {
				continue
			}
			total = append(total, n.Node)
			if out.Verbose == 0 {
				if n.Action == api.PlanActionCurrent {
					folded[t] = append(folded[t], pkg)
					continue
				}
				if n.Action == api.PlanActionSkip {
					continue
				}
			}
			reason := reasonText(st, *n)
			merged := false
			for _, r := range mine {
				if r.action == string(n.Action) && r.reason == reason {
					r.targets = append(r.targets, t)
					r.nodes = append(r.nodes, n.Node)
					merged = true
				}
			}
			if !merged {
				mine = append(mine, &planRow{pkg: pkg, action: string(n.Action), reason: reason, targets: []string{t}, nodes: []api.NodeId{n.Node}})
			}
		}
		rows = append(rows, mine...)
	}
	// A warning of a node goes under the row of the node (terminal.html#plan).
	shown := map[string]bool{}
	key := func(w api.Problem) string { return w.Code + "\x00" + w.Node + "\x00" + w.Message }
	if len(rows) > 0 {
		pkgW, tW, aW := len("PACKAGE"), len("TARGETS"), len("ACTION")
		for _, r := range rows {
			pkgW = max(pkgW, termtext.Width(r.pkg))
			tW = max(tW, termtext.Width(strings.Join(r.targets, " ")))
			aW = max(aW, len(r.action))
		}
		reasonCol := pkgW + tW + aW + 6
		out.Printf("%s\n", strings.TrimRight(fmt.Sprintf("%s  %s  %s  %s", pad("PACKAGE", pkgW), pad("TARGETS", tW), pad("ACTION", aW), "REASON"), " "))
		for _, r := range rows {
			out.Printf("%s\n", strings.TrimRight(fmt.Sprintf("%s  %s  %s  %s", pad(r.pkg, pkgW), pad(strings.Join(r.targets, " "), tW), pad(r.action, aW), termtext.Escape(r.reason)), " "))
			for _, id := range r.nodes {
				for _, w := range a.warnings {
					if w.Node == id && !shown[key(w)] {
						shown[key(w)] = true
						out.Printf("%s%s %s\n", strings.Repeat(" ", reasonCol), st.Mark("warning"), termtext.Escape(w.Message))
					}
				}
			}
		}
	}
	for _, t := range p.Targets {
		if names := folded[t]; len(names) > 0 {
			out.Printf("%s  %d current: %s\n", pad(t, maxTargetWidth(p.Targets)), len(names), strings.Join(names, " "))
		}
	}
	counts := map[string]int{}
	for _, id := range total {
		if n := c.Node(id); n != nil {
			counts[string(n.Action)]++
		}
	}
	var parts []string
	for _, a := range []string{"build", "staged", "current", "skip"} {
		if counts[a] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[a], a))
		}
	}
	line := fmt.Sprintf("%s: %s", plural(len(total), "node"), strings.Join(parts, ", "))
	if out.Verbose == 0 && (counts["current"] > 0 || counts["skip"] > 0) {
		line += "  (-v shows all)"
	}
	out.Printf("%s\n", line)

	// The other warnings go to stderr, with their hints.
	for _, w := range a.warnings {
		if !shown[key(w)] {
			out.Warning(w)
		}
	}
	return nil
}

func maxTargetWidth(ts []string) int {
	w := 0
	for _, t := range ts {
		w = max(w, len(t))
	}
	return w
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// baseText names the base of a plan: the publish of latest of each target
// in the local copy.
func baseText(ms map[string]*manifest.Manifest, targets []string, fresh map[string]localcopy.Freshness) string {
	ids := map[string][]string{}
	var order []string
	for _, t := range targets {
		id := "none"
		if m := ms[t]; m != nil {
			id = m.Publish
		}
		if _, ok := ids[id]; !ok {
			order = append(order, id)
		}
		ids[id] = append(ids[id], t)
	}
	copyText := "(" + freshText(targets, fresh) + ")"
	if len(order) == 1 {
		if order[0] == "none" {
			for _, f := range fresh {
				if f.Result == localcopy.FreshUnreachable {
					return "none (no local copy; registry unknown)"
				}
			}
			return "none (no snapshot yet)"
		}
		return "latest " + order[0] + " " + copyText
	}
	var parts []string
	for _, id := range order {
		parts = append(parts, strings.Join(ids[id], " ")+" "+id)
	}
	return "latest " + strings.Join(parts, ", ") + " " + copyText
}

// freshText gives the words of the freshness check in the plan head:
// "local copy same", or per target when the targets differ.
func freshText(targets []string, fresh map[string]localcopy.Freshness) string {
	if len(fresh) == 0 {
		return "local copy"
	}
	word := func(f localcopy.Freshness) string {
		switch f.Result {
		case localcopy.FreshSame, localcopy.FreshFirst:
			return "local copy same"
		case localcopy.FreshUnpushed:
			return "local copy, " + f.Publish + " not pushed"
		case localcopy.FreshMissing:
			return "local copy, not in the registry"
		case localcopy.FreshUnreachable:
			return "local copy, registry unknown"
		}
		return "local copy " + f.Result
	}
	var words []string
	seen := map[string]bool{}
	for _, t := range targets {
		f, ok := fresh[t]
		if !ok {
			continue
		}
		w := word(f)
		if !seen[w] {
			seen[w] = true
			words = append(words, w)
		}
	}
	if len(words) <= 1 {
		if len(words) == 0 {
			return "local copy"
		}
		return words[0]
	}
	var parts []string
	for _, t := range targets {
		if f, ok := fresh[t]; ok {
			parts = append(parts, t+" "+word(f))
		}
	}
	return strings.Join(parts, "; ")
}

// --- list and show ---------------------------------------------------------

// packageItem gives the Package item of a package from a computed plan:
// the source, the lock, and one row per target of targets.
func packageItem(set *config.Set, c *plan.Computed, pk config.Package, targets []string) api.Package {
	item := api.Package{Package: pk.ID, Role: "package", Notes: pk.Notes}
	if pk.Role != "" {
		item.Role = pk.Role
	}
	src := c.Sources[pk.ID]
	switch {
	case pk.InTree():
		item.Source = api.PackageSource{Kind: "tree", Path: pk.Path}
	case pk.External():
		item.Source = api.PackageSource{Kind: "external", Branch: pk.Branch, URL: pk.URL(), Mirror: src.Repo}
	default:
		item.Source = api.PackageSource{Kind: "branch", Branch: pk.Branch}
	}
	if e, ok := c.Lock.Get(pk.ID); ok && !pk.InTree() {
		item.Lock = &api.PackageLock{Commit: e.Commit}
	}
	if pk.Upstream != nil {
		item.Upstream = &api.PackageUpstream{Github: pk.Upstream.GitHub, Tags: pk.Upstream.Tags, Fedora: pk.Upstream.Fedora}
	}
	item.Versions = map[string]api.PackageVersionsValue{}
	for _, t := range targets {
		id := api.Node(pk.ID, t)
		row := api.PackageTarget{ID: t}
		if r := c.SRPMs[id]; r != nil {
			row.NVR = strings.TrimSuffix(r.File, ".src.rpm")
			item.Name = r.Name
			item.Versions[t] = api.PackageVersionsValue{Version: r.Version, Release: r.Release, NVR: row.NVR}
		}
		if g := c.Graphs[t]; g != nil {
			if n := g.Node(id); n != nil {
				row.DependsOn = n.DepPackages()
				row.UsedBy = n.UserPackages()
			}
		}
		if m := c.Manifests[t]; m != nil {
			if b, ok := m.Newest(pk.ID, item.Name); ok {
				row.Published = &api.PackageTargetPublished{NVR: manifest.NVR(b.SRPM), Commit: b.Commit, Publish: publishOf(m, b)}
			}
		}
		if n := c.Node(id); n != nil {
			row.Action = n.Action
			row.Reasons = n.Reasons
		}
		item.Targets = append(item.Targets, row)
	}
	if len(item.Versions) == 0 {
		item.Versions = nil
	}
	return item
}

func publishOf(m *manifest.Manifest, b *manifest.Build) string {
	if b.Publish != "" {
		return b.Publish
	}
	return m.Publish
}

// listTargets gives the targets of the selected nodes, in file order, and
// the selected targets of each package.
func listTargets(set *config.Set, sel *plan.Selection) ([]string, map[string][]string) {
	byPkg := map[string][]string{}
	used := map[string]bool{}
	for _, id := range sel.Nodes {
		p, t := api.SplitNode(id)
		byPkg[p] = append(byPkg[p], t)
		used[t] = true
	}
	var ts []string
	for _, t := range set.ActiveTargets() {
		if used[t.ID] {
			ts = append(ts, t.ID)
		}
	}
	return ts, byPkg
}

func hList(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	all, err := a.Targets(set, false)
	if err != nil {
		return nil, err
	}
	sels, err := plan.ParseSelectors(args)
	if err != nil {
		return nil, err
	}
	sel, err := plan.Select(set, sels, all, a.Here(set, setDir))
	if err != nil {
		return nil, err
	}
	targets, byPkg := listTargets(set, sel)
	c, err := a.computePlan(ctx, set, setDir, planOpts{selectors: sels, targets: targets, from: workspace.FromLock})
	if c != nil {
		a.warnAll(c.GraphWarnings)
	}
	if err != nil {
		return nil, err
	}
	brs, _ := cmd.Flags().GetBool("buildrequires")
	provs, _ := cmd.Flags().GetBool("provides")
	res := &Result{Kind: api.KindPackage, Targets: targets}
	var items []api.Package
	for _, pk := range set.Packages {
		ts := byPkg[pk.ID]
		if len(ts) == 0 {
			continue
		}
		it := packageItem(set, c, pk, ts)
		items = append(items, it)
		res.Items = append(res.Items, it)
	}
	res.Human = func(out *Out) error {
		heads := []string{"PKG", "VERSION", "COMMIT", "BRANCH", "TARGETS", "DEPENDS ON"}
		if brs {
			heads = append(heads, "BUILDREQUIRES")
		}
		if provs {
			heads = append(heads, "PROVIDES")
		}
		tb := NewTable(heads...)
		for _, it := range items {
			pk, _ := set.Package(it.Package)
			version, commit, branch := "-", "-", it.Source.Branch
			var ts []string
			for _, row := range it.Targets {
				ts = append(ts, row.ID)
				if v, ok := it.Versions[row.ID]; ok && version == "-" {
					version = v.Version
				}
			}
			switch it.Source.Kind {
			case "tree":
				branch = "in tree"
			case "external":
				branch += " (external)"
			}
			if it.Lock != nil {
				commit = api.ShortCommit(it.Lock.Commit)
			}
			cells := []string{it.Package, version, commit, branch, strings.Join(ts, " "), perTarget(it.Targets, func(r api.PackageTarget) []string { return r.DependsOn })}
			if brs {
				cells = append(cells, perTarget(it.Targets, func(r api.PackageTarget) []string {
					if s := c.SRPMs[api.Node(pk.ID, r.ID)]; s != nil {
						return s.Requires
					}
					return nil
				}))
			}
			if provs {
				cells = append(cells, perTarget(it.Targets, func(r api.PackageTarget) []string {
					if s := c.SRPMs[api.Node(pk.ID, r.ID)]; s != nil {
						return s.RPMs
					}
					return nil
				}))
			}
			tb.Row(cells...)
		}
		return out.Table(tb)
	}
	return res, nil
}

// perTarget joins a list of each target: "a b", or "a b(el9)" for an entry
// of some targets only.
func perTarget(rows []api.PackageTarget, list func(api.PackageTarget) []string) string {
	var order []string
	on := map[string][]string{}
	for _, r := range rows {
		for _, x := range list(r) {
			if _, ok := on[x]; !ok {
				order = append(order, x)
			}
			on[x] = append(on[x], r.ID)
		}
	}
	if len(order) == 0 {
		return "-"
	}
	var parts []string
	for _, x := range order {
		if len(on[x]) < len(rows) {
			parts = append(parts, x+"("+strings.Join(on[x], ",")+")")
		} else {
			parts = append(parts, x)
		}
	}
	return strings.Join(parts, " ")
}

func hShow(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	all, err := a.Targets(set, false)
	if err != nil {
		return nil, err
	}
	sels, err := plan.ParseSelectors(args)
	if err != nil {
		return nil, err
	}
	sel, err := plan.Select(set, sels, all, a.Here(set, setDir))
	if err != nil {
		return nil, err
	}
	if pkgs := sel.Packages(); len(pkgs) != 1 {
		if len(pkgs) == 0 {
			return nil, usagef("selector %s selects no node", strings.Join(args, " "))
		}
		return nil, usagef("show needs one package; %s selects %d: %s", strings.Join(args, " "), len(pkgs), strings.Join(pkgs, " "))
	}
	targets, byPkg := listTargets(set, sel)
	pkgID := sel.Packages()[0]
	pk, _ := set.Package(pkgID)
	c, err := a.computePlan(ctx, set, setDir, planOpts{selectors: sels, targets: targets, from: workspace.FromLock})
	var mine []api.Problem
	if c != nil {
		for _, w := range c.Warnings() {
			if w.Package == pkgID || strings.HasPrefix(w.Node, pkgID+"@") {
				mine = append(mine, w)
				a.Warn(w)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	it := packageItem(set, c, *pk, byPkg[pkgID])
	if !pk.InTree() {
		lc := ""
		if it.Lock != nil {
			lc = it.Lock.Commit
		}
		state, head := plan.WorktreeState(ctx, setDir, pkgID, lc)
		if state != api.LockStateMissing && head != "" {
			it.Worktree = &api.PackageWorktree{Path: filepath.Join(setDir, "packages", pkgID), Head: head, State: state}
		} else {
			it.Worktree = &api.PackageWorktree{State: state}
		}
	}
	for _, row := range it.Targets {
		if r := c.SRPMs[api.Node(pkgID, row.ID)]; r != nil && it.Worktree != nil {
			it.Worktree.Spec = r.Spec
		}
	}
	res := &Result{Kind: api.KindPackage, Targets: targets, Items: []any{it}, ShownWarnings: true}
	res.Human = func(out *Out) error {
		st := out.StdoutStyle
		const w = 10
		var src string
		switch it.Source.Kind {
		case "tree":
			src = fmt.Sprintf("%s · in tree %s", it.Package, it.Source.Path)
			if s, ok := c.Sources[pkgID]; ok {
				src += " · commit " + api.ShortCommit(s.Commit)
			}
		default:
			where := set.Remote()
			if it.Source.Kind == "external" {
				where = it.Source.URL
			}
			src = fmt.Sprintf("%s · branch %s (%s)", it.Package, it.Source.Branch, where)
			lockText := "not pinned"
			if it.Lock != nil {
				lockText = "lock " + api.ShortCommit(it.Lock.Commit)
			}
			wt := "worktree missing"
			if it.Worktree != nil && it.Worktree.State != "" {
				wt = "worktree " + st.State(string(it.Worktree.State))
			}
			src += " · " + lockText + ", " + wt
		}
		out.KV(w, "package", src)
		if u := it.Upstream; u != nil {
			var parts []string
			if u.Github != "" {
				parts = append(parts, "github "+u.Github)
			}
			if u.Tags != "" {
				parts = append(parts, "tags "+u.Tags)
			}
			if u.Fedora != "" {
				parts = append(parts, "fedora "+u.Fedora)
			}
			if len(parts) > 0 {
				out.KV(w, "upstream", strings.Join(parts, " · "))
			}
		}
		if it.Notes != "" {
			out.KV(w, "notes", it.Notes)
		}
		pubs := map[string][]string{}
		var porder []string
		for _, row := range it.Targets {
			key := "none"
			if p := row.Published; p != nil {
				key = fmt.Sprintf("%s (commit %s) in %s", p.NVR, api.ShortCommit(p.Commit), p.Publish)
			}
			if _, ok := pubs[key]; !ok {
				porder = append(porder, key)
			}
			pubs[key] = append(pubs[key], row.ID)
		}
		for i, key := range porder {
			text := key
			if len(porder) > 1 {
				text = strings.Join(pubs[key], " ") + ": " + key
			}
			k := "published"
			if i > 0 {
				k = ""
			}
			out.KV(w, k, text)
		}
		tb := NewTable("TARGET", "NVR", "USED BY", "ACTION")
		for _, row := range it.Targets {
			nvr := row.NVR
			if nvr == "" {
				nvr = "-"
			}
			used := "-"
			if len(row.UsedBy) > 0 {
				used = strings.Join(row.UsedBy, " ")
			}
			action := string(row.Action)
			if n := c.Node(api.Node(pkgID, row.ID)); n != nil && n.Action != api.PlanActionCurrent {
				if r := reasonText(st, *n); r != "" {
					action += "  " + r
				}
			}
			tb.Row(row.ID, nvr, used, action)
		}
		if err := out.Table(tb); err != nil {
			return err
		}
		if !out.Quiet {
			for _, p := range mine {
				fmt.Fprintf(out.Stderr, "%s %s (%s)\n", out.Style.Amber("warning:"), termtext.Escape(p.Message), p.Code)
			}
		}
		return nil
	}
	return res, nil
}

// exitOnlyError ends a command with an exit code and no error line: the
// output says it all, for example plan --check with a node to build (exit
// 11). The envelope has no errors[] entry for it.
type exitOnlyError struct{ code int }

func (e *exitOnlyError) Error() string { return "exit status " + strconv.Itoa(e.code) }

func exitOnly(code int) error {
	return &silent{err: cobratree.Coded(code, &exitOnlyError{code: code})}
}
