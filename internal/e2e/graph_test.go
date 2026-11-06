//go:build e2e

// Package e2e drives the repoctl binary through the story with the fake
// mock (docs/design/delivery.html#tests).
package e2e

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/testkit"
)

func needTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range append([]string{"rpmbuild", "rpmspec", "rpm"}, tools...) {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
}

// mockCalls counts the calls of the fake mock with an argument.
func mockCalls(t *testing.T, s *testkit.Story, arg string) int {
	t.Helper()
	b, err := os.ReadFile(s.MockLog)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var call struct{ Args []string }
		if line == "" || json.Unmarshal([]byte(line), &call) != nil {
			continue
		}
		for _, a := range call.Args {
			if a == arg {
				n++
				break
			}
		}
	}
	return n
}

// items gives the items of an envelope.
func items(t *testing.T, env map[string]any) []map[string]any {
	t.Helper()
	raw, _ := env["items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("item %v", it)
		}
		out = append(out, m)
	}
	return out
}

func list(m map[string]any, key string) []any {
	l, _ := m[key].([]any)
	return l
}

// planNodes gives the nodes of the plan of an envelope by node id.
func planNodes(t *testing.T, env map[string]any) (map[string]any, map[string]map[string]any) {
	t.Helper()
	its := items(t, env)
	if len(its) != 1 {
		t.Fatalf("plan: %d items", len(its))
	}
	nodes := map[string]map[string]any{}
	for _, n := range list(its[0], "nodes") {
		m := n.(map[string]any)
		nodes[m["node"].(string)] = m
	}
	return its[0], nodes
}

func reasonCodes(n map[string]any) []string {
	var out []string
	for _, r := range list(n, "reasons") {
		out = append(out, r.(map[string]any)["code"].(string))
	}
	return out
}

func TestGraph(t *testing.T) {
	needTools(t)
	s := testkit.NewStory(t)

	r := s.MustRun("graph", "--target", "el9")
	if !strings.Contains(r.Stdout, "el9  rocky+epel-9-x86_64  11 nodes, 14 edges, 4 layers") {
		t.Fatalf("graph el9:\n%s", r.Stdout)
	}
	for _, want := range []string{"munge", "slurm-singularity-exec", "├"} {
		if !strings.Contains(r.Stdout, want) {
			t.Fatalf("graph el9 has no %q:\n%s", want, r.Stdout)
		}
	}
	if strings.Contains(r.Stderr, "warning:") {
		t.Fatalf("graph el9 warns:\n%s", r.Stderr)
	}

	r = s.MustRun("graph", "--target", "el10")
	if !strings.Contains(r.Stdout, "10 nodes, 13 edges, 4 layers") || strings.Contains(r.Stdout, "rocm-smi") {
		t.Fatalf("graph el10:\n%s", r.Stdout)
	}
	for _, want := range []string{
		"warning: slurm@el10 needs rocm-smi-devel; only rocm-smi@el9 provides it",
		"hint: guard the BuildRequires in slurm.spec with %if 0%{?rhel} < 10, or add el10 to the targets of rocm-smi",
	} {
		if !strings.Contains(r.Stderr, want) {
			t.Fatalf("graph el10 stderr has no %q:\n%s", want, r.Stderr)
		}
	}

	env, r := s.RunJSON("graph")
	if r.Code != 0 {
		t.Fatalf("graph --json: exit %d\n%s", r.Code, r.Stderr)
	}
	gs := items(t, env)
	if len(gs) != 2 || len(list(gs[0], "nodes")) != 11 || len(list(gs[0], "edges")) != 14 ||
		len(list(gs[1], "nodes")) != 10 || len(list(gs[1], "edges")) != 13 {
		t.Fatalf("graph --json: %s", r.Stdout)
	}
	unresolved := list(gs[1], "unresolved")
	if len(unresolved) != 1 || unresolved[0].(map[string]any)["node"] != "slurm@el10" {
		t.Fatalf("el10 unresolved: %v", unresolved)
	}
	var codes []string
	for _, w := range list(env, "warnings") {
		codes = append(codes, w.(map[string]any)["code"].(string))
	}
	if strings.Join(codes, " ") != api.PDepNotForTarget {
		t.Fatalf("warnings %v", codes)
	}

	r = s.MustRun("graph", "--format", "dot", "--target", "el9")
	if !strings.HasPrefix(r.Stdout, `digraph "slurm-26.05" {`) || strings.Count(r.Stdout, " -> ") != 14 {
		t.Fatalf("dot:\n%s", r.Stdout)
	}
	r = s.MustRun("graph", "--format", "matrix")
	if !strings.Contains(r.Stdout, "rocm-smi") {
		t.Fatalf("matrix:\n%s", r.Stdout)
	}

	// Selectors give the subgraph.
	r = s.MustRun("graph", "--target", "el9", "--with-deps", "prrte")
	if !strings.Contains(r.Stdout, "3 nodes, 2 edges") {
		t.Fatalf("graph --with-deps prrte:\n%s", r.Stdout)
	}
}

func TestPlanNewPackages(t *testing.T) {
	needTools(t)
	s := testkit.NewStory(t)

	env, r := s.RunJSON("plan")
	if r.Code != 0 {
		t.Fatalf("plan --json: exit %d\n%s", r.Code, r.Stderr)
	}
	p, nodes := planNodes(t, env)
	sum := p["summary"].(map[string]any)
	by := sum["byTarget"].(map[string]any)
	if sum["build"] != 21.0 || by["el9"].(map[string]any)["build"] != 11.0 || by["el10"].(map[string]any)["build"] != 10.0 {
		t.Fatalf("summary %v", sum)
	}
	if p["baseline"] != "none" || len(nodes) != 21 {
		t.Fatalf("plan %v", p)
	}
	for id, n := range nodes {
		if n["action"] != "build" || strings.Join(reasonCodes(n), ",") != "new-package" {
			t.Fatalf("%s: %v", id, n)
		}
	}
	if deps := list(nodes["prrte@el9"], "deps"); len(deps) != 1 || deps[0] != "pmix@el9" {
		t.Fatalf("prrte@el9 deps %v", deps)
	}
	built := mockCalls(t, s, "--buildsrpm")
	if built != 21 {
		t.Fatalf("first plan made %d SRPMs, want 21", built)
	}

	// The second plan takes the SRPMs from the cache.
	r = s.MustRun("plan")
	if n := mockCalls(t, s, "--buildsrpm"); n != built {
		t.Fatalf("second plan called mock --buildsrpm %d times", n-built)
	}
	for _, want := range []string{"plan  slurm-26.05  from repoctl.lock", "new-package", "21 nodes: 21 build"} {
		if !strings.Contains(r.Stdout, want) {
			t.Fatalf("plan has no %q:\n%s", want, r.Stdout)
		}
	}
	if !strings.Contains(r.Stdout, "slurm@el10 needs rocm-smi-devel") {
		t.Fatalf("plan has no row warning:\n%s", r.Stdout)
	}

	r = s.Run("plan", "--check")
	if r.Code != api.ExitDifferences {
		t.Fatalf("plan --check: exit %d, want %d\n%s", r.Code, api.ExitDifferences, r.Stderr)
	}
	env, r = s.RunJSON("plan", "--check")
	if r.Code != api.ExitDifferences || len(list(env, "errors")) != 0 {
		t.Fatalf("plan --check --json: exit %d\n%s", r.Code, r.Stdout)
	}

	// One package and its dependencies.
	env, _ = s.RunJSON("plan", "--target", "el9", "prrte")
	_, nodes = planNodes(t, env)
	for id, want := range map[string]string{"prrte@el9": "build", "pmix@el9": "build", "munge@el9": "build", "slurm@el9": "skip", "prrte@el10": "skip"} {
		if nodes[id]["action"] != want {
			t.Fatalf("plan prrte: %s is %v, want %s", id, nodes[id]["action"], want)
		}
	}
}

// writeLatest writes the local copy of latest of a target: an empty rpm-md
// repository with a RepoManifest of these builds.
func writeLatest(t *testing.T, s *testkit.Story, target string, builds []manifest.Build) {
	t.Helper()
	dir := filepath.Join(s.Home, ".local", "share", "repoctl", testkit.SetName, "repo", target, "slurm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := manifest.New(testkit.SetName, target)
	m.Publish = "p-20260901-0900-ab1"
	m.Builds = builds
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// modifyrepo_c can fail to detect the compression of a plain JSON
	// file (libmagic); a gzip file it knows.
	mf := filepath.Join(t.TempDir(), "repoctl.json.gz")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mf, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"createrepo_c", "--quiet", dir},
		{"modifyrepo_c", "--mdtype=" + manifest.MDType, mf, filepath.Join(dir, "repodata")},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
}

func TestPlanCommitChanged(t *testing.T) {
	needTools(t, "createrepo_c", "modifyrepo_c")
	s := testkit.NewStory(t)
	commits := map[string]string{}
	for _, p := range s.Pkgs {
		if p.Path == "" {
			commits[p.ID] = s.BranchCommit(p.ID)
		}
	}
	old := commits["ucx"]
	bumped := s.Bump("ucx", "1.22.1")
	lockFile := filepath.Join(s.SetDir, "repoctl.lock")
	b, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockFile, []byte(strings.ReplaceAll(string(b), old, bumped)), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Git(s.SetDir, "commit", "-qam", "Pin ucx 1.22.1")

	// Without latest, the bump is a new package like the others.
	env, _ := s.RunJSON("plan", "--target", "el9")
	_, nodes := planNodes(t, env)
	if got := strings.Join(reasonCodes(nodes["ucx@el9"]), ","); got != "new-package" {
		t.Fatalf("ucx@el9 without latest: %s", got)
	}

	// latest of el9 has the old commit of ucx and the commits of the others.
	var builds []manifest.Build
	for _, p := range s.Pkgs {
		c, ok := commits[p.ID]
		if !ok {
			continue
		}
		if len(p.Targets) > 0 && p.Targets[0] != "el9" {
			continue
		}
		name := p.Name
		if name == "" {
			name = p.ID
		}
		nvr := name + "-" + p.Version + "-1.el9"
		if p.ID == "ucx" {
			nvr = "ucx-1.22.0-1.el9"
		}
		builds = append(builds, manifest.Build{Package: p.ID, Branch: p.Branch, Commit: c, Reason: "new-package",
			SRPM: nvr + ".src.rpm", RPMs: []string{nvr + ".x86_64.rpm"}})
	}
	writeLatest(t, s, "el9", builds)

	env, r := s.RunJSON("plan")
	if r.Code != 0 {
		t.Fatalf("plan: exit %d\n%s", r.Code, r.Stderr)
	}
	p, nodes := planNodes(t, env)
	if p["baseline"] != "latest" {
		t.Fatalf("baseline %v", p["baseline"])
	}
	ucx := nodes["ucx@el9"]
	if ucx["action"] != "build" || strings.Join(reasonCodes(ucx), ",") != "commit-changed" || ucx["previousCommit"] != old {
		t.Fatalf("ucx@el9: %v", ucx)
	}
	reason := list(ucx, "reasons")[0].(map[string]any)
	want := "ucx moved from " + old[:7] + " to " + bumped[:7] + " (1 commit: Update to 1.22.1)."
	if reason["message"] != want || reason["from"] != old || reason["to"] != bumped {
		t.Fatalf("reason %v, want message %q", reason, want)
	}
	for id, want := range map[string]string{
		"munge@el9": "current", "slurm@el9": "current", "slurm-release@el9": "build",
		"ucx@el10": "build", "munge@el10": "build",
	} {
		if nodes[id]["action"] != want {
			t.Fatalf("%s is %v, want %s", id, nodes[id]["action"], want)
		}
	}
	if got := strings.Join(reasonCodes(nodes["ucx@el10"]), ","); got != "new-target" {
		t.Fatalf("ucx@el10: %s", got)
	}
	if nodes["munge@el9"]["snapshot"] != "p-20260901-0900-ab1" {
		t.Fatalf("munge@el9 snapshot %v", nodes["munge@el9"]["snapshot"])
	}

	r = s.MustRun("plan", "--target", "el9")
	if !strings.Contains(r.Stdout, "commit-changed "+old[:7]+" → "+bumped[:7]) || !strings.Contains(r.Stdout, "11 nodes: 2 build, 9 current") {
		t.Fatalf("plan:\n%s", r.Stdout)
	}
}

func TestListShow(t *testing.T) {
	needTools(t)
	s := testkit.NewStory(t)

	r := s.MustRun("list")
	for _, want := range []string{"PKG", "slurm-singularity-exec", "rocm-smi"} {
		if !strings.Contains(r.Stdout, want) {
			t.Fatalf("list has no %q:\n%s", want, r.Stdout)
		}
	}
	r = s.MustRun("list", "--buildrequires", "--provides")
	// BUILDREQUIRES, then the binary RPM names under PROVIDES.
	if !strings.Contains(r.Stdout, "BUILDREQUIRES") || !strings.Contains(r.Stdout, "ucx-devel  ") || !strings.Contains(r.Stdout, "ucx ucx-devel ucx-debuginfo") {
		t.Fatalf("list --buildrequires --provides:\n%s", r.Stdout)
	}
	env, r := s.RunJSON("list")
	if r.Code != 0 || len(items(t, env)) != 11 {
		t.Fatalf("list --json: exit %d\n%s", r.Code, r.Stdout)
	}

	r = s.MustRun("show", "slurm")
	for _, want := range []string{"package", "slurm · branch slurm/slurm-26.05", "el9", "el10", "build"} {
		if !strings.Contains(r.Stdout, want) {
			t.Fatalf("show slurm has no %q:\n%s", want, r.Stdout)
		}
	}
	if !strings.Contains(r.Stdout+r.Stderr, "rocm-smi-devel") {
		t.Fatalf("show slurm has no not-for-target warning:\n%s\n%s", r.Stdout, r.Stderr)
	}
	env, r = s.RunJSON("show", "slurm")
	if r.Code != 0 || len(items(t, env)) != 1 {
		t.Fatalf("show --json: exit %d\n%s", r.Code, r.Stdout)
	}
	if r := s.Run("show"); r.Code != api.ExitUsage {
		t.Fatalf("show without a package: exit %d", r.Code)
	}
}
