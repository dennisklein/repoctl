package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// The results of checkout for one package (cli.html#cmd-checkout).
const (
	// ActCreated is a new worktree.
	ActCreated = "created"
	// ActMoved is a branch that checkout fast-forwarded to the lock commit.
	ActMoved = "moved"
	// ActInSync is a worktree at the lock commit: nothing moved.
	ActInSync = "in-sync"
	// ActInTree is an in-tree package: it has no worktree.
	ActInTree = "in-tree"
	// ActKept is a worktree that checkout kept as it is: ahead, diverged,
	// dirty, or on another branch.
	ActKept = "kept"
	// ActReset is a worktree that --force reset to the lock commit.
	ActReset = "reset"
	// ActFailed is a package that failed; Row.Err says why.
	ActFailed = "failed"
	// ActBranch is a branch that checkout --create made.
	ActBranch = "branch"
	// ActExists is a package of checkout --create whose branch exists.
	ActExists = "exists"
)

// CheckoutOptions are the flags of checkout.
type CheckoutOptions struct {
	// Packages are the selected package ids; nil selects all.
	Packages []string
	Fetch    bool
	Force    bool
}

// Row is the result of checkout for one package.
type Row struct {
	Package string
	Action  string
	// Note is a remark of the row, for example "detached: BRANCH is
	// checked out in PATH" or "not pinned: run repoctl pin PKG".
	Note string
	// State is the lock state after the step.
	State PackageState
	// Warning is the warning of a kept worktree.
	Warning *api.Problem
	// Err is the problem of a failed package.
	Err *api.Error
	// Start is the start point of a branch of checkout --create, for
	// example "forked from munge/slurm-26.05 at 3a244fd" or "orphan".
	Start string
	// Orphan reports a branch of checkout --create --orphan.
	Orphan bool
}

// Report is the result of checkout.
type Report struct {
	Rows []Row
	// FetchedSet reports a fetch of defaults.git.remote; Clones are the
	// packages whose bare clone checkout made or fetched.
	FetchedSet bool
	Clones     []string
	// FetchTime and WorktreeTime are the times of the two steps.
	FetchTime, WorktreeTime time.Duration
}

// Failed gives the rows that failed.
func (r *Report) Failed() []Row {
	var out []Row
	for _, x := range r.Rows {
		if x.Action == ActFailed {
			out = append(out, x)
		}
	}
	return out
}

// Selected gives the packages of the ids in the order of repoctl.yaml; nil
// gives all packages.
func Selected(set *config.Set, ids []string) []config.Package {
	if ids == nil {
		return append([]config.Package(nil), set.Packages...)
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []config.Package
	for _, p := range set.Packages {
		if want[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// checkout is the state of one checkout call.
type checkout struct {
	e  Env
	lf *lock.File
	// fetchedSet and fetchedClone record the fetches of this call, so that
	// a missing commit fetches each repository once.
	fetchedSet   bool
	fetchSetErr  error
	fetchedClone map[string]bool
	cloneErr     map[string]error
	report       *Report
	mu           sync.Mutex
}

func newCheckout(e Env, lf *lock.File) *checkout {
	return &checkout{e: e, lf: lf, fetchedClone: map[string]bool{}, cloneErr: map[string]error{}, report: &Report{}}
}

// Checkout creates or updates the worktrees packages/PKG from the lock
// (cli.html#cmd-checkout). It never moves a branch back and never discards
// a commit or a change, except with Force. A package that fails gets a row
// with its problem; the other packages go on.
func Checkout(ctx context.Context, e Env, lf *lock.File, o CheckoutOptions) (*Report, error) {
	c := newCheckout(e, lf)
	pkgs := Selected(e.Set, o.Packages)
	// The two steps of the progress display (terminal.html#checkout).
	start := time.Now()
	fctx, step := progress.Start(ctx, progress.KindStep, "fetch")
	c.fetch(fctx, pkgs, o.Fetch)
	step.End(nil, progress.Message(c.fetchMessage(pkgs)))
	c.report.FetchTime = time.Since(start)
	start = time.Now()
	wctx, step := progress.Start(ctx, progress.KindStep, "worktrees", progress.Work(progress.UnitItems, int64(len(pkgs))))
	c.prune(wctx, pkgs)
	for _, pk := range pkgs {
		c.report.Rows = append(c.report.Rows, c.one(wctx, pk, o.Force))
		step.Advance(1)
	}
	created := 0
	for _, r := range c.report.Rows {
		if r.Action == ActCreated {
			created++
		}
	}
	var err error
	if f := c.report.Failed(); len(f) > 0 {
		err = fmt.Errorf("%d failed", len(f))
	}
	step.End(err, progress.Message(fmt.Sprintf("%d created in %s/", created, PackagesDir)))
	c.report.WorktreeTime = time.Since(start)
	return c.report, nil
}

// fetchMessage gives the message of the fetch step: "origin and 1 clone
// (rocm-smi, git.gsi.de)".
func (c *checkout) fetchMessage(pkgs []config.Package) string {
	var parts []string
	if c.report.FetchedSet {
		parts = append(parts, c.e.Set.Remote())
	}
	if n := len(c.report.Clones); n > 0 {
		var names []string
		for _, id := range c.report.Clones {
			for _, pk := range pkgs {
				if pk.ID == id {
					if h := CloneHost(pk.URL()); h != "" {
						id += ", " + h
					}
				}
			}
			names = append(names, id)
		}
		word := "clones"
		if n == 1 {
			word = "clone"
		}
		parts = append(parts, fmt.Sprintf("%d %s (%s)", n, word, strings.Join(names, "; ")))
	}
	if len(parts) == 0 {
		return "nothing to fetch"
	}
	return strings.Join(parts, " and ")
}

// fetch is the first step: the bare clone of each external package, the
// remote of the set with --fetch, and each repository that lacks a commit
// of the lock.
func (c *checkout) fetch(ctx context.Context, pkgs []config.Package, all bool) {
	if all {
		c.fetchSet(ctx)
	}
	urls := map[string][]string{}
	var order []string
	for _, pk := range pkgs {
		if !pk.External() {
			continue
		}
		if _, ok := urls[pk.URL()]; !ok {
			order = append(order, pk.URL())
		}
		urls[pk.URL()] = append(urls[pk.URL()], pk.ID)
	}
	jobs := max(c.e.Jobs, 1)
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for _, u := range order {
		wg.Add(1)
		sem <- struct{}{}
		go func(u string) {
			defer wg.Done()
			defer func() { <-sem }()
			_, fetched, err := c.e.Clone(ctx, u, all)
			c.mu.Lock()
			defer c.mu.Unlock()
			if err != nil {
				c.cloneErr[u] = err
				return
			}
			if fetched {
				c.fetchedClone[u] = true
				c.report.Clones = append(c.report.Clones, urls[u]...)
			}
		}(u)
	}
	wg.Wait()
	// A commit of the lock, or the remote branch of an unpinned package,
	// that the repository does not have yet.
	for _, pk := range pkgs {
		if pk.InTree() {
			continue
		}
		repo := git.New(RepoOf(c.e.Paths, c.e.SetDir, pk))
		e, pinned := c.lf.Get(pk.ID)
		missing := false
		if pinned {
			missing = !repo.HasCommit(ctx, e.Commit)
		} else {
			tip, _ := repo.RevParse(ctx, "refs/remotes/"+c.e.RemoteOf(pk)+"/"+pk.Branch)
			missing = tip == ""
		}
		if !missing {
			continue
		}
		if pk.External() {
			c.fetchClone(ctx, pk.URL())
		} else {
			c.fetchSet(ctx)
		}
	}
}

func (c *checkout) fetchSet(ctx context.Context) {
	if c.fetchedSet {
		return
	}
	c.fetchedSet = true
	c.fetchSetErr = c.e.FetchSet(ctx)
	c.report.FetchedSet = c.fetchSetErr == nil
}

func (c *checkout) fetchClone(ctx context.Context, u string) {
	if c.fetchedClone[u] || c.cloneErr[u] != nil {
		return
	}
	_, _, err := c.e.Clone(ctx, u, true)
	if err != nil {
		c.cloneErr[u] = err
		return
	}
	c.fetchedClone[u] = true
}

// prune removes the records of deleted worktrees, so that git worktree add
// can make them again.
func (c *checkout) prune(ctx context.Context, pkgs []config.Package) {
	_, _ = git.New(c.e.SetDir).Run(ctx, "worktree", "prune")
	seen := map[string]bool{}
	for _, pk := range pkgs {
		if !pk.External() || seen[pk.URL()] || c.cloneErr[pk.URL()] != nil {
			continue
		}
		seen[pk.URL()] = true
		_ = c.e.WithCloneLock(ctx, pk.URL(), func() error {
			_, err := git.New(ClonePath(c.e.Paths, pk.URL())).Run(ctx, "worktree", "prune")
			return err
		})
	}
}

// one checks out one package.
func (c *checkout) one(ctx context.Context, pk config.Package, force bool) Row {
	if pk.InTree() {
		st, err := State(ctx, c.e.Paths, c.e.SetDir, pk, c.lf)
		if err != nil {
			return failRow(pk.ID, err)
		}
		return Row{Package: pk.ID, Action: ActInTree, State: st}
	}
	if pk.External() {
		if err := c.cloneErr[pk.URL()]; err != nil {
			return failRow(pk.ID, err)
		}
		var row Row
		err := c.e.WithCloneLock(ctx, pk.URL(), func() error {
			row = c.worktree(ctx, pk, force)
			return nil
		})
		if err != nil {
			return failRow(pk.ID, err)
		}
		return row
	}
	return c.worktree(ctx, pk, force)
}

func failRow(pkg string, err error) Row {
	ae, ok := api.AsError(err)
	if !ok {
		ae = api.Errorf(api.PInternal, "%s: %v", pkg, err).WithCause(err)
	}
	if ae.Package == "" {
		ae = ae.WithPackage(pkg)
	}
	return Row{Package: pkg, Action: ActFailed, Err: ae}
}

// target gives the commit that the worktree of pk goes to: the lock
// commit, or for an unpinned package the tip of the remote branch (else of
// the local branch).
func (c *checkout) target(ctx context.Context, pk config.Package) (string, string, error) {
	repo := git.New(RepoOf(c.e.Paths, c.e.SetDir, pk))
	remote := c.e.RemoteOf(pk)
	e, pinned := c.lf.Get(pk.ID)
	if pinned {
		if repo.HasCommit(ctx, e.Commit) {
			return e.Commit, "", nil
		}
		err := api.Errorf(api.PGitFetchFailed, "%s: commit %s of repoctl.lock is not on %s", pk.ID, api.ShortCommit(e.Commit), remote).
			WithPackage(pk.ID).WithHint("push the commit from the host that pinned it, then run repoctl checkout "+pk.ID, "git push "+remote+" "+pk.Branch)
		if c.fetchSetErr != nil && !pk.External() {
			return "", "", c.fetchSetErr
		}
		return "", "", err
	}
	note := "not pinned: run repoctl pin " + pk.ID
	tip, _ := repo.RevParse(ctx, "refs/remotes/"+remote+"/"+pk.Branch)
	if tip == "" {
		tip, _ = repo.Branch(ctx, pk.Branch)
	}
	if tip == "" {
		return "", "", api.Errorf(api.PBranchStartMissing, "%s: the branch %s is neither on %s nor local", pk.ID, pk.Branch, remote).
			WithPackage(pk.ID).WithHint("make the branch from a start point", "repoctl checkout --create "+pk.ID+" --from fedora")
	}
	return tip, note, nil
}

// otherWorktree gives the worktree, other than wt, that has branch checked
// out, or "".
func otherWorktree(ctx context.Context, repo *git.Git, branch, wt string) string {
	list, err := repo.Worktrees(ctx)
	if err != nil {
		return ""
	}
	for _, w := range list {
		if w.Branch == branch && !samePath(w.Path, wt) {
			return w.Path
		}
	}
	return ""
}

func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, err1 := evalPath(a)
	rb, err2 := evalPath(b)
	return err1 == nil && err2 == nil && ra == rb
}

// worktree makes or moves the worktree of a package with a branch. The
// caller holds the clone lock of an external package.
func (c *checkout) worktree(ctx context.Context, pk config.Package, force bool) Row {
	wt := WorktreePath(c.e.SetDir, pk.ID)
	repo := git.New(RepoOf(c.e.Paths, c.e.SetDir, pk))
	target, note, err := c.target(ctx, pk)
	if err != nil {
		return failRow(pk.ID, err)
	}
	row := Row{Package: pk.ID, Note: note}
	other := otherWorktree(ctx, repo, pk.Branch, wt)
	if !IsWorktree(wt) {
		if err := c.add(ctx, pk, repo, wt, target, other, force); err != nil {
			return failRow(pk.ID, err)
		}
		row.Action = ActCreated
		if other != "" {
			row.Note = joinNote(row.Note, "detached: "+pk.Branch+" is checked out in "+other)
		}
	} else {
		act, n, err := c.move(ctx, pk, wt, target, other, force)
		if err != nil {
			return failRow(pk.ID, err)
		}
		row.Action, row.Note = act, joinNote(row.Note, n)
	}
	st, err := State(ctx, c.e.Paths, c.e.SetDir, pk, c.lf)
	if err != nil {
		return failRow(pk.ID, err)
	}
	row.State = st
	if w := keptWarning(c.e, pk, st); w != nil && row.Action != ActReset {
		row.Warning = w
		if row.Action == ActInSync || row.Action == ActMoved {
			row.Action = ActKept
		}
	}
	return row
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// add makes the worktree of a package that has none (cli.html#cmd-checkout,
// the local branch table).
func (c *checkout) add(ctx context.Context, pk config.Package, repo *git.Git, wt, target, other string, force bool) error {
	if fi, err := os.Stat(wt); err == nil {
		if !fi.IsDir() || !emptyDir(wt) {
			return api.Errorf(api.PWorktreeMissing, "%s: %s exists and is not a git worktree", pk.ID, RelWorktree(pk.ID)).
				WithExit(api.ExitFailed).WithPackage(pk.ID).WithPath(RelWorktree(pk.ID)).
				WithHint("move the directory away, then run repoctl checkout "+pk.ID, "mv "+RelWorktree(pk.ID)+" "+RelWorktree(pk.ID)+".old")
		}
	}
	gitErr := func(err error) error {
		return api.Errorf(api.PWorktreeMissing, "%s: make %s: %v", pk.ID, RelWorktree(pk.ID), err).WithExit(api.ExitFailed).
			WithPackage(pk.ID).WithCause(err)
	}
	if other != "" {
		if _, err := repo.Run(ctx, "worktree", "add", "--quiet", "--detach", wt, target); err != nil {
			return gitErr(err)
		}
		return Exclude(ctx, c.e.SetDir, excludeWorktree(pk.ID))
	}
	local, err := repo.Branch(ctx, pk.Branch)
	if err != nil {
		return gitErr(err)
	}
	switch {
	case local == "":
		if _, err := repo.Run(ctx, "branch", "--no-track", pk.Branch, target); err != nil {
			return gitErr(err)
		}
		c.setUpstream(ctx, repo, pk)
	case local == target:
	case force:
		if _, err := repo.Run(ctx, "branch", "--force", pk.Branch, target); err != nil {
			return gitErr(err)
		}
	default:
		if ok, _ := repo.IsAncestor(ctx, local, target); ok {
			if _, err := repo.Run(ctx, "branch", "--force", pk.Branch, target); err != nil {
				return gitErr(err)
			}
		}
		// A local branch ahead of the lock or diverged keeps its commits.
	}
	if _, err := repo.Run(ctx, "worktree", "add", "--quiet", wt, pk.Branch); err != nil {
		return gitErr(err)
	}
	return Exclude(ctx, c.e.SetDir, excludeWorktree(pk.ID))
}

// setUpstream sets REMOTE/BRANCH as the upstream of a new local branch, so
// that git -C packages/PKG push origin BRANCH and git status work as in a
// normal clone.
func (c *checkout) setUpstream(ctx context.Context, repo *git.Git, pk config.Package) {
	remote := c.e.RemoteOf(pk)
	_ = repo.ConfigSet(ctx, "branch."+pk.Branch+".remote", remote)
	_ = repo.ConfigSet(ctx, "branch."+pk.Branch+".merge", "refs/heads/"+pk.Branch)
}

// move updates an existing worktree. It gives the action and a note.
func (c *checkout) move(ctx context.Context, pk config.Package, wt, target, other string, force bool) (string, string, error) {
	w := git.New(wt)
	head, err := w.RevParse(ctx, "HEAD")
	if err != nil {
		return "", "", worktreeError(pk.ID, err)
	}
	branch, err := w.CurrentBranch(ctx)
	if err != nil {
		return "", "", worktreeError(pk.ID, err)
	}
	dirty, err := w.Dirty(ctx)
	if err != nil {
		return "", "", worktreeError(pk.ID, err)
	}
	gitErr := func(err error) error {
		return api.Errorf(api.PWorktreeDirty, "%s: move %s to %s: %v", pk.ID, RelWorktree(pk.ID), api.ShortCommit(target), err).
			WithExit(api.ExitFailed).WithPackage(pk.ID).WithCause(err)
	}
	if force {
		if head == target && !dirty && (branch == pk.Branch || branch == "") {
			return ActInSync, "", nil
		}
		switch {
		case branch == pk.Branch:
			_, err = w.Run(ctx, "reset", "--quiet", "--hard", target)
		case other != "":
			_, err = w.Run(ctx, "checkout", "--quiet", "--force", "--detach", target)
		default:
			_, err = w.Run(ctx, "checkout", "--quiet", "--force", "-B", pk.Branch, target)
		}
		if err != nil {
			return "", "", gitErr(err)
		}
		return ActReset, "", nil
	}
	if dirty {
		return ActKept, "", nil
	}
	if branch != pk.Branch && branch != "" {
		if head == target {
			return ActInSync, "on branch " + branch, nil
		}
		return ActKept, "on branch " + branch, nil
	}
	if head == target {
		if branch == "" && other != "" {
			return ActInSync, "detached: " + pk.Branch + " is checked out in " + other, nil
		}
		return ActInSync, "", nil
	}
	forward := false
	if head != "" {
		forward, _ = w.IsAncestor(ctx, head, target)
	}
	if !forward {
		return ActKept, "", nil
	}
	if branch == "" {
		// A detached HEAD moves only when the branch is checked out in
		// another worktree: checkout made it so. A HEAD that the human
		// detached stays.
		if other == "" {
			return ActKept, "detached HEAD", nil
		}
		if _, err := w.Run(ctx, "checkout", "--quiet", "--detach", target); err != nil {
			return "", "", gitErr(err)
		}
		return ActMoved, "detached: " + pk.Branch + " is checked out in " + other, nil
	}
	if _, err := w.Run(ctx, "merge", "--quiet", "--ff-only", target); err != nil {
		return "", "", gitErr(err)
	}
	return ActMoved, "", nil
}

// keptWarning gives the warning worktree.dirty of a worktree that is not
// in-sync after checkout, with its hint (cli.html#cmd-checkout).
func keptWarning(e Env, pk config.Package, st PackageState) *api.Problem {
	wt := RelWorktree(pk.ID)
	var msg string
	switch st.State {
	case api.LockStateInSync, api.LockStateInTree, api.LockStateUnpinned:
		return nil
	case api.LockStateAhead:
		msg = fmt.Sprintf("%s is ahead of repoctl.lock: kept", wt)
	case api.LockStateBehind:
		msg = fmt.Sprintf("%s is behind repoctl.lock: kept", wt)
	case api.LockStateDiverged:
		msg = fmt.Sprintf("%s has diverged from repoctl.lock: kept", wt)
	case api.LockStateDirty:
		msg = fmt.Sprintf("%s has uncommitted changes: kept", wt)
	default:
		return nil
	}
	if st.HeadBranch != "" && st.HeadBranch != pk.Branch {
		msg += " (on branch " + st.HeadBranch + ")"
	}
	h := st.Hint()
	if st.State == api.LockStateDirty {
		h = api.Hint{Do: "commit in " + wt + ", then pin it", Run: "repoctl pin " + pk.ID}
	}
	if st.State == api.LockStateAhead && st.HeadBranch == "" {
		h = api.Hint{Do: "push the commit to its branch, then pin it", Run: pushCommand(e.RemoteOf(pk), pk.ID, pk.Branch, true)}
	}
	p := api.Warning(api.PWorktreeDirty, "%s", msg)
	p.Package = pk.ID
	p.Path = wt
	p.Hint = h.String()
	return &p
}

func emptyDir(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) == 0
}

func evalPath(p string) (string, error) { return filepath.EvalSymlinks(p) }
