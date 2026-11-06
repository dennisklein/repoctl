package ui

import (
	"slices"

	"github.com/dennisklein/repoctl/internal/api"
)

// repoPage is the repository of one target: latest, or the staging of a
// run.
type repoPage struct {
	Set      string
	Target   string
	Run      string
	NA       *notAvailable
	Contents []api.RepoContents
	Staging  []*foldedRun
	Removed  []api.RepoContentsRemovedItem
}

func buildRepo(pr *pageReq) *built {
	t, run := pr.p("target"), pr.p("run")
	args := []string{"repo", "ls", "--target", t}
	cli := pr.s.cliPrefix(pr.set.Dir) + " repo ls --target " + t
	if run != "" {
		args = append(args, "--run", run)
		cli += " --run " + run
	}
	res := pr.s.run(pr.ctx, pr.set, args...)
	p := repoPage{Set: pr.set.Name, Target: t, Run: run, Contents: Items[api.RepoContents](res)}
	if len(p.Contents) == 0 {
		p.NA = na("The repository list", "repoctl repo ls", res)
	}
	for _, f := range pr.s.runs.list(pr.s.o.Paths.Runs(pr.set.Name), 20) {
		if f.Run.Staging != nil && f.Run.Staging.Targets[t] != "" {
			p.Staging = append(p.Staging, f)
		}
	}
	b := &built{Title: t + " · " + pr.set.Name, Data: p, CLI: []string{cli}}
	if !slices.Contains(pr.set.Targets(), t) && pr.set.Conf != nil {
		if _, ok := pr.set.Conf.Target(t); !ok {
			b.Status = 404
		}
	}
	return b
}

type diffPage struct {
	Set    string
	Target string
	Run    string
	NA     *notAvailable
	Diffs  []api.RepoDiff
}

func buildDiff(pr *pageReq) *built {
	t, run := pr.p("target"), pr.p("run")
	args := []string{"repo", "diff", "--target", t}
	if run != "" {
		args = append(args, "--run", run)
	}
	res := pr.s.run(pr.ctx, pr.set, args...)
	p := diffPage{Set: pr.set.Name, Target: t, Run: run, Diffs: Items[api.RepoDiff](res)}
	if len(p.Diffs) == 0 {
		p.NA = na("The comparison with latest", "repoctl repo diff", res)
	}
	return &built{Title: "Diff " + t + " · " + pr.set.Name, Data: p, CLI: []string{pr.s.cliPrefix(pr.set.Dir) + " repo diff --target " + t}}
}

// publishPage is the review of a publish: what it signs and pushes per
// target, the gate of the advisories and what blocks it.
type publishPage struct {
	Head     setHead
	Target   string
	Targets  []string
	FromRun  string
	NA       *notAvailable
	Plan     *api.Run
	Order    []string
	Blockers []api.RunPublishBlocker
	Required []api.RunPublishValueGateItem
	Active   *foldedRun
	Last     *foldedRun
	Problem  *api.Problem
}

// Blocked reports whether something blocks the publish that the human
// cannot accept here.
func (p publishPage) Blocked() bool { return len(p.Blockers) > 0 }

func buildPublish(pr *pageReq) *built {
	p := publishPage{Head: pr.head(nil), Target: pr.p("target"), Targets: pr.set.Targets(), FromRun: pr.p("run")}
	args := []string{"publish", "--dry-run"}
	if p.Target != "" {
		args = append(args, "--target", p.Target)
	}
	if p.FromRun != "" {
		args = append(args, "--run", p.FromRun)
	}
	res := pr.s.run(pr.ctx, pr.set, args...)
	p.NA = na("The publish review", "repoctl publish --dry-run", res)
	if r, ok := First[api.Run](res); ok {
		p.NA = nil
		p.Plan = &r
		p.Blockers = r.PublishBlockers
		for t, v := range r.Publish {
			p.Order = append(p.Order, t)
			for _, g := range v.Gate {
				if g.Blocks && !g.Accepted && !slices.ContainsFunc(p.Required, func(x api.RunPublishValueGateItem) bool { return x.ID == g.ID }) {
					p.Required = append(p.Required, g)
				}
			}
		}
		slices.Sort(p.Order)
		if len(r.Nodes) == 0 && len(r.Publish) == 0 && len(res.Errors) > 0 {
			p.Problem = &res.Errors[0]
		}
	} else if pb := res.Problem(); pb != nil && !res.NotImplemented() && res.Err == nil {
		// publish --dry-run explains why there is nothing to publish.
		p.Problem, p.NA = pb, nil
	}
	for _, f := range pr.s.runs.list(pr.s.o.Paths.Runs(pr.set.Name), 20) {
		if !f.Publish() {
			continue
		}
		if f.Active() && p.Active == nil {
			p.Active = f
		}
		if !f.Active() && p.Last == nil {
			p.Last = f
		}
	}
	cli := pr.s.cliPrefix(pr.set.Dir)
	p.Head.CLI = []string{cli + " publish --dry-run", cli + " publish"}
	b := &built{Title: "Publish · " + pr.set.Name, Data: p, CLI: p.Head.CLI}
	b.Live = []liveRegion{{Event: "publish", Block: "publish-body", Data: p}}
	return b
}
