package workspace

import (
	"context"
	"fmt"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// PinOptions are the arguments and flags of pin.
type PinOptions struct {
	// Packages are the named packages; nil pins all packages and removes
	// the orphaned lines.
	Packages []string
	// Older writes a HEAD commit that is behind the lock (with Packages).
	Older bool
	// Orphans removes only the orphaned lines.
	Orphans bool
	// Fetch fetches first, so that the warning "not on origin" is current.
	Fetch bool
}

// PinRow is the result of pin for one package.
type PinRow struct {
	// Entry is the lock line after pin, with previous and changed.
	Entry api.LockEntry
	// Subject is the subject of a newly pinned commit.
	Subject string
	// OldBranch and OldURL are the branch and the URL of the line before a
	// change of them.
	OldBranch, OldURL string
	// Removed reports an orphaned line that pin removed.
	Removed bool
	// New reports a package that had no line.
	New bool
}

// Changed reports a row whose line pin wrote.
func (r PinRow) Changed() bool { return r.Entry.Changed }

// PinReport is the result of pin.
type PinReport struct {
	Rows     []PinRow
	Warnings []api.Problem
	// Written reports that pin wrote repoctl.lock.
	Written bool
}

// Pin writes the HEAD commit of each selected worktree into repoctl.lock,
// with the branch and the URL of repoctl.yaml (cli.html#cmd-pin). It checks
// every selected package before it writes: a dirty or diverged worktree,
// or a named worktree that is behind, stops it (exit 5), and then it
// writes nothing.
func Pin(ctx context.Context, e Env, o PinOptions) (*PinReport, error) {
	rep := &PinReport{}
	if o.Fetch {
		rep.Warnings = append(rep.Warnings, fetchAll(ctx, e, Selected(e.Set, o.Packages))...)
	}
	lf, err := lock.Read(e.SetDir)
	if err != nil {
		return nil, err
	}
	states, err := States(ctx, e.Paths, e.Set, e.SetDir, lf)
	if err != nil {
		return nil, err
	}
	named := map[string]bool{}
	for _, id := range o.Packages {
		named[id] = true
	}
	// The changes: a new line per package, or a removal.
	set := map[string]lock.Entry{}
	var remove []string
	var stops []*api.Error
	for _, s := range states {
		if o.Orphans {
			if s.State == api.LockStateOrphaned {
				remove = append(remove, s.Package)
				rep.Rows = append(rep.Rows, PinRow{Entry: removedEntry(s), Removed: true})
			}
			continue
		}
		if o.Packages != nil && !named[s.Package] {
			continue
		}
		row, entry, stop, warn := pinOne(e, s, named[s.Package], o.Older)
		if stop != nil {
			stops = append(stops, stop)
			continue
		}
		if warn != nil {
			rep.Warnings = append(rep.Warnings, *warn)
		}
		switch {
		case row.Removed:
			remove = append(remove, s.Package)
		case entry != nil:
			set[s.Package] = *entry
		}
		rep.Rows = append(rep.Rows, row)
	}
	if len(stops) > 0 {
		first := stops[0]
		if len(stops) > 1 {
			var rest []string
			for _, s := range stops[1:] {
				rest = append(rest, s.Package+" ("+s.Code+")")
			}
			first.Message += "; also " + strings.Join(rest, ", ")
		}
		first.Message += "; repoctl.lock is not written"
		return nil, first
	}
	if len(set) == 0 && len(remove) == 0 {
		return rep, nil
	}
	order := packageOrder(e.Set)
	if _, err := lock.Update(ctx, e.SetDir, order, func(f *lock.File) error {
		for id, entry := range set {
			f.Set(id, entry)
		}
		for _, id := range remove {
			f.Delete(id)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	rep.Written = true
	_ = Exclude(ctx, e.SetDir, excludeLockFile())
	for _, r := range rep.Rows {
		if r.Entry.Changed && !r.Removed && r.Entry.Commit != r.Entry.Previous {
			if w := unpushedWarning(ctx, e, r.Entry); w != nil {
				rep.Warnings = append(rep.Warnings, *w)
			}
		}
	}
	return rep, nil
}

// packageOrder gives the package ids in the order of repoctl.yaml: the
// order of the lock lines.
func packageOrder(set *config.Set) []string {
	var order []string
	for _, p := range set.Packages {
		order = append(order, p.ID)
	}
	return order
}

func removedEntry(s PackageState) api.LockEntry {
	return api.LockEntry{Package: s.Package, Branch: s.Lock.Branch, URL: s.Lock.URL, Commit: s.Lock.Commit,
		Previous: s.Lock.Commit, State: api.LockStateOrphaned, Changed: true}
}

// pinOne decides the line of one package by its lock state
// (cli.html#cmd-pin, the table of lock states).
func pinOne(e Env, s PackageState, named, older bool) (PinRow, *lock.Entry, *api.Error, *api.Problem) {
	row := PinRow{Entry: s.Entry()}
	write := func(commit string) *lock.Entry {
		ent := &lock.Entry{Branch: s.Branch, Commit: commit, URL: s.URL}
		row.Entry = api.LockEntry{Package: s.Package, Branch: s.Branch, URL: s.URL, Commit: commit,
			Previous: s.Lock.Commit, State: api.LockStateInSync, Changed: true}
		if commit != s.Lock.Commit {
			row.Subject = s.HeadSubject
		}
		if s.Pinned && s.Lock.Branch != s.Branch {
			row.OldBranch = s.Lock.Branch
		}
		if s.Pinned && s.Lock.URL != s.URL {
			row.OldURL = s.Lock.URL
		}
		row.New = !s.Pinned
		return ent
	}
	warning := func(code api.ProblemCode, msg string) *api.Problem {
		p := api.Warning(code, "%s", msg)
		p.Package = s.Package
		p.Hint = s.Hint().String()
		return &p
	}
	switch s.State {
	case api.LockStateInTree:
		return row, nil, nil, nil
	case api.LockStateDirty:
		err := StateError(api.PLockDirty, s)
		err.Message += "; uncommitted changes are never pinned"
		return row, nil, err, nil
	case api.LockStateDiverged:
		return row, nil, StateError(api.PLockDiverged, s), nil
	case api.LockStateBehind:
		if !named {
			return row, nil, nil, warning(api.PLockBehind, s.Message()+": the line is kept")
		}
		if !older {
			err := StateError(api.PLockBehind, s)
			err = err.WithHint("pin the older commit on purpose", "repoctl pin "+s.Package+" --older")
			return row, nil, err, nil
		}
		ent := write(s.Head)
		w := warning(api.PLockBehind, fmt.Sprintf("%s: pinned the older commit %s (was %s)", s.Package, api.ShortCommit(s.Head), api.ShortCommit(s.Lock.Commit)))
		w.Hint = ""
		return row, ent, nil, w
	case api.LockStateAhead:
		return row, write(s.Head), nil, nil
	case api.LockStateUnpinned:
		if s.Head == "" {
			msg := s.Package + ": " + RelWorktree(s.Package) + " is missing: nothing to pin"
			if IsWorktree(s.Worktree) {
				msg = s.Package + ": " + RelWorktree(s.Package) + " has no commit yet: nothing to pin"
			}
			w := warning(api.PWorktreeMissing, msg)
			if IsWorktree(s.Worktree) {
				w.Hint = api.HintText("commit the spec, then pin it", "git -C "+RelWorktree(s.Package)+" commit")
			}
			return row, nil, nil, w
		}
		return row, write(s.Head), nil, nil
	case api.LockStateInSync:
		if s.Changed() {
			return row, write(s.Lock.Commit), nil, nil
		}
		return row, nil, nil, nil
	case api.LockStateMissing:
		return row, nil, nil, warning(api.PWorktreeMissing, s.Message()+": the line is kept")
	case api.LockStateOrphaned:
		row.Entry = removedEntry(s)
		row.Removed = true
		return row, nil, nil, nil
	}
	return row, nil, nil, nil
}

// unpushedWarning gives the warning "PKG COMMIT is not on origin/BRANCH"
// with the push command, or nil when the remote branch holds the commit.
func unpushedWarning(ctx context.Context, e Env, ent api.LockEntry) *api.Problem {
	pk, ok := e.Set.Package(ent.Package)
	if !ok {
		return nil
	}
	repo := git.New(RepoOf(e.Paths, e.SetDir, *pk))
	remote := e.RemoteOf(*pk)
	ref := "refs/remotes/" + remote + "/" + ent.Branch
	if tip, _ := repo.RevParse(ctx, ref); tip != "" {
		if ok, _ := repo.IsAncestor(ctx, ent.Commit, tip); ok {
			return nil
		}
	}
	detached := false
	if b, err := git.New(WorktreePath(e.SetDir, ent.Package)).CurrentBranch(ctx); err == nil && b == "" {
		detached = true
	}
	p := api.Warning(api.PLockUnpushed, "%s %s is not on %s/%s", ent.Package, api.ShortCommit(ent.Commit), remote, ent.Branch)
	p.Package = ent.Package
	p.Hint = api.HintText("push the package branch before you publish", pushCommand(remote, ent.Package, ent.Branch, detached))
	return &p
}

// fetchAll fetches the remote of the set and the clone of each external
// package. A failed fetch is a warning.
func fetchAll(ctx context.Context, e Env, pkgs []config.Package) []api.Problem {
	var warns []api.Problem
	if err := e.FetchSet(ctx); err != nil {
		warns = append(warns, problemOf(err))
	}
	seen := map[string]bool{}
	for _, pk := range pkgs {
		if !pk.External() || seen[pk.URL()] {
			continue
		}
		seen[pk.URL()] = true
		if _, _, err := e.Clone(ctx, pk.URL(), true); err != nil {
			warns = append(warns, problemOf(err))
		}
	}
	return warns
}

// problemOf gives the warning of an error.
func problemOf(err error) api.Problem {
	if ae, ok := api.AsError(err); ok {
		p := ae.Problem
		p.ExitCode = 0
		return p
	}
	return api.Problem{Code: api.PGitFetchFailed, Message: err.Error()}
}
