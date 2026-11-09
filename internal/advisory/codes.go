package advisory

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/upstream"
)

// The codes of repoctl check that need no buildroot file. The codes that
// read the buildroot files of latest (buildroot.drift, provider.changed)
// are in buildroot.go, sign.expiring is in signexpiring.go.
func init() {
	RegisterCode(CodeCheck{Code: api.AdvisoryCodeClosureBroken, Run: checkClosure})
	RegisterCode(CodeCheck{Code: api.AdvisoryCodeMockDrift, Run: checkMockDrift})
	RegisterCode(CodeCheck{Code: api.AdvisoryCodeUpstreamRelease, AllTargets: true, Run: checkUpstream})
	RegisterCode(CodeCheck{Code: api.AdvisoryCodeRetiredPublished, Run: checkRetired})
}

// checkClosure runs repoclosure --newest on latest of the target plus its
// closure repos (closure.broken).
func checkClosure(ctx context.Context, in *Inputs, target string) (Outcome, error) {
	l, err := in.Latest(ctx, target)
	if err != nil {
		return Outcome{}, err
	}
	repos, err := in.ClosurePackages(ctx, target)
	if err != nil {
		return Outcome{}, err
	}
	broken := ClosureNewest(l.Packages, repos)
	advs, err := ClosureAdvisories(target, broken, l.Packages, repos, NewOwner(in.Set, l.Manifest, nil))
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Advisories: advs, Latest: l.Digest()}, nil
}

// checkMockDrift compares the mock config hash and the macros of this host
// with the newest build of each node in latest (mock.drift). Adopted
// entries have no hash and are skipped.
func checkMockDrift(ctx context.Context, in *Inputs, target string) (Outcome, error) {
	l, err := in.Latest(ctx, target)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{Latest: l.Digest()}
	if l.Manifest == nil {
		return out, nil
	}
	cfg, mac, err := in.HostMockHash(ctx, target)
	if err != nil {
		return Outcome{}, err
	}
	current := DriftHash(cfg, mac)
	for _, p := range in.Set.Packages {
		if !containsStr(in.Set.PackageTargets(p), target) {
			continue
		}
		b, ok := l.Manifest.Newest(p.ID, "")
		if !ok || b.MockConfig == "" || strings.HasPrefix(b.Reason, "adopted") {
			continue
		}
		recorded := DriftHash(b.MockConfig, b.Macros)
		if recorded == current {
			continue
		}
		a, err := New(api.AdvisoryCodeMockDrift, p.ID+"@"+target, api.AdvisoryLevelInfo, []api.TargetId{target}, api.FactsMockDrift{Recorded: recorded})
		if err != nil {
			return Outcome{}, err
		}
		a.HostFacts = map[string]any{
			"current":    current,
			"mockConfig": map[string]any{"recorded": normDigest(b.MockConfig), "current": cfg},
			"macros":     map[string]any{"recorded": normDigest(b.Macros), "current": mac},
		}
		a.Title = "the mock config of " + target + " on this host differs from the build of " + p.ID
		a.Suggestion = &api.AdvisorySuggestion{
			Text:    "Nothing, or a rebuild of " + p.ID + " against the config of this host.",
			Command: "repoctl rebuild " + p.ID + " --reason \"mock config of " + target + "\"",
		}
		out.Advisories = append(out.Advisories, a)
	}
	return out, nil
}

// checkRetired finds RPMs in latest of a source package that no package of
// repoctl.yaml builds (retired.published).
func checkRetired(ctx context.Context, in *Inputs, target string) (Outcome, error) {
	l, err := in.Latest(ctx, target)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{Latest: l.Digest()}
	if l.Manifest == nil {
		return out, nil
	}
	size := map[string]int64{}
	for _, p := range l.Packages {
		size[path.Base(p.Location)] = p.Size
	}
	type ret struct {
		nvrs  []string
		rpms  int
		bytes int64
	}
	byName := map[string]*ret{}
	for _, b := range l.Manifest.Builds {
		if _, ok := in.Set.Package(b.Package); ok {
			continue
		}
		name := manifest.NameOfSRPM(b.SRPM)
		if _, ok := in.Set.Package(name); ok {
			continue
		}
		r := byName[name]
		if r == nil {
			r = &ret{}
			byName[name] = r
		}
		r.nvrs = append(r.nvrs, manifest.NVR(b.SRPM))
		r.rpms += len(b.RPMs)
		for _, f := range b.RPMs {
			r.bytes += size[path.Base(f)]
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := byName[n]
		a, err := New(api.AdvisoryCodeRetiredPublished, n, api.AdvisoryLevelInfo, []api.TargetId{target},
			api.FactsRetiredPublished{NVRs: uniqSorted(r.nvrs), RPMs: r.rpms, SizeBytes: int(r.bytes)})
		if err != nil {
			return Outcome{}, err
		}
		a.Title = fmt.Sprintf("latest holds RPMs of the retired package %s", n)
		a.Suggestion = &api.AdvisorySuggestion{
			Text:    "Nothing (the default), or prune the retired package.",
			Command: "repoctl repo prune --package " + n + " --all",
		}
		out.Advisories = append(out.Advisories, a)
	}
	return out, nil
}

// checkUpstream gives upstream.release for each package with a newer
// upstream version. A package whose queries failed keeps its advisory of
// the last good result.
func checkUpstream(ctx context.Context, in *Inputs, _ string) (Outcome, error) {
	rep, err := in.UpstreamReport(ctx)
	if err != nil {
		return Outcome{}, err
	}
	if rep.AllFailed() {
		var first error
		for _, e := range rep.Failed {
			first = e
			break
		}
		return Outcome{}, api.Errorf(api.PUpstreamUnreachable, "no upstream answered: %v", first).WithCause(first).
			WithHint("try again later", "repoctl check --code upstream.release")
	}
	advs, err := UpstreamAdvisories(in, rep.Items)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{Advisories: advs}
	if len(rep.Failed) > 0 {
		var pkgs []string
		for p := range rep.Failed {
			pkgs = append(pkgs, p)
		}
		sort.Strings(pkgs)
		out.Carry = pkgs
		out.Partial = &api.Problem{Code: api.PUpstreamUnreachable, Message: "the upstream of " + strings.Join(pkgs, ", ") + " did not answer",
			Hint: "try again later. run: repoctl upstream --refresh " + strings.Join(pkgs, " ")}
	}
	return out, nil
}

// UpstreamAdvisories gives upstream.release for each item with the status
// update-available.
func UpstreamAdvisories(in *Inputs, items []api.Upstream) ([]api.Advisory, error) {
	var out []api.Advisory
	for _, u := range items {
		if u.Status != api.UpstreamStatusUpdateAvailable {
			continue
		}
		f := api.FactsUpstreamRelease{Current: u.Current.Version, Upstream: u.Latest, Source: upstream.Source(u)}
		if f.Source == "github" && u.Github != nil {
			f.Tag = u.Github.Tag
		}
		var targets []api.TargetId
		if p, ok := in.Set.Package(u.Package); ok {
			targets = in.Set.PackageTargets(*p)
		}
		a, err := New(api.AdvisoryCodeUpstreamRelease, u.Package, api.AdvisoryLevelInfo, targets, f)
		if err != nil {
			return nil, err
		}
		a.Source = "upstream"
		a.Title = u.Package + " " + u.Latest + " is released"
		a.Suggestion = &api.AdvisorySuggestion{Text: "Bump the version with the skill bump-package."}
		out = append(out, a)
	}
	return out, nil
}
