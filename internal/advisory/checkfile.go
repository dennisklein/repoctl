package advisory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/GSI-HPC/go-clikit/fsutil"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// CheckKind is the kind of check.json.
const CheckKind = "CheckResult"

// CheckFile is check.json in the state directory of the set: the last
// check result of this user and host. It holds one result per code and
// target; upstream.release has one result for all targets.
type CheckFile struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Set        string `json:"set"`
	// At is the end of the last check run, By who started it (timer,
	// human, ui, mcp) and Run its journal.
	At         api.Time  `json:"at"`
	By         string    `json:"by,omitempty"`
	Run        api.RunId `json:"run,omitempty"`
	DurationMs int       `json:"durationMs,omitempty"`
	Results    []Result  `json:"results"`
}

// Result is the result of one code on one target.
type Result struct {
	Code   api.AdvisoryCode `json:"code"`
	Target api.TargetId     `json:"target,omitempty"`
	At     api.Time         `json:"at"`
	Ok     bool             `json:"ok"`
	// Latest is the digest of latest of the target that the result used.
	Latest api.Digest `json:"latest,omitempty"`
	// Problem is the failure of a failed result.
	Problem *api.Problem `json:"problem,omitempty"`
	// GoodAt is the time of the last good result. A failed result keeps
	// its advisories, each with carriedFrom.
	GoodAt     api.Time       `json:"goodAt,omitempty"`
	Advisories []api.Advisory `json:"advisories"`
}

// key gives the key of a result in check.json.
func (r Result) key() string { return string(r.Code) + "\x00" + r.Target }

// ReadCheckFile reads check.json; nil without one.
func ReadCheckFile(p config.Paths, set string) (*CheckFile, error) {
	b, err := os.ReadFile(p.CheckJSON(set))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cf CheckFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, api.Errorf(api.PCheckFailed, "%s is not valid: %v", p.CheckJSON(set), err).WithPath(p.CheckJSON(set)).
			WithHint("run the check again", "repoctl check")
	}
	return &cf, nil
}

// Write writes check.json to a temporary file and renames it, so a reader
// never sees a part of it.
func (cf *CheckFile) Write(p config.Paths) error {
	cf.APIVersion, cf.Kind = api.APIVersion, CheckKind
	sort.SliceStable(cf.Results, func(i, j int) bool {
		a, b := cf.Results[i], cf.Results[j]
		if a.Code != b.Code {
			return codeRank(a.Code) < codeRank(b.Code)
		}
		return a.Target < b.Target
	})
	for i := range cf.Results {
		if cf.Results[i].Advisories == nil {
			cf.Results[i].Advisories = []api.Advisory{}
		}
	}
	b, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	path := p.CheckJSON(cf.Set)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(b, '\n'), 0o600)
}

func codeRank(c api.AdvisoryCode) int {
	for i, v := range api.AdvisoryCodeValues {
		if v == c {
			return i
		}
	}
	return len(api.AdvisoryCodeValues)
}

// Find gives the result of a code and target.
func (cf *CheckFile) Find(code api.AdvisoryCode, target string) (Result, bool) {
	if cf == nil {
		return Result{}, false
	}
	for _, r := range cf.Results {
		if r.Code == code && r.Target == target {
			return r, true
		}
	}
	return Result{}, false
}

// Advisories gives the advisories of the last check, one per id: the
// advisories of the same id from several targets (retired.published) are
// merged. The state is open; ApplyDismissals sets dismissed.
func (cf *CheckFile) Advisories() []api.Advisory {
	if cf == nil {
		return nil
	}
	byID := map[string]int{}
	var out []api.Advisory
	for _, r := range cf.Results {
		for _, a := range r.Advisories {
			a.State = api.AdvisoryStateOpen
			a.Dismissal = nil
			if i, ok := byID[a.ID]; ok {
				out[i] = merge(out[i], a)
				continue
			}
			byID[a.ID] = len(out)
			out = append(out, a)
		}
	}
	Sort(out)
	return out
}

// merge joins two advisories with the same id from two targets.
func merge(a, b api.Advisory) api.Advisory {
	seen := map[string]bool{}
	var t []api.TargetId
	for _, x := range append(append([]api.TargetId{}, a.Targets...), b.Targets...) {
		if !seen[x] {
			seen[x] = true
			t = append(t, x)
		}
	}
	sort.Strings(t)
	a.Targets = t
	if a.Code == api.AdvisoryCodeRetiredPublished {
		var fa, fb api.FactsRetiredPublished
		_ = DecodeFacts(a, &fa)
		_ = DecodeFacts(b, &fb)
		fa.NVRs = uniqSorted(append(fa.NVRs, fb.NVRs...))
		fa.RPMs += fb.RPMs
		fa.SizeBytes += fb.SizeBytes
		if m, err := factsMap(fa); err == nil {
			a.Facts = m
			a.FactsDigest, _ = FactsDigest(m)
		}
	}
	for _, c := range b.Closure {
		if !containsStr(a.Closure, c) {
			a.Closure = append(a.Closure, c)
		}
	}
	sort.Strings(a.Closure)
	if b.CarriedFrom != "" && (a.CarriedFrom == "" || b.CarriedFrom < a.CarriedFrom) {
		a.CarriedFrom = b.CarriedFrom
	}
	if b.Seen != "" && (a.Seen == "" || b.Seen < a.Seen) {
		a.Seen = b.Seen
	}
	return a
}

func uniqSorted(list []string) []string {
	sort.Strings(list)
	var out []string
	for i, s := range list {
		if i == 0 || s != list[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Complete reports whether each code of the registry has an ok result on
// each active target of the set (one result for a code of all targets).
func (cf *CheckFile) Complete(set *config.Set) bool {
	if cf == nil {
		return false
	}
	for _, c := range Codes() {
		if c.AllTargets {
			if r, ok := cf.Find(c.Code, ""); !ok || !r.Ok {
				return false
			}
			continue
		}
		for _, t := range set.ActiveTargets() {
			if r, ok := cf.Find(c.Code, t.ID); !ok || !r.Ok {
				return false
			}
		}
	}
	return true
}

// Failed gives the problems of the failed results.
func (cf *CheckFile) Failed() []api.Problem {
	if cf == nil {
		return nil
	}
	var out []api.Problem
	for _, r := range cf.Results {
		if !r.Ok && r.Problem != nil {
			out = append(out, *r.Problem)
		}
	}
	return out
}

// Ok reports that no result failed.
func (cf *CheckFile) Ok() bool { return cf != nil && len(cf.Failed()) == 0 }

// Time gives the time of the last run.
func (cf *CheckFile) Time() time.Time {
	if cf == nil {
		return time.Time{}
	}
	return api.MustTime(cf.At)
}

// Summary gives the check object of the kind AdvisoryList.
func (cf *CheckFile) Summary() api.AdvisoryListCheck {
	if cf == nil {
		return api.AdvisoryListCheck{}
	}
	c := api.AdvisoryListCheck{At: cf.At, By: cf.By, Ok: cf.Ok(), Failed: cf.Failed()}
	for _, r := range cf.Results {
		c.Results = append(c.Results, api.AdvisoryListCheckResult{Code: r.Code, Target: r.Target, At: r.At, Ok: r.Ok, Latest: r.Latest})
	}
	return c
}
