package ui

import (
	"slices"

	"github.com/dennisklein/repoctl/internal/api"
)

type runsPage struct {
	Set  string
	Runs []*foldedRun
}

func buildRuns(pr *pageReq) *built {
	p := runsPage{Set: pr.set.Name, Runs: pr.s.runs.list(pr.s.o.Paths.Runs(pr.set.Name), 50)}
	b := &built{Title: "Runs · " + pr.set.Name, Data: p, CLI: []string{pr.s.cliPrefix(pr.set.Dir) + " runs"}}
	b.Live = []liveRegion{{Event: "runs-list", Block: "runs-list", Data: p}}
	return b
}

// runPage is one run: its progress, its failures first (R1) and its nodes.
type runPage struct {
	Set      string
	NA       *notAvailable
	Run      *api.Run
	Fold     *foldedRun
	Failed   []api.NodeResult
	Builds   []api.NodeResult
	Current  int
	Done     int
	Segs     []progSeg
	LastLine map[api.NodeId]string
	Warnings []api.Problem
	Publish  bool
	Targets  []string
	Agent    bool
}

// progSeg is one part of the progress bar; Pct is its width.
type progSeg struct {
	Class string
	Pct   int
	N     int
	Label string
}

func (pr *pageReq) loadRun(id string) (*foldedRun, *api.Run, *notAvailable) {
	if f, err := pr.s.runs.get(pr.s.o.Paths.Journal(pr.set.Name, id)); err == nil {
		return f, &f.Run, nil
	}
	res := pr.s.run(pr.ctx, pr.set, "runs", id)
	if r, ok := First[api.Run](res); ok {
		return nil, &r, nil
	}
	n := na("The run "+id, "repoctl runs "+id, res)
	if n == nil {
		n = &notAvailable{What: "The run " + id, Command: "repoctl runs " + id}
	}
	if !n.NotImpl && n.Problem == nil {
		n.Problem = &api.Problem{Code: api.PRunNotFound, Message: "No journal of the run " + id + " is on this host."}
	}
	return nil, nil, n
}

func buildRun(pr *pageReq) *built {
	id := pr.p("run")
	p := runPage{Set: pr.set.Name, Targets: pr.set.Targets(), Agent: pr.s.agentOff() == ""}
	f, r, n := pr.loadRun(id)
	p.NA, p.Fold, p.Run = n, f, r
	b := &built{Title: shortRun(id) + " · " + pr.set.Name, CLI: []string{pr.s.cliPrefix(pr.set.Dir) + " runs " + id}}
	if r == nil {
		b.Status = 404
		b.Data = p
		return b
	}
	if f != nil {
		p.LastLine, p.Warnings, p.Publish = f.LastLine, f.Warnings, f.Publish()
	}
	for _, nd := range r.Nodes {
		switch {
		case nd.State == api.NodeStateFailed:
			p.Failed = append(p.Failed, nd)
			p.Builds = append(p.Builds, nd)
		case nd.State == api.NodeStateCurrent || nd.State == api.NodeStateStaged && len(nd.Reasons) == 0:
			p.Current++
		default:
			p.Builds = append(p.Builds, nd)
		}
	}
	// Exceptions first: failed and blocked, then running, then the rest.
	order := map[api.NodeState]int{api.NodeStateFailed: 0, api.NodeStateBlocked: 1, api.NodeStateRunning: 2, api.NodeStateQueued: 3, api.NodeStateWaiting: 4}
	slices.SortStableFunc(p.Builds, func(a, b api.NodeResult) int {
		oa, ok := order[a.State]
		if !ok {
			oa = 5
		}
		ob, ok := order[b.State]
		if !ok {
			ob = 5
		}
		return oa - ob
	})
	p.Segs, p.Done = progress(p.Builds)
	b.Data = p
	b.Live = []liveRegion{{Event: "run:" + id, Block: "run-body", Data: p}}
	return b
}

// progress gives the segments of the progress bar of the builds.
func progress(builds []api.NodeResult) ([]progSeg, int) {
	var ok, failed, blocked, running, waiting int
	for _, n := range builds {
		switch n.State {
		case api.NodeStateOk, api.NodeStateStaged:
			ok++
		case api.NodeStateFailed:
			failed++
		case api.NodeStateBlocked, api.NodeStateCanceled:
			blocked++
		case api.NodeStateRunning:
			running++
		default:
			waiting++
		}
	}
	total := len(builds)
	segs := []progSeg{
		{Class: "p-ok", N: ok, Label: "ok"}, {Class: "p-failed", N: failed, Label: "failed"},
		{Class: "p-blocked", N: blocked, Label: "blocked"}, {Class: "p-current", N: running, Label: "running"},
		{Class: "p-waiting", N: waiting, Label: "waiting"},
	}
	out := segs[:0]
	for _, s := range segs {
		if s.N > 0 {
			s.Pct = max(pct(s.N, total), 1)
			out = append(out, s)
		}
	}
	return out, ok + failed + blocked
}

// nodePage is the log of one node in a run.
type nodePage struct {
	Set   string
	Run   *api.Run
	NA    *notAvailable
	Node  *api.NodeResult
	ID    string
	Steps []stepView
	Log   *logTail
	Agent bool
	Live  bool
}

func buildNode(pr *pageReq) *built {
	id, node := pr.p("run"), pr.p("node")
	p := nodePage{Set: pr.set.Name, ID: node, Agent: pr.s.agentOff() == ""}
	f, r, n := pr.loadRun(id)
	p.Run, p.NA = r, n
	b := &built{Title: node + " · " + shortRun(id), CLI: []string{pr.s.cliPrefix(pr.set.Dir) + " logs " + id + " " + node}}
	if r == nil {
		b.Status, b.Data = 404, p
		return b
	}
	for i := range r.Nodes {
		if r.Nodes[i].Node == node {
			p.Node = &r.Nodes[i]
		}
	}
	if p.Node == nil {
		p.NA = &notAvailable{What: "The node " + node, Problem: &api.Problem{Code: api.PNodeUnknown, Message: "The run " + id + " has no node " + node + "."}}
		b.Status, b.Data = 404, p
		return b
	}
	p.Steps = stepsOf(*p.Node)
	p.Log = readLogTail(node, pr.s.o.Paths.NodeLog(pr.set.Name, id, node))
	p.Live = f != nil && f.Active()
	b.Data, b.Log = p, p.Log
	b.Live = []liveRegion{{Event: "node:" + node, Block: "node-head", Data: p}}
	return b
}
