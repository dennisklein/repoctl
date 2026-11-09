package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// MinReasonLength is the shortest reason of a rebuild.
const MinReasonLength = 8

// RebuildMessage gives the subject of a rebuild commit.
func RebuildMessage(reason string) string { return "Rebuild: " + reason }

// RebuildOptions are the arguments and flags of rebuild.
type RebuildOptions struct {
	// Packages are the selected packages, each with a branch.
	Packages []string
	Reason   string
	DryRun   bool
}

// RebuildRow is the result of rebuild for one package.
type RebuildRow struct {
	Package string
	Branch  string
	// Name is the Name of the spec.
	Name string
	// Previous is the lock commit before the rebuild; Commit the rebuild
	// commit ("" in a dry run).
	Previous, Commit string
	// OldVR and NewVR are VERSION-RELEASE without the dist tag.
	OldVR, NewVR string
	// Detached reports a commit on a detached HEAD.
	Detached bool
	// Remote is the remote of the push.
	Remote string
	// Entry is the lock line after the rebuild.
	Entry api.LockEntry
}

// OldNVR and NewNVR give NAME-VERSION-RELEASE without the dist tag.
func (r RebuildRow) OldNVR() string { return r.Name + "-" + r.OldVR }

// NewNVR gives the NVR of the rebuild commit, without the dist tag.
func (r RebuildRow) NewNVR() string { return r.Name + "-" + r.NewVR }

// PushCommand gives the push of the package branch.
func (r RebuildRow) PushCommand() string {
	return pushCommand(r.Remote, r.Package, r.Branch, r.Detached)
}

// RebuildReport is the result of rebuild.
type RebuildReport struct {
	Rows     []RebuildRow
	Warnings []api.Problem
}

// Rebuild adds an empty commit "Rebuild: REASON" to the local branch of
// each package and pins it (build.html#rebuild). It checks every package
// before the first commit: each worktree must be in-sync (lock.dirty, exit
// 5). When a commit fails, it resets each branch of this call to its lock
// commit and writes no lock. DryRun computes the new NVRs and commits
// nothing.
func Rebuild(ctx context.Context, e Env, o RebuildOptions) (*RebuildReport, error) {
	rep := &RebuildReport{}
	pkgs := Selected(e.Set, o.Packages)
	for _, pk := range pkgs {
		if pk.InTree() {
			return nil, InTreeError(pk)
		}
	}
	rep.Warnings = append(rep.Warnings, fetchAll(ctx, e, pkgs)...)
	lf, err := lock.Read(e.SetDir)
	if err != nil {
		return nil, err
	}
	var bad []PackageState
	states := map[string]PackageState{}
	for _, pk := range pkgs {
		s, err := State(ctx, e.Paths, e.SetDir, pk, lf)
		if err != nil {
			return nil, err
		}
		states[pk.ID] = s
		if s.State != api.LockStateInSync {
			bad = append(bad, s)
		}
	}
	if len(bad) > 0 {
		err := stateError(api.PLockDirty, bad)
		err.Message += "; no commit is made"
		return nil, err
	}
	macros := macroFiles(e.Set, e.SetDir)
	for _, pk := range pkgs {
		s := states[pk.ID]
		row := RebuildRow{Package: pk.ID, Branch: pk.Branch, Previous: s.Lock.Commit, Detached: s.HeadBranch == "", Remote: e.RemoteOf(pk)}
		if w := remoteAheadWarning(ctx, e, pk, s); w != nil {
			rep.Warnings = append(rep.Warnings, *w)
		}
		info, old, err := releaseOf(ctx, s, macros)
		if err != nil {
			return nil, api.Errorf(api.PConfigInvalid, "%s: read the spec: %v", pk.ID, err).WithPackage(pk.ID).WithCause(err)
		}
		row.Name = info.Name
		row.OldVR = info.Version + "-" + old
		row.NewVR = row.OldVR
		if info.AutoRelease {
			n, _ := strconv.Atoi(old)
			row.NewVR = info.Version + "-" + strconv.Itoa(n+1)
		} else {
			w := api.Warning(api.PNVRReused, "%s: the spec has no Release: %%autorelease, so the rebuild commit gives the same NVR %s", pk.ID, row.OldNVR())
			w.Package = pk.ID
			w.Hint = api.HintText("use Release: %autorelease in the spec, or raise the Release by hand", "")
			rep.Warnings = append(rep.Warnings, w)
		}
		row.Entry = api.LockEntry{Package: pk.ID, Branch: pk.Branch, URL: pk.URL(), Commit: s.Lock.Commit, State: api.LockStateInSync}
		rep.Rows = append(rep.Rows, row)
	}
	if o.DryRun {
		return rep, nil
	}
	// All or nothing: one commit per package, then one write of the lock.
	for i := range rep.Rows {
		r := &rep.Rows[i]
		wt := git.New(WorktreePath(e.SetDir, r.Package))
		_, err := wt.Run(ctx, "commit", "--quiet", "--allow-empty", "-m", RebuildMessage(o.Reason))
		var head string
		if err == nil {
			head, err = wt.RevParse(ctx, "HEAD")
		}
		if err != nil {
			rollback(ctx, e, rep.Rows[:i+1])
			return nil, fmt.Errorf("%s: git commit in %s: %w; no package got a rebuild commit", r.Package, RelWorktree(r.Package), err)
		}
		r.Commit = head
		r.Entry.Commit, r.Entry.Previous, r.Entry.Changed = head, r.Previous, true
	}
	if _, err := lock.Update(ctx, e.SetDir, packageOrder(e.Set), func(f *lock.File) error {
		for _, r := range rep.Rows {
			f.Set(r.Package, lock.Entry{Branch: r.Branch, Commit: r.Commit, URL: r.Entry.URL})
		}
		return nil
	}); err != nil {
		rollback(ctx, e, rep.Rows)
		return nil, err
	}
	_ = Exclude(ctx, e.SetDir, excludeLockFile())
	return rep, nil
}

// InTreeError is rebuild.in-tree for a named in-tree package.
func InTreeError(pk config.Package) *api.Error {
	return api.Errorf(api.PRebuildInTree, "%s is in-tree (%s): an empty commit does not change its commit", pk.ID, pk.Path).
		WithPackage(pk.ID).WithPath(pk.Path).
		WithHint("raise the Release in its spec, add a %changelog entry and commit it to the set branch", "")
}

// InTreeWarning is rebuild.in-tree for an in-tree package that a glob
// matched: rebuild leaves it out.
func InTreeWarning(pk config.Package) api.Problem {
	p := api.Warning(api.PRebuildInTree, "%s is in-tree (%s): left out of the rebuild", pk.ID, pk.Path)
	p.Package, p.Path = pk.ID, pk.Path
	p.Hint = api.HintText("raise the Release in its spec, add a %changelog entry and commit it to the set branch", "")
	return p
}

// rollback resets each branch of this call to its lock commit.
func rollback(ctx context.Context, e Env, rows []RebuildRow) {
	for _, r := range rows {
		wt := git.New(WorktreePath(e.SetDir, r.Package))
		if head, _ := wt.RevParse(ctx, "HEAD"); head != "" && head != r.Previous {
			_, _ = wt.Run(ctx, "reset", "--quiet", "--keep", r.Previous)
		}
	}
}

// macroFiles gives the macro files of the first active target, as paths
// in the set directory.
func macroFiles(set *config.Set, setDir string) []string {
	var out []string
	if ts := set.ActiveTargets(); len(ts) > 0 {
		for _, m := range ts[0].Macros {
			out = append(out, filepath.Join(setDir, filepath.FromSlash(m)))
		}
	}
	return out
}

// releaseOf gives the spec facts and the release of the HEAD of the
// worktree: the %autorelease count, or the Release of the spec.
func releaseOf(ctx context.Context, s PackageState, macros []string) (SpecInfo, string, error) {
	spec, err := SpecFile(s.Worktree, s.Package)
	if err != nil {
		return SpecInfo{}, "", err
	}
	info, err := ReadSpec(ctx, spec, macros)
	if err != nil {
		return info, "", err
	}
	if !info.AutoRelease {
		return info, info.Release, nil
	}
	rel, _ := filepath.Rel(s.Worktree, spec)
	n, err := AutoReleaseAt(ctx, s.Worktree, "HEAD", filepath.ToSlash(rel))
	if err != nil {
		return info, "", err
	}
	return info, strconv.Itoa(n), nil
}

// remoteAheadWarning warns when the remote branch has commits that the
// lock commit does not have: the push of the rebuild commit then needs a
// rebase.
func remoteAheadWarning(ctx context.Context, e Env, pk config.Package, s PackageState) *api.Problem {
	repo := git.New(RepoOf(e.Paths, e.SetDir, pk))
	remote := e.RemoteOf(pk)
	ref := "refs/remotes/" + remote + "/" + pk.Branch
	tip, _ := repo.RevParse(ctx, ref)
	if tip == "" || tip == s.Lock.Commit {
		return nil
	}
	n, err := repo.Lines(ctx, "rev-list", s.Lock.Commit+".."+tip)
	if err != nil || len(n) == 0 {
		return nil
	}
	p := api.Warning(api.PLockBehind, "%s: %s/%s has %d commits that repoctl.lock (%s) does not have", pk.ID, remote, pk.Branch, len(n), api.ShortCommit(s.Lock.Commit))
	p.Package = pk.ID
	p.Hint = api.HintText("read them before the push of the rebuild commit", "git -C "+RelWorktree(pk.ID)+" log --oneline HEAD.."+remote+"/"+pk.Branch)
	return &p
}
