package cli

import (
	"context"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/localcopy"
	"github.com/dennisklein/repoctl/internal/sign"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// The setup state of status (lifecycle.html#setup): the steps of a new set
// or a new target that are not done yet. status computes each step from
// facts and stores nothing, so a step can open again. A retired target has
// no setup steps. The step Host ready (repoctl doctor has no failed check)
// is left out: doctor runs mock, and status runs none.

// The names of the setup steps, as in the table of lifecycle.html#setup.
const (
	setupBranches = "Branches pushed"
	setupPinned   = "All packages pinned"
	setupSet      = "Set committed and pushed"
	setupPublish  = "First publish"
	setupPublic   = "Packages public"
	setupServed   = "Served"
	setupDeployed = "Release deployed"
	setupTimer    = "Check timer"
)

// rbStatusSetup gives the open setup steps, in the order of the table. The
// step text starts with the name of the step.
func rbStatusSetup(ctx context.Context, a *App, set *config.Set, setDir string, states []workspace.PackageState, sg *api.StatusSetGit, snaps []rbSnap, timer string) []api.StatusOpenItem {
	var out []api.StatusOpenItem
	open := func(step, text, cmd string) {
		out = append(out, api.StatusOpenItem{Step: step + ": " + text, Command: cmd})
	}
	out = append(out, rbSetupBranches(ctx, set, setDir, states)...)

	var unpinned, checkout, orphaned []string
	for _, s := range states {
		switch {
		case s.State == api.LockStateOrphaned:
			orphaned = append(orphaned, s.Package)
		case s.State == api.LockStateUnpinned && s.Head == "":
			checkout = append(checkout, s.Package)
		case s.State == api.LockStateUnpinned:
			unpinned = append(unpinned, s.Package)
		}
	}
	if len(unpinned) > 0 {
		open(setupPinned, rbAnd(unpinned)+" "+rbIsAre(len(unpinned))+" unpinned", "repoctl pin "+strings.Join(unpinned, " "))
	}
	if len(checkout) > 0 {
		open(setupPinned, rbAnd(checkout)+" "+rbHasHave(len(checkout))+" no worktree and no lock line", "repoctl checkout "+strings.Join(checkout, " "))
	}
	if len(orphaned) > 0 {
		open(setupPinned, "repoctl.lock has the orphaned "+rbPlural(len(orphaned), "line", "lines")+" "+rbAnd(orphaned), "repoctl pin --orphans")
	}

	if step, cmd := rbSetupSet(ctx, set, setDir, sg); step != "" {
		open(setupSet, step, cmd)
	}

	// The steps of each target with a latest, or without one.
	var first, published []string
	all := 0
	for _, sv := range snaps {
		all++
		f := sv.fresh
		switch {
		case sv.item.Latest != nil:
			published = append(published, sv.item.Target)
		case f == nil || f.Result == localcopy.FreshFirst:
			first = append(first, sv.item.Target)
		}
	}
	if len(first) > 0 {
		cmd := "repoctl publish"
		if len(first) < all {
			cmd += " --target " + strings.Join(first, ",")
		}
		open(setupPublish, rbAnd(first)+" "+rbHasHave(len(first))+" no latest", cmd)
	}
	if private := rbSetupPrivate(ctx, a, set, snaps); len(private) > 0 {
		open(setupPublic, "the OCI "+rbPlural(len(private), "package", "packages")+" "+rbAnd(private)+" "+rbIsAre(len(private))+" not public: the mirror job and the clients cannot pull",
			"repoctl doctor --only registry")
	}
	var unserved []string
	for _, sv := range snaps {
		s := sv.served
		if sv.item.Latest == nil || s == nil || s.Err != nil {
			continue
		}
		if s.Snapshot == nil || s.Mirror == nil {
			unserved = append(unserved, sv.item.Target)
		}
	}
	if len(unserved) > 0 {
		open(setupServed, "repo.url serves no tree of "+rbAnd(unserved)+": add a mirror line to /etc/repoctl-mirror.conf on the HTTP server", "")
	}
	if text, cmd := rbSetupDeployed(set, setDir, published); text != "" {
		open(setupDeployed, text, cmd)
	}
	if timer == "off" {
		open(setupTimer, "the timer of repoctl check for "+set.Name+" is not active on this host", "repoctl check --install-timer")
	}
	return out
}

// rbSetupBranches is the step Branches pushed: each local branch of a
// package exists on its remote. A branch that is neither local nor on the
// remote is a checkout problem, not a push.
func rbSetupBranches(ctx context.Context, set *config.Set, setDir string, states []workspace.PackageState) []api.StatusOpenItem {
	type repo struct {
		dir, remote string
		pkgs        []string
		branches    []string
	}
	var order []*repo
	byDir := map[string]*repo{}
	for _, s := range states {
		if s.Branch == "" || s.Path != "" || s.State == api.LockStateOrphaned || s.Repo == "" {
			continue
		}
		r := byDir[s.Repo]
		if r == nil {
			r = &repo{dir: s.Repo, remote: set.Remote()}
			if s.Repo != setDir {
				r.remote = "origin"
			}
			byDir[s.Repo] = r
			order = append(order, r)
		}
		r.pkgs = append(r.pkgs, s.Package)
		r.branches = append(r.branches, s.Branch)
	}
	var out []api.StatusOpenItem
	for _, r := range order {
		refs, err := git.New(r.dir).Lines(ctx, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes/"+r.remote)
		if err != nil {
			continue
		}
		has := map[string]bool{}
		for _, ref := range refs {
			has[ref] = true
		}
		var missing []string
		pkg := ""
		seen := map[string]bool{}
		for i, b := range r.branches {
			if seen[b] || !has["refs/heads/"+b] || has["refs/remotes/"+r.remote+"/"+b] {
				continue
			}
			seen[b] = true
			missing = append(missing, b)
			if pkg == "" {
				pkg = r.pkgs[i]
			}
		}
		if len(missing) == 0 {
			continue
		}
		cmd := "git push " + r.remote + " " + strings.Join(missing, " ")
		if r.dir != setDir {
			cmd = "git -C " + workspace.RelWorktree(pkg) + " push origin " + strings.Join(missing, " ")
		}
		out = append(out, api.StatusOpenItem{Step: setupBranches + ": " + rbAnd(missing) + " " + rbIsAre(len(missing)) + " not on " + r.remote, Command: cmd})
	}
	return out
}

// rbSetupSet is the step Set committed and pushed: repoctl.yaml and
// repoctl.lock are in a commit on the remote branch of the set branch. A
// change that is not committed yet is daily work, not setup.
func rbSetupSet(ctx context.Context, set *config.Set, setDir string, sg *api.StatusSetGit) (string, string) {
	if sg == nil || sg.Branch == "" {
		return "", ""
	}
	files := []string{"repoctl.yaml", "repoctl.lock"}
	g := git.New(setDir)
	var untracked []string
	for _, f := range files {
		if _, err := g.Run(ctx, "ls-files", "--error-unmatch", "--", f); err != nil {
			untracked = append(untracked, f)
		}
	}
	if len(untracked) > 0 {
		return rbAnd(untracked) + " " + rbIsAre(len(untracked)) + " not committed", "git add " + strings.Join(files, " ") + " && git commit"
	}
	remote := set.Remote() + "/" + sg.Branch
	var missing []string
	for _, f := range files {
		if _, err := g.Run(ctx, "cat-file", "-e", "refs/remotes/"+remote+":"+f); err != nil {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return "", ""
	}
	return rbAnd(missing) + " " + rbIsAre(len(missing)) + " not on " + remote, "git push " + set.Remote() + " " + sg.Branch
}

// rbSetupPrivate is the step Packages public: an anonymous read of latest
// of each OCI package with a latest (registry.public). A target whose
// registry state is unknown is left out.
func rbSetupPrivate(ctx context.Context, a *App, set *config.Set, snaps []rbSnap) []string {
	var pkgs, names []string
	for _, sv := range snaps {
		f := sv.fresh
		if sv.item.Latest == nil || f == nil || f.Result == localcopy.FreshUnreachable || f.Result == localcopy.FreshMissing {
			continue
		}
		pkgs = append(pkgs, set.OCIPackage(sv.item.Target))
		names = append(names, set.Name+"-"+sv.item.Target)
	}
	vis := make([]localcopy.Visibility, len(pkgs))
	errs := make([]error, len(pkgs))
	var wg sync.WaitGroup
	for i, pkg := range pkgs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vis[i], errs[i] = localcopy.PackageVisibility(ctx, pkg, a.Getenv)
		}()
	}
	wg.Wait()
	var out []string
	for i := range pkgs {
		if errs[i] == nil && vis[i] == localcopy.Private {
			out = append(out, names[i])
		}
	}
	return out
}

// rbSetupDeployed is the step Release deployed: for each target with a
// latest, keys/deployed.yaml has a record of at least one active key. A set
// without an active key has no step.
func rbSetupDeployed(set *config.Set, setDir string, published []string) (string, string) {
	if len(published) == 0 {
		return "", ""
	}
	active := sign.LoadTrust(set, setDir).InState(config.KeyActive)
	if len(active) == 0 {
		return "", ""
	}
	deps, err := sign.ReadDeployments(setDir)
	if err != nil {
		return "", ""
	}
	var missing []string
	for _, t := range published {
		found := false
		for _, k := range active {
			if deps.Find(k.ID, t) != nil {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, t)
		}
	}
	if len(missing) == 0 {
		return "", ""
	}
	return filepath.Join("keys", "deployed.yaml") + " has no record of an active key for " + rbAnd(missing), "repoctl sign onboard " + active[0].ID
}

// rbAnd gives "el9", "el9 and el10" or "el8, el9 and el10".
func rbAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

func rbIsAre(n int) string { return rbPlural(n, "is", "are") }

func rbHasHave(n int) string { return rbPlural(n, "has", "have") }

func rbPlural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
