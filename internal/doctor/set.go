package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/advisory"
	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/upstream"
	"github.com/dennisklein/repoctl/internal/workspace"
)

func registerSet() {
	Register(Spec{Group: GroupSet, Needs: []string{NeedBuild}, Run: checkSet})
}

func registerSetLate() {
	Register(Spec{Group: GroupSet, Run: checkRepoctlVersion})
	Register(Spec{Group: GroupSet, Run: checkUnpushed})
	Register(Spec{Group: GroupSet, Run: checkUnreachable})
	Register(Spec{Group: GroupSet, Run: checkConfigDiffers})
	Register(Spec{Group: GroupSet, Run: checkLicense})
}

// newerFile reports a set that this binary cannot read: its apiVersion or
// minRepoctl is newer.
func newerFile(err error) bool {
	e, ok := api.AsError(err)
	return ok && e.Code == api.PContractNewerFile
}

func checkSet(_ context.Context, env *Env) []Check {
	if env.SetErr != nil {
		if newerFile(env.SetErr) {
			return nil
		}
		c := Fail("set", "repoctl.yaml is not valid: "+firstLine(env.SetErr.Error()), "correct the file. run: repoctl schema config")
		c.Code = api.PConfigInvalid
		return []Check{c}
	}
	if env.Set == nil {
		return nil
	}
	s := env.Set
	worktrees, inTree := 0, 0
	for _, p := range s.Packages {
		if p.InTree() {
			inTree++
			continue
		}
		if _, err := os.Stat(filepath.Join(env.SetDir, "packages", p.ID, ".git")); err == nil {
			worktrees++
		}
	}
	text := fmt.Sprintf("set %s: repoctl.yaml valid, %s, %s", s.Name, count(worktrees, "worktree"), count(inTree, "package")+" in tree")
	return []Check{Ok("set", text)}
}

func count(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func checkRepoctlVersion(_ context.Context, env *Env) []Check {
	if env.SetErr != nil && newerFile(env.SetErr) {
		return []Check{Fail("set.repoctl-version", firstLine(env.SetErr.Error()), "upgrade repoctl on this host. run: repoctl help upgrade")}
	}
	if env.Set == nil {
		return nil
	}
	var newer []string
	for _, t := range env.Set.ActiveTargets() {
		snap := advisory.ReadSnapshotJSON(env.Paths, env.Set.Name, t.ID)
		if snap == nil || snap.Repoctl == "" {
			continue
		}
		if config.VersionLess(strings.TrimPrefix(env.Version, "v"), strings.TrimPrefix(snap.Repoctl, "v")) {
			newer = append(newer, fmt.Sprintf("repoctl %s wrote latest of %s", snap.Repoctl, t.ID))
		}
	}
	if len(newer) > 0 {
		return []Check{Warn("set.repoctl-version", strings.Join(newer, "; ")+"; this is "+env.Version,
			"upgrade repoctl before the next publish. run: repoctl help upgrade")}
	}
	text := "set.repoctl-version: repoctl " + env.Version
	if env.Set.MinRepoctl != "" {
		text += " (minRepoctl " + env.Set.MinRepoctl + ")"
	}
	return []Check{Ok("set.repoctl-version", text)}
}

// unpushed counts the commits of local branches that are on no remote
// branch.
func unpushed(ctx context.Context, dir string) (int, error) {
	g := git.New(dir)
	remotes, err := g.Run(ctx, "for-each-ref", "--count=1", "--format=%(refname)", "refs/remotes")
	if err != nil {
		return 0, err
	}
	if remotes == "" {
		return 0, nil
	}
	out, err := g.Run(ctx, "rev-list", "--count", "--branches", "--not", "--remotes")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

func checkUnpushed(ctx context.Context, env *Env) []Check {
	if env.Set == nil {
		return nil
	}
	type repo struct{ name, dir string }
	repos := []repo{{"the set repository", env.SetDir}}
	for _, p := range env.Set.Packages {
		if p.External() {
			repos = append(repos, repo{"the clone of " + p.ID, workspace.ClonePath(env.Paths, p.URL())})
		}
	}
	var found []string
	fix := ""
	for _, r := range repos {
		if _, err := os.Stat(r.dir); err != nil {
			continue
		}
		n, err := unpushed(ctx, r.dir)
		if err != nil || n == 0 {
			continue
		}
		found = append(found, fmt.Sprintf("%s has %s on no remote", r.name, count(n, "commit")))
		if fix == "" {
			fix = "push them. run: git -C " + env.tilde(r.dir) + " log --branches --not --remotes --oneline"
		}
	}
	if len(found) > 0 {
		return []Check{Warn("git.unpushed", strings.Join(found, "; "), fix)}
	}
	return []Check{Ok("git.unpushed", "git.unpushed: all commits are on their remotes")}
}

func checkUnreachable(ctx context.Context, env *Env) []Check {
	if env.Set == nil {
		return nil
	}
	var urls []string
	seen := map[string]bool{}
	for _, p := range env.Set.Packages {
		if p.External() && !seen[p.URL()] {
			seen[p.URL()] = true
			urls = append(urls, p.URL())
		}
	}
	if len(urls) == 0 {
		return nil
	}
	var bad []string
	for _, u := range urls {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := git.New(env.SetDir).Run(cctx, "ls-remote", "--heads", u)
		cancel()
		if err != nil {
			bad = append(bad, u)
		}
	}
	if len(bad) > 0 {
		return []Check{Warn("git.unreachable", strings.Join(bad, ", ")+" does not answer", "check the URL and the network. run: git ls-remote --heads "+bad[0])}
	}
	return []Check{Ok("git.unreachable", fmt.Sprintf("git.unreachable: %s answer", count(len(urls), "external URL")))}
}

// newestBuild gives the build of a manifest with the newest build time and
// a mock config hash.
func newestBuild(m *manifest.Manifest) *manifest.Build {
	var best *manifest.Build
	for i := range m.Builds {
		b := &m.Builds[i]
		if b.MockConfig == "" {
			continue
		}
		if best == nil || b.Built >= best.Built {
			best = b
		}
	}
	return best
}

func checkConfigDiffers(ctx context.Context, env *Env) []Check {
	if env.Set == nil {
		return nil
	}
	in := &advisory.Inputs{Paths: env.Paths, Set: env.Set, SetDir: env.SetDir, Mock: env.mock()}
	var differs, same, none []string
	for _, id := range env.TargetIDs() {
		m, err := manifest.ReadLocal(env.Paths, env.Set, id)
		if err != nil || m == nil {
			none = append(none, id)
			continue
		}
		b := newestBuild(m)
		if b == nil {
			none = append(none, id)
			continue
		}
		cfg, _, err := in.HostMockHash(ctx, id)
		if err != nil {
			continue
		}
		if strings.TrimPrefix(b.MockConfig, "sha256:") != strings.TrimPrefix(cfg, "sha256:") {
			differs = append(differs, fmt.Sprintf("%s (newest build %s)", id, manifest.NVR(b.SRPM)))
			continue
		}
		same = append(same, id)
	}
	if len(differs) > 0 {
		c := Warn("mock.config-differs", "the mock config on this host differs from the newest build in latest: "+strings.Join(differs, ", "),
			"install the mock config of the build host, or rebuild against this config. run: repoctl advisories ls --level info")
		return []Check{c}
	}
	var parts []string
	if len(same) > 0 {
		parts = append(parts, strings.Join(same, ", ")+" equal latest")
	}
	if len(none) > 0 {
		parts = append(parts, strings.Join(none, ", ")+": no build in latest")
	}
	return []Check{Ok("mock.config-differs", "mock.config-differs: "+strings.Join(parts, "; "))}
}

// spdxID matches one license id of an SPDX expression.
var spdxID = regexp.MustCompile(`^(LicenseRef-[A-Za-z0-9.-]+|[A-Za-z0-9][A-Za-z0-9.-]*\+?)$`)

// notSPDX are the old Fedora short names that are not SPDX ids.
var notSPDX = map[string]bool{
	"GPLv2": true, "GPLv2+": true, "GPLv3": true, "GPLv3+": true, "LGPLv2": true, "LGPLv2+": true, "LGPLv3": true,
	"LGPLv3+": true, "LGPLv2.1": true, "LGPLv2.1+": true, "AGPLv3": true, "AGPLv3+": true, "ASL": true, "BSD": true,
	"MPLv1.1": true, "MPLv2.0": true, "Public": true, "Boost": true, "Python": true, "zlib": true, "OpenSSL": true,
}

// SPDX reports whether a License tag is an SPDX expression: ids joined
// with AND, OR and WITH, with parentheses, and no old Fedora short name.
func SPDX(expr string) bool {
	s := strings.NewReplacer("(", " ", ")", " ").Replace(expr)
	f := strings.Fields(s)
	if len(f) == 0 {
		return false
	}
	wantID := true
	for _, w := range f {
		if wantID {
			if !spdxID.MatchString(w) || notSPDX[w] || w == "AND" || w == "OR" || w == "WITH" {
				return false
			}
			wantID = false
			continue
		}
		if w != "AND" && w != "OR" && w != "WITH" {
			return false
		}
		wantID = true
	}
	return !wantID
}

func checkLicense(ctx context.Context, env *Env) []Check {
	if env.Set == nil {
		return nil
	}
	lf, err := lock.Read(env.SetDir)
	if err != nil || lf == nil {
		return nil
	}
	srcs, err := workspace.Sources(ctx, env.Paths, env.Set, env.SetDir, lf, workspace.FromLock)
	if err != nil {
		return nil
	}
	var bad []string
	read := 0
	ids := make([]string, 0, len(srcs))
	for id := range srcs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		lic, err := upstream.SpecQuery(ctx, srcs[id], "%{LICENSE}")
		if err != nil {
			continue
		}
		read++
		if !SPDX(lic) {
			bad = append(bad, id+" ("+lic+")")
		}
	}
	if len(bad) > 0 {
		return []Check{Warn("sbom.license", "the License tag is not an SPDX expression: "+strings.Join(bad, ", "),
			"write the License tag as SPDX, for example GPL-2.0-or-later. run: repoctl help packages")}
	}
	text := fmt.Sprintf("sbom.license: %s with SPDX licenses", count(read, "spec"))
	if n := len(srcs) - read; n > 0 {
		text += fmt.Sprintf(", %d not read on this host", n)
	}
	return []Check{Ok("sbom.license", text)}
}
