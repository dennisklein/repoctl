// Package workspace is the set directory of a human: the package worktrees
// below packages/, the bare clones of external packages, the lock states,
// and the commands init, migrate, checkout, pin and rebuild
// (docs/design/lifecycle.html, cli.html#group-workspace).
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// Source is where the build of one package comes from: the commit and the
// git repository that holds it. A run clones Repo with git clone --local
// --no-checkout and checks out Commit (build.html#source-clone).
type Source struct {
	Package string
	// Commit is the full commit of the build identity.
	Commit string
	// Repo is the git directory to clone: the set repository for a branch
	// or in-tree package, the bare clone for an external package.
	Repo string
	// Path is the in-tree path below the root of the set repository; "" for
	// a branch package.
	Path   string
	Branch string
	// URL is git.url of an external package.
	URL string
}

// From is where Sources takes the package commits from (--from).
type From string

// The values of --from.
const (
	FromLock     From = "lock"
	FromWorktree From = "worktree"
)

// Sources gives the source of each package of the set: the commit of
// repoctl.lock (FromLock) or the HEAD of each worktree (FromWorktree). An
// in-tree package has the last set commit that changes its path. A package
// without a lock line is missing from the result; the caller reports
// lock.missing-package.
func Sources(ctx context.Context, p config.Paths, set *config.Set, setDir string, lf *lock.File, from From) (map[string]Source, error) {
	out := map[string]Source{}
	for _, pk := range set.Packages {
		s := Source{Package: pk.ID, Repo: setDir}
		switch {
		case pk.InTree():
			c, err := InTreeCommit(ctx, setDir, pk.Path)
			if err != nil {
				return nil, err
			}
			s.Commit, s.Path = c, pk.Path
		case from == FromWorktree:
			wt := filepath.Join(setDir, "packages", pk.ID)
			c, err := git.New(wt).RevParse(ctx, "HEAD")
			if err != nil {
				return nil, api.Errorf(api.PLockMissingPackage, "package %s has no worktree", pk.ID).
					WithPackage(pk.ID).WithHint("create the worktree", "repoctl checkout "+pk.ID)
			}
			s.Commit, s.Branch, s.URL = c, pk.Branch, pk.URL()
			if pk.External() {
				s.Repo = ClonePath(p, pk.URL())
			}
		default:
			e, ok := lf.Get(pk.ID)
			if !ok {
				continue
			}
			s.Commit, s.Branch, s.URL = e.Commit, e.Branch, pk.URL()
			if pk.External() {
				s.Repo = ClonePath(p, pk.URL())
			}
		}
		out[pk.ID] = s
	}
	return out, nil
}

// InTreeCommit gives the last set commit that changes path.
func InTreeCommit(ctx context.Context, setDir, path string) (string, error) {
	out, err := git.New(setDir).Run(ctx, "log", "-1", "--format=%H", "--", path)
	if err != nil {
		return "", err
	}
	c := strings.TrimSpace(out)
	if c == "" {
		return "", api.Errorf(api.PLockMissingPackage, "the in-tree path %s has no commit", path).WithPath(path)
	}
	return c, nil
}

// NormalizeURL lower-cases scheme and host and removes a trailing / and
// .git. An scp-like URL USER@HOST:PATH becomes ssh://USER@HOST/PATH.
func NormalizeURL(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.Contains(s, "://") {
		if at := strings.Index(s, "@"); at >= 0 {
			if colon := strings.Index(s[at:], ":"); colon > 0 {
				s = "ssh://" + s[:at+colon] + "/" + strings.TrimPrefix(s[at+colon+1:], "/")
			}
		}
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" {
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		s = u.String()
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return strings.TrimSuffix(s, "/")
}

// CloneHash gives the first 10 hex digits of the sha256 of the normalized
// URL.
func CloneHash(rawURL string) string {
	sum := sha256.Sum256([]byte(NormalizeURL(rawURL)))
	return hex.EncodeToString(sum[:])[:10]
}

// ClonePath gives the bare clone of an external URL:
// $XDG_DATA_HOME/repoctl/git/HASH.git.
func ClonePath(p config.Paths, rawURL string) string {
	return filepath.Join(p.GitClones(), CloneHash(rawURL)+".git")
}
