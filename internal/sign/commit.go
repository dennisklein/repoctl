package sign

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// CheckClean checks that the paths have no uncommitted change in the set
// directory: else lock.dirty (exit 5). A sign command commits only its own
// change.
func CheckClean(ctx context.Context, setDir string, paths ...string) error {
	out, err := git.New(setDir).Run(ctx, append([]string{"status", "--porcelain", "--untracked-files=all", "--"}, paths...)...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if len(l) > 3 {
			files = append(files, strings.TrimSpace(l[3:]))
		}
	}
	return api.Errorf(api.PLockDirty, "the set has uncommitted changes: %s", strings.Join(firstN(files, 5), ", ")).
		WithHint("commit or restore the changes first", "git -C "+setDir+" status")
}

// SetCommit makes one set commit of the paths, signed with the key of
// sign.key (git commit -S<FPR>). A nil signer makes an unsigned commit
// (sign revoke on a host without the secret key). It never pushes. It gives
// the commit.
func SetCommit(ctx context.Context, setDir string, s *Signer, paths []string, message string) (string, error) {
	g := &git.Git{Dir: setDir}
	if s != nil {
		g.Env = s.GPG.Env()
	}
	if _, err := g.Run(ctx, append([]string{"add", "-A", "--"}, paths...)...); err != nil {
		return "", err
	}
	if s == nil {
		if _, err := g.Run(ctx, append([]string{"commit", "-q", "--no-gpg-sign", "-m", message, "--"}, paths...)...); err != nil {
			return "", err
		}
		return g.Run(ctx, "rev-parse", "HEAD")
	}
	args := []string{"commit", "-q", "-S" + s.FPR, "-m", message, "--"}
	if _, err := g.Run(ctx, append(args, paths...)...); err != nil {
		return "", api.Errorf(api.PSignNoKey, "git commit -S%s: %v", s.FPR, err).WithCause(err).
			WithHint("check that gpg can sign with the key of sign.key", "repoctl doctor --only sign")
	}
	return g.Run(ctx, "rev-parse", "HEAD")
}

// KeyfileAtCommit gives the KeyfileAt of a set: git show COMMIT:PATH of the
// key file of the release package.
func KeyfileAtCommit(setDir string, set *config.Set) KeyfileAt {
	return func(ctx context.Context, commit string) ([]byte, error) {
		p, ok := set.ReleasePackage()
		if !ok {
			return nil, fmt.Errorf("the set has no release package")
		}
		path := filepath.ToSlash(filepath.Join(p.Path, KeyfileName(set)))
		c := &run.Cmd{Name: "git", Args: []string{"-C", setDir, "show", commit + ":" + path}, Env: []string{"LC_ALL=C"}}
		return c.Output(ctx)
	}
}

// LastCommit gives the last commit of HEAD that changed the path, or "".
func LastCommit(ctx context.Context, setDir, path string) string {
	out, err := git.New(setDir).Run(ctx, "log", "-1", "--format=%H", "--", path)
	if err != nil {
		return ""
	}
	return out
}

// Pushed reports whether the commit is on the remote branch of the set.
func Pushed(ctx context.Context, setDir string, set *config.Set, commit string) bool {
	if commit == "" {
		return false
	}
	ok, err := git.New(setDir).IsAncestor(ctx, commit, set.Remote()+"/set/"+set.Name)
	return err == nil && ok
}
