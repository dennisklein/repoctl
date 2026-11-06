package graph

import (
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
)

// storyInfos gives the infos of the story set on one target: the
// BuildRequires and the provides of the specs of internal/testkit.
func storyInfos(target string) []*Info {
	type pk struct {
		id   string
		brs  []string
		subs []string
	}
	pkgs := []pk{
		{"munge", nil, []string{"devel"}},
		{"ucx", nil, []string{"devel", "debuginfo"}},
		{"rocm-smi", nil, []string{"devel"}},
		{"pmix", []string{"munge-devel"}, []string{"devel"}},
		{"pmix_v5", []string{"munge-devel"}, []string{"devel"}},
		{"pmix_v4", []string{"munge-devel"}, []string{"devel"}},
		{"prrte", []string{"pmix-devel"}, []string{"devel"}},
		{"slurm", []string{"munge-devel", "ucx-devel", "rocm-smi-devel", "pmix-devel", "pmix_v5-devel", "pmix_v4-devel"}, []string{"devel"}},
		{"openmpi", []string{"ucx-devel", "pmix-devel", "prrte-devel"}, []string{"devel"}},
		{"slurm-singularity-exec", []string{"slurm-devel"}, []string{"devel"}},
		{"slurm-release", nil, nil},
	}
	var out []*Info
	for _, p := range pkgs {
		if p.id == "rocm-smi" && target != "el9" {
			continue
		}
		prov := []string{p.id, p.id + "(x86-64)"}
		for _, s := range p.subs {
			prov = append(prov, p.id+"-"+s, p.id+"-"+s+"(x86-64)")
		}
		out = append(out, &Info{Node: api.Node(p.id, target), Package: p.id, Target: target, Name: p.id,
			Requires: append([]string{"gcc", "make"}, p.brs...), Provides: prov, Spec: p.id + ".spec"})
	}
	return out
}

func TestStoryGraphs(t *testing.T) {
	el9 := storyInfos("el9")
	el10 := storyInfos("el10")
	g9, w9, err := New(Input{Target: "el9", RHEL: "9", Nodes: el9, Others: el10})
	if err != nil {
		t.Fatal(err)
	}
	if len(g9.Nodes) != 11 || len(g9.Edges) != 14 || g9.Layers != 4 {
		t.Fatalf("el9: %d nodes, %d edges, %d layers; want 11, 14, 4", len(g9.Nodes), len(g9.Edges), g9.Layers)
	}
	if len(w9) != 0 {
		t.Fatalf("el9 warnings: %v", w9)
	}
	var order []string
	for _, n := range g9.Nodes {
		order = append(order, n.Package)
	}
	want := "munge ucx rocm-smi slurm-release pmix pmix_v5 pmix_v4 prrte slurm openmpi slurm-singularity-exec"
	if got := strings.Join(order, " "); got != want {
		t.Fatalf("order %s, want %s", got, want)
	}
	if got := strings.Join(g9.ByPackage("slurm").DepPackages(), " "); got != "munge ucx rocm-smi pmix pmix_v5 pmix_v4" {
		t.Fatalf("slurm deps: %s", got)
	}
	if g9.ByPackage("slurm").Depth != 2 || g9.ByPackage("openmpi").Depth != 3 {
		t.Fatal("depths")
	}
	if got := g9.ByPackage("slurm").Unprovided; strings.Join(got, " ") != "gcc make" {
		t.Fatalf("unprovided %v", got)
	}

	g10, w10, err := New(Input{Target: "el10", RHEL: "10", Nodes: el10, Others: el9, NotBuilt: map[string]bool{"rocm-smi": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(g10.Nodes) != 10 || len(g10.Edges) != 13 || g10.Layers != 4 {
		t.Fatalf("el10: %d nodes, %d edges, %d layers; want 10, 13, 4", len(g10.Nodes), len(g10.Edges), g10.Layers)
	}
	if len(w10) != 1 || w10[0].Code != api.PDepNotForTarget || w10[0].Node != "slurm@el10" {
		t.Fatalf("el10 warnings: %+v", w10)
	}
	if w10[0].Message != "slurm@el10 needs rocm-smi-devel; only rocm-smi@el9 provides it" {
		t.Fatalf("message %q", w10[0].Message)
	}
	if w10[0].Hint != "guard the BuildRequires in slurm.spec with %if 0%{?rhel} < 10, or add el10 to the targets of rocm-smi" {
		t.Fatalf("hint %q", w10[0].Hint)
	}
	if len(g10.Unresolved) != 1 || g10.Unresolved[0].ProvidedOn[0] != "rocm-smi@el9" {
		t.Fatalf("unresolved %+v", g10.Unresolved)
	}
	j := g10.API()
	if j.Target != "el10" || len(j.Nodes) != 10 || len(j.Edges) != 13 {
		t.Fatalf("api %+v", j)
	}
}

func TestLanes(t *testing.T) {
	g, _, err := New(Input{Target: "el9", Nodes: storyInfos("el9")})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range Lanes(g, false) {
		s := r.Lanes
		if r.Node != nil {
			s += "*"
		}
		lines = append(lines, s)
	}
	want := []string{
		"*",
		"│ *",
		"│ │ *",
		"│ │ │ *",
		"│ │ │",
		"├─│─│─*",
		"├─│─│─│─*",
		"├─│─│─│─│─*",
		"│ │ │ │ │ │",
		"│ │ │ ├─│─│─*",
		"└─┼─┴─┼─┴─┴─│─*",
		"  │   │     │ │",
		"  └───┴─────┴─│─*",
		"              └─*",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lanes:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestCycle(t *testing.T) {
	nodes := []*Info{
		{Node: "a@el9", Package: "a", Target: "el9", Requires: []string{"b-devel"}, Provides: []string{"a", "a-devel"}},
		{Node: "b@el9", Package: "b", Target: "el9", Requires: []string{"c-devel"}, Provides: []string{"b", "b-devel"}},
		{Node: "c@el9", Package: "c", Target: "el9", Requires: []string{"a-devel"}, Provides: []string{"c", "c-devel"}},
		{Node: "d@el9", Package: "d", Target: "el9", Provides: []string{"d"}},
	}
	_, _, err := New(Input{Target: "el9", Nodes: nodes})
	e, ok := api.AsError(err)
	if !ok || e.Code != api.PGraphCycle || e.ExitCode != 1 {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(e.Message, "a@el9 → c@el9 → b@el9 → a@el9") && !strings.Contains(e.Message, "→") {
		t.Fatalf("message %q", e.Message)
	}
}

func TestWarnings(t *testing.T) {
	nodes := []*Info{
		{Node: "a@el9", Package: "a", Target: "el9", Provides: []string{"a", "libx.so.1()(64bit)"}},
		{Node: "b@el9", Package: "b", Target: "el9", Provides: []string{"b", "libx.so.1()(64bit)"}},
		{Node: "c@el9", Package: "c", Target: "el9", Requires: []string{"libx.so.1()(64bit)", "(a or z)", "c-self"}, Provides: []string{"c", "c-self"}, Dynamic: true},
		{Node: "f@el9", Package: "f", Target: "el9", Failed: true},
	}
	g, w, err := New(Input{Target: "el9", Nodes: nodes})
	if err != nil {
		t.Fatal(err)
	}
	codes := map[api.ProblemCode]int{}
	for _, p := range w {
		codes[p.Code]++
	}
	if codes[api.PGraphAmbiguousProvider] != 1 || codes[api.PGraphDynamicBuildrequires] != 1 {
		t.Fatalf("warnings %+v", w)
	}
	c := g.ByPackage("c")
	if got := strings.Join(c.DepPackages(), " "); got != "a b" {
		t.Fatalf("deps of c: %s", got)
	}
	if len(g.Dynamic) != 1 || g.Dynamic[0] != "c@el9" {
		t.Fatalf("dynamic %v", g.Dynamic)
	}
	if got := RequireNames("(foo-devel >= 1.2 or bar-devel)"); strings.Join(got, ",") != "foo-devel,bar-devel" {
		t.Fatalf("rich: %v", got)
	}
}
