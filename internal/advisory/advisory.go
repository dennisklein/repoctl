// Package advisory computes the advisories of a set: the deterministic
// checks that tell a human that a rebuild or another step can be necessary
// (docs/design/advisories.html). It holds the id and the facts digest, the
// codes that need no build (repoctl check), the result file check.json, the
// dismissal file .repoctl/advisories.yaml, the check timer, the health
// signals and the publish gate.
//
// repoctl computes advisories and does not store them. It stores only the
// last check result (per user and host) and the dismissals (per team,
// committed in the set).
package advisory

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
)

// ID gives the one id of an advisory, CODE/SUBJECT.
func ID(code api.AdvisoryCode, subject string) string { return string(code) + "/" + subject }

var idRE = regexp.MustCompile(`^[a-z]+\.[a-z-]+/[A-Za-z0-9][A-Za-z0-9_.+-]*(@[a-z0-9][a-z0-9._-]*)?$`)

// ValidID reports whether id has the form CODE/SUBJECT of the contract.
func ValidID(id string) bool { return idRE.MatchString(id) }

// SplitID gives the code and the subject of an id.
func SplitID(id string) (api.AdvisoryCode, string) {
	c, s, _ := strings.Cut(id, "/")
	return api.AdvisoryCode(c), s
}

// DOMID gives the element id of an advisory row: adv- plus the id, with /
// and @ changed to -.
func DOMID(id string) string {
	return "adv-" + strings.NewReplacer("/", "-", "@", "-").Replace(id)
}

// levelRank orders the levels: required first.
func levelRank(l api.AdvisoryLevel) int {
	switch l {
	case api.AdvisoryLevelRequired:
		return 0
	case api.AdvisoryLevelWarn:
		return 1
	}
	return 2
}

// AtLeast reports whether level l is min or higher.
func AtLeast(l, minLevel api.AdvisoryLevel) bool { return levelRank(l) <= levelRank(minLevel) }

// ParseLevel reads a level of --level and --min-level.
func ParseLevel(s string) (api.AdvisoryLevel, bool) {
	for _, l := range api.AdvisoryLevelValues {
		if string(l) == s {
			return l, true
		}
	}
	return "", false
}

// Sort orders advisories as the terminal shows them: required first, then
// warn, then info; inside a level by id.
func Sort(list []api.Advisory) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ra, rb := levelRank(a.Level), levelRank(b.Level); ra != rb {
			return ra < rb
		}
		return a.ID < b.ID
	})
}

// New makes an advisory of a code with its facts. It sets id, the facts
// as a JSON object and their digest. facts is a typed Facts… value of
// package api (or a map) with sorted lists.
func New(code api.AdvisoryCode, subject string, level api.AdvisoryLevel, targets []api.TargetId, facts any) (api.Advisory, error) {
	m, err := factsMap(facts)
	if err != nil {
		return api.Advisory{}, err
	}
	d, err := FactsDigest(m)
	if err != nil {
		return api.Advisory{}, err
	}
	t := append([]api.TargetId{}, targets...)
	sort.Strings(t)
	return api.Advisory{
		ID: ID(code, subject), Code: code, Level: level, Subject: subject, Targets: t,
		State: api.AdvisoryStateOpen, Facts: m, FactsDigest: d, Source: "check",
	}, nil
}

// factsMap gives facts as a JSON object.
func factsMap(facts any) (map[string]any, error) {
	if m, ok := facts.(map[string]any); ok {
		return m, nil
	}
	b, err := json.Marshal(facts)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("facts of %T are not an object: %w", facts, err)
	}
	return m, nil
}

// DecodeFacts reads the facts of an advisory into a typed Facts… value.
func DecodeFacts(a api.Advisory, into any) error {
	b, err := json.Marshal(a.Facts)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// Short gives the FACTS column of the terminal: the evidence in a few
// words.
func Short(a api.Advisory, arrow string) string {
	switch a.Code {
	case api.AdvisoryCodeUpstreamRelease:
		var f api.FactsUpstreamRelease
		_ = DecodeFacts(a, &f)
		return f.Current + " " + arrow + " " + f.Upstream
	case api.AdvisoryCodeClosureBroken:
		var f api.FactsClosureBroken
		_ = DecodeFacts(a, &f)
		var parts []string
		for _, b := range f.Broken {
			s := nvrOf(b.NVRA) + " needs " + shortCap(b.Requires)
			if b.Newest != "" {
				s += "; " + b.NewestRepo + " " + nameVersion(b.Newest) + " gives only " + shortCaps(b.NewestProvides)
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, "; ")
	case api.AdvisoryCodeSonameRemoved:
		var f api.FactsSonameRemoved
		_ = DecodeFacts(a, &f)
		var req []string
		for _, r := range f.RequiredBy {
			req = append(req, nvrOf(r.NVRA))
		}
		return shortCaps(f.Removed) + " removed; required by " + strings.Join(uniq(req), ", ")
	case api.AdvisoryCodeMockDrift:
		var f api.FactsMockDrift
		_ = DecodeFacts(a, &f)
		cur, _ := a.HostFacts["current"].(string)
		return "mock config " + api.ShortDigest(f.Recorded) + " " + arrow + " " + api.ShortDigest(cur)
	case api.AdvisoryCodeRetiredPublished:
		var f api.FactsRetiredPublished
		_ = DecodeFacts(a, &f)
		return fmt.Sprintf("%s of %s in latest", plural(f.RPMs, "RPM"), strings.Join(f.NVRs, ", "))
	case api.AdvisoryCodeBuildrootDrift:
		var f api.FactsBuildrootDrift
		_ = DecodeFacts(a, &f)
		var parts []string
		for _, d := range f.Drift {
			parts = append(parts, d.Package+" "+d.Recorded+" "+arrow+" "+d.Available)
		}
		return strings.Join(parts, ", ")
	case api.AdvisoryCodeProviderChanged:
		var f api.FactsProviderChanged
		_ = DecodeFacts(a, &f)
		var parts []string
		for _, c := range f.Changes {
			parts = append(parts, shortCap(c.Capability)+" "+nvrOf(c.Recorded)+" "+arrow+" set "+c.Provider)
		}
		return strings.Join(parts, "; ")
	case api.AdvisoryCodeSignExpiring:
		var f api.FactsSignExpiring
		_ = DecodeFacts(a, &f)
		return "key " + api.ShortFingerprint(f.Fingerprint) + " expires " + f.Expires
	}
	return a.Title
}

// nvrOf gives the NVR of an NVRA.
func nvrOf(nvra string) string {
	if i := strings.LastIndexByte(nvra, '.'); i > 0 {
		return nvra[:i]
	}
	return nvra
}

// nameVersion gives "NAME VERSION" of an NVRA.
func nameVersion(nvra string) string {
	nvr := nvrOf(nvra)
	i := strings.LastIndexByte(nvr, '-')
	if i < 0 {
		return nvr
	}
	rest := nvr[:i]
	j := strings.LastIndexByte(rest, '-')
	if j < 0 {
		return rest
	}
	return rest[:j] + " " + rest[j+1:]
}

func shortCap(c string) string { return strings.TrimSuffix(strings.TrimSuffix(c, "(64bit)"), "()") }

func shortCaps(list []string) string {
	var out []string
	for _, c := range list {
		s := shortCap(c)
		if i := strings.Index(s, ".so."); i > 0 && len(list) == 1 {
			s = s[i:]
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

func uniq(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// plural gives "1 RPM" or "3 RPMs".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
