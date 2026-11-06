// Package build runs a build: the plan, the nodes in mock, the staging of
// the run and its journal (docs/design/build.html#run).
package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/journal"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/plan"
	"github.com/dennisklein/repoctl/internal/staging"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// Timeouts are the limits build.timeout.* of the user config
// (build.html#failures). Zero is no limit.
type Timeouts struct {
	SRPM, RPM, Stage, Idle, Run time.Duration
}

// Options say what a run builds and how.
type Options struct {
	Set    *config.Set
	SetDir string
	Paths  config.Paths
	// Lock is repoctl.lock as the command read it.
	Lock *lock.File
	// SetLock is the held set lock; the run records its id in it and
	// checks every 30 s that the lock path still names the locked file.
	SetLock  *lock.SetLock
	LockWhat lock.What
	// RunID is the id of the run (journal.NewID).
	RunID  api.RunId
	Origin api.RunStartedOrigin
	// Selection is the result of the selectors; Selectors are the
	// selectors as given, Parsed the same selectors parsed, and Here the
	// package of the current directory.
	Selection *plan.Selection
	Selectors []string
	Parsed    []plan.Selector
	Here      string
	From      workspace.From
	Force     bool
	WithDeps  bool
	Rdeps     bool
	FailFast  bool
	Jobs      int
	Timeouts  Timeouts
	// SetCommit is HEAD of the set repository.
	SetCommit api.Commit
	Version   string
	// Mock runs mock; nil is mock.New(Paths, MockOptions).
	Mock        *mock.Mock
	MockOptions mock.Options
	// Fresh runs the freshness check of the selected targets at the start
	// of step plan (publish.html#freshness): its warnings go to the
	// journal, and its error stops the run like a plan error (state.behind,
	// exit 13). nil skips the check.
	Fresh func(ctx context.Context, targets []string) ([]api.Problem, error)
	// Planner makes the plan; nil is an Engine (plan.Compute).
	Planner Planner
	// SRPM runs step srpm; nil is an Engine (the SRPM host).
	SRPM SRPMBuilder
	// Staged is the staged index of the set; nil loads it.
	Staged *staging.Index
	// Cancel receives the cancel requests of the run: Ctrl-C, repoctl
	// cancel, the web UI or MCP. A request with Force stops the roots
	// without a wait, and the run ends without run.finished.
	Cancel <-chan journal.CancelRequest
	// Planned is called after the plan, before plan.ready and the spans of
	// the targets, for the note of the command.
	Planned func(r *api.Run, p *Plan)
	// Warn gets the warnings of the run for the result.
	Warn func(api.Problem)
	Now  func() time.Time

	// LockEvery, CancelWait and SampleEvery are 30 s, 60 s and 2 s; tests
	// make them shorter.
	LockEvery   time.Duration
	CancelWait  time.Duration
	SampleEvery time.Duration
	// TraceParent is TRACEPARENT of the environment, for a run whose
	// context has no Bus.
	TraceParent, TraceState string
}

// Result is the end of a run.
type Result struct {
	// Run is the folded journal of the run.
	Run *api.Run
	// Plan is the plan of the run; nil after a plan error.
	Plan *Plan
	// Forced reports a forced cancel: the run ended without run.finished.
	Forced bool
}

// Failed reports that a node of the run failed or is blocked.
func (r *Result) Failed() bool {
	return r.Run != nil && (r.Run.Counts.Failed > 0 || r.Run.Counts.Blocked > 0)
}

type runner struct {
	o    Options
	ctx  context.Context
	mock *mock.Mock
	now  func() time.Time

	emitMu sync.Mutex
	w      *journal.Writer
	fold   *journal.Fold
	jerr   error
	jerrCh chan struct{}

	cmdSpan *progress.Span
	cmdCtx  context.Context
	start   time.Time

	work    string
	plan    *Plan
	order   []*node
	nodes   map[api.NodeId]*node
	targets map[string]*targetRun
	tOrder  []string

	jobs     int
	free     []int
	nextSlot int
	running  int
	readySeq int
	done     chan *nodeDone

	cancel   *journal.CancelRequest
	failFast bool
	lockLost bool
	internal bool

	mockVerOnce sync.Once
	mockVer     string
}

type targetRun struct {
	id       string
	mt       mock.Target
	files    mock.Files
	hash     string
	dir      staging.Dir
	addRepos []string
	span     *progress.Span
	ctx      context.Context
	total    int
	started  time.Time
}

// Run runs a build: run.started, the plan, the nodes and run.finished. It
// gives an error only when the run cannot start, or after a plan error,
// which run.finished records; a failed node is in the Result.
func Run(ctx context.Context, o Options) (*Result, error) {
	r := &runner{o: o, now: o.Now, nodes: map[api.NodeId]*node{}, targets: map[string]*targetRun{}, jerrCh: make(chan struct{}, 1)}
	if r.now == nil {
		r.now = time.Now
	}
	if r.o.LockEvery == 0 {
		r.o.LockEvery = 30 * time.Second
	}
	if r.o.CancelWait == 0 {
		r.o.CancelWait = 60 * time.Second
	}
	if r.o.SampleEvery == 0 {
		r.o.SampleEvery = 2 * time.Second
	}
	if r.o.Planner == nil || r.o.SRPM == nil {
		e := &Engine{}
		if r.o.Planner == nil {
			r.o.Planner = e
		}
		if r.o.SRPM == nil {
			r.o.SRPM = e
		}
	}
	r.mock = o.Mock
	if r.mock == nil {
		r.mock = mock.New(o.Paths, o.MockOptions)
	}
	r.jobs = max(o.Jobs, 1)
	r.start = r.now()
	// The children and the journal outlive a cancel of the context: a
	// cancel comes through o.Cancel, and the run ends its nodes itself.
	ctx = context.WithoutCancel(ctx)
	var closeBus func()
	if progress.BusFrom(ctx) == nil {
		ctx, closeBus = r.ownBus(ctx)
		defer closeBus()
	}
	r.ctx = ctx
	r.cmdCtx, r.cmdSpan = ctx, progress.SpanFrom(ctx)
	if closeBus != nil {
		r.cmdCtx, r.cmdSpan = progress.Start(ctx, progress.KindCommand, "build")
	}

	jpath := o.Paths.Journal(o.Set.Name, o.RunID)
	w, err := journal.Create(jpath, o.RunID, journal.Options{Now: r.now})
	if err != nil {
		return nil, err
	}
	r.w = w
	p := o.Paths
	r.fold = journal.NewFold(journal.FoldOptions{Journal: jpath, Paths: &p, Host: o.Origin.Host})
	if o.SetLock != nil {
		_ = o.SetLock.SetRun(o.RunID)
	}
	r.work = o.Paths.MockWork(o.Set.Name, o.RunID)
	defer os.RemoveAll(filepath.Join(r.work, "src"))

	res, err := r.run()
	if res != nil && res.Forced {
		_ = r.w.Close()
		return res, err
	}
	_ = r.w.Close()
	if r.cmdSpan != nil && closeBus != nil {
		r.cmdSpan.End(spanErr(res))
	}
	return res, err
}

func spanErr(res *Result) error {
	if res == nil || res.Run == nil {
		return nil
	}
	switch res.Run.State {
	case api.RunStateCanceled:
		return context.Canceled
	case api.RunStateFailed:
		return errors.New("failed")
	}
	return nil
}

// ownBus makes the Bus of a run whose context has none: a run of the web
// UI or of MCP. Its sink is the progress log of the run.
func (r *runner) ownBus(ctx context.Context) (context.Context, func()) {
	path := r.o.Paths.ProgressLog(r.o.Set.Name, r.o.RunID)
	var sinks []progress.Sink
	var f *os.File
	var lg *progress.Log
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		if f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			lg = progress.NewLog(f, progress.LogOptions{Program: "repoctl", Version: r.o.Version, RunID: r.o.RunID})
			sinks = append(sinks, lg)
		}
	}
	tc, _ := progress.ParseTraceContext(r.o.TraceParent, r.o.TraceState)
	bus := progress.NewBus(progress.BusOptions{Sinks: sinks, Program: "repoctl", Trace: tc, Now: r.now})
	return progress.WithBus(ctx, bus), func() {
		bus.Close()
		if lg != nil {
			_ = lg.Close()
		}
		if f != nil {
			_ = f.Close()
		}
	}
}

func spanID(s *progress.Span) string {
	if s == nil {
		return ""
	}
	return s.TraceContext().Parent.String()
}

// emit writes one journal line with the span of the work and folds it.
func (r *runner) emit(n *node, span, parent *progress.Span, ev api.Event) {
	h := ev.Head()
	h.Span, h.Parent = spanID(span), spanID(parent)
	if n != nil {
		h.Node = n.id
	}
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if err := r.w.Write(ev); err != nil {
		if r.jerr == nil {
			r.jerr = err
			select {
			case r.jerrCh <- struct{}{}:
			default:
			}
		}
		return
	}
	r.fold.Add(journal.Line{EventHeader: *h, Event: ev})
}

func (r *runner) warn(p api.Problem, span *progress.Span) {
	ev := &api.WarningEvent{Code: p.Code, Message: p.Message, Target: p.Target}
	var n *node
	if p.Node != "" {
		n = &node{id: p.Node}
	}
	r.emit(n, span, nil, ev)
	if r.o.Warn != nil {
		r.o.Warn(p)
	}
}

func (r *runner) snapshot() *api.Run {
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	return r.fold.Run()
}

func (r *runner) run() (*Result, error) {
	o := r.o
	sel := &api.Selection{Packages: o.Selectors, Targets: o.Selection.Targets, From: string(o.From), Force: o.Force,
		WithDeps: o.WithDeps, Rdeps: o.Rdeps, FailFast: o.FailFast, Jobs: r.jobs}
	if sel.From == "" {
		sel.From = string(workspace.FromLock)
	}
	started := &api.RunStarted{Set: o.Set.Name, Kind: journal.KindBuild, Origin: o.Origin, Selection: sel,
		SetCommit: o.SetCommit, Repoctl: o.Version}
	bus := progress.BusFrom(r.ctx)
	started.Trace = bus.TraceID().String()
	if tc := bus.TraceContext(); tc.Parent != 0 {
		started.TraceParent = tc.Parent.String()
	}
	if o.Lock != nil {
		started.LockDigest = o.Lock.Digest()
	}
	r.emit(nil, r.cmdSpan, nil, started)
	if r.jerr != nil {
		return nil, r.jerr
	}

	// Step plan.
	pctx, pspan := progress.Start(r.cmdCtx, progress.KindStep, "plan")
	staged := o.Staged
	if staged == nil {
		var err error
		if staged, err = staging.Load(o.Paths, o.Set.Name, o.Origin.Host); err != nil {
			staged = staging.NewIndex()
		}
	}
	var pl *Plan
	var err error
	if o.Fresh != nil {
		var warns []api.Problem
		warns, err = o.Fresh(pctx, o.Selection.Targets)
		for _, w := range warns {
			r.warn(w, pspan)
		}
	}
	if err == nil {
		pl, err = o.Planner.Plan(pctx, PlanRequest{Set: o.Set, SetDir: o.SetDir, Paths: o.Paths, Lock: o.Lock, Selection: o.Selection,
			From: o.From, Force: o.Force, WithDeps: o.WithDeps, Rdeps: o.Rdeps, Staged: staged, Mock: r.mock,
			Selectors: o.Parsed, Here: o.Here, Work: filepath.Join(r.work, "plan")})
	}
	if err != nil {
		pspan.End(err)
		code := api.ExitFailed
		if ae, ok := api.AsError(err); ok && ae.ExitCode != 0 {
			code = ae.ExitCode
		}
		r.emit(nil, r.cmdSpan, nil, &api.RunFinished{State: api.RunStateFailed, DurationMs: r.since(r.start), ExitCode: code})
		return &Result{Run: r.snapshot()}, err
	}
	r.plan = pl
	for _, w := range pl.Warnings {
		r.warn(w, pspan)
	}
	if o.Planned != nil {
		o.Planned(r.snapshot(), pl)
	}
	if err := r.setup(pl); err != nil {
		pspan.End(err)
		r.emit(nil, r.cmdSpan, nil, &api.RunFinished{State: api.RunStateFailed, DurationMs: r.since(r.start), ExitCode: api.ExitFailed})
		return &Result{Run: r.snapshot(), Plan: pl}, err
	}
	r.emit(nil, pspan, r.cmdSpan, planReady(pl))
	pspan.End(nil, progress.Message(planWords(pl)))
	r.startSpans()

	forced := r.schedule()
	if forced {
		return &Result{Run: r.snapshot(), Plan: pl, Forced: true}, nil
	}
	r.finish()
	return &Result{Run: r.snapshot(), Plan: pl}, nil
}

func (r *runner) since(t time.Time) int { return int(r.now().Sub(t) / time.Millisecond) }

func planWords(p *Plan) string {
	parts := []string{fmt.Sprintf("%d build", p.Summary.Build)}
	if p.Summary.Current > 0 {
		parts = append(parts, fmt.Sprintf("%d current", p.Summary.Current))
	}
	if p.Summary.Staged > 0 {
		parts = append(parts, fmt.Sprintf("%d staged", p.Summary.Staged))
	}
	return strings.Join(parts, ", ")
}

// planReady gives the event plan.ready of a plan.
func planReady(p *Plan) *api.PlanReady {
	ev := &api.PlanReady{Commits: map[string]api.Commit{}}
	ev.Summary = api.PlanReadySummary{Build: p.Summary.Build, Current: p.Summary.Current, Staged: p.Summary.Staged, Skip: p.Summary.Skip}
	for _, n := range p.Nodes {
		switch n.Action {
		case api.PlanActionBuild:
			item := api.PlanReadyBuildItem{Node: n.Node, Deps: n.Deps}
			for _, rs := range n.Reasons {
				item.Reasons = append(item.Reasons, rs.Code)
			}
			ev.Build = append(ev.Build, item)
		case api.PlanActionCurrent:
			ev.Current = append(ev.Current, n.Node)
		case api.PlanActionStaged:
			ev.Staged = append(ev.Staged, n.Node)
			if ev.FromRun == nil {
				ev.FromRun = map[string]api.RunId{}
			}
			ev.FromRun[n.Node] = n.FromRun
		case api.PlanActionSkip:
			ev.Skip = append(ev.Skip, n.Node)
		}
		if n.Action != api.PlanActionSkip && n.Commit != "" {
			ev.Commits[n.Node] = n.Commit
		}
	}
	return ev
}

// setup makes the nodes, the staging of each target, the wrapper configs,
// the staged views and the spans of the targets.
func (r *runner) setup(p *Plan) error {
	o := r.o
	byID := map[api.NodeId]*api.PlanNode{}
	for i := range p.Nodes {
		byID[p.Nodes[i].Node] = &p.Nodes[i]
	}
	repos := staging.ReposOf(o.Set)
	for i := range p.Nodes {
		pn := &p.Nodes[i]
		if pn.Action != api.PlanActionBuild {
			continue
		}
		pkg, t := api.SplitNode(pn.Node)
		n := &node{id: pn.Node, pkg: pkg, target: t, index: len(r.order), pn: pn, state: api.NodeStateWaiting}
		r.nodes[n.id] = n
		r.order = append(r.order, n)
		if _, ok := r.targets[t]; !ok {
			r.targets[t] = &targetRun{id: t}
			r.tOrder = append(r.tOrder, t)
		}
		r.targets[t].total++
	}
	for _, n := range r.order {
		for _, d := range n.pn.Deps {
			if dn, ok := r.nodes[d]; ok {
				n.deps = append(n.deps, dn)
				dn.dependents = append(dn.dependents, n)
			}
		}
	}
	for _, n := range r.order {
		n.weight = len(reach(n))
	}
	// Targets in the order of the selection.
	sort.SliceStable(r.tOrder, func(i, j int) bool {
		return slices.Index(o.Selection.Targets, r.tOrder[i]) < slices.Index(o.Selection.Targets, r.tOrder[j])
	})
	stagedByTarget := map[string][]staging.Entry{}
	if o.Staged != nil || len(p.Nodes) > 0 {
		idx := o.Staged
		if idx == nil {
			idx, _ = staging.Load(o.Paths, o.Set.Name, o.Origin.Host)
		}
		for _, pn := range p.Nodes {
			if pn.Action != api.PlanActionStaged || idx == nil {
				continue
			}
			pkg, t := api.SplitNode(pn.Node)
			if e, ok := idx.Lookup(t, pkg, pn.Commit); ok {
				stagedByTarget[t] = append(stagedByTarget[t], e)
			}
		}
	}
	if len(r.order) > 0 {
		r.cleanOldRoots()
	}
	for _, t := range r.tOrder {
		tr := r.targets[t]
		ct, ok := o.Set.Target(t)
		if !ok {
			return fmt.Errorf("unknown target %s", t)
		}
		tr.mt = mock.FromConfig(*ct, o.SetDir)
		files, err := r.mock.WriteFiles(r.work, tr.mt)
		if err != nil {
			return err
		}
		tr.files = files
		if h, err := r.mock.ConfigHash(r.ctx, tr.mt.Config); err == nil {
			tr.hash = "sha256:" + h
		}
		tr.dir = staging.Dir{Paths: o.Paths, Set: o.Set.Name, Run: o.RunID, Target: t, Repos: repos}
		if err := tr.dir.Init(r.ctx); err != nil {
			return err
		}
		if local := manifest.LocalRepo(o.Paths, o.Set, t); fileExists(filepath.Join(local, "repodata", "repomd.xml")) {
			tr.addRepos = append(tr.addRepos, "file://"+local)
		}
		tr.addRepos = append(tr.addRepos, "file://"+tr.dir.Repo(repos.Binary))
		if es := stagedByTarget[t]; len(es) > 0 {
			view := filepath.Join(r.work, "view", t)
			if ok, err := staging.MakeView(r.ctx, view, es); err != nil {
				return err
			} else if ok {
				tr.addRepos = append(tr.addRepos, "file://"+view)
			}
		}
		tr.addRepos = append(tr.addRepos, tr.mt.Repos...)
	}
	return nil
}

// startSpans starts the step of each target and the span of each node,
// after the step plan ended, so that the display says the end of the plan
// before the start of the targets (terminal.html#build-plain).
func (r *runner) startSpans() {
	for _, t := range r.tOrder {
		tr := r.targets[t]
		tr.ctx, tr.span = progress.Start(r.cmdCtx, progress.KindStep, t, progress.Total(tr.total), progress.Limit(r.jobs),
			progress.Flagged(progress.FlagFold|progress.FlagShowLines))
		tr.started = r.now()
	}
	for _, n := range r.order {
		tr := r.targets[n.target]
		n.ctx, n.span = progress.Start(tr.ctx, progress.KindTarget, n.id, progress.Role(n.target), progress.Queued(), progress.Message(waitWords(n)))
	}
}

// reach gives the nodes that depend on n, transitively.
func reach(n *node) map[*node]bool {
	seen := map[*node]bool{}
	var walk func(x *node)
	walk = func(x *node) {
		for _, d := range x.dependents {
			if !seen[d] {
				seen[d] = true
				walk(d)
			}
		}
	}
	walk(n)
	return seen
}

func waitWords(n *node) string {
	var open []string
	for _, d := range n.deps {
		if d.state != api.NodeStateOk {
			open = append(open, d.id)
		}
	}
	if len(open) == 0 {
		return "waiting for a job slot"
	}
	return "waiting for " + strings.Join(open, ", ")
}

// cleanOldRoots removes the roots that failed or canceled nodes of older
// runs of the set kept for a look (build.html#mock).
func (r *runner) cleanOldRoots() {
	roots, err := r.mock.Roots()
	if err != nil {
		return
	}
	mark := "-" + config.MockRootPrefix() + "-" + r.o.Set.Name + "-r-"
	for _, root := range roots {
		base := filepath.Base(root)
		i := strings.Index(base, mark)
		if i < 0 || strings.Contains(base, "-bootstrap-") || strings.Contains(base, r.o.RunID) {
			continue
		}
		call := mock.Call{Files: mock.Files{Config: base[:i]}, Uniqueext: base[i+1:]}
		_ = r.mock.Scrub(r.ctx, call, "all")
	}
}

// finish writes the end of the run: the final repodata, the target spans
// and run.finished.
func (r *runner) finish() {
	for _, t := range r.tOrder {
		tr := r.targets[t]
		if tr.span != nil {
			tr.span.End(r.targetErr(tr), progress.Message(r.targetWords(tr)))
		}
	}
	snap := r.snapshot()
	state, code := api.RunStateOk, api.ExitOK
	switch {
	case r.cancel != nil || r.lockLost:
		state, code = api.RunStateCanceled, api.ExitInterrupted
	case r.internal:
		state, code = api.RunStateFailed, api.ExitFailed
	case snap.Counts.Failed > 0 || snap.Counts.Blocked > 0 || snap.Counts.Canceled > 0:
		state, code = api.RunStateFailed, api.ExitBuildFailed
	}
	fin := &api.RunFinished{State: state, Counts: snap.Counts, DurationMs: r.since(r.start), ExitCode: code}
	if !r.unpinned() {
		fin.Publishable = r.publishable()
	}
	r.emit(nil, r.cmdSpan, nil, fin)
}

func (r *runner) unpinned() bool { return r.o.From == workspace.FromWorktree }

// publishable gives the targets whose nodes are all current or staged
// after the run: each build node ended ok.
func (r *runner) publishable() []api.TargetId {
	var out []api.TargetId
	for _, t := range r.o.Selection.Targets {
		ok, any := true, false
		for _, pn := range r.plan.Nodes {
			if _, nt := api.SplitNode(pn.Node); nt != t || pn.Action == api.PlanActionSkip {
				continue
			}
			any = true
			if n, built := r.nodes[pn.Node]; built && n.state != api.NodeStateOk {
				ok = false
			}
		}
		if ok && any {
			out = append(out, t)
		}
	}
	return out
}

func (r *runner) targetErr(tr *targetRun) error {
	snap := r.snapshot()
	failed, canceled := false, false
	for _, n := range snap.Nodes {
		if n.Target != tr.id {
			continue
		}
		switch n.State {
		case api.NodeStateFailed, api.NodeStateBlocked:
			failed = true
		case api.NodeStateCanceled:
			canceled = true
		}
	}
	switch {
	case failed:
		return errors.New("failed")
	case canceled:
		return context.Canceled
	}
	return nil
}

func (r *runner) targetWords(tr *targetRun) string {
	var c api.StateCounts
	for _, n := range r.order {
		if n.target != tr.id {
			continue
		}
		switch n.state {
		case api.NodeStateOk:
			c.Ok++
		case api.NodeStateFailed:
			c.Failed++
		case api.NodeStateBlocked:
			c.Blocked++
		case api.NodeStateCanceled:
			c.Canceled++
		case api.NodeStateRunning:
			c.Running++
		case api.NodeStateQueued:
			c.Queued++
		case api.NodeStateWaiting:
			c.Waiting++
		}
	}
	return CountWords(c, false)
}

// CountWords gives the counts of a run as words, "6 ok, 1 failed, 1
// blocked, 13 current", in the order of the node states. withPlan adds the
// current and staged nodes.
func CountWords(c api.StateCounts, withPlan bool) string {
	var parts []string
	add := func(n int, w string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, w))
		}
	}
	add(c.Ok, "ok")
	add(c.Failed, "failed")
	add(c.Blocked, "blocked")
	add(c.Canceled, "canceled")
	add(c.Running, "running")
	add(c.Queued, "queued")
	add(c.Waiting, "waiting")
	if withPlan {
		add(c.Current, "current")
		add(c.Staged, "staged")
	}
	if len(parts) == 0 {
		return "nothing to build"
	}
	return strings.Join(parts, ", ")
}
