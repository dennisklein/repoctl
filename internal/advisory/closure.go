package advisory

import (
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/advisory/rpmmd"
	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/manifest"
)

// Broken is one broken requirement: an RPM and a requirement that no
// package provides.
type Broken struct {
	RPM *rpmmd.Package
	Req rpmmd.Cap
}

// provIndex finds the providers of a requirement.
type provIndex struct {
	byName map[string][]provEntry
	files  map[string][]*rpmmd.Package
}

type provEntry struct {
	pkg *rpmmd.Package
	cap rpmmd.Cap
}

func newProvIndex(pkgs []*rpmmd.Package) *provIndex {
	x := &provIndex{byName: map[string][]provEntry{}, files: map[string][]*rpmmd.Package{}}
	for _, p := range pkgs {
		for _, c := range p.Provides {
			x.byName[c.Name] = append(x.byName[c.Name], provEntry{p, c})
		}
		for _, f := range p.Files {
			x.files[f] = append(x.files[f], p)
		}
	}
	return x
}

// providers gives the packages that provide req.
func (x *provIndex) providers(req rpmmd.Cap) []*rpmmd.Package {
	var out []*rpmmd.Package
	for _, e := range x.byName[req.Name] {
		if rpmmd.Satisfies(e.cap, req) {
			out = append(out, e.pkg)
		}
	}
	if strings.HasPrefix(req.Name, "/") {
		out = append(out, x.files[req.Name]...)
	}
	return out
}

// skipReq reports requirements that the closure does not check: rpmlib()
// features of rpm itself and rich dependencies.
func skipReq(r rpmmd.Cap) bool {
	return strings.HasPrefix(r.Name, "rpmlib(") || strings.HasPrefix(r.Name, "(")
}

// Repoclosure checks each requirement of each package of check against the
// providers, as dnf repoclosure does. The caller picks the newest packages
// for --newest.
func Repoclosure(check, providers []*rpmmd.Package) []Broken {
	x := newProvIndex(providers)
	var out []Broken
	for _, p := range check {
		seen := map[string]bool{}
		for _, r := range p.Requires {
			if skipReq(r) || seen[r.String()] {
				continue
			}
			seen[r.String()] = true
			if len(x.providers(r)) == 0 {
				out = append(out, Broken{RPM: p, Req: r})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i].RPM.NVRA(), out[j].RPM.NVRA(); a != b {
			return a < b
		}
		return out[i].Req.String() < out[j].Req.String()
	})
	return out
}

// ClosureNewest runs the closure with --newest: the newest package of each
// name and architecture of the tree and the repos is the only provider,
// and only the packages of the tree among them are checked.
func ClosureNewest(tree, repos []*rpmmd.Package) []Broken {
	inTree := map[*rpmmd.Package]bool{}
	for _, p := range tree {
		inTree[p] = true
	}
	all := append(append([]*rpmmd.Package{}, tree...), repos...)
	newest := rpmmd.Newest(all)
	var check []*rpmmd.Package
	for _, p := range newest {
		if inTree[p] {
			check = append(check, p)
		}
	}
	return Repoclosure(check, newest)
}

// ClosureOld gives the broken requirements of the old NVRs of the tree (not
// the newest of their name) against all packages: the second repoclosure
// run of the gate, without --newest.
func ClosureOld(tree, repos []*rpmmd.Package) []Broken {
	all := append(append([]*rpmmd.Package{}, tree...), repos...)
	newest := map[*rpmmd.Package]bool{}
	for _, p := range rpmmd.Newest(all) {
		newest[p] = true
	}
	var old []*rpmmd.Package
	for _, p := range tree {
		if !newest[p] {
			old = append(old, p)
		}
	}
	return Repoclosure(old, all)
}

// Owner maps the RPMs of a tree to the set packages that built them,
// through the source RPM of each build of the RepoManifest.
type Owner struct {
	bySRPM map[string]string
	set    *config.Set
}

// NewOwner gives the owner map of a manifest and the builds of a run (extra
// maps an SRPM file name to a package id).
func NewOwner(set *config.Set, m *manifest.Manifest, extra map[string]string) *Owner {
	o := &Owner{bySRPM: map[string]string{}, set: set}
	if m != nil {
		for _, b := range m.Builds {
			o.bySRPM[b.SRPM] = b.Package
		}
	}
	for k, v := range extra {
		o.bySRPM[k] = v
	}
	return o
}

// Package gives the set package that built an RPM, or "" for a retired
// package (no package in repoctl.yaml builds it). name is the source
// package name.
func (o *Owner) Package(p *rpmmd.Package) (pkg, name string) {
	name = manifest.NameOfSRPM(p.SourceRPM)
	if name == "" {
		name = p.Name
	}
	id := o.bySRPM[p.SourceRPM]
	if id == "" {
		id = name
	}
	if o.set != nil {
		if _, ok := o.set.Package(id); ok {
			return id, name
		}
		return "", name
	}
	return id, name
}

// formerProvider finds the package in the repos that gave the requirement
// before: for a soname, the newest package that provides the same library
// with another version; else the newest package that provides the name with
// another version.
func formerProvider(req rpmmd.Cap, repos []*rpmmd.Package) (*rpmmd.Package, []string) {
	base, suffix := sonameParts(req.Name)
	var best *rpmmd.Package
	var caps []string
	for _, p := range rpmmd.Newest(repos) {
		var match []string
		for _, c := range p.Provides {
			switch {
			case base != "" && c.Name != req.Name && strings.HasPrefix(c.Name, base) && strings.HasSuffix(c.Name, suffix):
				match = append(match, c.String())
			case base == "" && c.Name == req.Name && req.Flags != "":
				match = append(match, c.String())
			}
		}
		if len(match) == 0 {
			continue
		}
		if best == nil || p.Name < best.Name {
			best, caps = p, match
		}
	}
	sort.Strings(caps)
	return best, uniq(caps)
}

// sonameParts splits libhwloc.so.15()(64bit) into libhwloc.so. and
// ()(64bit). A name that is no soname gives "".
func sonameParts(name string) (string, string) {
	i := strings.Index(name, ".so.")
	if i < 0 {
		return "", ""
	}
	base := name[:i+4]
	rest := name[i+4:]
	j := strings.IndexByte(rest, '(')
	if j < 0 {
		return base, ""
	}
	return base, rest[j:]
}

// ClosureAdvisories names the advisories of the broken requirements of a
// tree: one closure.broken per requirer and target. The closure of a
// required advisory is the requirer plus each set package with an older NVR
// in the tree that provides the requirement. A retired requirer gives level
// warn and no closure.
func ClosureAdvisories(target string, broken []Broken, tree, repos []*rpmmd.Package, owner *Owner) ([]api.Advisory, error) {
	newest := map[*rpmmd.Package]bool{}
	for _, p := range rpmmd.Newest(append(append([]*rpmmd.Package{}, tree...), repos...)) {
		newest[p] = true
	}
	type group struct {
		subject, pkg string
		items        []api.FactsClosureBrokenBrokenItem
		closure      map[string]bool
		first        *Broken
	}
	groups := map[string]*group{}
	var order []string
	for i := range broken {
		b := &broken[i]
		pkg, name := owner.Package(b.RPM)
		subject := name + "@" + target
		if pkg != "" {
			subject = pkg + "@" + target
		}
		g, ok := groups[subject]
		if !ok {
			g = &group{subject: subject, pkg: pkg, closure: map[string]bool{}, first: b}
			groups[subject] = g
			order = append(order, subject)
		}
		item := api.FactsClosureBrokenBrokenItem{NVRA: b.RPM.NVRA(), Requires: b.Req.String()}
		if fp, caps := formerProvider(b.Req, repos); fp != nil {
			item.Newest, item.NewestRepo, item.NewestProvides = fp.NVRA(), fp.Repo, caps
		}
		g.items = append(g.items, item)
		if pkg != "" {
			g.closure[pkg] = true
			for _, p := range tree {
				if newest[p] {
					continue
				}
				for _, c := range p.Provides {
					if rpmmd.Satisfies(c, b.Req) {
						if op, _ := owner.Package(p); op != "" {
							g.closure[op] = true
						}
					}
				}
			}
		}
	}
	var out []api.Advisory
	for _, s := range order {
		g := groups[s]
		sort.Slice(g.items, func(i, j int) bool {
			if g.items[i].NVRA != g.items[j].NVRA {
				return g.items[i].NVRA < g.items[j].NVRA
			}
			return g.items[i].Requires < g.items[j].Requires
		})
		level := api.AdvisoryLevelRequired
		if g.pkg == "" {
			level = api.AdvisoryLevelWarn
		}
		a, err := New(api.AdvisoryCodeClosureBroken, g.subject, level, []api.TargetId{target}, api.FactsClosureBroken{Broken: g.items})
		if err != nil {
			return nil, err
		}
		name, _ := api.SplitNode(g.subject)
		if g.pkg != "" {
			for p := range g.closure {
				a.Closure = append(a.Closure, p)
			}
			sort.Strings(a.Closure)
			a.Title = name + " cannot be installed on a new " + target + " host"
			reason := "closure: needs " + shortCap(g.items[0].Requires)
			if g.items[0].Newest != "" {
				reason = nameOnly(g.items[0].Newest) + ": " + shortCap(g.items[0].Requires) + " -> " + shortCaps(g.items[0].NewestProvides)
			}
			a.Suggestion = &api.AdvisorySuggestion{
				Text:    "Rebuild " + name + " against the new provider, or change its spec.",
				Command: "repoctl rebuild " + name + " --reason \"" + reason + "\"",
			}
		} else {
			a.Title = "the retired package " + name + " cannot be installed on a new " + target + " host"
			a.Suggestion = &api.AdvisorySuggestion{
				Text:    "Nothing, or prune the retired package. Never rebuild it.",
				Command: "repoctl repo prune --package " + name + " --all",
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// nameOnly gives the name of an NVRA.
func nameOnly(nvra string) string {
	n, _, _ := manifest.SplitNVR(nvrOf(nvra))
	return n
}
