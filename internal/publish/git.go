package publish

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// GitChecks runs the checks of the set inputs in step 1: lock.dirty,
// lock.unpushed, lock.orphaned and lock.missing-package. It gives the set
// commit and the commit of each package of the lock.
type GitChecks interface {
	Check(ctx context.Context, r Rules, targets []string) (setCommit string, commits map[string]string, err error)
}

// SetGit is the default GitChecks on the set checkout.
type SetGit struct {
	Set    *config.Set
	SetDir string
	Paths  config.Paths
	// NoFetch skips git fetch (tests without a network remote).
	NoFetch bool
}

// porcelainFiles gives the paths of git status --porcelain output. Each
// line is "XY PATH", and X or Y may be a space, so the lines are not
// trimmed before the status is cut off.
func porcelainFiles(out string) []string {
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if len(l) > 3 {
			files = append(files, l[3:])
		}
	}
	return files
}

// Check runs the checks.
func (g *SetGit) Check(ctx context.Context, r Rules, targets []string) (string, map[string]string, error) {
	gt := git.New(g.SetDir)
	inputs := []string{"repoctl.yaml", "repoctl.lock", "keys"}
	for _, t := range g.Set.Targets {
		inputs = append(inputs, t.Macros...)
	}
	for _, p := range g.Set.Packages {
		if p.InTree() {
			inputs = append(inputs, p.Path)
		}
	}
	out, err := gt.Run(ctx, append([]string{"status", "--porcelain", "--untracked-files=all", "--"}, inputs...)...)
	if err != nil {
		return "", nil, err
	}
	if files := porcelainFiles(out); len(files) > 0 {
		return "", nil, api.Errorf(api.PLockDirty, "the set inputs have changes that are not committed: %s", strings.Join(files, ", ")).
			WithHint("commit and push them", "git commit && git push")
	}
	head, err := gt.RevParse(ctx, "HEAD")
	if err != nil {
		return "", nil, err
	}
	branch, _ := gt.CurrentBranch(ctx)
	remote := g.Set.Remote()
	if remote == "" {
		remote = "origin"
	}
	if !g.NoFetch {
		if _, err := gt.Run(ctx, "fetch", "--quiet", remote); err != nil {
			return "", nil, api.Errorf(api.PLockUnpushed, "git fetch %s failed: %v", remote, err).WithCause(err)
		}
	}
	if branch == "" {
		return "", nil, api.Errorf(api.PLockUnpushed, "the set checkout is not on a branch")
	}
	if ok, err := gt.IsAncestor(ctx, head, remote+"/"+branch); err != nil || !ok {
		return "", nil, api.Errorf(api.PLockUnpushed, "the set commit %s is not on %s/%s", api.ShortCommit(head), remote, branch).
			WithHint("push the set branch", "git push")
	}
	lf, err := lock.Read(g.SetDir)
	if err != nil {
		return "", nil, err
	}
	var orphaned []string
	for id := range lf.Packages {
		if _, ok := g.Set.Package(id); !ok {
			orphaned = append(orphaned, id)
		}
	}
	if len(orphaned) > 0 {
		sort.Strings(orphaned)
		return "", nil, api.Errorf(api.PLockOrphaned, "repoctl.lock has lines for packages that repoctl.yaml does not have: %s", strings.Join(orphaned, ", ")).
			WithHint("remove the lines", "repoctl pin --prune")
	}
	src, err := workspace.Sources(ctx, g.Paths, g.Set, g.SetDir, lf, workspace.FromLock)
	if err != nil {
		return "", nil, err
	}
	commits := map[string]string{}
	var missing []string
	for _, p := range g.Set.Packages {
		s, ok := src[p.ID]
		if !ok || s.Commit == "" {
			for _, t := range g.Set.PackageTargets(p) {
				if containsStr(targets, t) {
					missing = append(missing, p.ID)
					break
				}
			}
			continue
		}
		commits[p.ID] = s.Commit
		if r.LockUnpushed != LockAll || p.InTree() {
			continue
		}
		pg := git.New(s.Repo)
		if !g.NoFetch && p.External() {
			_, _ = pg.Run(ctx, "fetch", "--quiet", "origin")
		}
		ref := remote + "/" + s.Branch
		if p.External() {
			ref = "origin/" + s.Branch
		}
		if ok, err := pg.IsAncestor(ctx, s.Commit, ref); err != nil || !ok {
			return "", nil, api.Errorf(api.PLockUnpushed, "the pinned commit %s of %s is not on %s", api.ShortCommit(s.Commit), p.ID, ref).
				WithPackage(p.ID).WithHint("push the package branch", fmt.Sprintf("git -C packages/%s push", p.ID))
		}
	}
	if len(missing) > 0 && r.TakesStaged {
		return "", nil, api.Errorf(api.PLockMissingPackage, "repoctl.lock has no line for %s", strings.Join(missing, ", ")).
			WithHint("pin the packages", "repoctl pin "+strings.Join(missing, " "))
	}
	return head, commits, nil
}
