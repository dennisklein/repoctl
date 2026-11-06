package advisory

import (
	"fmt"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
)

// FactsLines gives the facts of an advisory as lines for repoctl
// advisories show.
func FactsLines(a api.Advisory) []string {
	var out []string
	switch a.Code {
	case api.AdvisoryCodeClosureBroken:
		var f api.FactsClosureBroken
		_ = DecodeFacts(a, &f)
		for _, b := range f.Broken {
			out = append(out, nvrOf(b.NVRA)+" requires "+b.Requires)
			if b.Newest != "" {
				out = append(out, fmt.Sprintf("%s has %s, which provides only %s", b.NewestRepo, nameVersion(b.Newest), strings.Join(b.NewestProvides, ", ")))
			}
		}
	case api.AdvisoryCodeSonameRemoved:
		var f api.FactsSonameRemoved
		_ = DecodeFacts(a, &f)
		out = append(out, "removed: "+strings.Join(f.Removed, ", "))
		for _, r := range f.RequiredBy {
			out = append(out, nvrOf(r.NVRA)+" requires "+r.Requires)
		}
	case api.AdvisoryCodeUpstreamRelease:
		var f api.FactsUpstreamRelease
		_ = DecodeFacts(a, &f)
		s := fmt.Sprintf("%s at the lock commit, %s upstream (%s", f.Current, f.Upstream, f.Source)
		if f.Tag != "" {
			s += " tag " + f.Tag
		}
		out = append(out, s+")")
	case api.AdvisoryCodeMockDrift:
		var f api.FactsMockDrift
		_ = DecodeFacts(a, &f)
		cur, _ := a.HostFacts["current"].(string)
		out = append(out, "recorded "+f.Recorded, "this host "+cur)
	case api.AdvisoryCodeRetiredPublished:
		var f api.FactsRetiredPublished
		_ = DecodeFacts(a, &f)
		out = append(out, fmt.Sprintf("%s, %s: %s", plural(f.RPMs, "RPM"), plural(f.SizeBytes, "byte"), strings.Join(f.NVRs, ", ")))
	case api.AdvisoryCodeBuildrootDrift:
		var f api.FactsBuildrootDrift
		_ = DecodeFacts(a, &f)
		for _, d := range f.Drift {
			out = append(out, fmt.Sprintf("%s %s in the buildroot, %s in %s", d.Package, d.Recorded, d.Available, d.Repo))
		}
	case api.AdvisoryCodeProviderChanged:
		var f api.FactsProviderChanged
		_ = DecodeFacts(a, &f)
		for _, c := range f.Changes {
			out = append(out, fmt.Sprintf("%s: recorded %s, now the set package %s", c.Capability, c.Recorded, c.Provider))
		}
	case api.AdvisoryCodeABIChanged:
		var f api.FactsAbiChanged
		_ = DecodeFacts(a, &f)
		for _, l := range f.Libraries {
			out = append(out, fmt.Sprintf("%s: %s → %s, abidiff %d", l.Soname, l.Old, l.New, l.Status))
		}
	case api.AdvisoryCodeSignExpiring:
		var f api.FactsSignExpiring
		_ = DecodeFacts(a, &f)
		out = append(out, "key "+f.Fingerprint+" expires "+f.Expires)
	}
	if len(out) == 0 {
		out = append(out, a.Title)
	}
	return out
}

// ClosureLine gives the closure line of a required advisory.
func ClosureLine(a api.Advisory) string {
	if a.Level != api.AdvisoryLevelRequired {
		return ""
	}
	if len(a.Closure) == 0 {
		return "none: it never blocks a publish"
	}
	return fmt.Sprintf("%s: a publish of %s that changes %s is blocked", strings.Join(a.Closure, ", "),
		strings.Join(a.Targets, ", "), strings.Join(a.Closure, " or "))
}
