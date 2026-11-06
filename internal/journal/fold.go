package journal

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// FoldOptions say what a fold needs besides the journal.
type FoldOptions struct {
	// Journal is the path of the journal. The fold gives the files of the
	// run from it: the progress log next to it and the logs directory.
	Journal string
	// Paths are the XDG paths, for the staging of a build run and the
	// paths of its artifacts; nil leaves them out.
	Paths *config.Paths
	// Alive reports whether a process of the run still holds the lock of
	// the run: the set lock, or the check lock for a check run. A run
	// without run.finished whose process is gone is abandoned. Nil never
	// makes a run abandoned.
	Alive func(r *api.Run) bool
	// Host is this host. A run of another host is never abandoned: a
	// reader cannot check its lock (contract.html#readers).
	Host string
}

// Fold folds the events of one run into the run state: the Run of the
// contract with its node results and counts. Every reader that shows node
// state uses it (contract.html#readers). It is also an eventlog.Feed, so
// that a Tail of the journal feeds it as the journal grows.
//
// A Fold is not safe for concurrent use.
type Fold struct {
	o        FoldOptions
	run      api.Run
	started  bool
	planned  bool
	finished bool
	lastSeq  int
	order    []api.NodeId
	nodes    map[api.NodeId]*foldNode
	warnings []api.Problem
	publish  map[string]*api.RunPublishValue
	// staged gives the run whose staging each staged node reuses.
	staged map[api.NodeId]api.RunId
	// finishedPublishable is publishable of run.finished.
	finishedPublishable []api.TargetId
}

type foldNode struct {
	r     api.NodeResult
	deps  []api.NodeId
	build bool
}

// NewFold gives an empty fold.
func NewFold(o FoldOptions) *Fold {
	f := &Fold{o: o}
	f.Reset()
	return f
}

// Reset forgets every event: the Tail reads the journal again from its
// start.
func (f *Fold) Reset() {
	o := f.o
	*f = Fold{o: o, nodes: map[api.NodeId]*foldNode{}, publish: map[string]*api.RunPublishValue{}, staged: map[api.NodeId]api.RunId{}}
}

// Take folds one line of the journal; a line that cannot be decoded is
// skipped.
func (f *Fold) Take(b []byte) {
	l, err := Decode(b)
	if err != nil || l.Event == nil {
		return
	}
	f.Add(l)
}

// Started reports that the fold saw run.started.
func (f *Fold) Started() bool { return f.started }

// Finished reports that the fold saw run.finished.
func (f *Fold) Finished() bool { return f.finished }

// Warnings gives the warning events of the run, as problems.
func (f *Fold) Warnings() []api.Problem { return slices.Clone(f.warnings) }

// StagedFrom gives the run whose staging a staged node of the plan reuses
// (plan.ready fromRun).
func (f *Fold) StagedFrom(n api.NodeId) api.RunId { return f.staged[n] }

// Deps gives the dependencies of a build node (plan.ready build[].deps).
func (f *Fold) Deps(n api.NodeId) []api.NodeId {
	if fn, ok := f.nodes[n]; ok {
		return slices.Clone(fn.deps)
	}
	return nil
}

func (f *Fold) node(id api.NodeId) *foldNode {
	if n, ok := f.nodes[id]; ok {
		return n
	}
	pkg, target := api.SplitNode(id)
	n := &foldNode{r: api.NodeResult{Node: id, Package: pkg, Target: target, State: api.NodeStateWaiting}}
	f.nodes[id] = n
	f.order = append(f.order, id)
	return n
}

// Add folds one decoded line. Lines are taken in seq order; a line whose
// seq is not past the last one is skipped, so that a journal read again
// changes nothing.
func (f *Fold) Add(l Line) {
	if l.Seq <= f.lastSeq {
		return
	}
	f.lastSeq = l.Seq
	switch ev := l.Event.(type) {
	case *api.RunStarted:
		f.started = true
		f.run.ID = l.Run
		f.run.Kind = ev.Kind
		f.run.Set = ev.Set
		f.run.State = api.RunStatePlanning
		f.run.Origin = api.RunOriginItem{Via: ev.Origin.Via, User: ev.Origin.User, Host: ev.Origin.Host, PID: ev.Origin.PID, Client: ev.Origin.Client, Session: ev.Origin.Session}
		if ev.Trace != "" {
			f.run.Trace = &api.TraceRef{TraceID: ev.Trace, SpanID: l.Span, ParentSpanID: ev.TraceParent}
		}
		f.run.Selection = ev.Selection
		f.run.SetCommit = ev.SetCommit
		f.run.LockDigest = ev.LockDigest
		f.run.StartedAt = l.Time
		f.run.PublishKind = ev.PublishKind
		f.run.Unpinned = ev.Selection != nil && ev.Selection.From == "worktree"
	case *api.PlanReady:
		f.planned = true
		f.run.State = api.RunStateRunning
		f.run.Plan = &api.RunPlan{Build: ev.Summary.Build, Current: ev.Summary.Current, Staged: ev.Summary.Staged, Skip: ev.Summary.Skip}
		for _, b := range ev.Build {
			n := f.node(b.Node)
			n.build = true
			n.r.State = api.NodeStateWaiting
			n.r.Reasons = slices.Clone(b.Reasons)
			n.deps = slices.Clone(b.Deps)
		}
		for _, c := range ev.Current {
			f.node(c).r.State = api.NodeStateCurrent
		}
		for _, s := range ev.Staged {
			f.node(s).r.State = api.NodeStateStaged
		}
		for id, c := range ev.Commits {
			if n, ok := f.nodes[id]; ok {
				n.r.Commit = c
			}
		}
		for id, r := range ev.FromRun {
			f.staged[id] = r
		}
	case *api.NodeStateEvent:
		n := f.node(l.Node)
		n.r.State = ev.State
		if ev.State == api.NodeStateRunning && n.r.StartedAt == "" {
			n.r.StartedAt = l.Time
		}
		if IsTerminal(ev.State) {
			n.r.FinishedAt = l.Time
			if ev.DurationMs > 0 {
				n.r.DurationMs = ev.DurationMs
			}
			n.r.WaitingFor = nil
		}
		if ev.Error != nil {
			n.r.Error = ev.Error
		}
		if len(ev.BlockedBy) > 0 {
			n.r.BlockedBy = slices.Clone(ev.BlockedBy)
		}
		if len(ev.NVRs) > 0 {
			n.r.NVRs = slices.Clone(ev.NVRs)
		}
		if ev.Checks != nil {
			c := *ev.Checks
			n.r.Checks = &c
		}
	case *api.NodeStepEvent:
		n := f.node(l.Node)
		switch ev.Phase {
		case "started":
			n.r.Steps = append(n.r.Steps, api.NodeResultStep{Step: ev.Step, State: "running", StartedAt: l.Time})
			if ev.Root != "" {
				n.r.Root = ev.Root
			}
		case "ended":
			i := len(n.r.Steps) - 1
			for ; i >= 0 && n.r.Steps[i].Step != ev.Step; i-- {
			}
			if i < 0 {
				n.r.Steps = append(n.r.Steps, api.NodeResultStep{Step: ev.Step})
				i = len(n.r.Steps) - 1
			}
			st := &n.r.Steps[i]
			st.State = ev.Status
			if st.State == "" {
				st.State = "ok"
			}
			st.DurationMs = ev.DurationMs
			st.ExitCode = ev.ExitCode
		}
	case *api.ArtifactAdded:
		n := f.node(l.Node)
		a := api.Artifact{Kind: ev.Kind, Repo: ev.Repo, NVRA: ev.NVRA, File: ev.File, SizeBytes: ev.SizeBytes, Sha256: ev.Sha256}
		if f.o.Paths != nil && f.run.Set != "" {
			a.Path = StagedPath(*f.o.Paths, f.run.Set, f.run.ID, n.r.Target, ev.Repo, ev.File)
		}
		n.r.Artifacts = append(n.r.Artifacts, a)
		switch ev.Kind {
		case "srpm":
			n.r.SRPM = ev.File
		case "rpm":
			if !slices.Contains(n.r.NVRs, ev.NVRA) {
				n.r.NVRs = append(n.r.NVRs, ev.NVRA)
				slices.Sort(n.r.NVRs)
			}
		}
	case *api.WarningEvent:
		p := api.Problem{Code: ev.Code, Message: ev.Message, Target: ev.Target, Run: l.Run}
		if l.Node != "" {
			p.Node = l.Node
			p.Package, p.Target = api.SplitNode(l.Node)
		}
		f.warnings = append(f.warnings, p)
	case *api.CancelRequested:
		f.run.CancelRequested = &api.RunCancelRequested{By: ev.By, User: ev.User, Reason: ev.Reason, Force: ev.Force, At: l.Time}
	case *api.RunFinished:
		f.finished = true
		f.run.State = ev.State
		f.run.FinishedAt = l.Time
		f.run.DurationMs = ev.DurationMs
		f.run.ExitCode = ev.ExitCode
		f.finishedPublishable = slices.Clone(ev.Publishable)
	case *api.PublishStarted:
		f.run.State = api.RunStateRunning
		f.run.FromRuns = slices.Clone(ev.FromRuns)
		s := ev.Signer
		f.run.Signer = &s
		for _, t := range ev.Targets {
			v := f.pub(t)
			if b, ok := ev.Base[t]; ok {
				v.Base = b
			}
		}
	case *api.PublishStep:
		v := f.pub(ev.Target)
		if ev.Phase == "ended" {
			v.Steps = append(v.Steps, api.RunPublishValueStep{Step: ev.Step, State: ev.Status, Count: ev.Count})
		}
	case *api.PublishFinished:
		for _, t := range ev.Targets {
			v := f.pub(t)
			if v.State == "" {
				v.State = ev.State
			}
			if s, ok := ev.Snapshots[t]; ok {
				v.Snapshot = &api.RunPublishValueSnapshot{Tag: s.Tag, Digest: s.Digest}
				v.State = "ok"
			}
		}
	}
}

func (f *Fold) pub(t string) *api.RunPublishValue {
	v, ok := f.publish[t]
	if !ok {
		v = &api.RunPublishValue{}
		f.publish[t] = v
	}
	return v
}

// Abandoned reports that the run has no run.finished and that no process
// of the run holds its lock (Alive), for a run of this host.
func (f *Fold) Abandoned() bool {
	if !f.started || f.finished || f.o.Alive == nil {
		return false
	}
	if f.o.Host != "" && f.run.Origin.Host != "" && f.run.Origin.Host != f.o.Host {
		return false
	}
	r := f.run
	return !f.o.Alive(&r)
}

// Run gives the state of the run so far: a new value that the fold does
// not change later.
func (f *Fold) Run() *api.Run {
	r := f.run
	if r.Selection != nil {
		s := *r.Selection
		r.Selection = &s
	}
	if r.Trace != nil {
		t := *r.Trace
		r.Trace = &t
	}
	if r.CancelRequested != nil {
		c := *r.CancelRequested
		r.CancelRequested = &c
	}
	abandoned := f.Abandoned()
	if abandoned {
		r.State = api.RunStateAbandoned
	}
	r.Nodes = make([]api.NodeResult, 0, len(f.order))
	state := map[api.NodeId]api.NodeState{}
	for _, id := range f.order {
		state[id] = f.nodes[id].r.State
	}
	for _, id := range f.sortedOrder() {
		n := f.nodes[id]
		nr := n.r
		nr.Steps = slices.Clone(nr.Steps)
		nr.Artifacts = slices.Clone(nr.Artifacts)
		nr.NVRs = slices.Clone(nr.NVRs)
		if abandoned && open(nr.State) {
			nr.State = api.NodeStateCanceled
			nr.Checks = &api.NodeChecks{Soname: "skipped"}
			for i := range nr.Steps {
				if nr.Steps[i].State == "running" {
					nr.Steps[i].State = "canceled"
				}
			}
		}
		if nr.State == api.NodeStateWaiting {
			for _, d := range n.deps {
				if s, ok := state[d]; !ok || !done(s) {
					nr.WaitingFor = append(nr.WaitingFor, d)
				}
			}
		}
		if n.build || nr.State == api.NodeStateOk || nr.State == api.NodeStateFailed {
			nr.Logs = f.logs(nr)
		}
		r.Nodes = append(r.Nodes, nr)
		f.count(&r.Counts, nr.State)
	}
	if f.o.Journal != "" {
		r.Files.Journal = f.o.Journal
		if p := strings.TrimSuffix(f.o.Journal, ".jsonl") + ".progress.jsonl"; exists(p) {
			r.Files.Progress = p
		}
		if f.run.ID != "" {
			r.Files.Logs = filepath.Join(f.logsDir(), "") + string(filepath.Separator)
		}
	}
	if r.Kind == KindBuild && f.o.Paths != nil && r.Set != "" && r.ID != "" {
		st := &api.RunStaging{Path: f.o.Paths.Staging(r.Set, r.ID), Targets: map[string]string{}}
		for _, t := range f.targets() {
			st.Targets[t] = f.o.Paths.StagingTarget(r.Set, r.ID, t)
		}
		r.Staging = st
	}
	if f.finished {
		r.Publishable = slices.Clone(f.finishedPublishable)
	}
	if r.Kind == KindBuild && (f.finished || abandoned) && f.planned {
		r.PublishBlockers = f.blockers(r)
	}
	if len(f.publish) > 0 {
		r.Publish = map[string]api.RunPublishValue{}
		for t, v := range f.publish {
			c := *v
			c.Steps = slices.Clone(v.Steps)
			r.Publish[t] = c
		}
	}
	return &r
}

// sortedOrder gives the nodes by target (in the order of the selection),
// then in the order of the plan.
func (f *Fold) sortedOrder() []api.NodeId {
	idx := map[string]int{}
	for i, t := range f.targets() {
		idx[t] = i
	}
	out := slices.Clone(f.order)
	slices.SortStableFunc(out, func(a, b api.NodeId) int {
		_, ta := api.SplitNode(a)
		_, tb := api.SplitNode(b)
		return idx[ta] - idx[tb]
	})
	return out
}

// targets gives the targets of the run: those of the selection, then
// those of the nodes.
func (f *Fold) targets() []string {
	var out []string
	if f.run.Selection != nil {
		out = append(out, f.run.Selection.Targets...)
	}
	for _, id := range f.order {
		_, t := api.SplitNode(id)
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

func (f *Fold) logsDir() string {
	if f.o.Journal == "" {
		return ""
	}
	state := filepath.Dir(filepath.Dir(f.o.Journal))
	return filepath.Join(state, "logs", f.run.ID)
}

// logs gives the log files of a node: its node log, and the mock logs of
// its failed or last step that exist.
func (f *Fold) logs(n api.NodeResult) *api.NodeResultLogs {
	dir := f.logsDir()
	if dir == "" {
		return nil
	}
	l := &api.NodeResultLogs{Node: filepath.Join(dir, n.Node+".log")}
	step := ""
	for _, s := range n.Steps {
		if s.Step != api.StepStage {
			step = string(s.Step)
		}
		if s.State == "failed" {
			break
		}
	}
	if n.Error != nil && n.Error.Step != api.StepStage {
		step = string(n.Error.Step)
	}
	if step != "" {
		for _, name := range []string{"root.log", "build.log", "state.log", "hw_info.log"} {
			p := filepath.Join(dir, n.Node, step, name)
			if exists(p) {
				if l.Mock == nil {
					l.Mock = map[string]string{}
				}
				l.Mock[name] = p
			}
		}
	}
	return l
}

// blockers gives why each target of a finished build run is not
// publishable.
func (f *Fold) blockers(r api.Run) []api.RunPublishBlocker {
	var out []api.RunPublishBlocker
	for _, t := range f.targets() {
		if slices.Contains(r.Publishable, t) {
			continue
		}
		if r.Unpinned {
			out = append(out, api.RunPublishBlocker{Target: t, Code: api.PRunUnpinned, ExitCode: api.ExitNotPublishable,
				Message: "the run built worktree commits (--from worktree); it is never publishable"})
			continue
		}
		var nodes []api.NodeId
		var parts []string
		for _, n := range r.Nodes {
			if n.Target != t {
				continue
			}
			switch n.State {
			case api.NodeStateFailed:
				nodes = append(nodes, n.Node)
				parts = append(parts, n.Node+" failed")
			case api.NodeStateBlocked:
				nodes = append(nodes, n.Node)
				parts = append(parts, n.Node+" is blocked")
			case api.NodeStateCanceled, api.NodeStateWaiting, api.NodeStateQueued, api.NodeStateRunning:
				nodes = append(nodes, n.Node)
				parts = append(parts, n.Node+" is "+string(n.State))
			}
		}
		if len(nodes) == 0 {
			continue
		}
		out = append(out, api.RunPublishBlocker{Target: t, Code: api.PPublishNotReady, ExitCode: api.ExitNotPublishable,
			Nodes: nodes, Message: strings.Join(parts, "; ") + "."})
	}
	return out
}

func (f *Fold) count(c *api.StateCounts, s api.NodeState) {
	switch s {
	case api.NodeStateWaiting:
		c.Waiting++
	case api.NodeStateQueued:
		c.Queued++
	case api.NodeStateRunning:
		c.Running++
	case api.NodeStateOk:
		c.Ok++
	case api.NodeStateCurrent:
		c.Current++
	case api.NodeStateStaged:
		c.Staged++
	case api.NodeStateFailed:
		c.Failed++
	case api.NodeStateBlocked:
		c.Blocked++
	case api.NodeStateCanceled:
		c.Canceled++
	}
}

// open reports a node state that is not terminal in a run.
func open(s api.NodeState) bool {
	return s == api.NodeStateWaiting || s == api.NodeStateQueued || s == api.NodeStateRunning
}

// done reports a state that lets a dependent start.
func done(s api.NodeState) bool {
	return s == api.NodeStateOk || s == api.NodeStateCurrent || s == api.NodeStateStaged
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// FoldLines folds the lines of one journal.
func FoldLines(lines []Line, o FoldOptions) *Fold {
	f := NewFold(o)
	for _, l := range lines {
		f.Add(l)
	}
	return f
}

// FoldFile reads and folds the journal at o.Journal.
func FoldFile(o FoldOptions) (*Fold, error) {
	lines, err := Read(o.Journal)
	if err != nil {
		return nil, err
	}
	return FoldLines(lines, o), nil
}

// StagedPath gives the path of a staged file of a run: the staging of the
// target, the repo and Packages/<first letter>/FILE, as in the published
// layout (build.html#staging).
func StagedPath(p config.Paths, set, run, target, repo, file string) string {
	return filepath.Join(p.StagingTarget(set, run, target), repo, "Packages", letterOf(file), file)
}

func letterOf(file string) string {
	if file == "" {
		return "_"
	}
	return strings.ToLower(file[:1])
}
