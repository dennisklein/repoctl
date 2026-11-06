package build

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/journal"
	"github.com/dennisklein/repoctl/internal/mock"
)

// node is one build node of the run.
type node struct {
	id          api.NodeId
	pkg, target string
	index       int
	pn          *api.PlanNode
	deps        []*node
	dependents  []*node
	// weight is the number of nodes that depend on the node.
	weight   int
	state    api.NodeState
	readyAt  int
	span     *progress.Span
	ctx      context.Context
	log      *nodeLog
	slot     int
	ext      string
	started  time.Time
	stop     stopper
	finished chan struct{}
}

// stopper is how the run stops the mock child of a node: the cause, which
// gives the failure class, and the kill of the child after the wait.
type stopper struct {
	mu    sync.Mutex
	class api.FailureClass
	msg   string
	kill  context.CancelFunc
}

func (s *stopper) cause() (api.FailureClass, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.class, s.msg
}

// nodeDone is the end of a node as its goroutine tells it.
type nodeDone struct {
	n      *node
	err    *api.NodeError
	short  string
	nvrs   []string
	checks *api.NodeChecks
}

// schedule starts the nodes as their dependencies end and job slots are
// free, most dependents first, then the oldest (build.html#jobs). It
// reports a forced cancel.
func (r *runner) schedule() bool {
	r.done = make(chan *nodeDone, len(r.order))
	for i := 1; i <= r.jobs; i++ {
		r.free = append(r.free, i)
	}
	r.nextSlot = r.jobs + 1
	var deadline <-chan time.Time
	if r.o.Timeouts.Run > 0 {
		t := time.NewTimer(r.o.Timeouts.Run)
		defer t.Stop()
		deadline = t.C
	}
	lockTick := time.NewTicker(r.o.LockEvery)
	defer lockTick.Stop()
	for {
		if !r.stopping() {
			r.startReady()
		}
		if r.running == 0 {
			break
		}
		select {
		case d := <-r.done:
			r.ended(d)
		case req := <-r.o.Cancel:
			if req.Force {
				r.forceCancel(req)
				return true
			}
			r.requestCancel(req)
		case <-deadline:
			r.requestCancel(journal.CancelRequest{By: r.o.Origin.Via, User: r.o.Origin.User, Reason: "deadline"})
		case <-lockTick.C:
			r.checkLock()
		case <-r.jerrCh:
			r.internal = true
			r.stopAll(api.FailureClassInternal, "the journal cannot be written: "+r.jerr.Error())
		}
	}
	// What is still open could not start: its dependencies never ended.
	for _, n := range r.order {
		if n.state == api.NodeStateWaiting || n.state == api.NodeStateQueued {
			r.endOpen(n)
		}
	}
	return false
}

func (r *runner) stopping() bool {
	return r.cancel != nil || r.failFast || r.lockLost || r.internal
}

func depsOK(n *node) bool {
	for _, d := range n.deps {
		if d.state != api.NodeStateOk {
			return false
		}
	}
	return true
}

// startReady moves the nodes whose dependencies are done to running while
// a slot is free, and the others to queued.
func (r *runner) startReady() {
	var ready []*node
	for _, n := range r.order {
		switch n.state {
		case api.NodeStateQueued:
			ready = append(ready, n)
		case api.NodeStateWaiting:
			if depsOK(n) {
				r.readySeq++
				n.readyAt = r.readySeq
				ready = append(ready, n)
			}
		}
	}
	sort.SliceStable(ready, func(i, j int) bool {
		a, b := ready[i], ready[j]
		if a.weight != b.weight {
			return a.weight > b.weight
		}
		if a.readyAt != b.readyAt {
			return a.readyAt < b.readyAt
		}
		return a.index < b.index
	})
	for _, n := range ready {
		if len(r.free) > 0 {
			slot := r.free[0]
			r.free = r.free[1:]
			r.startNode(n, slot)
			continue
		}
		if n.state == api.NodeStateWaiting {
			n.state = api.NodeStateQueued
			n.span.Update(progress.Message("waiting for a job slot"))
			r.emit(n, n.span, r.targets[n.target].span, &api.NodeStateEvent{State: api.NodeStateQueued, Prev: api.NodeStateWaiting})
		}
	}
}

func (r *runner) startNode(n *node, slot int) {
	prev := n.state
	n.state = api.NodeStateRunning
	n.slot = slot
	n.ext = mock.BuildExt(r.o.Set.Name, r.o.RunID, slot)
	n.started = r.now()
	n.finished = make(chan struct{})
	r.running++
	n.span.Run()
	n.span.Update(progress.Message(""))
	r.emit(n, n.span, r.targets[n.target].span, &api.NodeStateEvent{State: api.NodeStateRunning, Prev: prev})
	go r.runNode(n)
}

// ended takes the end of a node: its terminal state, the blocked
// dependents of a failed node, and the slot.
func (r *runner) ended(d *nodeDone) {
	n := d.n
	r.running--
	tr := r.targets[n.target]
	parent := tr.span
	dur := r.since(n.started)
	class, _ := n.stop.cause()
	switch {
	case d.err == nil:
		r.free = append(r.free, n.slot)
		n.state = api.NodeStateOk
		r.emit(n, n.span, parent, &api.NodeStateEvent{State: api.NodeStateOk, Prev: api.NodeStateRunning, DurationMs: dur, NVRs: d.nvrs, Checks: d.checks})
		n.span.End(nil)
		n.log.Marker(fmt.Sprintf("node ok after %s", Dur(time.Duration(dur)*time.Millisecond)))
	case class == api.FailureClassCanceled || d.err.Class == api.FailureClassCanceled:
		r.free = append(r.free, r.newSlot())
		n.state = api.NodeStateCanceled
		r.emit(n, n.span, parent, &api.NodeStateEvent{State: api.NodeStateCanceled, Prev: api.NodeStateRunning, DurationMs: dur,
			Error: d.err, Checks: &api.NodeChecks{Soname: "skipped"}})
		n.span.End(context.Canceled)
		n.log.Marker(fmt.Sprintf("node canceled after %s", Dur(time.Duration(dur)*time.Millisecond)))
	default:
		r.free = append(r.free, r.newSlot())
		n.state = api.NodeStateFailed
		r.emit(n, n.span, parent, &api.NodeStateEvent{State: api.NodeStateFailed, Prev: api.NodeStateRunning, DurationMs: dur,
			Error: d.err, Checks: &api.NodeChecks{Soname: "skipped"}})
		n.span.End(errors.New(d.short))
		for _, b := range r.block(n) {
			n.log.Marker("blocked: " + b.id)
		}
		n.log.Marker(fmt.Sprintf("node failed after %s", Dur(time.Duration(dur)*time.Millisecond)))
		if r.o.FailFast && !r.failFast {
			r.failFast = true
			r.stopAll(api.FailureClassCanceled, "canceled: --fail-fast after the failure of "+n.id)
		}
	}
	n.log.Close()
}

func (r *runner) newSlot() int {
	s := r.nextSlot
	r.nextSlot++
	return s
}

// block ends each open node that depends on the failed node n as
// blocked, with the failed nodes upstream of it.
func (r *runner) block(n *node) []*node {
	var out []*node
	for _, x := range r.order {
		if x.state != api.NodeStateWaiting && x.state != api.NodeStateQueued {
			continue
		}
		if !reach(n)[x] {
			continue
		}
		by := r.failedUpstream(x)
		prev := x.state
		x.state = api.NodeStateBlocked
		r.emit(x, x.span, r.targets[x.target].span, &api.NodeStateEvent{State: api.NodeStateBlocked, Prev: prev, BlockedBy: by,
			Checks: &api.NodeChecks{Soname: "skipped"}})
		x.span.End(blockedErr(by))
		out = append(out, x)
	}
	return out
}

// blockedErr ends the span of a blocked node canceled, with the words of
// the tree (build.html#states).
type blockedErr []api.NodeId

func (b blockedErr) Error() string {
	return "blocked: it needs " + joinIDs(b)
}

func (b blockedErr) Unwrap() error { return context.Canceled }

func joinIDs(ids []api.NodeId) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += id
	}
	return out
}

// failedUpstream gives the failed nodes that x depends on, transitively.
func (r *runner) failedUpstream(x *node) []api.NodeId {
	seen := map[*node]bool{}
	var out []api.NodeId
	var walk func(y *node)
	walk = func(y *node) {
		for _, d := range y.deps {
			if seen[d] {
				continue
			}
			seen[d] = true
			if d.state == api.NodeStateFailed {
				out = append(out, d.id)
				continue
			}
			walk(d)
		}
	}
	walk(x)
	slices.Sort(out)
	return out
}

// endOpen ends a waiting or queued node canceled.
func (r *runner) endOpen(n *node) {
	prev := n.state
	n.state = api.NodeStateCanceled
	r.emit(n, n.span, r.targets[n.target].span, &api.NodeStateEvent{State: api.NodeStateCanceled, Prev: prev,
		Checks: &api.NodeChecks{Soname: "skipped"}})
	n.span.End(context.Canceled)
}

// requestCancel cancels the run: run.cancel-requested, the open nodes end
// canceled, and the running nodes stop.
func (r *runner) requestCancel(req journal.CancelRequest) {
	if r.cancel != nil {
		return
	}
	r.cancel = &req
	r.emit(nil, r.cmdSpan, nil, &api.CancelRequested{By: req.By, User: req.User, Reason: req.Reason})
	r.stopAll(api.FailureClassCanceled, "canceled")
}

// stopAll ends the open nodes canceled and stops the running nodes with
// the class.
func (r *runner) stopAll(class api.FailureClass, msg string) {
	for _, n := range r.order {
		switch n.state {
		case api.NodeStateWaiting, api.NodeStateQueued:
			r.endOpen(n)
		case api.NodeStateRunning:
			r.stopNode(n, class, msg, true)
		}
	}
}

// forceCancel is a second Ctrl-C or repoctl cancel --force: mock
// --orphanskill on the root of each running node without a wait. The run
// ends without run.finished.
func (r *runner) forceCancel(req journal.CancelRequest) {
	r.emit(nil, r.cmdSpan, nil, &api.CancelRequested{By: req.By, User: req.User, Reason: req.Reason, Force: true})
	_ = r.w.Flush()
	var wg sync.WaitGroup
	for _, n := range r.order {
		if n.state == api.NodeStateRunning {
			wg.Add(1)
			go func(n *node) {
				defer wg.Done()
				r.stopNode(n, api.FailureClassCanceled, "canceled", false)
			}(n)
		}
	}
	wg.Wait()
}

// stopNode stops the mock child of a running node: mock --orphanskill on
// its root, then, when wait is set, at most CancelWait for the child
// before the kill. The cause goes first, so that the step that fails
// reads it.
func (r *runner) stopNode(n *node, class api.FailureClass, msg string, wait bool) {
	n.stop.mu.Lock()
	if n.stop.class != "" {
		n.stop.mu.Unlock()
		return
	}
	n.stop.class, n.stop.msg = class, msg
	kill := n.stop.kill
	n.stop.mu.Unlock()
	tr := r.targets[n.target]
	skill := func() {
		ctx, cancel := context.WithTimeout(r.ctx, 2*time.Minute)
		defer cancel()
		_ = r.mock.OrphanSkill(ctx, mock.Call{Files: tr.files, Uniqueext: n.ext})
	}
	if !wait {
		skill()
		return
	}
	go func() {
		skill()
		select {
		case <-n.finished:
		case <-time.After(r.o.CancelWait):
			if kill != nil {
				kill()
			}
		}
	}()
}

// checkLock checks that the lock path still names the locked file
// (build.html#lock-logout).
func (r *runner) checkLock() {
	l := r.o.SetLock
	if l == nil || !l.Lost() {
		return
	}
	if err := l.Retake(r.ctx, r.o.LockWhat); err == nil {
		return
	}
	r.lockLost = true
	r.warn(api.Warning(api.PSetLockLost, "the set lock of %s was removed, and another process holds it now; the run stops", r.o.Set.Name), r.cmdSpan)
	r.requestCancel(journal.CancelRequest{By: r.o.Origin.Via, User: r.o.Origin.User})
}

// Dur gives a duration in the words of the terminal: 4.2s, 1m48s,
// 41m07s, 1h12m.
func Dur(d time.Duration) string {
	switch {
	case d < time.Minute:
		s := d.Seconds()
		if s < 10 {
			return fmt.Sprintf("%.1fs", s)
		}
		return fmt.Sprintf("%ds", int(s))
	case d < time.Hour:
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		return fmt.Sprintf("%dh%02dm", h, m)
	}
}
