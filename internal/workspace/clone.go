package workspace

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// Env is what the workspace commands take from the command: the paths,
// the set, the holder record of the clone lock and the wait for a busy
// lock.
type Env struct {
	Paths  config.Paths
	Set    *config.Set
	SetDir string
	// Holder is recorded in the clone lock while a git step runs on a bare
	// clone.
	Holder lock.Holder
	// LockWait is --lock-wait. A clone lock waits at least 60 s.
	LockWait time.Duration
	// Jobs is the number of packages that checkout fetches at the same
	// time; 0 or less is 1.
	Jobs int
}

// CloneLockMinWait is the shortest wait for a busy clone lock
// (lifecycle.html#clones).
const CloneLockMinWait = 60 * time.Second

// CloneFetchRefspec is the fetch refspec of a bare clone: the remote
// branches go to refs/remotes/origin/*, so that a worktree pushes as in a
// normal clone.
const CloneFetchRefspec = "+refs/heads/*:refs/remotes/origin/*"

// CloneHost gives the host of an external URL for the terminal, for
// example git.gsi.de; "" when the URL has none (file://).
func CloneHost(rawURL string) string {
	if u, err := url.Parse(NormalizeURL(rawURL)); err == nil {
		return u.Hostname()
	}
	return ""
}

// WithCloneLock runs step under the clone lock of the URL: all sets of the
// user on the host share the bare clone, so each git step on it takes the
// lock (lifecycle.html#clones). A busy lock gives git.busy (exit 7) after
// the wait.
func (e Env) WithCloneLock(ctx context.Context, rawURL string, step func() error) error {
	hash := CloneHash(rawURL)
	wait := max(e.LockWait, CloneLockMinWait)
	l, err := lock.Acquire(ctx, e.Paths.CloneLock(hash), e.Holder, wait, false, lock.What{
		Code: api.PGitBusy, Name: "the clone " + ClonePath(e.Paths, rawURL) + " (" + rawURL + ")",
		Lock: "the clone lock", Retry: "repoctl " + e.Holder.Command + " --lock-wait 10m",
	})
	if err != nil {
		return err
	}
	defer l.Release()
	return step()
}

// Clone makes the bare clone of an external URL when it is missing, and
// fetches it when fetch is set, under the clone lock. It gives the path of
// the clone and whether it fetched.
func (e Env) Clone(ctx context.Context, rawURL string, fetch bool) (string, bool, error) {
	path := ClonePath(e.Paths, rawURL)
	fetched := false
	err := e.WithCloneLock(ctx, rawURL, func() error {
		if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
			if !fetch {
				return nil
			}
			fetched = true
			return FetchClone(ctx, path, rawURL)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fetched = true
		return NewClone(ctx, path, rawURL)
	})
	return path, fetched, err
}

// NewClone makes the bare clone of an external URL at path, with the fetch
// refspec of a normal clone (CloneFetchRefspec), so that origin/BRANCH
// exists for each branch. The caller holds the clone lock.
func NewClone(ctx context.Context, path, rawURL string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	_ = os.RemoveAll(tmp)
	if _, err := git.New(filepath.Dir(path)).Run(ctx, "clone", "--quiet", "--bare", rawURL, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return api.Errorf(api.PGitFetchFailed, "clone %s: %v", rawURL, err).WithCause(err).
			WithHint("examine the remote and the credentials", "git ls-remote "+rawURL)
	}
	g := git.New(tmp)
	if err := g.ConfigSet(ctx, "remote.origin.fetch", CloneFetchRefspec); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := g.Fetch(ctx, "origin"); err != nil {
		_ = os.RemoveAll(tmp)
		return api.Errorf(api.PGitFetchFailed, "fetch %s: %v", rawURL, err).WithCause(err).
			WithHint("examine the remote and the credentials", "git ls-remote "+rawURL)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

// FetchClone fetches the bare clone at path from origin. A clone without a
// fetch refspec gets CloneFetchRefspec first. The caller holds the clone
// lock.
func FetchClone(ctx context.Context, path, rawURL string) error {
	g := git.New(path)
	if out, err := g.Run(ctx, "config", "--get-all", "remote.origin.fetch"); err != nil || strings.TrimSpace(out) == "" {
		if err := g.ConfigSet(ctx, "remote.origin.fetch", CloneFetchRefspec); err != nil {
			return err
		}
	}
	if err := g.Fetch(ctx, "origin"); err != nil {
		return api.Errorf(api.PGitFetchFailed, "fetch %s into %s: %v", rawURL, path, err).WithCause(err).
			WithHint("examine the remote and the credentials", "git -C "+path+" fetch origin")
	}
	return nil
}

// FetchSet fetches the remote of the set repository (defaults.git.remote).
func (e Env) FetchSet(ctx context.Context, refspecs ...string) error {
	remote := e.Set.Remote()
	if err := git.New(e.SetDir).Fetch(ctx, remote, refspecs...); err != nil {
		return api.Errorf(api.PGitFetchFailed, "fetch %s: %v", remote, err).WithCause(err).
			WithHint("examine the remote and the credentials", "git fetch "+remote)
	}
	return nil
}

// RemoteOf gives the remote of a package in its repository: origin in a
// bare clone, defaults.git.remote in the set repository.
func (e Env) RemoteOf(pk config.Package) string {
	if pk.External() {
		return "origin"
	}
	return e.Set.Remote()
}

// pushCommand gives the command that pushes the branch of a package from
// its worktree. A detached HEAD pushes HEAD:BRANCH.
func pushCommand(remote, pkg, branch string, detached bool) string {
	ref := branch
	if detached {
		ref = "HEAD:" + branch
	}
	return "git -C " + RelWorktree(pkg) + " push " + remote + " " + ref
}
