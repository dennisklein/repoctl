package advisory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/advisory/rpmmd"
	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/records"
)

// The codes that read the buildroot file of each current node in latest
// (advisories.html#codes): buildroot.drift and provider.changed. A current
// node is the newest build of a set package in latest of the target. Its
// BuildRequires are the requirements of its SRPM in the source repo of
// latest; its buildroot file is buildroot/NVR.json of the binary repo
// (model.html#manifest). An adopted entry has no buildroot file, so both
// codes skip it until the next build of the node writes one.
//
// repoctl check reads only latest, as each host does (advisories.html#data);
// the staged builds of this host are not an input. A set without a source
// repo (repo.source: false) has no BuildRequires in latest, so both codes
// find nothing there.
func init() {
	RegisterCode(CodeCheck{Code: api.AdvisoryCodeProviderChanged, Run: checkProviderChanged})
	RegisterCode(CodeCheck{Code: api.AdvisoryCodeBuildrootDrift, Run: checkBuildrootDrift})
}

// BuildrootNode is one current node as the buildroot codes read it.
type BuildrootNode struct {
	// Package is the set package of the node.
	Package string
	// Buildroot are the RPMs of the buildroot file of its build.
	Buildroot []records.BuildrootRPM
	// BuildRequires are the requirements of its SRPM.
	BuildRequires []rpmmd.Cap
}

// BuildrootEnv holds what the buildroot codes compare a node with on one
// target: the packages of the closure repos and the RPMs of latest.
type BuildrootEnv struct {
	closure *provIndex
	// newest is the newest package of each name and architecture in the
	// closure repos.
	newest map[string][]*rpmmd.Package
	// latest indexes the newest RPMs of latest; inLatest holds each NVRA of
	// latest, old NVRs too: a buildroot RPM with one of them came from the
	// set.
	latest   *provIndex
	inLatest map[string]bool
	owner    *Owner
}

// NewBuildrootEnv indexes the closure repos and latest (its binary RPMs)
// of a target. owner maps an RPM of latest to its set package.
func NewBuildrootEnv(closure, latest []*rpmmd.Package, owner *Owner) *BuildrootEnv {
	e := &BuildrootEnv{closure: newProvIndex(closure), newest: map[string][]*rpmmd.Package{},
		latest: newProvIndex(rpmmd.Newest(latest)), inLatest: map[string]bool{}, owner: owner}
	for _, p := range rpmmd.Newest(closure) {
		e.newest[p.Name] = append(e.newest[p.Name], p)
	}
	for _, p := range latest {
		e.inLatest[p.NVRA()] = true
	}
	return e
}

// rootProvider is a BuildRequires and the buildroot RPM that gave it.
type rootProvider struct {
	req rpmmd.Cap
	rpm records.BuildrootRPM
}

// providers finds the buildroot RPM of each BuildRequires of the node: an
// RPM of the buildroot whose name is the name of a provider in the closure
// repos or in latest, or the name of the requirement. A requirement whose
// provider the buildroot does not show is left out.
func (e *BuildrootEnv) providers(n BuildrootNode) []rootProvider {
	byName := map[string][]records.BuildrootRPM{}
	for _, r := range n.Buildroot {
		byName[r.Name] = append(byName[r.Name], r)
	}
	var out []rootProvider
	seen := map[string]bool{}
	for _, req := range n.BuildRequires {
		if skipReq(req) || seen[req.String()] {
			continue
		}
		seen[req.String()] = true
		names := map[string]bool{}
		if !strings.HasPrefix(req.Name, "/") {
			names[req.Name] = true
		}
		for _, p := range e.closure.providers(req) {
			names[p.Name] = true
		}
		for _, p := range e.latest.providers(req) {
			names[p.Name] = true
		}
		var cands []records.BuildrootRPM
		for name := range names {
			cands = append(cands, byName[name]...)
		}
		if len(cands) == 0 {
			continue
		}
		sort.Slice(cands, func(i, j int) bool { return brNVRA(cands[i]) < brNVRA(cands[j]) })
		out = append(out, rootProvider{req: req, rpm: cands[0]})
	}
	return out
}

// fromSet reports a buildroot RPM that latest holds: the set built it.
func (e *BuildrootEnv) fromSet(r records.BuildrootRPM) bool { return e.inLatest[brNVRA(r)] }

// Drift gives the facts of buildroot.drift of a node: each BuildRequires
// provider of the buildroot from outside the set whose name has a newer
// version in the closure repos. Only the version counts, not the release.
// The list is sorted by package; nil when nothing drifted.
func (e *BuildrootEnv) Drift(n BuildrootNode) []api.FactsBuildrootDriftDriftItem {
	byPkg := map[string]api.FactsBuildrootDriftDriftItem{}
	for _, rp := range e.providers(n) {
		if e.fromSet(rp.rpm) {
			continue
		}
		avail := e.newestOf(rp.rpm)
		if avail == nil {
			continue
		}
		recorded := brEVR(rp.rpm)
		if rpmmd.CompareEVR(versionOnly(avail.EVR), versionOnly(recorded)) <= 0 {
			continue
		}
		byPkg[rp.rpm.Name] = api.FactsBuildrootDriftDriftItem{Package: rp.rpm.Name, Recorded: versionText(recorded),
			Available: versionText(avail.EVR), Repo: avail.Repo}
	}
	out := make([]api.FactsBuildrootDriftDriftItem, 0, len(byPkg))
	for _, d := range byPkg {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Package < out[j].Package })
	if len(out) == 0 {
		return nil
	}
	return out
}

// newestOf gives the newest package of the closure repos with the name of
// a buildroot RPM: the same architecture first, then noarch or another
// one.
func (e *BuildrootEnv) newestOf(r records.BuildrootRPM) *rpmmd.Package {
	var best *rpmmd.Package
	for _, p := range e.newest[r.Name] {
		switch {
		case best == nil:
			best = p
		case (p.Arch == r.Arch) != (best.Arch == r.Arch):
			if p.Arch == r.Arch {
				best = p
			}
		case p.Newer(best):
			best = p
		}
	}
	return best
}

// Changes gives the facts of provider.changed of a node: each BuildRequires
// that a provider from outside the set gave in the recorded buildroot, and
// that a set package provides now. The list is sorted by capability; nil
// when no provider changed.
func (e *BuildrootEnv) Changes(n BuildrootNode) []api.FactsProviderChangedChange {
	byCap := map[string]api.FactsProviderChangedChange{}
	for _, rp := range e.providers(n) {
		if e.fromSet(rp.rpm) {
			continue
		}
		var pkgs []string
		for _, p := range e.latest.providers(rp.req) {
			id, _ := e.owner.Package(p)
			if id != "" && id != n.Package && !containsStr(pkgs, id) {
				pkgs = append(pkgs, id)
			}
		}
		if len(pkgs) == 0 {
			continue
		}
		sort.Strings(pkgs)
		c := rp.req.String()
		byCap[c] = api.FactsProviderChangedChange{Capability: c, Recorded: brNVRA(rp.rpm), Provider: pkgs[0]}
	}
	out := make([]api.FactsProviderChangedChange, 0, len(byCap))
	for _, c := range byCap {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Capability < out[j].Capability })
	if len(out) == 0 {
		return nil
	}
	return out
}

// brEVR gives the EVR of a buildroot RPM.
func brEVR(r records.BuildrootRPM) rpmmd.EVR {
	e := rpmmd.EVR{Version: r.Version, Release: r.Release}
	if r.Epoch != nil {
		e.Epoch = *r.Epoch
	}
	return e
}

// brNVRA gives NAME-VERSION-RELEASE.ARCH of a buildroot RPM.
func brNVRA(r records.BuildrootRPM) string {
	return r.Name + "-" + r.Version + "-" + r.Release + "." + r.Arch
}

func versionOnly(e rpmmd.EVR) rpmmd.EVR { return rpmmd.EVR{Epoch: e.Epoch, Version: e.Version} }

// versionText gives [EPOCH:]VERSION, without an epoch of 0.
func versionText(e rpmmd.EVR) string { return versionOnly(e).String() }

// buildrootEntry is the input of the buildroot codes on one target, read
// once per check.
type buildrootEntry struct {
	env    *BuildrootEnv
	nodes  []BuildrootNode
	latest *Latest
	err    error
}

// buildroot gives the current nodes of a target with their buildroot file
// and BuildRequires, and the index of the closure repos and latest.
func (in *Inputs) buildroot(ctx context.Context, target string) (*buildrootEntry, error) {
	in.mu.Lock()
	if in.brs == nil {
		in.brs = map[string]*buildrootEntry{}
	}
	if e, ok := in.brs[target]; ok {
		in.mu.Unlock()
		return e, e.err
	}
	in.mu.Unlock()
	e := &buildrootEntry{}
	e.latest, e.nodes, e.err = in.readBuildroot(ctx, target)
	if e.err == nil && len(e.nodes) > 0 {
		var repos []*rpmmd.Package
		repos, e.err = in.ClosurePackages(ctx, target)
		if e.err == nil {
			e.env = NewBuildrootEnv(repos, e.latest.Packages, NewOwner(in.Set, e.latest.Manifest, nil))
		}
	}
	in.mu.Lock()
	in.brs[target] = e
	in.mu.Unlock()
	return e, e.err
}

// readBuildroot reads the current nodes of a target from latest.
func (in *Inputs) readBuildroot(ctx context.Context, target string) (*Latest, []BuildrootNode, error) {
	l, err := in.Latest(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	if l.Manifest == nil {
		return l, nil, nil
	}
	srpms, err := in.sourcePackages(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	var nodes []BuildrootNode
	for _, p := range in.Set.Packages {
		if !containsStr(in.Set.PackageTargets(p), target) {
			continue
		}
		b, ok := l.Manifest.Newest(p.ID, "")
		if !ok || b.BuildrootFile == "" || strings.HasPrefix(b.Reason, "adopted") {
			continue
		}
		src := srpms[manifest.NVR(b.SRPM)]
		if src == nil {
			continue
		}
		path := filepath.Join(l.Dir, filepath.FromSlash(b.BuildrootFile))
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("the buildroot file of %s@%s is not in the local copy; run repoctl repo pull: %w", p.ID, target, err)
		}
		f, err := records.ParseBuildroot(data)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", b.BuildrootFile, err)
		}
		nodes = append(nodes, BuildrootNode{Package: p.ID, Buildroot: f.Buildroot.RPMs, BuildRequires: src.Requires})
	}
	return l, nodes, nil
}

// sourcePackages gives the SRPMs of the source repo of the local copy of
// latest, by NVR; nil when the set or latest has no source repo.
func (in *Inputs) sourcePackages(ctx context.Context, target string) (map[string]*rpmmd.Package, error) {
	if !in.Set.Repo.HasSource() {
		return nil, nil
	}
	dir := filepath.Join(in.Paths.LocalCopy(in.Set.Name, target), in.Set.Repo.Name+"-source")
	if !rpmmd.HasRepodata(dir) {
		return nil, nil
	}
	pkgs, err := in.reader().Read(ctx, dir, "latest-source")
	if err != nil {
		return nil, err
	}
	out := map[string]*rpmmd.Package{}
	for _, p := range pkgs {
		if p.Arch == "src" {
			out[p.NVR()] = p
		}
	}
	return out, nil
}

// checkBuildrootDrift gives buildroot.drift for each current node whose
// BuildRequires providers have a newer version in the closure repos.
func checkBuildrootDrift(ctx context.Context, in *Inputs, target string) (Outcome, error) {
	e, err := in.buildroot(ctx, target)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{Latest: e.latest.Digest()}
	for _, n := range e.nodes {
		drift := e.env.Drift(n)
		if len(drift) == 0 {
			continue
		}
		a, err := BuildrootDriftAdvisory(n.Package, target, drift)
		if err != nil {
			return Outcome{}, err
		}
		out.Advisories = append(out.Advisories, a)
	}
	return out, nil
}

// BuildrootDriftAdvisory gives the advisory buildroot.drift of a node
// (level info).
func BuildrootDriftAdvisory(pkg, target string, drift []api.FactsBuildrootDriftDriftItem) (api.Advisory, error) {
	a, err := New(api.AdvisoryCodeBuildrootDrift, pkg+"@"+target, api.AdvisoryLevelInfo, []api.TargetId{target}, api.FactsBuildrootDrift{Drift: drift})
	if err != nil {
		return a, err
	}
	d := drift[0]
	a.Title = d.Package + " " + d.Recorded + " in the buildroot, " + d.Available + " in the repos"
	if len(drift) > 1 {
		a.Title += fmt.Sprintf(", and %d more", len(drift)-1)
	}
	a.Suggestion = &api.AdvisorySuggestion{
		Text:    "Nothing, or a rebuild of " + pkg + ".",
		Command: fmt.Sprintf("repoctl rebuild %s --reason %q", pkg, strings.TrimSuffix(d.Package, "-devel")+" "+d.Available),
	}
	return a, nil
}

// checkProviderChanged gives provider.changed for each current node that
// took a BuildRequires from outside the set that a set package provides now.
func checkProviderChanged(ctx context.Context, in *Inputs, target string) (Outcome, error) {
	e, err := in.buildroot(ctx, target)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{Latest: e.latest.Digest()}
	for _, n := range e.nodes {
		changes := e.env.Changes(n)
		if len(changes) == 0 {
			continue
		}
		a, err := ProviderChangedAdvisory(n.Package, target, changes)
		if err != nil {
			return Outcome{}, err
		}
		out.Advisories = append(out.Advisories, a)
	}
	return out, nil
}

// ProviderChangedAdvisory gives the advisory provider.changed of a node
// (level warn).
func ProviderChangedAdvisory(pkg, target string, changes []api.FactsProviderChangedChange) (api.Advisory, error) {
	a, err := New(api.AdvisoryCodeProviderChanged, pkg+"@"+target, api.AdvisoryLevelWarn, []api.TargetId{target}, api.FactsProviderChanged{Changes: changes})
	if err != nil {
		return a, err
	}
	c := changes[0]
	a.Title = fmt.Sprintf("%s@%s took %s from %s; the set package %s provides it now", pkg, target, c.Capability, nvrOf(c.Recorded), c.Provider)
	if len(changes) > 1 {
		a.Title += fmt.Sprintf(" (and %d more)", len(changes)-1)
	}
	a.Suggestion = &api.AdvisorySuggestion{
		Text:    "Rebuild " + pkg + " against the set package, or keep the build.",
		Command: fmt.Sprintf("repoctl rebuild %s --reason %q", pkg, "build against set "+c.Provider),
	}
	return a, nil
}
