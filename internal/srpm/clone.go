package srpm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// cloneLockWait is how long a git step waits for the clone lock of an
// external package (another command fetches into the same bare clone).
const cloneLockWait = 2 * time.Minute

var (
	repoMu    sync.Mutex
	repoLocks = map[string]*sync.Mutex{}
)

func repoLock(repo string) *sync.Mutex {
	repoMu.Lock()
	defer repoMu.Unlock()
	m, ok := repoLocks[repo]
	if !ok {
		m = &sync.Mutex{}
		repoLocks[repo] = m
	}
	return m
}

// hasCommit reports whether the repository has the commit object.
func hasCommit(ctx context.Context, repo, commit string) bool {
	if _, err := os.Stat(repo); err != nil {
		return false
	}
	c, err := git.New(repo).RevParse(ctx, commit)
	return err == nil && c != ""
}

// EnsureCommit makes sure that the package repository of src holds the
// commit, so that a source clone can check it out. For a branch package it
// fetches the branch from the remote of the set into the set repository
// (git fetch REMOTE BRANCH). For an external package it makes the bare
// clone of git.url when it is missing (git clone --bare) and fetches the
// branch into it, under the clone lock. The worktrees packages/PKG are not
// needed.
func EnsureCommit(ctx context.Context, p config.Paths, src workspace.Source, remote string) error {
	if src.Commit == "" {
		return api.Errorf(api.PLockMissingPackage, "package %s has no commit in repoctl.lock", src.Package).
			WithPackage(src.Package).WithHint("pin it", "repoctl pin "+src.Package)
	}
	if hasCommit(ctx, src.Repo, src.Commit) {
		return nil
	}
	// One fetch into a repository at a time in this process: the nodes of
	// two targets can need the same commit.
	mu := repoLock(src.Repo)
	mu.Lock()
	defer mu.Unlock()
	if hasCommit(ctx, src.Repo, src.Commit) {
		return nil
	}
	if src.URL != "" {
		return ensureExternal(ctx, p, src)
	}
	if src.Branch == "" {
		return api.Errorf(api.PGitFetchFailed, "commit %s of %s is not in the set repository", api.ShortCommit(src.Commit), src.Package).
			WithPackage(src.Package)
	}
	if remote == "" {
		remote = "origin"
	}
	if _, err := git.New(src.Repo).Run(ctx, "fetch", "--quiet", remote, src.Branch); err != nil {
		return api.Errorf(api.PGitFetchFailed, "fetch %s %s for %s: %v", remote, src.Branch, src.Package, err).
			WithPackage(src.Package).WithCause(err)
	}
	if !hasCommit(ctx, src.Repo, src.Commit) {
		return missingCommit(src)
	}
	return nil
}

func ensureExternal(ctx context.Context, p config.Paths, src workspace.Source) error {
	hash := workspace.CloneHash(src.URL)
	l, err := lock.Acquire(ctx, p.CloneLock(hash), lock.Holder{Command: "plan", PID: os.Getpid()}, cloneLockWait, false, lock.What{
		Code: api.PGitBusy, Name: "the clone of " + src.Package, Lock: "the clone lock",
	})
	if err != nil {
		return err
	}
	defer l.Release()
	// The same bare clone serves checkout and pin: make it with the fetch
	// refspec of the workspace, so that origin/BRANCH exists.
	if _, err := os.Stat(filepath.Join(src.Repo, "HEAD")); err != nil {
		if err := workspace.NewClone(ctx, src.Repo, src.URL); err != nil {
			return api.Errorf(api.PGitUnreachable, "clone %s for %s: %v", src.URL, src.Package, err).
				WithPackage(src.Package).WithCause(err)
		}
	}
	if !hasCommit(ctx, src.Repo, src.Commit) {
		if err := workspace.FetchClone(ctx, src.Repo, src.URL); err != nil {
			return api.Errorf(api.PGitFetchFailed, "fetch %s for %s: %v", src.URL, src.Package, err).
				WithPackage(src.Package).WithCause(err)
		}
	}
	if !hasCommit(ctx, src.Repo, src.Commit) {
		return missingCommit(src)
	}
	return nil
}

func missingCommit(src workspace.Source) error {
	where := "branch " + src.Branch
	if src.URL != "" {
		where += " of " + src.URL
	}
	return api.Errorf(api.PGitFetchFailed, "commit %s of %s is not on %s", api.ShortCommit(src.Commit), src.Package, where).
		WithPackage(src.Package).WithHint("push the package branch, or pin a commit of the remote", "repoctl pin "+src.Package)
}

// Clone makes the source clone of a node in dir: git clone --local
// --no-checkout of the package repository, then a detached checkout of the
// commit (build.html#source-clone). It is a full clone, never a worktree,
// a shallow clone or a clone with --shared, because the rpmautospec plugin
// reads the whole history. It gives the package directory: dir, or the
// in-tree path below it.
func Clone(ctx context.Context, src workspace.Source, dir string) (string, error) {
	removeAll(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	if _, err := git.New(filepath.Dir(dir)).Run(ctx, "clone", "--quiet", "--local", "--no-checkout", src.Repo, dir); err != nil {
		return "", fmt.Errorf("source clone of %s: %w", src.Package, err)
	}
	if _, err := git.New(dir).Run(ctx, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", src.Commit); err != nil {
		return "", fmt.Errorf("source clone of %s: checkout %s: %w", src.Package, api.ShortCommit(src.Commit), err)
	}
	if src.Path != "" {
		return filepath.Join(dir, src.Path), nil
	}
	return dir, nil
}
