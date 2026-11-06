package advisory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/advisory/rpmmd"
	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// The digests of the contract examples (contract/examples/advisories.json).
func TestFactsDigestExamples(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(testkit.ModuleRoot(), "docs", "design", "contract", "examples", "advisories.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Items []api.AdvisoryList `json:"items"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range env.Items[0].Advisories {
		got, err := FactsDigest(a.Facts)
		if err != nil {
			t.Fatal(err)
		}
		if got != a.FactsDigest {
			t.Errorf("%s: digest %s, want %s", a.ID, got, a.FactsDigest)
		}
		n++
	}
	if n < 5 {
		t.Fatalf("only %d advisories in the example", n)
	}
	// New gives the same digest from the typed facts.
	a, err := New(api.AdvisoryCodeUpstreamRelease, "slurm", api.AdvisoryLevelInfo, nil,
		api.FactsUpstreamRelease{Current: "26.05.4", Upstream: "26.05.5", Source: "github", Tag: "slurm-26-05-5-1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "upstream.release/slurm" || a.FactsDigest != "sha256:18a1814d46da857ae8d9c6a44f69e56b8c0898d9ccd6565c26577dc7ff735a7b" {
		t.Errorf("New: %s %s", a.ID, a.FactsDigest)
	}
}

func TestCanonical(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{`{"b":1,"a":[true,null,"x"]}`, `{"a":[true,null,"x"],"b":1}`},
		{`{"n":1.0,"m":1e21,"k":0.000001,"j":-0}`, `{"j":0,"k":0.000001,"m":1e+21,"n":1}`},
		{`{"s":"é\n\u001f\"\\/"}`, `{"s":"é\n\u001f\"\\/"}`},
		// UTF-16 order: U+1F600 (a surrogate pair) sorts before U+FB01.
		{`{"ﬁ":1,"😀":2}`, `{"😀":2,"ﬁ":1}`},
	} {
		var v any
		if err := json.Unmarshal([]byte(c.in), &v); err != nil {
			t.Fatal(err)
		}
		got, err := Canonical(v)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("Canonical(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestIDs(t *testing.T) {
	for id, ok := range map[string]bool{
		"closure.broken/pmix@el9": true, "upstream.release/slurm": true, "retired.published/old-lib": true,
		"closure.broken": false, "Closure.broken/pmix": false, "x/": false,
	} {
		if ValidID(id) != ok {
			t.Errorf("ValidID(%q) = %v", id, !ok)
		}
	}
	if code, subject := SplitID("closure.broken/pmix@el9"); code != api.AdvisoryCodeClosureBroken || subject != "pmix@el9" {
		t.Errorf("SplitID: %s %s", code, subject)
	}
	if !AtLeast(api.AdvisoryLevelRequired, api.AdvisoryLevelWarn) || AtLeast(api.AdvisoryLevelInfo, api.AdvisoryLevelWarn) {
		t.Error("AtLeast")
	}
}

func adv(t *testing.T, code api.AdvisoryCode, subject string, level api.AdvisoryLevel, facts any) api.Advisory {
	t.Helper()
	a, err := New(code, subject, level, nil, facts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func problemCode(err error) string {
	if e, ok := api.AsError(err); ok {
		return e.Code
	}
	return ""
}

func TestDismissRules(t *testing.T) {
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	info := adv(t, api.AdvisoryCodeUpstreamRelease, "slurm-singularity-exec", api.AdvisoryLevelInfo,
		api.FactsUpstreamRelease{Current: "3.2.0", Upstream: "3.2.1", Source: "github", Tag: "v3.2.1"})
	req := adv(t, api.AdvisoryCodeClosureBroken, "pmix@el9", api.AdvisoryLevelRequired,
		api.FactsClosureBroken{Broken: []api.FactsClosureBrokenBrokenItem{{NVRA: "pmix-6.1.0-1.el9.x86_64", Requires: "libhwloc.so.15()(64bit)"}}})
	drift := adv(t, api.AdvisoryCodeMockDrift, "el9", api.AdvisoryLevelInfo, map[string]any{"recorded": "sha256:aa"})
	current := []api.Advisory{info, req, drift}

	d := &Dismissals{Path: filepath.Join(t.TempDir(), DismissalPath)}
	if _, err := Dismiss(d, current, []string{info.ID}, "short", "d.klein@example.org", now); err == nil || exitOf(err) != api.ExitUsage {
		t.Errorf("a note of 5 characters: %v", err)
	}
	if _, err := Dismiss(d, current, []string{req.ID}, "the build host has hwloc", "d", now); problemCode(err) != api.PAdvisoryNotDismissable {
		t.Errorf("required: %v", err)
	}
	if _, err := Dismiss(d, current, []string{"closure.broken/nope@el9"}, "not a real one", "d", now); problemCode(err) != api.PAdvisoryUnknown {
		t.Errorf("unknown: %v", err)
	}
	// One failed id changes nothing.
	if _, err := Dismiss(d, current, []string{info.ID, req.ID}, "two at a time", "d", now); err == nil || len(d.Entries) != 0 {
		t.Errorf("a failed id dismissed %v", d.Entries)
	}
	res, err := Dismiss(d, current, []string{info.ID, drift.ID}, "3.2.1 only changes the docs", "d.klein@example.org", now)
	if err != nil || len(res) != 2 || res[0].Facts != info.FactsDigest {
		t.Fatalf("Dismiss: %v %v", res, err)
	}
	list := append([]api.Advisory(nil), current...)
	ApplyDismissals(list, d)
	if list[0].State != api.AdvisoryStateDismissed || list[0].Dismissal.Note != "3.2.1 only changes the docs" || list[1].State != api.AdvisoryStateOpen {
		t.Errorf("ApplyDismissals: %+v", list)
	}
	// A dismissed advisory is not open: a second dismiss fails.
	if _, err := Dismiss(d, list, []string{info.ID}, "once more please", "d", now); problemCode(err) != api.PAdvisoryUnknown {
		t.Errorf("second dismiss: %v", err)
	}

	// The file round-trips.
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	d2, err := LoadDismissals(filepath.Dir(filepath.Dir(d.Path)))
	if err != nil || len(d2.Entries) != 2 {
		t.Fatalf("LoadDismissals: %v %v", d2, err)
	}
	if e, _ := d2.Get(info.ID); e.Note != "3.2.1 only changes the docs" || !e.At.Equal(now) {
		t.Errorf("entry: %+v", e)
	}

	// New facts make the dismissal stale; the advisory is open again.
	newer := adv(t, api.AdvisoryCodeUpstreamRelease, "slurm-singularity-exec", api.AdvisoryLevelInfo,
		api.FactsUpstreamRelease{Current: "3.2.0", Upstream: "3.2.2", Source: "github", Tag: "v3.2.2"})
	list = []api.Advisory{newer}
	ApplyDismissals(list, d2)
	if list[0].State != api.AdvisoryStateOpen || list[0].Dismissal == nil || !list[0].Dismissal.Stale {
		t.Errorf("stale: %+v", list[0])
	}

	// --stale needs a complete check, keeps the mock.drift entry of
	// another host, and removes the stale one.
	if _, err := UndismissStale(d2, list, false); problemCode(err) != api.PCheckIncomplete {
		t.Errorf("incomplete: %v", err)
	}
	removed, err := UndismissStale(d2, list, true)
	if err != nil || len(removed) != 1 || removed[0] != info.ID {
		t.Errorf("UndismissStale: %v %v", removed, err)
	}
	if err := Undismiss(d2, []string{info.ID}); problemCode(err) != api.PAdvisoryUnknown {
		t.Errorf("undismiss without an entry: %v", err)
	}
	if err := Undismiss(d2, []string{drift.ID}); err != nil || len(d2.Entries) != 0 {
		t.Errorf("Undismiss: %v %v", err, d2.Entries)
	}
}

func exitOf(err error) int {
	if e, ok := api.AsError(err); ok {
		return e.ExitCode
	}
	return 0
}

func TestParseDismissals(t *testing.T) {
	good := "apiVersion: " + api.APIVersion + `
dismissed:
  - id: upstream.release/slurm-singularity-exec
    facts: sha256:802ef6e343cee3acc37df2475c8831ab8a74d7f2aa4ea83248bdb0f2afb598a9
    note: 3.2.1 only changes the docs
    by: d.klein@example.org
    at: 2026-11-02T09:10:00Z
  - id: upstream.release/slurm-singularity-exec
    facts: sha256:802ef6e343cee3acc37df2475c8831ab8a74d7f2aa4ea83248bdb0f2afb598a9
    note: the newer entry of a merge
    by: m.example@example.org
    at: 2026-11-03T09:10:00Z
`
	entries, err := ParseDismissals([]byte(good), "f")
	if err != nil || len(entries) != 2 {
		t.Fatalf("good file: %v %v", entries, err)
	}
	d := &Dismissals{Entries: entries}
	if e, _ := d.Get("upstream.release/slurm-singularity-exec"); e.Note != "the newer entry of a merge" {
		t.Errorf("Get gives %q, want the newest entry", e.Note)
	}
	if b, _ := d.Format(); strings.Count(string(b), "- id:") != 1 {
		t.Errorf("Format keeps one entry per id:\n%s", b)
	}
	if e, err := ParseDismissals([]byte("# only a comment\n"), "f"); err != nil || e != nil {
		t.Errorf("empty file: %v %v", e, err)
	}
	for name, text := range map[string]string{
		"conflict":   "<<<<<<< HEAD\n" + good,
		"apiVersion": strings.Replace(good, api.APIVersion, "repoctl.io/v9", 1),
		"short note": strings.Replace(good, "3.2.1 only changes the docs", "docs", 1),
		"bad digest": strings.Replace(good, "sha256:802ef6", "sha1:802ef6", 1),
		"bad id":     strings.Replace(good, "id: upstream.release/slurm-singularity-exec", "id: nope", 1),
		"unknown":    good + "extra: 1\n",
		"no at":      strings.Replace(good, "    at: 2026-11-02T09:10:00Z\n", "", 1),
	} {
		if _, err := ParseDismissals([]byte(text), "f"); problemCode(err) != api.PAdvisoriesInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	// LoadDismissals gives the error and an empty list.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".repoctl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DismissalPath), []byte("=======\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, err := LoadDismissals(dir); err == nil || d == nil || len(d.Entries) != 0 {
		t.Errorf("LoadDismissals of an invalid file: %v %v", d, err)
	}
}

// pkg builds a binary RPM of the tests: name-version-release.x86_64 from
// srpm, with provides and requires as "NAME" or "NAME OP EVR".
func pkg(nvr, srpm string, provides, requires []string) *rpmmd.Package {
	i := strings.LastIndex(nvr, "-")
	j := strings.LastIndex(nvr[:i], "-")
	p := &rpmmd.Package{Name: nvr[:j], Arch: "x86_64", EVR: rpmmd.EVR{Version: nvr[j+1 : i], Release: nvr[i+1:]}, SourceRPM: srpm}
	for _, c := range provides {
		p.Provides = append(p.Provides, rpmmd.ParseCap(c))
	}
	for _, c := range requires {
		p.Requires = append(p.Requires, rpmmd.ParseCap(c))
	}
	return p
}

func epel(p *rpmmd.Package) *rpmmd.Package {
	p.Repo = "epel"
	return p
}

func TestRepoclosure(t *testing.T) {
	repos := []*rpmmd.Package{
		epel(pkg("hwloc-libs-2.11.2-1.el9", "hwloc-2.11.2-1.el9.src.rpm", []string{"libhwloc.so.16()(64bit)"}, nil)),
		pkg("hwloc-libs-2.4.1-5.el9", "hwloc-2.4.1-5.el9.src.rpm", []string{"libhwloc.so.15()(64bit)"}, nil),
		pkg("glibc-2.34-100.el9", "glibc-2.34-100.el9.src.rpm", []string{"libc.so.6()(64bit)", "glibc = 2.34-100.el9"}, nil),
	}
	tree := []*rpmmd.Package{
		pkg("pmix-6.1.0-1.el9", "pmix-6.1.0-1.el9.src.rpm", []string{"libpmix.so.2()(64bit)"},
			[]string{"libhwloc.so.15()(64bit)", "libc.so.6()(64bit)", "rpmlib(CompressedFileNames) <= 3.0.4-1", "(foo if bar)", "glibc >= 2.34"}),
		pkg("slurm-26.05.4-1.el9", "slurm-26.05.4-1.el9.src.rpm", nil, []string{"libpmix.so.2()(64bit)", "glibc >= 2.40"}),
	}
	broken := ClosureNewest(tree, repos)
	var got []string
	for _, b := range broken {
		got = append(got, b.RPM.NVRA()+" "+b.Req.String())
	}
	want := []string{"pmix-6.1.0-1.el9.x86_64 libhwloc.so.15()(64bit)", "slurm-26.05.4-1.el9.x86_64 glibc >= 2.40"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("ClosureNewest: %v, want %v", got, want)
	}
	advs, err := ClosureAdvisories("el9", broken[:1], tree, repos, NewOwner(nil, nil, nil))
	if err != nil || len(advs) != 1 {
		t.Fatalf("ClosureAdvisories: %v %v", advs, err)
	}
	a := advs[0]
	if a.ID != "closure.broken/pmix@el9" || a.Level != api.AdvisoryLevelRequired || strings.Join(a.Closure, ",") != "pmix" {
		t.Errorf("advisory: %s %s %v", a.ID, a.Level, a.Closure)
	}
	if a.FactsDigest != "sha256:43bbb57469da8d836a23e3eb954afbe49335377a9e12c4411f158c59c02661ad" {
		t.Errorf("the facts differ from the example: %v %s", a.Facts, a.FactsDigest)
	}
}

func TestGate(t *testing.T) {
	repos := []*rpmmd.Package{
		pkg("hwloc-libs-2.11.2-1.el9", "hwloc-2.11.2-1.el9.src.rpm", []string{"libhwloc.so.16()(64bit)"}, nil),
	}
	pmixOld := pkg("pmix-6.0.0-1.el9", "pmix-6.0.0-1.el9.src.rpm", []string{"libpmix.so.2()(64bit)"}, nil)
	pmixNew := pkg("pmix-6.1.0-1.el9", "pmix-6.1.0-1.el9.src.rpm", []string{"libpmix.so.2()(64bit)"}, []string{"libhwloc.so.15()(64bit)"})
	slurm := pkg("slurm-26.05.4-1.el9", "slurm-26.05.4-1.el9.src.rpm", nil, []string{"libpmix.so.2()(64bit)"})
	base := []*rpmmd.Package{pmixOld, slurm}
	tree := []*rpmmd.Package{pmixOld, pmixNew, slurm}

	// pmix of this publish breaks: the advisory blocks.
	in := GateInput{Target: "el9", Kind: GatePublish, Base: base, Tree: tree, Repos: repos, Packages: []string{"pmix"}}
	res, err := Gate(in)
	if problemCode(err) != api.PAdvisoryOpen || !res.Blocked() || res.Rows[0].Status != RowBlocks {
		t.Fatalf("Gate: %v %+v", err, res)
	}
	if e, _ := api.AsError(err); e.HumanAction == nil || !strings.Contains(e.HumanAction.Command, "--accept-advisory closure.broken/pmix@el9") {
		t.Errorf("no humanAction: %+v", e)
	}
	// The same advisory does not block a publish of slurm alone.
	in.Packages = []string{"slurm"}
	res, err = Gate(in)
	if err != nil || res.Blocked() || res.Rows[0].Status != RowOpen {
		t.Errorf("not in the closure: %v %+v", err, res)
	}
	// An acceptance of an id that does not block is refused.
	in.Accept, in.Reason = []string{"closure.broken/pmix@el9"}, "hwloc 2.11 comes next week"
	if _, err := Gate(in); problemCode(err) != api.PAdvisoryNotBlocking {
		t.Errorf("accept in advance: %v", err)
	}
	// An acceptance removes the block and records the acceptance.
	in.Packages = []string{"pmix"}
	in.User, in.Host = "d.klein", "build01"
	res, err = Gate(in)
	if err != nil || res.Blocked() || len(res.Accepted) != 1 || res.Rows[0].Status != RowAccepted {
		t.Fatalf("accepted: %v %+v", err, res)
	}
	if acc := res.Accepted[0]; acc.ID != "closure.broken/pmix@el9" || acc.Reason != "hwloc 2.11 comes next week" || acc.FactsDigest == "" {
		t.Errorf("acceptance: %+v", acc)
	}
	// prune and resign never block on an advisory.
	in.Accept, in.Reason = nil, ""
	in.Kind = GateResign
	if res, err := Gate(in); err != nil || len(res.Rows) != 0 {
		t.Errorf("resign: %v %+v", err, res)
	}
	// A prune of the old pmix keeps the break of the new one: no new
	// break, and a prune never blocks on an advisory.
	in.Kind, in.Base, in.Tree = GatePrune, tree, []*rpmmd.Package{pmixNew, slurm}
	if res, err := Gate(in); err != nil || res.Blocked() {
		t.Errorf("prune that breaks nothing new: %v %+v", err, res)
	}

	// A new pmix without libpmix.so.2: soname.removed with slurm.
	pmixSo3 := pkg("pmix-7.0.0-1.el9", "pmix-7.0.0-1.el9.src.rpm", []string{"libpmix.so.3()(64bit)"}, nil)
	in = GateInput{Target: "el9", Kind: GatePublish, Base: base, Tree: []*rpmmd.Package{pmixOld, pmixSo3, slurm}, Repos: repos, Packages: []string{"pmix"}}
	res, err = Gate(in)
	if problemCode(err) != api.PAdvisoryOpen || len(res.Rows) == 0 {
		t.Fatalf("soname: %v %+v", err, res)
	}
	r := res.Rows[0]
	if r.Advisory.ID != "soname.removed/pmix@el9" || strings.Join(r.Advisory.Closure, ",") != "pmix,slurm" || strings.Join(r.Affected, ",") != "slurm-26.05.4-1.el9" {
		t.Errorf("soname row: %s %v %v", r.Advisory.ID, r.Advisory.Closure, r.Affected)
	}
	if e, _ := api.AsError(err); len(e.Hints) == 0 || !strings.Contains(e.Hints[0].Run, "repoctl rebuild slurm") {
		t.Errorf("hints: %+v", e)
	}
	// Removing the provider in a prune breaks the closure.
	in = GateInput{Target: "el9", Kind: GatePrune, Base: base, Tree: []*rpmmd.Package{slurm}, Repos: repos, Packages: []string{"pmix"}}
	if _, err := Gate(in); problemCode(err) != api.PPruneBreaksClosure {
		t.Errorf("prune: %v", err)
	}
}

func TestValidateAccept(t *testing.T) {
	if err := ValidateAccept(nil, "", false); err != nil {
		t.Error(err)
	}
	if err := ValidateAccept(nil, "a reason", true); exitOf(err) != api.ExitUsage {
		t.Errorf("--reason alone: %v", err)
	}
	if err := ValidateAccept([]string{"closure.broken/pmix@el9"}, "short", true); exitOf(err) != api.ExitUsage {
		t.Errorf("short reason: %v", err)
	}
	if err := ValidateAccept([]string{"closure.broken/pmix@el9"}, "hwloc next week", false); problemCode(err) != api.PPublishUnattendedRefused {
		t.Errorf("unattended: %v", err)
	}
	if err := ValidateAccept([]string{"closure.broken/pmix@el9"}, "hwloc next week", true); err != nil {
		t.Error(err)
	}
}

func TestEscapePath(t *testing.T) {
	for in, want := range map[string]string{
		"/home/dk/sets/slurm-26.05": `home-dk-sets-slurm\x2d26.05`,
		"/srv/a b/.x":               `srv-a\x20b-.x`,
		"/.hidden":                  `\x2ehidden`,
		"/":                         "-",
	} {
		if got := EscapePath(in); got != want {
			t.Errorf("EscapePath(%q) = %q, want %q", in, got, want)
		}
		if got := UnescapePath(EscapePath(in)); got != filepath.Clean(in) {
			t.Errorf("round trip of %q: %q", in, got)
		}
	}
}

func TestTimerTestHost(t *testing.T) {
	dir := t.TempDir()
	setDir := filepath.Join(dir, "set")
	if err := os.MkdirAll(setDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tm := &Timer{UnitDir: filepath.Join(dir, "units"), SetDir: setDir, TestHost: true}
	if tm.State(t.Context()) != "off" {
		t.Error("state before install")
	}
	res, err := tm.Install(t.Context())
	if err != nil || len(res.Wrote) != 2 || tm.State(t.Context()) != "active" {
		t.Fatalf("Install: %+v %v", res, err)
	}
	// The set has no repoctl.yaml: the instance is an orphan.
	if o := Orphans(tm.UnitDir); len(o) != 1 || o[0] != tm.Instance() {
		t.Errorf("Orphans: %v", o)
	}
	if err := os.WriteFile(filepath.Join(setDir, config.SetFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if o := Orphans(tm.UnitDir); len(o) != 0 {
		t.Errorf("Orphans: %v", o)
	}
	if err := os.WriteFile(filepath.Join(tm.UnitDir, ServiceTemplate), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !tm.UnitsDiffer() {
		t.Error("UnitsDiffer")
	}
	if res, _ := tm.Install(t.Context()); len(res.Updated) != 1 || tm.UnitsDiffer() {
		t.Errorf("update: %+v", res)
	}
	if had, err := tm.Remove(t.Context()); !had || err != nil || tm.State(t.Context()) != "off" {
		t.Errorf("Remove: %v %v", had, err)
	}
}

// memJournal records the events of a run.
type memJournal struct{ events []string }

func (m *memJournal) Started(api.RunId, string, time.Time) error { m.add("started"); return nil }
func (m *memJournal) Advisory(a api.Advisory) error              { m.add("advisory " + a.ID); return nil }
func (m *memJournal) Gone(id string) error                       { m.add("gone " + id); return nil }
func (m *memJournal) Finished(s api.RunState, _ time.Duration, _ int) error {
	m.add("finished " + string(s))
	return nil
}
func (m *memJournal) add(s string) { m.events = append(m.events, s) }

// TestRunMerge runs fake codes: a failure carries the advisories of the
// last good result, a skip keeps the old result, and a gone id is written.
func TestRunMerge(t *testing.T) {
	codesMu.Lock()
	saved := codes
	codes = map[api.AdvisoryCode]CodeCheck{}
	codesMu.Unlock()
	t.Cleanup(func() {
		codesMu.Lock()
		codes = saved
		codesMu.Unlock()
	})
	state := map[string]string{"a": "ok", "b": "ok"}
	mk := func(sub string) api.Advisory {
		return adv(t, api.AdvisoryCodeRetiredPublished, sub, api.AdvisoryLevelWarn, map[string]any{"nvrs": []string{sub + "-1-1"}})
	}
	RegisterCode(CodeCheck{Code: api.AdvisoryCodeRetiredPublished, Run: func(_ context.Context, _ *Inputs, target string) (Outcome, error) {
		switch state[target] {
		case "fail":
			return Outcome{}, api.Errorf(api.PUpstreamUnreachable, "the repo of %s does not answer", target)
		case "skip":
			return Outcome{}, &SkipError{Reason: "no local copy of latest of " + target}
		case "fixed":
			return Outcome{}, nil
		}
		return Outcome{Advisories: []api.Advisory{mk("old-" + target)}}, nil
	}})
	dir := t.TempDir()
	set := &config.Set{Name: "s", Targets: []config.Target{{ID: "a"}, {ID: "b"}}}
	p := config.Paths{StateHome: filepath.Join(dir, "state"), RuntimeDir: filepath.Join(dir, "run")}
	clock := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	in := &Inputs{Paths: p, Set: set, SetDir: dir, Now: func() time.Time { return clock }}
	run := func() (*Summary, *memJournal) {
		t.Helper()
		j := &memJournal{}
		sum, err := Run(t.Context(), in, Options{Journal: j, By: "human"}, NewRunID(clock))
		if err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(time.Hour)
		return sum, j
	}
	sum, _ := run()
	if len(sum.File.Advisories()) != 2 || !sum.File.Complete(set) {
		t.Fatalf("first run: %+v", sum.File)
	}
	state["a"], state["b"] = "fail", "skip"
	sum, j := run()
	list := sum.File.Advisories()
	if len(list) != 2 || list[0].CarriedFrom == "" || sum.File.Complete(set) || len(sum.Failed()) != 1 {
		t.Fatalf("failed run: %+v %v", list, sum.Failed())
	}
	if strings.Contains(strings.Join(j.events, "|"), "gone") {
		t.Errorf("a failed or skipped code wrote gone: %v", j.events)
	}
	state["a"], state["b"] = "fixed", "ok"
	sum, j = run()
	if len(sum.File.Advisories()) != 1 || strings.Join(sum.Gone, ",") != "retired.published/old-a" {
		t.Errorf("fixed run: %v gone %v", sum.File.Advisories(), sum.Gone)
	}
	if !strings.Contains(strings.Join(j.events, "|"), "gone retired.published/old-a") {
		t.Errorf("journal: %v", j.events)
	}
	// check.json round-trips.
	cf, err := ReadCheckFile(p, "s")
	if err != nil || cf == nil || len(cf.Results) != 2 {
		t.Fatalf("ReadCheckFile: %+v %v", cf, err)
	}
}

func TestFileJournalValidates(t *testing.T) {
	dir := t.TempDir()
	p := config.Paths{StateHome: dir}
	j := &FileJournal{Paths: p, Origin: Origin{Via: "cli", User: "dk", Host: "build01", PID: 42}, Repoctl: "0.1.0"}
	run := NewRunID(time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC))
	if err := j.Started(run, "slurm-26.05", time.Now()); err != nil {
		t.Fatal(err)
	}
	a := adv(t, api.AdvisoryCodeUpstreamRelease, "slurm", api.AdvisoryLevelInfo,
		api.FactsUpstreamRelease{Current: "26.05.4", Upstream: "26.05.5", Source: "github", Tag: "slurm-26-05-5-1"})
	if err := errors.Join(j.Advisory(a), j.Gone("closure.broken/pmix@el9"), j.Finished(api.RunStateOk, time.Second, 0)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.Journal("slurm-26.05", run))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 4 {
		t.Errorf("%d lines", n)
	}
	testkit.ValidateJournal(t, data)
}
