package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// PackagesDir is the directory of the package worktrees in the set
// directory.
const PackagesDir = "packages"

// WorktreePath gives the worktree packages/PKG of a package.
func WorktreePath(setDir, pkg string) string {
	return filepath.Join(setDir, PackagesDir, pkg)
}

// RelWorktree gives packages/PKG, the path of the worktree in hints.
func RelWorktree(pkg string) string { return PackagesDir + "/" + pkg }

// RepoOf gives the git directory that holds the commits of a package: the
// bare clone for an external package, else the set repository.
func RepoOf(p config.Paths, setDir string, pk config.Package) string {
	if pk.External() {
		return ClonePath(p, pk.URL())
	}
	return setDir
}

// IsWorktree reports whether dir is a git worktree: it has a .git file or
// directory.
func IsWorktree(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// PackageState is the lock state of one package: the commit of
// repoctl.lock against its worktree (lifecycle.html#lock-states).
type PackageState struct {
	Package string
	State   api.LockState
	// Branch and URL are the branch and git.url of repoctl.yaml; for an
	// orphaned line, of the lock line.
	Branch string
	URL    string
	// Path is the in-tree path; "" for a package with a branch.
	Path string
	// Pinned reports a lock line; Lock is that line.
	Pinned bool
	Lock   lock.Entry
	// LockCommit is the build identity from the lock: the commit of the
	// lock line, or for an in-tree package the last set commit that
	// changes its path.
	LockCommit string
	// Worktree is packages/PKG (absolute); "" for in-tree and orphaned.
	Worktree string
	// Repo is the git directory with the commits: the set repository or
	// the bare clone.
	Repo string
	// Head is the HEAD commit of the worktree; "" without a worktree or
	// before the first commit of an orphan branch.
	Head string
	// HeadBranch is the branch of HEAD; "" for a detached HEAD.
	HeadBranch string
	// HeadSubject is the subject of HEAD when it is not the lock commit.
	HeadSubject string
	// Ahead and Behind count the commits that only HEAD and only the lock
	// commit have.
	Ahead, Behind int
	// Dirty reports uncommitted changes of tracked files; DirtyFiles names
	// them.
	Dirty      bool
	DirtyFiles []string
	// LockKnown reports that Repo has the lock commit. Without it, the
	// state is behind: checkout fetches the commit.
	LockKnown bool
}

// InSync reports a package that a build from the lock can take as is:
// in-sync or in-tree.
func (s PackageState) InSync() bool {
	return s.State == api.LockStateInSync || s.State == api.LockStateInTree
}

// Changed reports a lock line whose branch or URL differs from
// repoctl.yaml.
func (s PackageState) Changed() bool {
	return s.Pinned && s.State != api.LockStateOrphaned && (s.Lock.Branch != s.Branch || s.Lock.URL != s.URL)
}

// Entry gives the LockEntry of the contract for the state.
func (s PackageState) Entry() api.LockEntry {
	e := api.LockEntry{Package: s.Package, State: s.State}
	switch {
	case s.Path != "":
		e.Path, e.Commit = s.Path, s.LockCommit
	case s.State == api.LockStateOrphaned:
		e.Branch, e.URL, e.Commit = s.Lock.Branch, s.Lock.URL, s.Lock.Commit
	default:
		e.Branch, e.URL = s.Branch, s.URL
		if s.Pinned {
			e.Commit = s.Lock.Commit
		}
	}
	return e
}

// Describe gives the words of the state for a table cell, for example
// `ahead 1 ("Bump to 1.22.1")` or "dirty (ucx.spec)".
func (s PackageState) Describe() string {
	switch s.State {
	case api.LockStateAhead:
		return fmt.Sprintf("ahead %d%s", s.Ahead, quoteSubject(s.HeadSubject))
	case api.LockStateBehind:
		if s.Behind == 0 {
			return "behind (the lock commit is not fetched)"
		}
		return fmt.Sprintf("behind %d", s.Behind)
	case api.LockStateDiverged:
		return fmt.Sprintf("diverged (%d ahead, %d behind)", s.Ahead, s.Behind)
	case api.LockStateDirty:
		if len(s.DirtyFiles) > 0 {
			return "dirty (" + joinShort(s.DirtyFiles, 3) + ")"
		}
		return "dirty"
	case api.LockStateInTree:
		return "in tree"
	}
	return string(s.State)
}

func quoteSubject(subject string) string {
	if subject == "" {
		return ""
	}
	return fmt.Sprintf(" (%q)", subject)
}

func joinShort(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, ", ")
	}
	return strings.Join(list[:n], ", ") + fmt.Sprintf(" and %d more", len(list)-n)
}

// Hint gives the hint of the state: what to do, and the command
// (lifecycle.html#lock-states, the status hint column). An in-sync or
// in-tree package has none.
func (s PackageState) Hint() api.Hint {
	wt := RelWorktree(s.Package)
	switch s.State {
	case api.LockStateAhead:
		return api.Hint{Do: "record the new commit", Run: "repoctl pin " + s.Package}
	case api.LockStateBehind:
		return api.Hint{Do: "move the worktree to the lock commit", Run: "repoctl checkout " + s.Package}
	case api.LockStateDiverged:
		return api.Hint{Do: "rebase " + wt + " on the lock commit, then run repoctl pin " + s.Package,
			Run: "git -C " + wt + " rebase " + api.ShortCommit(s.Lock.Commit)}
	case api.LockStateDirty:
		if s.Path != "" {
			return api.Hint{Do: "commit the change under " + s.Path + " to the set branch", Run: "git commit " + s.Path}
		}
		return api.Hint{Do: "commit in " + wt + ", then run repoctl pin " + s.Package, Run: "git -C " + wt + " status"}
	case api.LockStateMissing:
		return api.Hint{Do: "create the worktree", Run: "repoctl checkout " + s.Package}
	case api.LockStateUnpinned:
		if s.Worktree == "" || !IsWorktree(s.Worktree) {
			return api.Hint{Do: "create the worktree, then pin it", Run: "repoctl checkout " + s.Package}
		}
		return api.Hint{Do: "pin the package", Run: "repoctl pin " + s.Package}
	case api.LockStateOrphaned:
		return api.Hint{Do: "remove the line of the package that repoctl.yaml no longer has", Run: "repoctl pin --orphans"}
	}
	return api.Hint{}
}

// Message gives the sentence of a state that is not in-sync, for an error
// or a warning, for example "ucx: worktree 4b1e9c2 is ahead of
// repoctl.lock (907c60d)".
func (s PackageState) Message() string {
	lockShort := api.ShortCommit(s.Lock.Commit)
	switch s.State {
	case api.LockStateAhead:
		return fmt.Sprintf("%s: worktree %s is ahead of repoctl.lock (%s)", s.Package, api.ShortCommit(s.Head), lockShort)
	case api.LockStateBehind:
		return fmt.Sprintf("%s: worktree %s is behind repoctl.lock (%s)", s.Package, api.ShortCommit(s.Head), lockShort)
	case api.LockStateDiverged:
		return fmt.Sprintf("%s: worktree %s and repoctl.lock (%s) have diverged", s.Package, api.ShortCommit(s.Head), lockShort)
	case api.LockStateDirty:
		where := RelWorktree(s.Package)
		if s.Path != "" {
			where = s.Path
		}
		if len(s.DirtyFiles) > 0 {
			return fmt.Sprintf("%s: %s has uncommitted changes (%s)", s.Package, where, joinShort(s.DirtyFiles, 3))
		}
		return fmt.Sprintf("%s: %s has uncommitted changes", s.Package, where)
	case api.LockStateMissing:
		return fmt.Sprintf("%s: %s is missing", s.Package, RelWorktree(s.Package))
	case api.LockStateUnpinned:
		return fmt.Sprintf("%s: repoctl.lock has no line for the package", s.Package)
	case api.LockStateOrphaned:
		return fmt.Sprintf("%s: repoctl.lock has a line for a package that repoctl.yaml does not have", s.Package)
	}
	return s.Package + ": " + string(s.State)
}

// States gives the lock state of each package of the set, in the order of
// repoctl.yaml, then each orphaned lock line in the order of the lock. lf
// nil reads repoctl.lock of setDir. status, list, build and the web UI
// call it.
func States(ctx context.Context, p config.Paths, set *config.Set, setDir string, lf *lock.File) ([]PackageState, error) {
	if lf == nil {
		var err error
		if lf, err = lock.Read(setDir); err != nil {
			return nil, err
		}
	}
	var out []PackageState
	for _, pk := range set.Packages {
		s, err := State(ctx, p, setDir, pk, lf)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	for _, id := range lf.Order {
		if _, ok := set.Package(id); ok {
			continue
		}
		e := lf.Packages[id]
		out = append(out, PackageState{Package: id, State: api.LockStateOrphaned, Branch: e.Branch, URL: e.URL,
			Pinned: true, Lock: e, LockCommit: e.Commit})
	}
	return out, nil
}

// StateMap gives the states by package.
func StateMap(states []PackageState) map[string]PackageState {
	m := make(map[string]PackageState, len(states))
	for _, s := range states {
		m[s.Package] = s
	}
	return m
}

// State gives the lock state of one package of the set.
func State(ctx context.Context, p config.Paths, setDir string, pk config.Package, lf *lock.File) (PackageState, error) {
	s := PackageState{Package: pk.ID, Branch: pk.Branch, URL: pk.URL(), Path: pk.Path}
	if pk.InTree() {
		return inTreeState(ctx, setDir, s)
	}
	s.Repo = RepoOf(p, setDir, pk)
	s.Worktree = WorktreePath(setDir, pk.ID)
	s.Lock, s.Pinned = lf.Get(pk.ID)
	s.LockCommit = s.Lock.Commit
	if !IsWorktree(s.Worktree) {
		if s.Pinned {
			s.State = api.LockStateMissing
		} else {
			s.State = api.LockStateUnpinned
		}
		return s, nil
	}
	wt := git.New(s.Worktree)
	head, err := wt.RevParse(ctx, "HEAD")
	if err != nil {
		return s, worktreeError(pk.ID, err)
	}
	s.Head = head
	if s.HeadBranch, err = wt.CurrentBranch(ctx); err != nil {
		return s, worktreeError(pk.ID, err)
	}
	files, err := wt.Changed(ctx)
	if err != nil {
		return s, worktreeError(pk.ID, err)
	}
	s.Dirty, s.DirtyFiles = len(files) > 0, files
	if s.Pinned {
		s.LockKnown = wt.HasCommit(ctx, s.Lock.Commit)
	}
	if s.Pinned && s.LockKnown && head != "" && head != s.Lock.Commit {
		if s.Behind, s.Ahead, err = wt.Count(ctx, s.Lock.Commit, head); err != nil {
			return s, worktreeError(pk.ID, err)
		}
	}
	if head != "" && head != s.Lock.Commit {
		s.HeadSubject, _ = wt.Subject(ctx, head)
	}
	switch {
	case s.Dirty:
		s.State = api.LockStateDirty
	case !s.Pinned:
		s.State = api.LockStateUnpinned
	case head == s.Lock.Commit:
		s.State = api.LockStateInSync
	case head == "":
		// An orphan branch before its first commit: nothing to compare.
		s.State = api.LockStateDiverged
	case !s.LockKnown:
		s.State = api.LockStateBehind
	case s.Ahead > 0 && s.Behind == 0:
		s.State = api.LockStateAhead
	case s.Behind > 0 && s.Ahead == 0:
		s.State = api.LockStateBehind
	default:
		s.State = api.LockStateDiverged
	}
	return s, nil
}

func inTreeState(ctx context.Context, setDir string, s PackageState) (PackageState, error) {
	s.Repo = setDir
	files, err := git.New(setDir).Changed(ctx, s.Path)
	if err != nil {
		return s, api.Errorf(api.PConfigInvalid, "%s: git status of %s: %v", s.Package, s.Path, err).WithPackage(s.Package).WithPath(s.Path)
	}
	s.Dirty, s.DirtyFiles = len(files) > 0, files
	c, err := InTreeCommit(ctx, setDir, s.Path)
	if err != nil {
		var ae *api.Error
		if !errors.As(err, &ae) {
			return s, err
		}
		// The path has no commit yet: its files are not committed.
		s.Dirty = true
	}
	s.LockCommit = c
	s.State = api.LockStateInTree
	if s.Dirty {
		s.State = api.LockStateDirty
	}
	return s, nil
}

func worktreeError(pkg string, err error) error {
	return api.Errorf(api.PConfigInvalid, "%s: read %s: %v", pkg, RelWorktree(pkg), err).WithPackage(pkg).WithPath(RelWorktree(pkg)).
		WithHint("repair the worktree", "git worktree repair "+RelWorktree(pkg))
}

// CheckClean checks that each package of pkgs can build from the lock, as
// build does before run.started (build.html#run-states): lock.dirty (exit 5)
// for a package that is dirty, ahead, behind or diverged, and
// lock.missing-package (exit 5) for a package without a lock line. A
// missing worktree is clean: the build takes the lock commit from the
// repository. pkgs nil checks every package of the set.
func CheckClean(ctx context.Context, p config.Paths, set *config.Set, setDir string, lf *lock.File, pkgs []string) error {
	states, err := States(ctx, p, set, setDir, lf)
	if err != nil {
		return err
	}
	return CleanError(states, pkgs)
}

// CleanError is CheckClean on states that the caller has, for example plan,
// which shows all rows before it stops.
func CleanError(states []PackageState, pkgs []string) error {
	want := map[string]bool{}
	for _, p := range pkgs {
		want[p] = true
	}
	var dirty, unpinned []PackageState
	for _, s := range states {
		if s.State == api.LockStateOrphaned || (pkgs != nil && !want[s.Package]) {
			continue
		}
		switch s.State {
		case api.LockStateInSync, api.LockStateInTree, api.LockStateMissing:
		case api.LockStateUnpinned:
			unpinned = append(unpinned, s)
		default:
			dirty = append(dirty, s)
		}
	}
	if len(dirty) > 0 {
		return stateError(api.PLockDirty, dirty)
	}
	if len(unpinned) > 0 {
		return stateError(api.PLockMissingPackage, unpinned)
	}
	return nil
}

// StateError gives the error of a package that is not in-sync: its
// message, its hint and, for a worktree, the step of the human.
func StateError(code api.ProblemCode, s PackageState) *api.Error {
	return stateError(code, []PackageState{s})
}

func stateError(code api.ProblemCode, list []PackageState) *api.Error {
	first := list[0]
	msg := first.Message()
	if len(list) > 1 {
		var rest []string
		for _, s := range list[1:] {
			rest = append(rest, s.Package+" "+string(s.State))
		}
		msg += " (also " + strings.Join(rest, ", ") + ")"
	}
	e := api.Errorf(code, "%s", msg).WithPackage(first.Package)
	h := first.Hint()
	e = e.WithHint(h.Do, h.Run)
	switch first.State {
	case api.LockStateAhead:
		e = e.WithHint("to try it without a pin (not publishable)", "repoctl build --from worktree")
		e = e.WithHuman("commit-lock", "The new commit must be pinned before a build from the lock.", "repoctl pin "+first.Package)
	case api.LockStateDirty:
		e = e.WithHuman("commit-worktree", "A build input must be a commit.", h.Run)
	case api.LockStateBehind, api.LockStateDiverged:
		e = e.WithHuman("resolve-dirty-worktree", "The worktree and the lock disagree.", h.Run)
	case api.LockStateUnpinned:
		e = e.WithHuman("commit-lock", "Each selected package needs a line in repoctl.lock.", "repoctl pin "+first.Package)
	}
	if first.Path != "" {
		e = e.WithPath(first.Path)
	} else if first.Worktree != "" {
		e = e.WithPath(RelWorktree(first.Package))
	}
	return e
}

// CheckLockCommitted gives lock.dirty when repoctl.lock has uncommitted
// changes (publish refuses a lock that is not committed).
func CheckLockCommitted(ctx context.Context, setDir string) error {
	out, err := git.New(setDir).Run(ctx, "status", "--porcelain", "--untracked-files=all", "--", lock.FileName)
	if err != nil {
		return api.Errorf(api.PConfigInvalid, "git status of %s: %v", lock.FileName, err)
	}
	if out == "" {
		return nil
	}
	return api.Errorf(api.PLockDirty, "repoctl.lock has changes that are not committed").WithPath(lock.FileName).
		WithHint("commit and push the lock", "git add repoctl.lock && git commit && git push").
		WithHuman("commit-lock", "Publish refuses a lock that is not committed.", "git add repoctl.lock && git commit && git push")
}
