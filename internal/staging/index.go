package staging

import (
	"os"
	"sort"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/journal"
	"github.com/dennisklein/repoctl/internal/plan"
)

// Entry is one staged build: a node that ended ok in a build run from the
// lock of this user on this host.
type Entry struct {
	Run    api.RunId
	Node   api.NodeId
	Commit api.Commit
	// SRPM is the file name of the SRPM of the build.
	SRPM string
	// Artifacts are the artifact.added files of the node, with their
	// staged paths.
	Artifacts []api.Artifact
	// Finished is the time of the terminal state of the node.
	Finished api.Time
	// Missing names the files of the node that are gone from the staging
	// or have another size.
	Missing []string
}

// Intact reports that each file of the build is in the staging with its
// size.
func (e Entry) Intact() bool { return len(e.Missing) == 0 }

type key struct{ target, pkg, commit string }

// Index is the staged index of a set on this host (build.html#staging): a
// node is staged when a staging of a run of this user holds a build of the
// same commit for the target, and the staging is intact. The newest run
// wins. A run from worktrees, a run of another host and a node that did
// not end ok never stage a node; a canceled or an abandoned run does.
//
// The index does not know the published state: the plan asks "commit in
// latest?" before it asks the index, so a node that a publish took is
// current, not staged.
type Index struct {
	entries map[key]Entry
}

// NewIndex gives an index of the entries, for tests and for the readers
// that folded the journals themselves.
func NewIndex(entries ...Entry) *Index {
	x := &Index{entries: map[key]Entry{}}
	for _, e := range entries {
		x.add(e)
	}
	return x
}

func (x *Index) add(e Entry) {
	pkg, target := api.SplitNode(e.Node)
	k := key{target, pkg, e.Commit}
	if old, ok := x.entries[k]; ok && old.Finished >= e.Finished {
		return
	}
	x.entries[k] = e
}

// Load reads the staged index of a set from the journals of the runs of
// this host.
func Load(p config.Paths, set, host string) (*Index, error) {
	o := journal.Readers(p, set, host)
	es, err := journal.List(o, set, false)
	if err != nil {
		return nil, err
	}
	x := NewIndex()
	for _, e := range es {
		r := e.Run()
		x.AddRun(r)
	}
	return x, nil
}

// AddRun adds the ok nodes of a folded build run.
func (x *Index) AddRun(r *api.Run) {
	if r.Kind != journal.KindBuild || r.Unpinned || r.Selection == nil || r.Selection.From == "worktree" {
		return
	}
	for _, n := range r.Nodes {
		if n.State != api.NodeStateOk || n.Commit == "" {
			continue
		}
		e := Entry{Run: r.ID, Node: n.Node, Commit: n.Commit, SRPM: n.SRPM, Artifacts: n.Artifacts, Finished: n.FinishedAt}
		for _, a := range n.Artifacts {
			if a.Path == "" {
				continue
			}
			fi, err := os.Stat(a.Path)
			if err != nil || fi.Size() != int64(a.SizeBytes) {
				e.Missing = append(e.Missing, a.Path)
			}
		}
		if len(n.Artifacts) == 0 {
			e.Missing = append(e.Missing, "(no artifact.added)")
		}
		x.add(e)
	}
}

// Find gives the staged build of a node: the newest ok build of the
// commit for the target. Missing reports a staging that is not intact
// (reason staging-missing). It is the lookup of the plan (plan.Staged).
func (x *Index) Find(target, pkg, commit string) (plan.StagedBuild, bool) {
	e, found := x.Lookup(target, pkg, commit)
	if !found {
		return plan.StagedBuild{}, false
	}
	return plan.StagedBuild{Run: e.Run, SRPM: e.SRPM, Missing: !e.Intact()}, true
}

var _ plan.Staged = (*Index)(nil)

// Lookup gives the newest ok build of the commit for the target, intact
// or not. A build whose staging is not intact gives the plan reason
// staging-missing.
func (x *Index) Lookup(target, pkg, commit string) (Entry, bool) {
	e, ok := x.entries[key{target, pkg, commit}]
	return e, ok
}

// Entries gives the newest build of each node and commit, by run and node.
func (x *Index) Entries() []Entry {
	out := make([]Entry, 0, len(x.entries))
	for _, e := range x.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Run != out[j].Run {
			return out[i].Run < out[j].Run
		}
		return out[i].Node < out[j].Node
	})
	return out
}

// Runs gives the runs that hold the newest build of a node, with the
// nodes of each.
func (x *Index) Runs() map[api.RunId][]api.NodeId {
	out := map[api.RunId][]api.NodeId{}
	for _, e := range x.Entries() {
		out[e.Run] = append(out[e.Run], e.Node)
	}
	return out
}
