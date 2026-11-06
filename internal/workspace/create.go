package workspace

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// FedoraURL is the prefix of the Fedora dist-git repositories.
const FedoraURL = "https://src.fedoraproject.org/rpms/"

// EnvTestFedoraURL replaces FedoraURL in the tests. It is not part of the
// documented environment.
const EnvTestFedoraURL = "REPOCTL_TEST_FEDORA_URL"

// CreateOptions are the flags of checkout --create: exactly one start
// point (lifecycle.html#checkout-create).
type CreateOptions struct {
	// Packages are the selected package ids; nil selects each package with
	// a branch.
	Packages []string
	// FromSet is --from-set SET.
	FromSet string
	// From is --from REF; FromFedora reports --from fedora[:BRANCH], with
	// the branch in FedoraBranch.
	From         string
	FromFedora   bool
	FedoraBranch string
	Orphan       bool
	// FedoraURL is the prefix of the dist-git URLs; "" is FedoraURL.
	FedoraURL string
}

// ParseFrom reads the value of --from: fedora or fedora:BRANCH (default
// rawhide), else a git revision.
func ParseFrom(v string) (ref string, fedora bool, branch string) {
	if v == "fedora" {
		return "", true, "rawhide"
	}
	if b, ok := strings.CutPrefix(v, "fedora:"); ok && b != "" {
		return "", true, b
	}
	return v, false, ""
}

// start is the start point of one new branch.
type start struct {
	commit string
	label  string
	tmpRef string
}

// Create makes the branch and the worktree of each selected package whose
// branch does not exist (checkout --create). Before it makes a branch, it
// asks the remote of the package: a branch on the remote gives
// branch.exists-remote, an external server that does not answer gives
// git.unreachable. It never pushes.
func Create(ctx context.Context, e Env, lf *lock.File, o CreateOptions) (*Report, error) {
	c := newCheckout(e, lf)
	var src *lock.File
	if o.FromSet != "" {
		ss, err := OpenSetSource(ctx, e.SetDir, e.Set.Remote(), o.FromSet)
		if err != nil {
			return nil, err
		}
		if src, err = ss.Lock(ctx); err != nil {
			return nil, err
		}
	}
	t0 := time.Now()
	pkgs := Selected(e.Set, o.Packages)
	c.prune(ctx, pkgs)
	for _, pk := range pkgs {
		if pk.InTree() {
			if o.Packages != nil {
				c.report.Rows = append(c.report.Rows, Row{Package: pk.ID, Action: ActInTree, Note: "in tree: no branch"})
			}
			continue
		}
		c.report.Rows = append(c.report.Rows, c.create(ctx, pk, src, o))
	}
	c.report.WorktreeTime = time.Since(t0)
	return c.report, nil
}

func (c *checkout) create(ctx context.Context, pk config.Package, src *lock.File, o CreateOptions) Row {
	remote := c.e.RemoteOf(pk)
	// Ask the remote of the package first.
	where, asker := remote, git.New(c.e.SetDir)
	if pk.External() {
		where = pk.URL()
	}
	onRemote, err := asker.LsRemote(ctx, where, pk.Branch)
	if err != nil {
		if pk.External() {
			return failRow(pk.ID, api.Errorf(api.PGitUnreachable, "%s: git ls-remote %s failed: %v", pk.ID, pk.URL(), err).
				WithPackage(pk.ID).WithCause(err).WithHint("create the project on the git server first", ""))
		}
		return failRow(pk.ID, api.Errorf(api.PGitFetchFailed, "%s: git ls-remote %s failed: %v", pk.ID, remote, err).
			WithPackage(pk.ID).WithCause(err).WithHint("examine the remote and the credentials", "git ls-remote "+remote))
	}
	if onRemote != "" {
		return failRow(pk.ID, api.Errorf(api.PBranchExistsRemote, "%s: the branch %s exists on %s", pk.ID, pk.Branch, where).
			WithPackage(pk.ID).WithHint("check out the branch", "repoctl checkout "+pk.ID))
	}
	if pk.External() {
		if _, _, err := c.e.Clone(ctx, pk.URL(), false); err != nil {
			return failRow(pk.ID, err)
		}
		var row Row
		err := c.e.WithCloneLock(ctx, pk.URL(), func() error {
			row = c.createIn(ctx, pk, src, o)
			return nil
		})
		if err != nil {
			return failRow(pk.ID, err)
		}
		return row
	}
	return c.createIn(ctx, pk, src, o)
}

// createIn makes the branch in the repository of the package and attaches
// a new worktree to it. The caller holds the clone lock of an external
// package.
func (c *checkout) createIn(ctx context.Context, pk config.Package, src *lock.File, o CreateOptions) Row {
	repo := git.New(RepoOf(c.e.Paths, c.e.SetDir, pk))
	wt := WorktreePath(c.e.SetDir, pk.ID)
	local, err := repo.Branch(ctx, pk.Branch)
	if err != nil {
		return failRow(pk.ID, err)
	}
	if local != "" {
		// The branch exists here but not on the remote: attach it.
		row := c.worktree(ctx, pk, false)
		if row.Action != ActFailed {
			row.Action, row.Note = ActExists, joinNote("the local branch exists", row.Note)
		}
		return row
	}
	// A worktree of the same repository on another branch, as in a set
	// directory that init --from forked in place, moves to the new branch
	// when it is clean.
	reuse := false
	switch {
	case IsWorktree(wt):
		if err := reusable(ctx, pk, repo, wt); err != nil {
			return failRow(pk.ID, err)
		}
		reuse = true
	case dirExists(wt) && !emptyDir(wt):
		return failRow(pk.ID, api.Errorf(api.PWorktreeMissing, "%s: %s exists and is not a worktree", pk.ID, RelWorktree(pk.ID)).WithExit(api.ExitFailed).
			WithPackage(pk.ID).WithPath(RelWorktree(pk.ID)).WithHint("move the directory away, then make the branch", ""))
	}
	gitErr := func(err error) Row {
		return failRow(pk.ID, api.Errorf(api.PWorktreeMissing, "%s: make the branch %s: %v", pk.ID, pk.Branch, err).WithExit(api.ExitFailed).
			WithPackage(pk.ID).WithCause(err))
	}
	row := Row{Package: pk.ID, Action: ActBranch}
	if o.Orphan {
		if reuse {
			_, err = git.New(wt).Run(ctx, "switch", "--quiet", "--orphan", pk.Branch)
		} else {
			_, err = repo.Run(ctx, "worktree", "add", "--quiet", "--orphan", "-b", pk.Branch, wt)
		}
		if err != nil {
			return gitErr(err)
		}
		row.Start, row.Orphan = "orphan", true
	} else {
		st, err := c.startPoint(ctx, pk, repo, src, o)
		if err != nil {
			return failRow(pk.ID, err)
		}
		_, err = repo.Run(ctx, "branch", "--no-track", pk.Branch, st.commit)
		if st.tmpRef != "" {
			_, _ = repo.Run(ctx, "update-ref", "-d", st.tmpRef)
		}
		if err != nil {
			return gitErr(err)
		}
		if reuse {
			_, err = git.New(wt).Run(ctx, "switch", "--quiet", pk.Branch)
		} else {
			_, err = repo.Run(ctx, "worktree", "add", "--quiet", wt, pk.Branch)
		}
		if err != nil {
			return gitErr(err)
		}
		row.Start = st.label
	}
	c.setUpstream(ctx, repo, pk)
	if err := Exclude(ctx, c.e.SetDir, excludeWorktree(pk.ID)); err != nil {
		return failRow(pk.ID, err)
	}
	s, err := State(ctx, c.e.Paths, c.e.SetDir, pk, c.lf)
	if err != nil {
		return failRow(pk.ID, err)
	}
	row.State = s
	return row
}

// startPoint gives the commit that a new branch starts at.
func (c *checkout) startPoint(ctx context.Context, pk config.Package, repo *git.Git, src *lock.File, o CreateOptions) (start, error) {
	remote := c.e.RemoteOf(pk)
	have := func(commit string) bool {
		if repo.HasCommit(ctx, commit) {
			return true
		}
		if pk.External() {
			if err := FetchClone(ctx, ClonePath(c.e.Paths, pk.URL()), pk.URL()); err != nil {
				return false
			}
		} else {
			c.fetchSet(ctx)
		}
		return repo.HasCommit(ctx, commit)
	}
	switch {
	case src != nil:
		e, ok := src.Get(pk.ID)
		if !ok {
			return start{}, api.Errorf(api.PBranchStartMissing, "%s: set %s has no package %s", pk.ID, o.FromSet, pk.ID).WithPackage(pk.ID).
				WithHint("name another start point", "repoctl checkout --create "+pk.ID+" --from REF")
		}
		if !have(e.Commit) {
			return start{}, api.Errorf(api.PBranchStartMissing, "%s: commit %s of %s (%s) is not in %s", pk.ID, api.ShortCommit(e.Commit), o.FromSet, e.Branch, RepoOf(c.e.Paths, c.e.SetDir, pk)).
				WithPackage(pk.ID).WithHint("name another start point", "repoctl checkout --create "+pk.ID+" --from REF")
		}
		return start{commit: e.Commit, label: "forked from " + e.Branch + " at " + api.ShortCommit(e.Commit)}, nil
	case o.FromFedora:
		name := pk.ID
		if pk.Upstream != nil && pk.Upstream.Fedora != "" {
			name = pk.Upstream.Fedora
		}
		prefix := o.FedoraURL
		if prefix == "" {
			prefix = FedoraURL
		}
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		u := prefix + name + ".git"
		tmp := "refs/repoctl/fedora/" + pk.ID
		if _, err := repo.Run(ctx, "fetch", "--quiet", "--no-tags", u, "+refs/heads/"+o.FedoraBranch+":"+tmp); err != nil {
			return start{}, api.Errorf(api.PGitFetchFailed, "%s: fetch %s %s: %v", pk.ID, u, o.FedoraBranch, err).WithPackage(pk.ID).WithCause(err).
				WithHint("examine the Fedora package name upstream.fedora and the branch", "git ls-remote "+u)
		}
		commit, err := repo.RevParse(ctx, tmp)
		if err != nil || commit == "" {
			return start{}, api.Errorf(api.PGitFetchFailed, "%s: fetch %s %s gave no commit", pk.ID, u, o.FedoraBranch).WithPackage(pk.ID)
		}
		return start{commit: commit, tmpRef: tmp, label: "forked from fedora " + name + " " + o.FedoraBranch + " at " + api.ShortCommit(commit)}, nil
	default:
		for _, rev := range []string{o.From, "refs/remotes/" + remote + "/" + o.From} {
			if commit, err := repo.RevParse(ctx, rev); err == nil && commit != "" {
				return start{commit: commit, label: "forked from " + o.From + " at " + api.ShortCommit(commit)}, nil
			}
		}
		if !pk.External() && !c.fetchedSet {
			c.fetchSet(ctx)
			if commit, err := repo.RevParse(ctx, "refs/remotes/"+remote+"/"+o.From); err == nil && commit != "" {
				return start{commit: commit, label: "forked from " + o.From + " at " + api.ShortCommit(commit)}, nil
			}
		}
		return start{}, api.Errorf(api.PBranchStartMissing, "%s: %s is not a revision of %s", pk.ID, o.From, RepoOf(c.e.Paths, c.e.SetDir, pk)).
			WithPackage(pk.ID).WithHint("name a branch or a commit of the package repository", "git branch -a")
	}
}

// reusable checks that the worktree wt belongs to the repository of the
// package and has no uncommitted change.
func reusable(ctx context.Context, pk config.Package, repo *git.Git, wt string) error {
	rel := RelWorktree(pk.ID)
	want, err := repo.CommonDir(ctx)
	if err != nil {
		return err
	}
	w := git.New(wt)
	have, err := w.CommonDir(ctx)
	if err != nil || !samePath(have, want) {
		return api.Errorf(api.PWorktreeMissing, "%s: %s is not a worktree of %s", pk.ID, rel, want).WithExit(api.ExitFailed).
			WithPackage(pk.ID).WithPath(rel).WithHint("remove the worktree, then make the branch", "git worktree remove "+rel)
	}
	files, err := w.Changed(ctx)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		return api.Errorf(api.PWorktreeDirty, "%s: %s has uncommitted changes (%s)", pk.ID, rel, strings.Join(files, ", ")).WithExit(api.ExitFailed).
			WithPackage(pk.ID).WithPath(rel).WithHint("commit or drop the changes, then make the branch", "git -C "+rel+" status")
	}
	return nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
