package journal

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
)

// Readers gives the fold options of the readers of a set on this host:
// the paths, the host and the abandoned rule of the set lock (the check
// lock for a check run).
func Readers(p config.Paths, set, host string) FoldOptions {
	pp := p
	return FoldOptions{Paths: &pp, Host: host, Alive: LockAlive(p, set)}
}

// LockAlive gives the rule that tells whether a process of a run still
// holds the lock of the run: the set lock of the set, or the check lock
// for a check run, held by a holder that names the run (or, without a run
// in the holder record, has the pid of the run).
func LockAlive(p config.Paths, set string) func(r *api.Run) bool {
	return func(r *api.Run) bool {
		path := p.SetLock(set)
		if r.Kind == KindCheck {
			path = p.CheckLock(set)
		}
		h, err := lock.ReadHolder(path)
		if err != nil {
			return true
		}
		if h == nil {
			return false
		}
		if h.Run != "" {
			return h.Run == r.ID
		}
		return h.PID != 0 && h.PID == r.Origin.PID
	}
}

// Entry is one run of the runs directory.
type Entry struct {
	ID   api.RunId
	Path string
	Fold *Fold
}

// Run gives the folded run of the entry.
func (e Entry) Run() *api.Run { return e.Fold.Run() }

// List folds the journals of the runs directory of a set, newest first.
// Runs of another host are left out unless all is set
// (contract.html#readers).
func List(o FoldOptions, set string, all bool) ([]Entry, error) {
	if o.Paths == nil {
		return nil, nil
	}
	dir := o.Paths.Runs(set)
	des, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		if de.IsDir() || !IsJournal(de.Name()) {
			continue
		}
		id := strings.TrimSuffix(de.Name(), ".jsonl")
		oo := o
		oo.Journal = filepath.Join(dir, de.Name())
		f, err := FoldFile(oo)
		if err != nil || !f.Started() {
			continue
		}
		if !all && o.Host != "" && f.run.Origin.Host != "" && f.run.Origin.Host != o.Host {
			continue
		}
		out = append(out, Entry{ID: id, Path: oo.Journal, Fold: f})
	}
	sortNewest(out)
	return out, nil
}

// sortNewest sorts the runs by their start, newest first.
func sortNewest(es []Entry) {
	sort.SliceStable(es, func(i, j int) bool {
		a, b := es[i].Fold.run.StartedAt, es[j].Fold.run.StartedAt
		if a != b {
			return a > b
		}
		return es[i].ID > es[j].ID
	})
}

// Find folds the journal of one run. "latest" names the newest run of the
// kind (any kind for ""). A run that does not exist gives run.not-found.
func Find(o FoldOptions, set, id, kind string) (Entry, error) {
	if id == "latest" || id == "" {
		es, err := List(o, set, false)
		if err != nil {
			return Entry{}, err
		}
		for _, e := range es {
			if kind == "" || e.Fold.run.Kind == kind {
				return e, nil
			}
		}
		return Entry{}, api.Errorf(api.PRunNotFound, "set %s has no run yet", set).WithHint("start one", "repoctl build")
	}
	if o.Paths == nil {
		return Entry{}, api.Errorf(api.PRunNotFound, "no run %s", id)
	}
	oo := o
	oo.Journal = o.Paths.Journal(set, id)
	f, err := FoldFile(oo)
	if err != nil || !f.Started() {
		return Entry{}, api.Errorf(api.PRunNotFound, "set %s has no run %s on this host", set, id).WithRun(id).
			WithHint("list the runs", "repoctl runs")
	}
	return Entry{ID: id, Path: oo.Journal, Fold: f}, nil
}
