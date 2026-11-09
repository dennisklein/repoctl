package ui

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
)

// foldedRun is a run as its journal tells it so far: the Run of the
// contract, and what a live page shows besides (the last log line of each
// node, the warnings, the publish steps).
type foldedRun struct {
	Run      api.Run
	LastLine map[api.NodeId]string
	Warnings []api.Problem
	Path     string
	Size     int64
	Mod      time.Time
}

// Active reports whether the run has not finished.
func (f *foldedRun) Active() bool {
	return f.Run.State == api.RunStatePlanning || f.Run.State == api.RunStateRunning
}

// Publish reports whether the run is a publish run.
func (f *foldedRun) Publish() bool {
	return f.Run.Kind == "publish" || strings.HasPrefix(f.Run.ID, "p-")
}

// Node gives the result of node n.
func (f *foldedRun) Node(n api.NodeId) *api.NodeResult {
	for i := range f.Run.Nodes {
		if f.Run.Nodes[i].Node == n {
			return &f.Run.Nodes[i]
		}
	}
	return nil
}

// Failed gives the failed nodes.
func (f *foldedRun) Failed() []api.NodeResult {
	var out []api.NodeResult
	for _, n := range f.Run.Nodes {
		if n.State == api.NodeStateFailed {
			out = append(out, n)
		}
	}
	return out
}

// Builds gives the nodes the run builds (not those current or staged before).
func (f *foldedRun) Builds() []api.NodeResult {
	var out []api.NodeResult
	for _, n := range f.Run.Nodes {
		if n.State != api.NodeStateCurrent && n.State != api.NodeStateStaged || len(n.Reasons) > 0 {
			out = append(out, n)
		}
	}
	return out
}

// Done counts the builds that ended.
func (f *foldedRun) Done() int {
	n := 0
	for _, b := range f.Builds() {
		switch b.State {
		case api.NodeStateOk, api.NodeStateFailed, api.NodeStateBlocked, api.NodeStateCanceled:
			n++
		}
	}
	return n
}

// foldJournal reads the journal at path. A line that does not decode is
// skipped: the last line of a live journal may be half written.
func foldJournal(path string) (*foldedRun, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a journal of the user's state directory
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fr := &foldedRun{Path: path, Size: st.Size(), Mod: st.ModTime(), LastLine: map[api.NodeId]string{}}
	fr.Run.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	fr.Run.State = api.RunStatePlanning
	fr.Run.Files.Journal = path
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		fr.apply(sc.Bytes())
	}
	fr.finish()
	return fr, nil
}

func (f *foldedRun) apply(line []byte) {
	var h api.EventHeader
	if json.Unmarshal(line, &h) != nil || h.Type == "" {
		return
	}
	mk, ok := api.EventStructs[h.Type]
	if !ok {
		return
	}
	ev := mk()
	if json.Unmarshal(line, ev) != nil {
		return
	}
	r := &f.Run
	switch e := ev.(type) {
	case *api.RunStarted:
		r.ID, r.Kind, r.Set, r.StartedAt = e.Run, e.Kind, e.Set, e.Time
		r.Origin = api.RunOriginItem{Via: e.Origin.Via, User: e.Origin.User, Host: e.Origin.Host, PID: e.Origin.PID, Client: e.Origin.Client, Session: e.Origin.Session}
		r.Selection, r.SetCommit, r.LockDigest, r.PublishKind = e.Selection, e.SetCommit, e.LockDigest, e.PublishKind
		if e.Trace != "" {
			r.Trace = &api.TraceRef{TraceID: e.Trace, SpanID: e.Span}
		}
	case *api.PlanReady:
		r.State = api.RunStateRunning
		r.Plan = &api.RunPlan{Build: e.Summary.Build, Current: e.Summary.Current, Staged: e.Summary.Staged, Skip: e.Summary.Skip}
		for _, b := range e.Build {
			f.addNode(b.Node, api.NodeStateWaiting, e.Commits[b.Node], b.Reasons, b.Deps)
		}
		for _, n := range e.Current {
			f.addNode(n, api.NodeStateCurrent, e.Commits[n], nil, nil)
		}
		for _, n := range e.Staged {
			f.addNode(n, api.NodeStateStaged, e.Commits[n], nil, nil)
		}
	case *api.NodeStateEvent:
		n := f.Node(e.Node)
		if n == nil {
			f.addNode(e.Node, e.State, "", nil, nil)
			n = f.Node(e.Node)
		}
		n.State = e.State
		switch e.State {
		case api.NodeStateRunning:
			if n.StartedAt == "" {
				n.StartedAt = e.Time
			}
		case api.NodeStateOk, api.NodeStateFailed, api.NodeStateBlocked, api.NodeStateCanceled:
			n.FinishedAt = e.Time
		}
		if e.DurationMs > 0 {
			n.DurationMs = e.DurationMs
		}
		if e.Error != nil {
			n.Error = e.Error
		}
		if len(e.BlockedBy) > 0 {
			n.BlockedBy = e.BlockedBy
		}
		if len(e.NVRs) > 0 {
			n.NVRs = e.NVRs
		}
		if e.Checks != nil {
			n.Checks = e.Checks
		}
	case *api.NodeStepEvent:
		n := f.Node(e.Node)
		if n == nil {
			return
		}
		switch e.Phase {
		case "started":
			n.Steps = append(n.Steps, api.NodeResultStep{Step: e.Step, State: "running", StartedAt: e.Time})
		case "ended":
			for i := range n.Steps {
				if n.Steps[i].Step == e.Step {
					n.Steps[i].State, n.Steps[i].DurationMs, n.Steps[i].ExitCode = e.Status, e.DurationMs, e.ExitCode
					return
				}
			}
			n.Steps = append(n.Steps, api.NodeResultStep{Step: e.Step, State: e.Status, DurationMs: e.DurationMs, ExitCode: e.ExitCode})
		}
	case *api.NodeLogEvent:
		f.LastLine[e.Node] = e.Line
	case *api.StagingUpdated:
		if r.Staging == nil {
			r.Staging = &api.RunStaging{Targets: map[string]string{}}
		}
		r.Staging.Targets[e.Target] = e.Revision
	case *api.WarningEvent:
		f.Warnings = append(f.Warnings, api.Problem{Code: e.Code, Message: e.Message, Node: e.Node, Target: e.Target})
	case *api.CancelRequested:
		r.CancelRequested = &api.RunCancelRequested{By: e.By, User: e.User, Reason: e.Reason, Force: e.Force, At: e.Time}
	case *api.RunFinished:
		r.State, r.Counts, r.DurationMs, r.ExitCode, r.FinishedAt = e.State, e.Counts, e.DurationMs, e.ExitCode, e.Time
		r.Publishable = e.Publishable
	case *api.PublishStarted:
		r.Kind = "publish"
		r.State = api.RunStateRunning
		r.FromRuns = e.FromRuns
		s := e.Signer
		r.Signer = &s
		if r.Publish == nil {
			r.Publish = map[string]api.RunPublishValue{}
		}
		for _, t := range e.Targets {
			p := r.Publish[t]
			p.State = "running"
			p.Base = e.Base[t]
			r.Publish[t] = p
		}
	case *api.PublishStep:
		if r.Publish == nil {
			r.Publish = map[string]api.RunPublishValue{}
		}
		p := r.Publish[e.Target]
		state := e.Status
		if e.Phase == "started" {
			state = "running"
		}
		found := false
		for i := range p.Steps {
			if p.Steps[i].Step == e.Step {
				p.Steps[i].State, p.Steps[i].Count, found = state, e.Count, true
			}
		}
		if !found {
			p.Steps = append(p.Steps, api.RunPublishValueStep{Step: e.Step, State: state, Count: e.Count})
		}
		r.Publish[e.Target] = p
	case *api.PublishFinished:
		r.State = api.RunState(e.State)
		r.ExitCode, r.FinishedAt = e.ExitCode, e.Time
		for t, s := range e.Snapshots {
			p := r.Publish[t]
			p.State = e.State
			p.Snapshot = &api.RunPublishValueSnapshot{Tag: s.Tag, Digest: s.Digest}
			if r.Publish == nil {
				r.Publish = map[string]api.RunPublishValue{}
			}
			r.Publish[t] = p
		}
	}
}

func (f *foldedRun) addNode(n api.NodeId, st api.NodeState, commit api.Commit, reasons []api.PlanReasonCode, deps []api.NodeId) {
	if f.Node(n) != nil {
		return
	}
	pkg, tgt := api.SplitNode(n)
	f.Run.Nodes = append(f.Run.Nodes, api.NodeResult{Node: n, Package: pkg, Target: tgt, State: st, Commit: commit, Reasons: reasons, WaitingFor: deps})
}

// finish counts the node states of a run that has not finished, and marks
// a run abandoned whose process on this host is gone.
func (f *foldedRun) finish() {
	r := &f.Run
	if r.FinishedAt != "" {
		return
	}
	var c api.StateCounts
	for _, n := range r.Nodes {
		switch n.State {
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
	r.Counts = c
	if r.Origin.PID > 0 && (r.Origin.Host == "" || r.Origin.Host == shortHost()) && !processAlive(r.Origin.PID) {
		r.State = api.RunStateAbandoned
	}
}

// processAlive reports whether the process pid runs on this host.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

var shortHost = sync.OnceValue(func() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	return h
})

// runStore folds the journals of the sets and keeps each fold until its
// file changes.
type runStore struct {
	mu    sync.Mutex
	folds map[string]*foldedRun
}

func newRunStore() *runStore { return &runStore{folds: map[string]*foldedRun{}} }

// get gives the fold of the journal at path.
func (s *runStore) get(path string) (*foldedRun, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	f, ok := s.folds[path]
	s.mu.Unlock()
	if ok && f.Size == st.Size() && f.Mod.Equal(st.ModTime()) && !f.Active() {
		return f, nil
	}
	if ok && f.Size == st.Size() && f.Mod.Equal(st.ModTime()) && f.Run.State != api.RunStateAbandoned {
		// An active run: its process may have died since.
		if f.Run.Origin.PID == 0 || processAlive(f.Run.Origin.PID) {
			return f, nil
		}
	}
	f, err = foldJournal(path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.folds[path] = f
	s.mu.Unlock()
	return f, nil
}

// list gives the runs in dir, newest first, at most limit (0: all).
func (s *runStore) list(dir string, limit int) []*foldedRun {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".jsonl") && !strings.HasSuffix(n, ".progress.jsonl") && (strings.HasPrefix(n, "r-") || strings.HasPrefix(n, "p-")) {
			names = append(names, n)
		}
	}
	// r-YYYYMMDD-HHMM-xxx and p-…: newest first by the time in the id.
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(b[2:], a[2:]) })
	var out []*foldedRun
	for _, n := range names {
		f, err := s.get(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		out = append(out, f)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}
