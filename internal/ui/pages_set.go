package ui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
)

// setHead is the head of the pages of a set.
type setHead struct {
	Set    *setCtx
	Branch string
	Dirty  bool
	Busy   string
	CLI    []string
}

func (pr *pageReq) head(st *api.Status) setHead {
	h := setHead{Set: pr.set}
	if st != nil && st.Set.Git != nil {
		h.Branch, h.Dirty = st.Set.Git.Branch, st.Set.Git.Dirty
	}
	h.Busy = pr.s.busy(pr.set)
	return h
}

// busy says why a build or publish cannot start now: the set lock is
// held. "" when it is free.
func (s *Server) busy(sc *setCtx) string {
	h, err := lock.ReadHolder(s.o.Paths.SetLock(sc.Name))
	if err != nil || h == nil {
		return ""
	}
	what := h.Command
	if what == "" {
		what = "another command"
	}
	msg := fmt.Sprintf("repoctl %s holds the set lock", what)
	if h.Run != "" {
		msg += " (run " + h.Run + ")"
	}
	return msg + ". A second build or publish stops with exit 7 (set.busy)."
}

// todoItem is one item of "Needs you", in work order.
type todoItem struct {
	Class   string
	Kind    string
	Node    string
	State   string
	Text    string
	Detail  string
	ErrLine string
	Code    string
	Href    string
	HrefTxt string
	Pill    string
	Agent   string
	AgentQ  map[string]string
	Dialog  string
}

// matrixRow is a package with its state on each target.
type matrixRow struct {
	Package string
	Version string
	Cells   []matrixCell
	Update  string
}

type matrixCell struct {
	Node   string
	Target string
	State  string
	NVR    string
	Note   string
	Href   string
	Absent bool
}

type overviewPage struct {
	Head      setHead
	StatusNA  *notAvailable
	Status    *api.Status
	Todo      []todoItem
	Adv       advCard
	Active    *foldedRun
	Recent    []*foldedRun
	Snapshots []api.StatusSnapshot
	Matrix    []matrixRow
	Current   int
	Targets   []string
	AgentOff  string
}

// advCard is the advisories card: required ones go to Needs you; warn
// ones show; info ones fold (R1).
type advCard struct {
	NA        *notAvailable
	Required  []api.Advisory
	Warn      []api.Advisory
	Info      []api.Advisory
	Dismissed int
	CheckedAt string
	Stale     bool
}

func (pr *pageReq) advisories(args ...string) (advCard, *api.AdvisoryList) {
	res := pr.s.run(pr.ctx, pr.set, append([]string{"advisories", "ls"}, args...)...)
	c := advCard{NA: na("The advisories", "repoctl advisories ls", res)}
	l, ok := First[api.AdvisoryList](res)
	if !ok {
		return c, nil
	}
	c.NA = nil
	c.CheckedAt, c.Stale = l.Check.At, l.Check.Stale
	for _, a := range l.Advisories {
		switch {
		case a.State == api.AdvisoryStateDismissed:
			c.Dismissed++
		case a.Level == api.AdvisoryLevelRequired:
			c.Required = append(c.Required, a)
		case a.Level == api.AdvisoryLevelWarn:
			c.Warn = append(c.Warn, a)
		default:
			c.Info = append(c.Info, a)
		}
	}
	return c, &l
}

func buildOverview(pr *pageReq) *built {
	o := overviewPage{Targets: pr.set.Targets(), AgentOff: pr.s.agentOff()}
	res := pr.s.run(pr.ctx, pr.set, "status")
	o.StatusNA = na("The status of the set", "repoctl status", res)
	if st, ok := First[api.Status](res); ok {
		o.Status = &st
		o.Snapshots = st.Snapshots
	}
	o.Head = pr.head(o.Status)
	runs := pr.s.runs.list(pr.s.o.Paths.Runs(pr.set.Name), 6)
	for _, f := range runs {
		if f.Active() && o.Active == nil {
			o.Active = f
		}
	}
	o.Recent = runs[:min(len(runs), 4)]
	o.Adv, _ = pr.advisories()
	o.Todo = pr.todo(o.Status, runs, o.Adv)
	o.Matrix, o.Current = pr.matrix(o.Status, runs)
	cli := pr.s.cliPrefix(pr.set.Dir)
	o.Head.CLI = []string{cli + " status", cli + " advisories ls", cli + " runs"}
	b := &built{Title: pr.set.Name, Data: o, CLI: o.Head.CLI}
	b.Live = []liveRegion{
		{Event: "set-head", Block: "set-head", Data: o.Head},
		{Event: "attention", Block: "attention", Data: o},
		{Event: "advisories", Block: "adv-card", Data: o},
		{Event: "recent-runs", Block: "recent-runs", Data: o},
		{Event: "last-publish", Block: "last-publish", Data: o},
		{Event: "matrix", Block: "matrix-card", Data: o},
	}
	return b
}

// todo gives the items of Needs you: failed nodes of the newest run,
// required advisories, the lock, the age of latest.
func (pr *pageReq) todo(st *api.Status, runs []*foldedRun, adv advCard) []todoItem {
	var out []todoItem
	// The open setup steps of a new set or a new target come first
	// (lifecycle.html#setup).
	if st != nil {
		for _, o := range st.Open {
			if o.Done {
				continue
			}
			it := todoItem{Class: "t-warn", Kind: "Setup", Text: o.Step}
			if o.Command != "" {
				it.Detail = "Run: " + o.Command
			}
			out = append(out, it)
		}
	}
	for _, f := range runs {
		if f.Publish() {
			continue
		}
		for _, n := range f.Failed() {
			it := todoItem{Class: "t-failed", Kind: "Run", Node: n.Node, State: "failed", Href: nodeHref(pr.set.Name, f.Run.ID, n.Node), HrefTxt: "Show log",
				Agent: "explain-failure", AgentQ: map[string]string{"run": f.Run.ID, "node": n.Node}}
			it.Text = "failed in " + runLabel(f.Run.ID, pr.s.o.Now())
			if n.Error != nil {
				it.Detail = n.Error.Message
				if n.Error.Excerpt != nil && len(n.Error.Excerpt.Lines) > 0 {
					it.ErrLine = n.Error.Excerpt.Lines[len(n.Error.Excerpt.Lines)-1]
				}
				it.Code = string(n.Error.Class)
			}
			if it.ErrLine == "" {
				it.ErrLine = f.LastLine[n.Node]
			}
			out = append(out, it)
		}
		break // the newest build run only
	}
	for _, a := range adv.Required {
		out = append(out, todoItem{Class: "t-failed", Kind: "Advisory", Node: a.Subject, Text: advTitle(a), Pill: "required", Code: string(a.Code),
			Detail: suggestion(a), Dialog: "rebuild?advisory=" + a.ID, Agent: "rebuild-check", AgentQ: map[string]string{"advisory": a.ID}})
	}
	if st != nil {
		var dirty []string
		for _, p := range st.Packages {
			if p.Lock == api.LockStateDirty || p.Lock == api.LockStateAhead || p.Lock == api.LockStateDiverged {
				dirty = append(dirty, p.Package)
			}
		}
		if len(dirty) > 0 {
			out = append(out, todoItem{Class: "t-warn", Kind: "Lock", Text: "repoctl.lock does not match the worktrees",
				Detail: strings.Join(dirty, ", ") + " have new commits. Pin and commit the lock before you publish.", Code: api.PLockDirty})
		}
		if st.Check != nil && st.Check.Stale {
			out = append(out, todoItem{Class: "t-info", Kind: "Check", Text: "The advisories are old", Detail: "The last check ran " + when(st.Check.At, pr.s.o.Now()) + ".", Code: api.PCheckStale})
		}
		// A stale served tree (served.stale, publish.html#stale-served).
		for _, sn := range st.Snapshots {
			m := sn.Mirror
			if m == nil || !m.Stale {
				continue
			}
			it := todoItem{Class: "t-warn", Kind: "Repo", Code: api.PServedStale}
			if m.BehindSince != nil {
				it.Text = sn.Target + ": the served tree is behind latest since " + when(*m.BehindSince, pr.s.o.Now())
			} else {
				it.Text = sn.Target + ": the mirror job has not checked the served tree since " + when(m.Checked, pr.s.o.Now())
			}
			it.Detail = "Read the journal of the mirror job on the HTTP server: journalctl -u repoctl-mirror.service."
			if m.Error != nil && *m.Error != "" {
				it.ErrLine = "Last error of the mirror job: " + *m.Error
			}
			out = append(out, it)
		}
	}
	return out
}

func advTitle(a api.Advisory) string {
	if a.Title != "" {
		return a.Title
	}
	return a.ID
}

func suggestion(a api.Advisory) string {
	if a.Suggestion != nil {
		return a.Suggestion.Text
	}
	return ""
}

// matrix gives the packages and targets table: from status when it is
// there, else from repoctl.yaml and the newest runs. It leaves out the
// packages that are current on all their targets and counts them.
func (pr *pageReq) matrix(st *api.Status, runs []*foldedRun) ([]matrixRow, int) {
	conf := pr.set.Conf
	if conf == nil {
		return nil, 0
	}
	newest := map[string]*api.NodeResult{}
	runOf := map[string]string{}
	for _, f := range runs {
		for i := range f.Run.Nodes {
			n := &f.Run.Nodes[i]
			if _, ok := newest[n.Node]; !ok {
				newest[n.Node] = n
				runOf[n.Node] = f.Run.ID
			}
		}
	}
	var rows []matrixRow
	current := 0
	for _, p := range conf.Packages {
		row := matrixRow{Package: p.ID}
		allCurrent := true
		pts := conf.PackageTargets(p)
		for _, t := range conf.ActiveTargets() {
			node := api.Node(p.ID, t.ID)
			c := matrixCell{Node: node, Target: t.ID, State: "", Absent: !slices.Contains(pts, t.ID)}
			if n := newest[node]; n != nil && !c.Absent {
				c.State = string(n.State)
				if len(n.NVRs) > 0 {
					c.NVR = n.NVRs[0]
				}
				c.Href = nodeHref(pr.set.Name, runOf[node], node)
				if n.Error != nil {
					c.Note = string(n.Error.Class)
				}
			}
			if !c.Absent && c.State != string(api.NodeStateCurrent) {
				allCurrent = false
			}
			row.Cells = append(row.Cells, c)
		}
		if st != nil && st.Upstream != nil {
			for _, u := range st.Upstream.Updates {
				if u.Package == p.ID {
					row.Update = u.Version
				}
			}
		}
		if allCurrent && row.Update == "" {
			current++
			continue
		}
		rows = append(rows, row)
	}
	return rows, current
}

// packagesPage lists the packages of the set.
type packagesPage struct {
	NA       *notAvailable
	Packages []api.Package
	Conf     []config.Package
	Targets  []string
	Status   map[string]api.StatusPackage
}

func buildPackages(pr *pageReq) *built {
	res := pr.s.run(pr.ctx, pr.set, "list")
	p := packagesPage{Packages: Items[api.Package](res), Targets: pr.set.Targets(), Status: map[string]api.StatusPackage{}}
	if len(p.Packages) == 0 {
		p.NA = na("The package list", "repoctl list", res)
		if pr.set.Conf != nil {
			p.Conf = pr.set.Conf.Packages
		}
	}
	if st, ok := First[api.Status](pr.s.run(pr.ctx, pr.set, "status")); ok {
		for _, sp := range st.Packages {
			p.Status[sp.Package] = sp
		}
	}
	cli := pr.s.cliPrefix(pr.set.Dir)
	return &built{Title: "Packages · " + pr.set.Name, Data: p, CLI: []string{cli + " list"}}
}

// packagePage is one package.
type packagePage struct {
	ID      string
	NA      *notAvailable
	Pkg     *api.Package
	Conf    *config.Package
	Targets []string
	Runs    []pkgRun
}

type pkgRun struct {
	Run  *foldedRun
	Node api.NodeResult
}

func buildPackage(pr *pageReq) *built {
	id := pr.p("pkg")
	res := pr.s.run(pr.ctx, pr.set, "show", id)
	p := packagePage{ID: id, NA: na("The package details", "repoctl show "+id, res)}
	if pk, ok := First[api.Package](res); ok {
		p.Pkg, p.NA = &pk, nil
	}
	if pr.set.Conf != nil {
		if c, ok := pr.set.Conf.Package(id); ok {
			p.Conf = c
			p.Targets = pr.set.Conf.PackageTargets(*c)
		}
	}
	b := &built{Title: id + " · " + pr.set.Name, Data: p, CLI: []string{pr.s.cliPrefix(pr.set.Dir) + " show " + id}}
	if p.Pkg == nil && p.Conf == nil {
		b.Status = 404
	}
	for _, f := range pr.s.runs.list(pr.s.o.Paths.Runs(pr.set.Name), 30) {
		for _, n := range f.Run.Nodes {
			if n.Package == id && n.State != api.NodeStateCurrent {
				p.Runs = append(p.Runs, pkgRun{Run: f, Node: n})
			}
		}
		if len(p.Runs) >= 12 {
			break
		}
	}
	b.Data = p
	return b
}

// planPage is the plan of a build.
type planPage struct {
	Head   setHead
	NA     *notAvailable
	Plan   *api.Plan
	Build  []api.PlanNode
	Staged []api.PlanNode
	Others []api.PlanNode
}

func buildPlan(pr *pageReq) *built {
	res := pr.s.run(pr.ctx, pr.set, "plan")
	p := planPage{Head: pr.head(nil), NA: na("The plan", "repoctl plan", res)}
	if pl, ok := First[api.Plan](res); ok {
		p.Plan = &pl
		for _, n := range pl.Nodes {
			switch n.Action {
			case api.PlanActionBuild:
				p.Build = append(p.Build, n)
			case api.PlanActionStaged:
				p.Staged = append(p.Staged, n)
			default:
				p.Others = append(p.Others, n)
			}
		}
	}
	p.Head.CLI = []string{pr.s.cliPrefix(pr.set.Dir) + " plan"}
	b := &built{Title: "Plan · " + pr.set.Name, Data: p, CLI: p.Head.CLI}
	b.Live = []liveRegion{{Event: "plan", Block: "plan-body", Data: p}}
	return b
}

type advisoriesPage struct {
	Set  string
	Card advCard
	List *api.AdvisoryList
	All  bool
}

func buildAdvisories(pr *pageReq) *built {
	all := pr.p("dismissed") != ""
	var args []string
	if all {
		args = append(args, "--dismissed")
	}
	c, l := pr.advisories(args...)
	p := advisoriesPage{Set: pr.set.Name, Card: c, List: l, All: all}
	cli := pr.s.cliPrefix(pr.set.Dir)
	b := &built{Title: "Advisories · " + pr.set.Name, Data: p, CLI: []string{cli + " advisories ls", cli + " check"}}
	b.Live = []liveRegion{{Event: "advisories", Block: "advisories-body", Data: p}}
	return b
}

type upstreamPage struct {
	NA    *notAvailable
	Items []api.Upstream
}

func buildUpstream(pr *pageReq) *built {
	res := pr.s.run(pr.ctx, pr.set, "upstream")
	p := upstreamPage{Items: Items[api.Upstream](res)}
	if len(p.Items) == 0 {
		p.NA = na("The upstream versions", "repoctl upstream", res)
	}
	return &built{Title: "Upstream · " + pr.set.Name, Data: p, CLI: []string{pr.s.cliPrefix(pr.set.Dir) + " upstream"}}
}

type configPage struct {
	NA       *notAvailable
	Settings []api.Config
	Paths    []api.Path
	Set      *setCtx
}

func buildConfig(pr *pageReq) *built {
	res := pr.s.run(pr.ctx, pr.set, "config", "show")
	p := configPage{Settings: Items[api.Config](res), Set: pr.set, NA: na("The settings", "repoctl config show", res)}
	p.Paths = Items[api.Path](pr.s.run(pr.ctx, pr.set, "config", "paths"))
	cli := pr.s.cliPrefix(pr.set.Dir)
	return &built{Title: "Config · " + pr.set.Name, Data: p, CLI: []string{cli + " config show", cli + " config paths"}}
}

// signingPage is the trusted keys of the set.
type signingPage struct {
	NA      *notAvailable
	List    *api.SignerList
	Keys    []signKey
	Onboard *api.OnboardReport
	Busy    string
}

type signKey struct {
	ID, Fingerprint, State, File string
	Deployed                     bool
	Raw                          map[string]any
}

func buildSigning(pr *pageReq) *built {
	res := pr.s.run(pr.ctx, pr.set, "sign", "ls")
	p := signingPage{NA: na("The key list", "repoctl sign ls", res), Busy: pr.s.busy(pr.set)}
	if l, ok := First[api.SignerList](res); ok {
		p.List = &l
		for _, k := range l.Keys {
			m, _ := k.(map[string]any)
			sk := signKey{Raw: m}
			sk.ID, _ = m["id"].(string)
			sk.Fingerprint, _ = m["fingerprint"].(string)
			sk.State, _ = m["state"].(string)
			sk.Deployed, _ = m["deployed"].(bool)
			p.Keys = append(p.Keys, sk)
		}
	} else if pr.set.Conf != nil {
		for _, k := range pr.set.Conf.Sign.Keys {
			p.Keys = append(p.Keys, signKey{ID: k.ID, Fingerprint: k.Fingerprint, State: k.State, File: k.File})
		}
	}
	if key := pr.p("key"); key != "" {
		if r, ok := First[api.OnboardReport](pr.s.run(pr.ctx, pr.set, "sign", "onboard", key)); ok {
			p.Onboard = &r
		}
	}
	return &built{Title: "Signing · " + pr.set.Name, Data: p, CLI: []string{pr.s.cliPrefix(pr.set.Dir) + " sign ls"}}
}
