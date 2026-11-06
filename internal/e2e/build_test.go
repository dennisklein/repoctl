//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/testkit"
)

// storyPkgs gives the packages of the story with the ids.
func storyPkgs(ids ...string) []*testkit.Pkg {
	var out []*testkit.Pkg
	for _, p := range testkit.StoryPackages() {
		if slices.Contains(ids, p.ID) {
			out = append(out, p)
		}
	}
	return out
}

func stateDir(s *testkit.Story) string {
	return filepath.Join(s.Home, ".local", "state", "repoctl", testkit.SetName)
}

func dataDir(s *testkit.Story) string {
	return filepath.Join(s.Home, ".local", "share", "repoctl", testkit.SetName)
}

// runOf gives the run item of a build envelope and its nodes by id.
func runOf(t *testing.T, env map[string]any) (map[string]any, map[string]map[string]any) {
	t.Helper()
	its := items(t, env)
	if len(its) != 1 {
		t.Fatalf("run: %d items", len(its))
	}
	nodes := map[string]map[string]any{}
	for _, n := range list(its[0], "nodes") {
		m := n.(map[string]any)
		nodes[m["node"].(string)] = m
	}
	return its[0], nodes
}

// journalOf validates the journal of a run against events.schema.json and
// gives its events.
func journalOf(t *testing.T, s *testkit.Story, run string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir(s), "runs", run+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	testkit.ValidateJournal(t, data)
	var out []map[string]any
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if seq := int(ev["seq"].(float64)); seq != i+1 {
			t.Fatalf("journal line %d has seq %d", i+1, seq)
		}
		out = append(out, ev)
	}
	return out
}

func eventsOf(evs []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func mustContain(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Fatalf("%s has no %q:\n%s", what, w, text)
		}
	}
}

// TestBuildTwoNodes builds two independent nodes, then plans them staged.
func TestBuildTwoNodes(t *testing.T) {
	needTools(t, "createrepo_c")
	s := testkit.NewStory(t, testkit.WithPackages(storyPkgs("munge", "ucx")...))

	env, r := s.RunJSON("build", "--target", "el9")
	if r.Code != 0 {
		t.Fatalf("build: exit %d\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	run, nodes := runOf(t, env)
	id := run["id"].(string)
	if run["state"] != "ok" || run["exitCode"] != nil && run["exitCode"].(float64) != 0 || len(nodes) != 2 {
		t.Fatalf("run: %s", r.Stdout)
	}
	for _, n := range []string{"munge@el9", "ucx@el9"} {
		if nodes[n]["state"] != "ok" {
			t.Fatalf("%s: %v", n, nodes[n])
		}
	}
	if pub := list(run, "publishable"); len(pub) != 1 || pub[0] != "el9" {
		t.Fatalf("publishable: %v", pub)
	}
	evs := journalOf(t, s, id)
	if evs[0]["type"] != "run.started" || evs[len(evs)-1]["type"] != "run.finished" {
		t.Fatalf("journal: first %v, last %v", evs[0]["type"], evs[len(evs)-1]["type"])
	}
	// One staging.updated per repository that a node changed: munge the
	// binary and source repositories, ucx also the debuginfo repository.
	if len(eventsOf(evs, "artifact.added")) == 0 || len(eventsOf(evs, "staging.updated")) != 5 {
		t.Fatalf("journal: %d artifact.added, %d staging.updated", len(eventsOf(evs, "artifact.added")), len(eventsOf(evs, "staging.updated")))
	}
	staging := filepath.Join(dataDir(s), "staging", id, "el9")
	for _, p := range []string{"slurm/repodata/repomd.xml", "slurm-source/repodata/repomd.xml", "slurm/buildroot"} {
		if _, err := os.Stat(filepath.Join(staging, p)); err != nil {
			t.Fatalf("staging: %v", err)
		}
	}
	rpms, _ := filepath.Glob(filepath.Join(staging, "slurm", "Packages", "*", "munge-0.5.18-*.rpm"))
	if len(rpms) == 0 {
		t.Fatalf("staging has no munge RPM")
	}
	if _, err := os.Stat(filepath.Join(stateDir(s), "logs", id, "munge@el9.log")); err != nil {
		t.Fatalf("node log: %v", err)
	}

	// The second build finds both nodes staged. It is a build of the MCP
	// server, with the hidden flags of the origin.
	r = s.MustRun("build", "--target", "el9", "--run-id", "r-20261106-1200-mcp", "--origin", "mcp",
		"--origin-session", "0123456789abcdef", "--origin-client", "claude-code")
	mustContain(t, "build stderr", r.Stderr, "0 to build, 2 staged", "Next: repoctl publish")
	env, _ = s.RunJSON("runs", "r-20261106-1200-mcp")
	if o := items(t, env)[0]["origin"].(map[string]any); o["via"] != "mcp" || o["session"] != "0123456789abcdef" || o["client"] != "claude-code" {
		t.Fatalf("origin: %v", o)
	}
	if r := s.Run("build", "--run-id", "r-20261106-1200-mcp"); r.Code != 2 {
		t.Fatalf("build with the id of a run: exit %d", r.Code)
	}
	s.Setenv("REPOCTL_ORIGIN", "ui")
	s.Setenv("REPOCTL_ORIGIN_SESSION", "fedcba9876543210")
	s.MustRun("build", "--target", "el9")
	env, _ = s.RunJSON("runs")
	ui := 0
	for _, it := range items(t, env) {
		if o := it["origin"].(map[string]any); o["via"] == "ui" && o["session"] == "fedcba9876543210" {
			ui++
		}
	}
	if ui != 1 {
		t.Fatalf("runs from the UI: %d\n%s", ui, r.Stdout)
	}
	s.Setenv("REPOCTL_ORIGIN", "")
	s.Setenv("REPOCTL_ORIGIN_SESSION", "")

	// runs, logs and status read the run.
	r = s.MustRun("runs")
	mustContain(t, "runs", r.Stdout, "RUN", id, "r-20261106-1200-mcp", "ok")
	if _, r = s.RunJSON("runs", id); r.Code != 0 {
		t.Fatalf("runs %s: exit %d\n%s", id, r.Code, r.Stderr)
	}
	r = s.MustRun("logs", id, "munge@el9")
	mustContain(t, "logs", r.Stdout, "munge@el9")
	if _, r = s.RunJSON("status"); r.Code != 0 {
		t.Fatalf("status: exit %d\n%s", r.Code, r.Stderr)
	}
	r = s.MustRun("status")
	mustContain(t, "status", r.Stdout, "set         slurm-26.05", "last run", "2 packages: ")
	if _, r = s.RunJSON("clean", "--staging", "--dry-run"); r.Code != 0 {
		t.Fatalf("clean --dry-run: exit %d\n%s", r.Code, r.Stderr)
	}
}

// TestBuildFailingNode fails pmix@el9 in step rpm and blocks its
// dependents; the other nodes are ok and staged.
func TestBuildFailingNode(t *testing.T) {
	needTools(t, "createrepo_c")
	s := testkit.NewStory(t, testkit.WithPackages(storyPkgs("munge", "pmix", "prrte", "ucx")...),
		testkit.Modify("pmix", func(p *testkit.Pkg) { p.Build = "false" }))

	env, r := s.RunJSON("build", "--target", "el9")
	if r.Code != 3 {
		t.Fatalf("build: exit %d, want 3\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	run, nodes := runOf(t, env)
	id := run["id"].(string)
	if run["state"] != "failed" {
		t.Fatalf("run state %v", run["state"])
	}
	pm := nodes["pmix@el9"]
	if pm["state"] != "failed" {
		t.Fatalf("pmix@el9: %v", pm)
	}
	if e := pm["error"].(map[string]any); e["step"] != "rpm" || e["class"] != "build" {
		t.Fatalf("pmix@el9 error: %v", e)
	}
	if nodes["prrte@el9"]["state"] != "blocked" || nodes["munge@el9"]["state"] != "ok" || nodes["ucx@el9"]["state"] != "ok" {
		t.Fatalf("nodes: %s", r.Stdout)
	}
	var codes []string
	for _, e := range list(env, "errors") {
		codes = append(codes, e.(map[string]any)["code"].(string))
	}
	if !slices.Equal(codes, []string{"node.failed", "node.blocked"}) {
		t.Fatalf("errors: %v", codes)
	}
	evs := journalOf(t, s, id)
	fin := eventsOf(evs, "run.finished")
	if len(fin) != 1 || fin[0]["state"] != "failed" || fin[0]["exitCode"].(float64) != 3 {
		t.Fatalf("run.finished: %v", fin)
	}

	// The human output: the report on stdout, the error and hints on
	// stderr.
	r = s.Run("build", "--target", "el9", "--progress", "plain")
	if r.Code != 3 {
		t.Fatalf("build again: exit %d\n%s", r.Code, r.Stderr)
	}
	mustContain(t, "build stdout", r.Stdout, "✗ pmix@el9 failed in step rpm", "staging  ~/.local/share/repoctl/slurm-26.05/staging/")
	mustContain(t, "build stderr", r.Stderr, "2 to build, 2 staged", "repoctl: ✗ build: failed in", "1 failed, 1 blocked, 2 staged",
		"repoctl: error: pmix@el9:", "hint: read the log. run: repoctl logs pmix@el9")

	r = s.Run("logs", id, "pmix@el9")
	if r.Code != 0 {
		t.Fatalf("logs: exit %d\n%s", r.Code, r.Stderr)
	}
	mustContain(t, "logs", r.Stdout, "── repoctl ── rpm failed: Bad exit status", "── repoctl ── blocked: prrte@el9")
	r = s.Run("logs", id, "pmix@el9", "--file", "root")
	if r.Code != 0 {
		t.Fatalf("logs --file root: exit %d\n%s", r.Code, r.Stderr)
	}
}

// TestBuildStory builds the 21 nodes of the story: slurm@el10 fails in
// builddep (rocm-smi is built for el9 only) and blocks
// slurm-singularity-exec@el10 (build.html#story).
func TestBuildStory(t *testing.T) {
	needTools(t, "createrepo_c")
	s := testkit.NewStory(t)

	r := s.Run("build", "--progress", "plain")
	if r.Code != 3 {
		t.Fatalf("build: exit %d, want 3\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	mustContain(t, "build stdout", r.Stdout,
		"✗ slurm@el10 failed in step rpm after",
		"  root.log  No matching package to install: 'rocm-smi-devel'",
		"(el9 complete)")
	mustContain(t, "build stderr", r.Stderr,
		"21 to build, 2 at a time",
		"repoctl: ✗ build: failed in", "19 ok, 1 failed, 1 blocked",
		"repoctl: error: slurm@el10: Failed build dependencies: rocm-smi-devel is needed",
		"hint: read the log. run: repoctl logs slurm@el10",
		"hint: rocm-smi builds for el9 only: guard its BuildRequires in slurm.spec with %if 0%{?rhel} < 10, or build it for el10",
		"hint: el9 is publishable now. run: repoctl publish --target el9")
	if strings.Contains(r.Stderr, "warning: slurm@el10 needs rocm-smi-devel") {
		t.Fatalf("the %%if hint repeats the warning:\n%s", r.Stderr)
	}

	env, r := s.RunJSON("runs", "latest")
	if r.Code != 0 {
		t.Fatalf("runs latest: exit %d\n%s", r.Code, r.Stderr)
	}
	run, nodes := runOf(t, env)
	if len(nodes) != 21 {
		t.Fatalf("run has %d nodes", len(nodes))
	}
	sl := nodes["slurm@el10"]
	e := sl["error"].(map[string]any)
	if sl["state"] != "failed" || e["class"] != "builddep" || e["step"] != "rpm" || e["exitCode"].(float64) != 30 {
		t.Fatalf("slurm@el10: %v", sl)
	}
	if m := list(e, "missing"); len(m) != 1 || m[0] != "rocm-smi-devel" {
		t.Fatalf("missing: %v", m)
	}
	if n := nodes["slurm-singularity-exec@el10"]; n["state"] != "blocked" || list(n, "blockedBy")[0] != "slurm@el10" {
		t.Fatalf("slurm-singularity-exec@el10: %v", n)
	}
	ok := 0
	for _, n := range nodes {
		if n["state"] == "ok" {
			ok++
		}
	}
	if ok != 19 {
		t.Fatalf("%d ok nodes", ok)
	}
	evs := journalOf(t, s, run["id"].(string))
	ready := eventsOf(evs, "plan.ready")
	if len(ready) != 1 || len(list(ready[0], "build")) != 21 {
		t.Fatalf("plan.ready: %v", ready)
	}

	// The next plan: the ok nodes are staged; slurm and
	// slurm-singularity-exec on el10 build.
	env, r = s.RunJSON("plan")
	_, pn := planNodes(t, env)
	for id, n := range pn {
		want := "staged"
		if id == "slurm@el10" || id == "slurm-singularity-exec@el10" {
			want = "build"
		}
		if n["action"] != want {
			t.Fatalf("plan %s: %v, want %s", id, n["action"], want)
		}
	}
}

// TestBuildCancel cancels a running build with repoctl cancel: exit 130,
// the run canceled.
func TestBuildCancel(t *testing.T) {
	needTools(t, "createrepo_c")
	s := testkit.NewStory(t, testkit.WithPackages(storyPkgs("munge")...),
		testkit.Modify("munge", func(p *testkit.Pkg) { p.Build = "sleep 120" }))

	cmd := exec.Command(filepath.Join(s.Bin, "repoctl"), "build", "--target", "el9", "--progress", "none")
	cmd.Dir = s.SetDir
	cmd.Env = s.Env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Wait for step rpm of munge@el9.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("munge@el9 never ran step rpm\n%s", stderr.String())
		}
		files, _ := filepath.Glob(filepath.Join(stateDir(s), "runs", "r-*.jsonl"))
		started := false
		for _, f := range files {
			if strings.HasSuffix(f, ".progress.jsonl") {
				continue
			}
			b, _ := os.ReadFile(f)
			started = started || bytes.Contains(b, []byte(`"step":"rpm","phase":"started"`))
		}
		if started {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	// runs --follow draws the progress log of the run and ends with it.
	follow := exec.Command(filepath.Join(s.Bin, "repoctl"), "runs", "latest", "--follow", "--progress", "plain")
	follow.Dir = s.SetDir
	follow.Env = s.Env()
	var fout, ferr bytes.Buffer
	follow.Stdout, follow.Stderr = &fout, &ferr
	if err := follow.Start(); err != nil {
		t.Fatal(err)
	}
	followed := make(chan error, 1)
	go func() { followed <- follow.Wait() }()
	time.Sleep(time.Second)

	r := s.Run("cancel")
	if r.Code != 0 {
		t.Fatalf("cancel: exit %d\n%s", r.Code, r.Stderr)
	}
	select {
	case <-followed:
	case <-time.After(90 * time.Second):
		_ = follow.Process.Kill()
		t.Fatalf("runs --follow did not end\n%s", ferr.String())
	}
	if c := follow.ProcessState.ExitCode(); c != 130 {
		t.Fatalf("runs --follow: exit %d, want 130\n%s\n%s", c, fout.String(), ferr.String())
	}
	mustContain(t, "runs --follow stderr", ferr.String(), "build › el9: start, 1 node", "build › el9: canceled in", "repoctl: error: run r-")
	mustContain(t, "runs --follow stdout", fout.String(), "munge@el9")
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("build did not end after cancel\n%s", stderr.String())
	}
	if c := cmd.ProcessState.ExitCode(); c != 130 {
		t.Fatalf("build: exit %d, want 130\n%s\n%s", c, stdout.String(), stderr.String())
	}
	env, r := s.RunJSON("runs", "latest")
	run, nodes := runOf(t, env)
	if run["state"] != "canceled" || nodes["munge@el9"]["state"] != "canceled" {
		t.Fatalf("run: %s", r.Stdout)
	}
	evs := journalOf(t, s, run["id"].(string))
	if len(eventsOf(evs, "run.cancel-requested")) != 1 {
		t.Fatalf("journal has no run.cancel-requested")
	}
}

// TestBuildThenPublish hands a build run to publish: repoctl build stages
// the nodes of el9, and repoctl publish takes them from the journal and
// the staging (publish.JournalStager).
func TestBuildThenPublish(t *testing.T) {
	s := publishStory(t)
	r := s.MustRun("build", "--target", "el9")
	mustContain(t, "build stderr", r.Stderr, "2 to build", "Next: repoctl publish")
	env, _ := s.RunJSON("runs", "latest")
	run, _ := runOf(t, env)
	id := run["id"].(string)

	r = s.MustRun("publish", "--target", "el9", "--dry-run")
	mustContain(t, "publish --dry-run", r.Stdout, "el9 of slurm-26.05", "hello", "slurm-release", "dry run", id)

	s.Setenv(publishUnattended, "1")
	r = s.MustRun("publish", "--target", "el9", "--yes")
	env, _ = s.RunJSON("plan", "--target", "el9")
	_, nodes := planNodes(t, env)
	for _, n := range []string{"hello@el9", "slurm-release@el9"} {
		if nodes[n]["action"] != "current" {
			t.Fatalf("after the publish, %s is %v\n%s", n, nodes[n]["action"], r.Stderr)
		}
	}
	if r := s.Run("repo", "verify", "--target", "el9"); r.Code != 0 {
		t.Fatalf("repo verify: exit %d\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	_, r = s.RunJSON("repo", "provenance", "hello", "--target", "el9")
	mustContain(t, "repo provenance", r.Stdout, id, mockEL9)
}
