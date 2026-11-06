package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/GSI-HPC/go-clikit/progress"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/graph"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/plan"
	"github.com/dennisklein/repoctl/internal/srpm"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// Engine is the plan and step srpm of repoctl: plan.Compute for step plan
// and the SRPM host of the srpm package for step srpm. It implements
// Planner and SRPMBuilder; one Engine serves one run.
//
// Step srpm reads the SRPM cache with the targets of the plan, so a node
// whose SRPM the plan built gets that SRPM: the key is the commit, the
// mock config hash, the macro file and the mock versions
// (build.html#graph).
type Engine struct {
	mu       sync.Mutex
	host     *srpm.Host
	targets  map[string]*srpm.Target
	computed *plan.Computed
}

func (e *Engine) srpmHost(m *mock.Mock, r PlanRequest) *srpm.Host {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.host == nil {
		e.host = srpm.NewHost(m, r.Paths)
	}
	return e.host
}

// Computed gives the result of plan.Compute of the run, or nil before the
// plan.
func (e *Engine) Computed() *plan.Computed {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.computed
}

// Plan runs plan.Compute in the plan root, with a call span for each SRPM
// that is not in the SRPM cache.
func (e *Engine) Plan(ctx context.Context, r PlanRequest) (*Plan, error) {
	h := e.srpmHost(r.Mock, r)
	work := r.Work
	if work == "" {
		work = r.Paths.MockWork(r.Set.Name, "plan-"+fmt.Sprint(os.Getpid()))
		defer os.RemoveAll(work)
	}
	var smu sync.Mutex
	spans := map[api.NodeId]*progress.Span{}
	env := plan.Env{Paths: r.Paths, Set: r.Set, SetDir: r.SetDir, Host: h, Work: work, Uniqueext: mock.PlanExt(), Log: r.Log,
		Progress: func(node api.NodeId, done bool, err error) {
			smu.Lock()
			defer smu.Unlock()
			if !done {
				_, spans[node] = progress.Start(ctx, progress.KindCall, "srpm "+node)
				return
			}
			if sp := spans[node]; sp != nil {
				sp.End(err)
				delete(spans, node)
			}
		}}
	if r.Staged != nil {
		env.Staged = r.Staged
	}
	c, err := plan.Compute(ctx, env, plan.Request{Selectors: r.Selectors, Here: r.Here, Targets: r.Selection.Targets,
		From: r.From, Force: r.Force, WithDeps: r.WithDeps, Rdeps: r.Rdeps})
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.computed = c
	e.targets = c.Targets
	e.mu.Unlock()
	return PlanOf(c), nil
}

// PlanOf gives the plan of a run from the result of plan.Compute.
func PlanOf(c *plan.Computed) *Plan {
	p := &Plan{Plan: c.Plan, Sources: map[api.PackageId]workspace.Source{}, Warnings: c.Warnings()}
	for id, s := range c.Sources {
		p.Sources[id] = s
	}
	p.NotForTarget = notForTarget(c)
	return p
}

// notForTarget gives the requires of the nodes of the graphs that no node
// of the target provides and a node of another target does
// (dep.not-for-target), for the %if hint of a builddep failure.
func notForTarget(c *plan.Computed) []NotForTarget {
	provides := map[string]map[string][]*graph.Info{} // target, capability
	for _, info := range c.Infos {
		if provides[info.Target] == nil {
			provides[info.Target] = map[string][]*graph.Info{}
		}
		for _, p := range info.Provides {
			provides[info.Target][p] = append(provides[info.Target][p], info)
		}
		for _, name := range info.RPMs {
			provides[info.Target][name] = append(provides[info.Target][name], info)
		}
	}
	var out []NotForTarget
	for id, info := range c.Infos {
		if _, ok := c.Graphs[info.Target]; !ok {
			continue
		}
		for _, req := range info.Requires {
			names := graph.RequireNames(req)
			here := false
			for _, n := range names {
				here = here || len(provides[info.Target][n]) > 0
			}
			if here {
				continue
			}
			for _, n := range names {
				byPkg := map[string][]api.TargetId{}
				var pkgs []string
				for t, caps := range provides {
					if t == info.Target {
						continue
					}
					for _, o := range caps[n] {
						if _, ok := byPkg[o.Package]; !ok {
							pkgs = append(pkgs, o.Package)
						}
						if !slices.Contains(byPkg[o.Package], t) {
							byPkg[o.Package] = append(byPkg[o.Package], t)
						}
					}
				}
				sort.Strings(pkgs)
				for _, pk := range pkgs {
					ts := byPkg[pk]
					sort.Strings(ts)
					out = append(out, NotForTarget{Node: id, Requires: n, Package: pk, Targets: ts})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Requires < out[j].Requires
	})
	return out
}

// BuildSRPM runs step srpm with the SRPM host: the SRPM cache first, else
// the source clone, the sources and mock --buildsrpm in the build root of
// the job slot. It moves the mock logs into the result directory of the
// call.
func (e *Engine) BuildSRPM(ctx context.Context, r SRPMRequest) (*SRPMResult, error) {
	h := e.srpmHost(r.Mock, PlanRequest{Paths: r.Paths})
	pkg, target := api.SplitNode(r.Node)
	st, err := e.target(ctx, h, r, target)
	if err != nil {
		return nil, err
	}
	work := r.Work
	if work == "" {
		work = filepath.Dir(r.Clone)
	}
	fedora := ""
	if pk, ok := r.Set.Package(pkg); ok && pk.Upstream != nil {
		fedora = pk.Upstream.Fedora
	}
	log := r.Call.Log
	if log == nil {
		log = io.Discard
	}
	in := srpm.Input{Node: r.Node, Source: r.Source, Target: st, Work: work, Log: log, Uniqueext: r.Call.Uniqueext,
		Fedora: fedora, Remote: r.Set.Remote(), Cache: true}
	res, err := h.Build(ctx, in)
	moveLogs(filepath.Join(work, r.Node, "result"), r.Call.ResultDir)
	if err != nil {
		return nil, err
	}
	if res.Cached {
		fmt.Fprintf(log, "%s from the SRPM cache (key %s)\n", res.File, short(res.Key))
	}
	return &SRPMResult{SRPM: res.SRPM, Sources: res.Sources, Warnings: res.Warnings}, nil
}

// target gives the SRPM target of the plan, or prepares it.
func (e *Engine) target(ctx context.Context, h *srpm.Host, r SRPMRequest, id string) (*srpm.Target, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.targets[id]; st != nil {
		return st, nil
	}
	t, ok := r.Set.Target(id)
	if !ok {
		return nil, fmt.Errorf("target %s is not in the set", id)
	}
	work := r.Work
	if work == "" {
		work = filepath.Dir(r.Clone)
	}
	st, err := h.PrepareTarget(ctx, filepath.Join(work, "targets"), mock.FromConfig(*t, r.SetDir), t.Arch())
	if err != nil {
		return nil, err
	}
	if e.targets == nil {
		e.targets = map[string]*srpm.Target{}
	}
	e.targets[id] = st
	return st, nil
}

// moveLogs moves the mock logs of a result directory into another one.
func moveLogs(from, to string) {
	if to == "" {
		return
	}
	logs, _ := filepath.Glob(filepath.Join(from, "*.log"))
	if len(logs) == 0 {
		return
	}
	_ = os.MkdirAll(to, 0o755)
	for _, l := range logs {
		_ = os.Rename(l, filepath.Join(to, filepath.Base(l)))
	}
}
