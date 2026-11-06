package plan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/graph"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/srpm"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/tools/run"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// Env is the host side of Compute: the set, the SRPM host and the
// directories of one command.
type Env struct {
	Paths  config.Paths
	Set    *config.Set
	SetDir string
	Host   *srpm.Host
	// Work is the work directory of the SRPM builds: the wrapper configs,
	// the macro files, the source clones and the result directories. The
	// caller removes it.
	Work string
	// Uniqueext names the plan root (mock.PlanExt).
	Uniqueext string
	// Staged finds the staged nodes; nil finds none.
	Staged Staged
	// Log gets the mock output of each SRPM build, one node after the
	// other; nil discards it.
	Log io.Writer
	// Progress, if set, is called before (done false) and after (done true)
	// each SRPM that is not in the SRPM cache.
	Progress func(node api.NodeId, done bool, err error)
}

// Request is what the command line asks for.
type Request struct {
	Selectors []Selector
	// Here is the package of the current directory, for ".".
	Here string
	// Targets are the targets after --target (FilterTargets).
	Targets []string
	From    workspace.From
	Force   bool
	// WithDeps and Rdeps add the dependencies or the dependents.
	WithDeps, Rdeps bool
}

// Computed is the result of Compute: the plan, the graphs and what they
// came from.
type Computed struct {
	*Result
	Selection *Selection
	Lock      *lock.File
	Sources   map[string]workspace.Source
	Targets   map[string]*srpm.Target
	// Graphs are the graphs of the targets of the request.
	Graphs map[string]*graph.Graph
	// SRPMs are the SRPMs of the nodes of the targets of the request.
	SRPMs map[api.NodeId]*srpm.Result
	// Infos are the graph infos of all nodes that the command read, also
	// of other targets (Inspect).
	Infos     map[api.NodeId]*graph.Info
	Manifests map[string]*manifest.Manifest
	// GraphWarnings are the warnings of the SRPMs and the graphs:
	// graph.srpm-failed, source.*, dep.not-for-target and the others.
	GraphWarnings []api.Problem
	// FailedLogs holds the mock output of each failed SRPM.
	FailedLogs map[api.NodeId]string
	// Built is the number of SRPMs that were not in the SRPM cache.
	Built int
}

// Warnings gives all warnings: of the graphs, then of the plan.
func (c *Computed) Warnings() []api.Problem {
	return append(append([]api.Problem{}, c.GraphWarnings...), c.Result.Warnings...)
}

// Compute makes the graphs and the plan of a request, as repoctl plan does
// and as repoctl build does in its step plan (build.html#plan): it reads
// repoctl.lock (or the worktree HEADs), builds the SRPM of each node of
// the targets of the request in the plan root (SRPM cache first), makes
// the graph of each target, reads the RepoManifest of the local copy of
// latest of each target and gives each node its plan action. A cycle gives
// graph.cycle (exit 1). Compute cleans the plan root at its end.
func Compute(ctx context.Context, env Env, req Request) (*Computed, error) {
	set := env.Set
	c := &Computed{Graphs: map[string]*graph.Graph{}, SRPMs: map[api.NodeId]*srpm.Result{},
		Infos: map[api.NodeId]*graph.Info{}, Manifests: map[string]*manifest.Manifest{},
		Targets: map[string]*srpm.Target{}, FailedLogs: map[api.NodeId]string{}}
	sel, err := Select(set, req.Selectors, req.Targets, req.Here)
	if err != nil {
		return nil, err
	}
	c.Selection = sel
	lf, err := lock.Read(env.SetDir)
	if err != nil {
		return nil, err
	}
	c.Lock = lf
	from := req.From
	if from == "" {
		from = workspace.FromLock
	}
	c.Sources, err = workspace.Sources(ctx, env.Paths, set, env.SetDir, lf, from)
	if err != nil {
		return nil, err
	}
	defer env.Host.Cleanup(context.WithoutCancel(ctx))
	if err := c.resolve(ctx, env, req.Targets); err != nil {
		return nil, err
	}
	for _, t := range set.ActiveTargets() {
		m, err := manifest.ReadLocal(env.Paths, set, t.ID)
		if err != nil {
			return nil, err
		}
		c.Manifests[t.ID] = m
	}
	commits := map[string]string{}
	for id, s := range c.Sources {
		commits[id] = s.Commit
	}
	positive := false
	for _, s := range req.Selectors {
		if !s.Not {
			positive = true
		}
	}
	_, abi := run.LookPath("abidiff")
	c.Result = Make(Input{
		Set: set, Targets: req.Targets, Selection: sel, Positive: positive, From: string(from),
		Force: req.Force, WithDeps: req.WithDeps, Rdeps: req.Rdeps, Commits: commits,
		Graphs: c.Graphs, Manifests: c.Manifests, Staged: env.Staged,
		Describe: GitDescriber(ctx, c.Sources), ABI: abi,
	})
	return c, nil
}

// resolve builds the SRPMs of the nodes of the targets and makes their
// graphs.
func (c *Computed) resolve(ctx context.Context, env Env, targets []string) error {
	set := env.Set
	inTargets := map[string]bool{}
	for _, t := range targets {
		inTargets[t] = true
	}
	for _, t := range set.ActiveTargets() {
		if !inTargets[t.ID] {
			continue
		}
		st, err := prepareTarget(ctx, env, t)
		if err != nil {
			return err
		}
		c.Targets[t.ID] = st
	}
	// The SRPMs: one goroutine per mock config, the nodes of one config
	// one after the other in its plan root.
	groups := map[string][]api.NodeId{}
	var configs []string
	for _, id := range set.Nodes() {
		pkg, t := api.SplitNode(id)
		if !inTargets[t] {
			continue
		}
		if _, ok := c.Sources[pkg]; !ok {
			continue
		}
		cfg := c.Targets[t].Mock.Config
		if _, ok := groups[cfg]; !ok {
			configs = append(configs, cfg)
		}
		groups[cfg] = append(groups[cfg], id)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, cfg := range configs {
		wg.Add(1)
		go func(ids []api.NodeId) {
			defer wg.Done()
			for _, id := range ids {
				if ctx.Err() != nil {
					return
				}
				pkg, t := api.SplitNode(id)
				info, res, warn, log, built := c.buildOne(ctx, env, id, c.Sources[pkg], c.Targets[t])
				mu.Lock()
				c.Infos[id] = info
				if res != nil {
					c.SRPMs[id] = res
				}
				if warn != nil {
					c.GraphWarnings = append(c.GraphWarnings, *warn)
					c.FailedLogs[id] = log
				}
				if res != nil {
					c.GraphWarnings = append(c.GraphWarnings, res.Warnings...)
				}
				if built {
					c.Built++
				}
				if env.Log != nil && log != "" {
					_, _ = io.WriteString(env.Log, log)
				}
				mu.Unlock()
			}
		}(groups[cfg])
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	sortWarnings(set, c.GraphWarnings)

	// The graphs. A requirement that no node of the target provides can
	// come from a node of another target (dep.not-for-target): read the
	// provides of the nodes of the other targets.
	var gw []api.Problem
	inspected := false
	for _, t := range set.ActiveTargets() {
		if !inTargets[t.ID] {
			continue
		}
		in := graph.Input{Target: t.ID, RHEL: c.Targets[t.ID].RHEL, NotBuilt: map[string]bool{}}
		for _, pk := range set.Packages {
			built := false
			for _, pt := range set.PackageTargets(pk) {
				if pt == t.ID {
					built = true
				}
			}
			if !built {
				in.NotBuilt[pk.ID] = true
				continue
			}
			if info := c.Infos[api.Node(pk.ID, t.ID)]; info != nil {
				in.Nodes = append(in.Nodes, info)
			}
		}
		if !inspected && len(set.ActiveTargets()) > len(c.Targets) && unprovided(in) {
			c.inspectOthers(ctx, env, inTargets)
			inspected = true
		}
		for id, info := range c.Infos {
			if _, it := api.SplitNode(id); it != t.ID {
				in.Others = append(in.Others, info)
			}
		}
		sortInfos(set, in.Others)
		g, w, err := graph.New(in)
		gw = append(gw, w...)
		if err != nil {
			c.GraphWarnings = append(c.GraphWarnings, gw...)
			return err
		}
		c.Graphs[t.ID] = g
	}
	c.GraphWarnings = append(c.GraphWarnings, gw...)
	return nil
}

// unprovided reports a requirement that no node of the input provides.
func unprovided(in graph.Input) bool {
	prov := map[string]bool{}
	for _, n := range in.Nodes {
		for _, p := range n.Provides {
			prov[p] = true
		}
	}
	for _, n := range in.Nodes {
		for _, r := range n.Requires {
			ok := false
			for _, name := range graph.RequireNames(r) {
				ok = ok || prov[name]
			}
			if !ok {
				return true
			}
		}
	}
	return false
}

// inspectOthers reads the provides of the nodes of the active targets that
// the request does not build, with rpmspec (no mock).
func (c *Computed) inspectOthers(ctx context.Context, env Env, inTargets map[string]bool) {
	for _, t := range env.Set.ActiveTargets() {
		if inTargets[t.ID] {
			continue
		}
		st, err := prepareTarget(ctx, env, t)
		if err != nil {
			continue
		}
		for _, pk := range env.Set.Packages {
			src, ok := c.Sources[pk.ID]
			if !ok {
				continue
			}
			for _, pt := range env.Set.PackageTargets(pk) {
				if pt != t.ID {
					continue
				}
				id := api.Node(pk.ID, t.ID)
				r, err := env.Host.Inspect(ctx, srpm.Input{Node: id, Source: src, Target: st, Work: env.Work, Remote: env.Set.Remote()})
				if err != nil {
					continue
				}
				c.Infos[id] = infoOf(pk, t.ID, r)
			}
		}
	}
}

func prepareTarget(ctx context.Context, env Env, t config.Target) (*srpm.Target, error) {
	if _, err := os.Stat(env.Paths.MockConfig(t.Mock)); err != nil {
		return nil, api.Errorf(api.PMockConfigMissing, "the mock config %s of target %s is not on this host (%s)", t.Mock, t.ID, env.Paths.MockConfig(t.Mock)).
			WithTarget(t.ID).WithHint("install it, or correct targets[].mock", "repoctl doctor")
	}
	return env.Host.PrepareTarget(ctx, env.Work, mock.FromConfig(t, env.SetDir), t.Arch())
}

// buildOne builds the SRPM of one node. A failure is a warning: the node
// keeps its plan action and has no known dependencies.
func (c *Computed) buildOne(ctx context.Context, env Env, id api.NodeId, src workspace.Source, st *srpm.Target) (*graph.Info, *srpm.Result, *api.Problem, string, bool) {
	pkg, t := api.SplitNode(id)
	pk, _ := env.Set.Package(pkg)
	fedora := ""
	if pk.Upstream != nil {
		fedora = pk.Upstream.Fedora
	}
	var log bytes.Buffer
	in := srpm.Input{Node: id, Source: src, Target: st, Work: env.Work, Log: &log, Uniqueext: env.Uniqueext,
		Fedora: fedora, Remote: env.Set.Remote(), Cache: true, Plan: true}
	cached := env.Host.Cached(ctx, in)
	if !cached && env.Progress != nil {
		env.Progress(id, false, nil)
	}
	r, err := env.Host.Build(ctx, in)
	if !cached && env.Progress != nil {
		env.Progress(id, true, err)
	}
	if err != nil {
		info := &graph.Info{Node: id, Package: pkg, Target: t, Role: roleOf(*pk), Failed: true}
		w := api.Problem{Node: id, Package: pkg, Target: t}
		if e, ok := api.AsError(err); ok && strings.HasPrefix(e.Code, "source.") {
			w.Code, w.Message, w.Hint = e.Code, fmt.Sprintf("%s: %s", id, e.Message), e.Hint
			if w.Hint == "" {
				w.Hint = api.Problems[e.Code].Hint
			}
		} else {
			w.Code = api.PGraphSRPMFailed
			w.Message = fmt.Sprintf("the SRPM of %s failed: %s", id, firstLine(err.Error()))
			w.Hint = api.HintText("fix the spec; graph -v shows the mock output", "repoctl graph -v "+id)
		}
		return info, nil, &w, log.String(), !cached
	}
	return infoOf(*pk, t, r), r, nil, "", !cached
}

func infoOf(pk config.Package, target string, r *srpm.Result) *graph.Info {
	info := &graph.Info{Node: api.Node(pk.ID, target), Package: pk.ID, Target: target, Role: roleOf(pk),
		Name: r.Name, Version: r.Version, Requires: r.Requires, Provides: r.Provides, RPMs: r.RPMs,
		Dynamic: r.Dynamic, Spec: r.Spec}
	if r.Release != "" {
		info.EVR = r.EVR()
	}
	return info
}

func roleOf(pk config.Package) string {
	if pk.Role != "" {
		return pk.Role
	}
	return "package"
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// sortWarnings orders warnings by node in the order of Set.Nodes.
func sortWarnings(set *config.Set, ws []api.Problem) {
	pos := map[api.NodeId]int{}
	for i, n := range set.Nodes() {
		pos[n] = i
	}
	for i := 1; i < len(ws); i++ {
		for j := i; j > 0 && pos[ws[j].Node] < pos[ws[j-1].Node]; j-- {
			ws[j], ws[j-1] = ws[j-1], ws[j]
		}
	}
}

func sortInfos(set *config.Set, in []*graph.Info) {
	pos := map[api.NodeId]int{}
	for i, n := range set.Nodes() {
		pos[n] = i
	}
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && pos[in[j].Node] < pos[in[j-1].Node]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// GitDescriber describes a commit change from the package repositories of
// the sources: the subject of the new commit, the number of commits, and
// whether the new commits are empty "Rebuild: TEXT" commits.
func GitDescriber(ctx context.Context, sources map[string]workspace.Source) Describer {
	return func(pkg, from, to string) Change {
		src, ok := sources[pkg]
		if !ok {
			return Change{}
		}
		g := git.New(src.Repo)
		var ch Change
		ch.Subject, _ = g.Subject(ctx, to)
		args := []string{"log", "--format=%H%x09%T%x09%P%x09%s", from + ".." + to}
		if src.Path != "" {
			args = append(args, "--", src.Path)
		}
		lines, err := g.Lines(ctx, args...)
		if err != nil {
			return ch
		}
		ch.Commits = len(lines)
		ch.RebuildOnly = len(lines) > 0 && src.Path == ""
		for i, l := range lines {
			f := strings.SplitN(l, "\t", 4)
			if len(f) < 4 {
				ch.RebuildOnly = false
				continue
			}
			text, isRebuild := strings.CutPrefix(f[3], "Rebuild: ")
			if isRebuild {
				parents := strings.Fields(f[2])
				ptree := ""
				if len(parents) == 1 {
					ptree, _ = g.Run(ctx, "rev-parse", parents[0]+"^{tree}")
				}
				isRebuild = ptree != "" && ptree == f[1]
			}
			if i == 0 && isRebuild {
				ch.Rebuild, ch.Text = true, text
			}
			if !isRebuild {
				ch.RebuildOnly = false
			}
		}
		return ch
	}
}

// WorktreeState gives the lock state of the worktree packages/PKG of a
// branch or external package against the lock commit: in-sync, ahead,
// behind, diverged, dirty or missing.
func WorktreeState(ctx context.Context, setDir, pkg, lockCommit string) (api.LockState, string) {
	wt := setDir + "/packages/" + pkg
	if _, err := os.Stat(wt); err != nil {
		return api.LockStateMissing, ""
	}
	g := git.New(wt)
	head, err := g.RevParse(ctx, "HEAD")
	if err != nil || head == "" {
		return api.LockStateMissing, ""
	}
	if dirty, err := g.Dirty(ctx); err == nil && dirty {
		return api.LockStateDirty, head
	}
	switch {
	case lockCommit == "":
		return api.LockStateUnpinned, head
	case head == lockCommit:
		return api.LockStateInSync, head
	}
	if ok, _ := g.IsAncestor(ctx, lockCommit, head); ok {
		return api.LockStateAhead, head
	}
	if ok, _ := g.IsAncestor(ctx, head, lockCommit); ok {
		return api.LockStateBehind, head
	}
	return api.LockStateDiverged, head
}

// CheckWorktrees compares the worktrees with the lock (build.html#plan): a
// worktree of a package that the plan uses (a node that is not skip) and
// that is not in-sync gives lock.dirty; another one gives the warning
// worktree.dirty. A missing worktree is fine: the plan reads the set
// repository.
func CheckWorktrees(ctx context.Context, set *config.Set, setDir string, lf *lock.File, r *Result) (*api.Error, []api.Problem) {
	used := map[string]bool{}
	for _, n := range r.Plan.Nodes {
		if n.Action != api.PlanActionSkip {
			p, _ := api.SplitNode(n.Node)
			used[p] = true
		}
	}
	var bad []string
	var warns []api.Problem
	for _, pk := range set.Packages {
		if pk.InTree() {
			continue
		}
		e, ok := lf.Get(pk.ID)
		if !ok {
			continue
		}
		st, _ := WorktreeState(ctx, setDir, pk.ID, e.Commit)
		if st == api.LockStateInSync || st == api.LockStateMissing {
			continue
		}
		if used[pk.ID] {
			bad = append(bad, fmt.Sprintf("%s (%s)", pk.ID, st))
			continue
		}
		warns = append(warns, api.Problem{Code: api.PWorktreeDirty, Package: pk.ID,
			Message: fmt.Sprintf("packages/%s is %s; no selected node uses it, so the plan uses repoctl.lock", pk.ID, st),
			Hint:    api.HintText("commit and pin it", "repoctl pin "+pk.ID)})
	}
	if len(bad) == 0 {
		return nil, warns
	}
	e := api.Errorf(api.PLockDirty, "%s not in-sync with repoctl.lock: %s", plural(len(bad), "worktree is", "worktrees are"), strings.Join(bad, ", ")).
		WithHint("commit and pin the worktrees", "repoctl pin").
		WithHint("or build the HEAD commits without a pin", "repoctl plan --from worktree")
	return e, warns
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// IsCycle reports a graph.cycle error.
func IsCycle(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Code == api.PGraphCycle
}
