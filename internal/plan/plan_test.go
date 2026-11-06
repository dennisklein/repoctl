package plan

import (
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/graph"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// The test set: a; b needs a; c needs b; d needs a and is for el9 only.
const testSet = `apiVersion: repoctl/v1alpha1
name: t-1
repo:
  name: t
  url: https://repo.example.org/t
targets:
  - id: el9
    mock: rocky+epel-9-x86_64
    plugins: [rpmautospec]
  - id: el10
    mock: rocky+epel-10-x86_64
    plugins: [rpmautospec]
packages:
  - id: a
    branch: a/t
  - id: b
    branch: b/t
  - id: c
    branch: c/t
  - id: d
    branch: d/t
    targets: [el9]
`

var commits = map[string]string{
	"a": strings.Repeat("a", 40),
	"b": strings.Repeat("b", 40),
	"c": strings.Repeat("c", 40),
	"d": strings.Repeat("d", 40),
}

const oldB = "0123456789abcdef0123456789abcdef01234567"

type fixture struct {
	set    *config.Set
	graphs map[string]*graph.Graph
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	set, err := config.ParseSet([]byte(testSet), "repoctl.yaml")
	if err != nil {
		t.Fatal(err)
	}
	brs := map[string][]string{"b": {"a-devel"}, "c": {"b-devel"}, "d": {"a-devel"}}
	f := &fixture{set: set, graphs: map[string]*graph.Graph{}}
	for _, tg := range []string{"el9", "el10"} {
		var infos []*graph.Info
		for _, p := range []string{"a", "b", "c", "d"} {
			if p == "d" && tg != "el9" {
				continue
			}
			infos = append(infos, &graph.Info{Node: api.Node(p, tg), Package: p, Target: tg, Name: p, EVR: "1.0-1",
				Requires: brs[p], Provides: []string{p, p + "-devel"}})
		}
		g, _, err := graph.New(graph.Input{Target: tg, Nodes: infos})
		if err != nil {
			t.Fatal(err)
		}
		f.graphs[tg] = g
	}
	return f
}

// input gives the input of a plan of the targets, with every node selected
// unless sel names nodes.
func (f *fixture) input(targets []string, sel ...api.NodeId) Input {
	in := Input{Set: f.set, Targets: targets, From: "lock", Commits: map[string]string{}, Graphs: f.graphs,
		Manifests: map[string]*manifest.Manifest{}}
	for k, v := range commits {
		in.Commits[k] = v
	}
	s := &Selection{Targets: targets}
	if len(sel) > 0 {
		s.Nodes = sel
		in.Positive = true
	} else {
		for _, tg := range targets {
			for _, n := range f.graphs[tg].Nodes {
				s.Nodes = append(s.Nodes, n.ID)
			}
		}
	}
	in.Selection = s
	return in
}

func latest(target string, builds map[string]string) *manifest.Manifest {
	m := manifest.New("t-1", target)
	m.Publish = "20260901T090000Z"
	for _, p := range []string{"a", "b", "c", "d"} {
		c, ok := builds[p]
		if !ok {
			continue
		}
		m.Builds = append(m.Builds, manifest.Build{Package: p, Branch: p + "/t", Commit: c, Reason: "new-package",
			SRPM: p + "-1.0-1.el9.src.rpm", RPMs: []string{p + "-1.0-1.el9.x86_64.rpm"}})
	}
	return m
}

func validate(t *testing.T, r *Result) {
	t.Helper()
	if err := testkit.Validate("Plan", testkit.MarshalItem(t, r.Plan)); err != nil {
		t.Fatalf("plan does not agree with the schema: %v\n%s", err, testkit.MarshalItem(t, r.Plan))
	}
}

// actions gives "node:action[:reason,…]" of every node, in plan order.
func actions(r *Result) string {
	var out []string
	for _, n := range r.Plan.Nodes {
		s := n.Node + ":" + string(n.Action)
		var rs []string
		for _, x := range n.Reasons {
			rs = append(rs, string(x.Code))
		}
		if n.SkipBecause != "" {
			rs = append(rs, n.SkipBecause)
		}
		if len(rs) > 0 {
			s += ":" + strings.Join(rs, ",")
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func TestNewPackages(t *testing.T) {
	f := newFixture(t)
	r := Make(f.input([]string{"el9", "el10"}))
	validate(t, r)
	want := "a@el9:build:new-package b@el9:build:new-package c@el9:build:new-package d@el9:build:new-package " +
		"a@el10:build:new-package b@el10:build:new-package c@el10:build:new-package"
	if got := actions(r); got != want {
		t.Fatalf("actions\n got %s\nwant %s", got, want)
	}
	if r.Plan.Baseline != "none" || r.BuildCount() != 7 || r.Plan.Summary.ByTarget["el9"].Build != 4 || r.Plan.Summary.ByTarget["el10"].Build != 3 {
		t.Fatalf("summary %+v, baseline %s", r.Plan.Summary, r.Plan.Baseline)
	}
	n := r.Node("c@el9")
	if strings.Join(n.Deps, " ") != "b@el9" || n.Checks == nil || n.Checks.Soname != "none" || n.Checks.ABI != "off" {
		t.Fatalf("c@el9: %+v", n)
	}
}

func TestCurrentChangedNewTarget(t *testing.T) {
	f := newFixture(t)
	in := f.input([]string{"el9", "el10"})
	in.Manifests["el9"] = latest("el9", map[string]string{"a": commits["a"], "b": oldB, "c": commits["c"], "d": commits["d"]})
	in.Describe = func(pkg, from, to string) Change {
		if pkg != "b" || from != oldB || to != commits["b"] {
			t.Errorf("describe %s %s %s", pkg, from, to)
		}
		return Change{Subject: "Update to 1.1", Commits: 1}
	}
	r := Make(in)
	validate(t, r)
	want := "a@el9:current b@el9:build:commit-changed c@el9:current d@el9:current " +
		"a@el10:build:new-target b@el10:build:new-target c@el10:build:new-target"
	if got := actions(r); got != want {
		t.Fatalf("actions\n got %s\nwant %s", got, want)
	}
	b := r.Node("b@el9")
	if b.PreviousCommit != oldB || b.Checks.Soname != "pending" {
		t.Fatalf("b@el9: %+v", b)
	}
	if msg := b.Reasons[0].Message; msg != "b moved from 0123456 to bbbbbbb (1 commit: Update to 1.1)." {
		t.Fatalf("message %q", msg)
	}
	if a := r.Node("a@el9"); a.Snapshot != "20260901T090000Z" || a.Checks != nil {
		t.Fatalf("a@el9: %+v", a)
	}
	if r.Plan.Baseline != "latest" {
		t.Fatalf("baseline %s", r.Plan.Baseline)
	}

	// Only rebuild commits since latest: the reason is rebuild.
	in.Describe = func(string, string, string) Change {
		return Change{Subject: "Rebuild: new libfoo", Commits: 1, Rebuild: true, Text: "new libfoo", RebuildOnly: true}
	}
	r = Make(in)
	if rs := r.Node("b@el9").Reasons; len(rs) != 1 || rs[0].Code != api.PlanReasonCodeRebuild || rs[0].Text != "new libfoo" {
		t.Fatalf("reasons %+v", rs)
	}
}

func TestSelectionNeeded(t *testing.T) {
	f := newFixture(t)
	in := f.input([]string{"el9", "el10"}, "c@el9")
	r := Make(in)
	validate(t, r)
	want := "a@el9:build:new-package b@el9:build:new-package c@el9:build:new-package d@el9:skip:not-selected " +
		"a@el10:skip:not-selected b@el10:skip:not-selected c@el10:skip:not-selected"
	if got := actions(r); got != want {
		t.Fatalf("actions\n got %s\nwant %s", got, want)
	}
	if !r.Selected["c@el9"] || !r.Needed["b@el9"] || !r.Needed["a@el9"] || r.Selected["b@el9"] {
		t.Fatalf("selected %v needed %v", r.Selected, r.Needed)
	}
	if got := strings.Join(r.Plan.Selection.Packages, " "); got != "c" {
		t.Fatalf("selection packages %q", got)
	}

	// A current dependency stays current; the walk stops there.
	in.Manifests["el9"] = latest("el9", map[string]string{"a": commits["a"], "b": commits["b"]})
	r = Make(in)
	want = "a@el9:skip:not-selected b@el9:current c@el9:build:new-package d@el9:skip:not-selected"
	if got := actions(r); !strings.HasPrefix(got, want) {
		t.Fatalf("actions\n got %s\nwant %s", got, want)
	}
}

func TestDepsAndRdeps(t *testing.T) {
	f := newFixture(t)
	in := f.input([]string{"el9"}, "b@el9")
	in.Manifests["el9"] = latest("el9", commits)
	in.Rdeps = true
	r := Make(in)
	validate(t, r)
	if !r.Selected["c@el9"] || r.Selected["a@el9"] || r.Selected["d@el9"] {
		t.Fatalf("--rdeps selected %v", r.Selected)
	}
	in.Rdeps, in.WithDeps = false, true
	r = Make(in)
	if !r.Selected["a@el9"] || r.Selected["c@el9"] {
		t.Fatalf("--with-deps selected %v", r.Selected)
	}
	if got := actions(r); got != "a@el9:current b@el9:current c@el9:skip:not-selected d@el9:skip:not-selected a@el10:skip:target-excluded b@el10:skip:target-excluded c@el10:skip:target-excluded" {
		t.Fatalf("actions %s", got)
	}
}

func TestForce(t *testing.T) {
	f := newFixture(t)
	in := f.input([]string{"el9"}, "b@el9")
	in.Manifests["el9"] = latest("el9", commits)
	in.Force = true
	r := Make(in)
	validate(t, r)
	b := r.Node("b@el9")
	if b.Action != api.PlanActionBuild || len(b.Reasons) != 1 || b.Reasons[0].Code != api.PlanReasonCodeForced ||
		b.PreviousCommit != commits["b"] || b.Snapshot != "" || b.Checks.Soname != "pending" {
		t.Fatalf("b@el9: %+v", b)
	}
	// The dependency a is current: it is needed, not forced.
	if a := r.Node("a@el9"); a.Action != api.PlanActionCurrent || !r.Needed["a@el9"] {
		t.Fatalf("a@el9: %+v", a)
	}
	if !r.Plan.Selection.Force {
		t.Fatal("selection.force")
	}
}

type fakeStaged map[string]StagedBuild

func (s fakeStaged) Find(target, pkg, commit string) (StagedBuild, bool) {
	b, ok := s[pkg+"@"+target+" "+commit]
	return b, ok
}

func TestStaged(t *testing.T) {
	f := newFixture(t)
	in := f.input([]string{"el9"})
	in.Staged = fakeStaged{
		"a@el9 " + commits["a"]: {Run: "r-20260901-0900-ab1", SRPM: "a-1.0-1.el9.src.rpm"},
		"b@el9 " + commits["b"]: {Run: "r-20260901-0900-ab1", SRPM: "b-1.0-1.el9.src.rpm", Missing: true},
	}
	r := Make(in)
	validate(t, r)
	if a := r.Node("a@el9"); a.Action != api.PlanActionStaged || a.FromRun != "r-20260901-0900-ab1" {
		t.Fatalf("a@el9: %+v", a)
	}
	b := r.Node("b@el9")
	if b.Action != api.PlanActionBuild || len(b.Reasons) != 2 || b.Reasons[1].Code != api.PlanReasonCodeStagingMissing {
		t.Fatalf("b@el9: %+v", b)
	}
	if len(r.Warnings) != 1 || r.Warnings[0].Code != api.PRunStagingMissing || r.Warnings[0].Node != "b@el9" {
		t.Fatalf("warnings %+v", r.Warnings)
	}
	if r.Plan.Summary.Staged != 1 {
		t.Fatalf("summary %+v", r.Plan.Summary)
	}
}

func TestUnpinned(t *testing.T) {
	f := newFixture(t)
	in := f.input([]string{"el9", "el10"})
	delete(in.Commits, "d")
	r := Make(in)
	validate(t, r)
	d := r.Node("d@el9")
	if d.Action != api.PlanActionSkip || d.SkipBecause != "unpinned" || d.Commit != "" {
		t.Fatalf("d@el9: %+v", d)
	}
	if len(r.Unpinned) != 1 || len(r.Warnings) != 1 || r.Warnings[0].Code != api.PLockMissingPackage {
		t.Fatalf("unpinned %v warnings %+v", r.Unpinned, r.Warnings)
	}
}

func TestNilStagedAndDescriber(t *testing.T) {
	f := newFixture(t)
	in := f.input([]string{"el9"})
	in.Staged, in.Describe = nil, nil
	in.Manifests["el9"] = latest("el9", map[string]string{"a": oldB})
	r := Make(in)
	if rs := r.Node("a@el9").Reasons; len(rs) != 1 || rs[0].Message != "a moved from 0123456 to aaaaaaa." {
		t.Fatalf("reasons %+v", rs)
	}
}

func TestSortedNodes(t *testing.T) {
	f := newFixture(t)
	r := Make(f.input([]string{"el9"}))
	var got []string
	for _, n := range SortedNodes(&r.Plan, f.graphs["el9"], "el9") {
		got = append(got, n.Node)
	}
	if strings.Join(got, " ") != "a@el9 b@el9 d@el9 c@el9" {
		t.Fatalf("order %v", got)
	}
}
