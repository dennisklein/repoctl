//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// servedSite is the HTTP server of repo.url: it answers the files that a
// test puts, else 404, as the served trees of the mirror job.
type servedSite struct {
	mu    sync.Mutex
	files map[string]string
	srv   *httptest.Server
}

func newServedSite(t *testing.T) *servedSite {
	t.Helper()
	s := &servedSite{files: map[string]string{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		text, ok := s.files[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(text))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// put serves v as JSON at /slurm-26.05/PATH.
func (s *servedSite) put(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.files["/slurm-26.05/"+path] = string(b)
	s.mu.Unlock()
}

var reRepoURL = regexp.MustCompile(`(?m)^  url: .*$`)

// setRepoURL points repo.url (and, when oci is not "", store.oci) of the
// story to new values, in a pushed set commit.
func setRepoURL(t *testing.T, s *testkit.Story, url, oci string) {
	t.Helper()
	p := filepath.Join(s.SetDir, "repoctl.yaml")
	yml := reRepoURL.ReplaceAllString(readFile(t, p), "  url: "+url)
	if oci != "" {
		if i := strings.Index(yml, "\nstore:\n"); i >= 0 {
			yml = yml[:i]
		}
		yml += "\nstore:\n  oci: " + oci + "\n"
	}
	writeFile(t, p, yml)
	s.Git(s.SetDir, "commit", "-qam", "Move the registry and the served tree")
	s.Git(s.SetDir, "push", "-q", "origin", testkit.SetBranch)
}

// publishedStory is the publish story with repo.url on a served site and
// el9 built and published.
func publishedStory(t *testing.T) (*testkit.Story, *servedSite) {
	t.Helper()
	s := publishStory(t)
	site := newServedSite(t)
	setRepoURL(t, s, site.srv.URL+"/slurm-26.05", "")
	s.MustRun("build", "--target", "el9")
	s.Setenv(publishUnattended, "1")
	s.MustRun("publish", "--target", "el9", "--yes")
	return s, site
}

// localSnapshot reads snapshot.json of the local copy of el9.
func localSnapshot(t *testing.T) map[string]any {
	t.Helper()
	paths := config.NewPaths(os.Getenv)
	var m map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(paths.LocalCopy(testkit.SetName, "el9"), "snapshot.json"))), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// openSteps gives the step texts of Status.open.
func openSteps(t *testing.T, env map[string]any) []string {
	t.Helper()
	var out []string
	for _, o := range list(items(t, env)[0], "open") {
		out = append(out, o.(map[string]any)["step"].(string))
	}
	return out
}

func hasPrefixIn(xs []string, prefix string) bool {
	for _, x := range xs {
		if strings.HasPrefix(x, prefix) {
			return true
		}
	}
	return false
}

func warningCodes(env map[string]any) []string {
	var out []string
	for _, w := range list(env, "warnings") {
		out = append(out, w.(map[string]any)["code"].(string))
	}
	return out
}

func hasCode(codes []string, code string) bool {
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}

// TestFreshnessBehind: plan and build compare latest with the local copy
// (publish.html#freshness). Without a local copy, latest is newer: they
// stop with state.behind (exit 13) and the hint repo pull; build ends its
// run failed with exit 13 and no plan.ready. A local publish that is not on
// the chain of latest also warns snapshot.orphaned.
func TestFreshnessBehind(t *testing.T) {
	s, _ := publishedStory(t)
	r := s.MustRun("plan", "--target", "el9")
	t.Logf("%s\n%s", r.Stdout, r.Stderr)
	mustContain(t, "plan", r.Stdout, "(local copy same)")

	paths := config.NewPaths(os.Getenv)
	local := paths.LocalCopy(testkit.SetName, "el9")
	if err := os.RemoveAll(local); err != nil {
		t.Fatal(err)
	}
	r = s.Run("plan", "--target", "el9")
	if r.Code != 13 {
		t.Fatalf("plan without a local copy: exit %d, want 13\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	mustContain(t, "plan stderr", r.Stderr, "repoctl: error: latest of el9 is p-", "no local copy", "hint: run: repoctl repo pull --target el9")
	if code, p := problem(s, "plan", "--target", "el9"); code != 13 || p != "state.behind" {
		t.Fatalf("plan --json: exit %d %s", code, p)
	}

	env, r := s.RunJSON("build", "--target", "el9")
	if r.Code != 13 {
		t.Fatalf("build without a local copy: exit %d, want 13\n%s", r.Code, r.Stderr)
	}
	run, _ := runOf(t, env)
	if run["state"] != "failed" || run["exitCode"].(float64) != 13 {
		t.Fatalf("run: %v", run)
	}
	evs := journalOf(t, s, run["id"].(string))
	if len(eventsOf(evs, "plan.ready")) != 0 || evs[len(evs)-1]["type"] != "run.finished" || evs[len(evs)-1]["exitCode"].(float64) != 13 {
		t.Fatalf("journal of a behind build: %v", evs)
	}

	// repo pull makes the local copy equal to latest: plan works again.
	s.MustRun("repo", "pull", "--target", "el9")
	if r := s.Run("plan", "--target", "el9"); r.Code != 0 {
		t.Fatalf("plan after the pull: exit %d\n%s", r.Code, r.Stderr)
	}

	// A local publish of this user that is not on the chain of latest.
	snap := localSnapshot(t)
	snap["digest"] = "sha256:" + strings.Repeat("e", 64)
	b, _ := json.Marshal(snap)
	writeFile(t, filepath.Join(local, "snapshot.json"), string(b))
	env, r = s.RunJSON("plan", "--target", "el9")
	if r.Code != 13 || !hasCode(warningCodes(env), "snapshot.orphaned") {
		t.Fatalf("orphaned: exit %d, warnings %v\n%s", r.Code, warningCodes(env), r.Stderr)
	}
	// status shows the local copy behind and gives the pull as next step.
	env, r = s.RunJSON("status")
	if r.Code != 0 {
		t.Fatalf("status: exit %d\n%s", r.Code, r.Stderr)
	}
	st := items(t, env)[0]
	el9 := list(st, "snapshots")[0].(map[string]any)
	if el9["local"] != "behind" || !strings.HasPrefix(list(st, "next")[0].(map[string]any)["command"].(string), "repoctl repo pull") {
		t.Fatalf("status: %v next %v", el9, st["next"])
	}
}

// TestFreshnessUnreachable: plan and build work offline. A registry that
// does not answer gives the warning registry.unreachable in the envelope
// and in the journal, and they use the local copy.
func TestFreshnessUnreachable(t *testing.T) {
	needTools(t, "createrepo_c")
	s := testkit.NewStory(t, testkit.WithPackages(storyPkgs("munge")...))
	dead := httptest.NewServer(http.NotFoundHandler())
	host := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	site := newServedSite(t)
	setRepoURL(t, s, site.srv.URL+"/slurm-26.05", host+"/gsi-hpc")

	env, r := s.RunJSON("plan", "--target", "el9")
	if r.Code != 0 || !hasCode(warningCodes(env), "registry.unreachable") {
		t.Fatalf("plan: exit %d, warnings %v\n%s", r.Code, warningCodes(env), r.Stderr)
	}
	r = s.MustRun("plan", "--target", "el9")
	t.Logf("%s\n%s", r.Stdout, r.Stderr)
	mustContain(t, "plan", r.Stdout, "registry unknown")
	mustContain(t, "plan stderr", r.Stderr, "warning: the registry does not answer for el9", "repoctl uses the local copy")

	env, r = s.RunJSON("build", "--target", "el9")
	if r.Code != 0 || !hasCode(warningCodes(env), "registry.unreachable") {
		t.Fatalf("build: exit %d, warnings %v\n%s", r.Code, warningCodes(env), r.Stderr)
	}
	run, _ := runOf(t, env)
	found := false
	for _, w := range eventsOf(journalOf(t, s, run["id"].(string)), "warning") {
		found = found || w["code"] == "registry.unreachable"
	}
	if !found || run["state"] != "ok" {
		t.Fatalf("build: state %v, warning in the journal %v", run["state"], found)
	}

	// status: registry unknown, and the warning.
	env, r = s.RunJSON("status")
	if r.Code != 0 || !hasCode(warningCodes(env), "registry.unreachable") {
		t.Fatalf("status: exit %d, warnings %v", r.Code, warningCodes(env))
	}
	r = s.MustRun("status")
	t.Logf("%s\n%s", r.Stdout, r.Stderr)
	mustContain(t, "status", r.Stdout, "el9", "registry unknown")
}

// TestStatusSnapshots: status shows latest, the local copy and the served
// tree, and reads the heartbeat of the mirror line: served behind, then
// served.stale after 15 minutes, which doctor shows too, then same.
func TestStatusSnapshots(t *testing.T) {
	s, site := publishedStory(t)
	snap := localSnapshot(t)
	latest := snap["digest"].(string)
	pub := snap["publish"].(string)

	statusOf := func() (map[string]any, map[string]any, []string) {
		t.Helper()
		env, r := s.RunJSON("status")
		if r.Code != 0 {
			t.Fatalf("status: exit %d\n%s", r.Code, r.Stderr)
		}
		st := items(t, env)[0]
		snaps := list(st, "snapshots")
		if len(snaps) != 2 {
			t.Fatalf("snapshots: %v", snaps)
		}
		return snaps[0].(map[string]any), snaps[1].(map[string]any), warningCodes(env)
	}

	// No served tree yet.
	el9, el10, _ := statusOf()
	l9, _ := el9["latest"].(map[string]any)
	if l9 == nil || l9["publish"] != pub || l9["digest"] != latest || l9["user"] == "" || el9["local"] != "same" || el9["served"] != "unknown" {
		t.Fatalf("el9: %v", el9)
	}
	if el10["latest"] != nil || el10["local"] != "none" {
		t.Fatalf("el10: %v", el10)
	}
	r := s.MustRun("status")
	t.Logf("%s\n%s", r.Stdout, r.Stderr)
	mustContain(t, "status", r.Stdout, "published   el9   "+pub, "local same, served unknown", "el10  not published")
	// The open setup steps (lifecycle.html#setup): el10 is not published,
	// repo.url serves nothing, the check timer is off.
	env, _ := s.RunJSON("status")
	open := openSteps(t, env)
	for _, want := range []string{"First publish: el10 has no latest", "Served: repo.url serves no tree of el9", "Check timer: "} {
		if !hasPrefixIn(open, want) {
			t.Errorf("open setup steps %v: no %q", open, want)
		}
	}
	mustContain(t, "status", r.Stdout, "setup       first publish: el10 has no latest", "served: repo.url serves no tree of el9")

	// The served tree is the old snapshot; the job saw latest 2 min ago.
	now := time.Now().UTC()
	old := "sha256:" + strings.Repeat("c", 64)
	site.put(t, "el9/repoctl-snapshot.json", map[string]any{"set": testkit.SetName, "target": "el9", "digest": old, "publish": "p-20260912-1403-c21", "pulled": now.Add(-time.Hour).Format(time.RFC3339)})
	since := now.Add(-2 * time.Minute).Format(time.RFC3339)
	site.put(t, "el9/repoctl-mirror.json", map[string]any{"set": testkit.SetName, "target": "el9", "checked": now.Add(-time.Minute).Format(time.RFC3339),
		"result": "failed", "served": old, "latest": latest, "behindSince": since, "error": "gpgv: BAD signature"})
	el9, _, warns := statusOf()
	m, _ := el9["mirror"].(map[string]any)
	if el9["served"] != "behind" || m == nil || m["result"] != "failed" || m["stale"] == true || hasCode(warns, "served.stale") {
		t.Fatalf("served behind: %v %v", el9, warns)
	}
	r = s.MustRun("status")
	t.Logf("%s\n%s", r.Stdout, r.Stderr)
	mustContain(t, "status", r.Stdout, "served behind (p-20260912-1403-c21)")
	mustContain(t, "status stderr", r.Stderr, "hint: the mirror job on the HTTP server runs every 5 min")

	// 20 minutes behind: served.stale with the last error and the hint.
	since = now.Add(-20 * time.Minute).Format(time.RFC3339)
	site.put(t, "el9/repoctl-mirror.json", map[string]any{"set": testkit.SetName, "target": "el9", "checked": now.Add(-time.Minute).Format(time.RFC3339),
		"result": "failed", "served": old, "latest": latest, "behindSince": since, "error": "gpgv: BAD signature"})
	el9, _, warns = statusOf()
	if m, _ := el9["mirror"].(map[string]any); m == nil || m["stale"] != true || !hasCode(warns, "served.stale") {
		t.Fatalf("stale: %v %v", el9, warns)
	}
	r = s.MustRun("status")
	t.Logf("%s\n%s", r.Stdout, r.Stderr)
	mustContain(t, "status", r.Stdout, "served behind since ")
	mustContain(t, "status stderr", r.Stderr, "el9 is behind since", "Last error of the mirror job: gpgv: BAD signature",
		"hint: read the journal of the mirror job on the HTTP server. run: journalctl -u repoctl-mirror.service")
	r = s.MustRun("doctor", "--only", "registry")
	t.Logf("%s\n%s", r.Stdout, r.Stderr)
	mustContain(t, "doctor", r.Stdout, "! served.stale: el9 is behind since", "journalctl -u repoctl-mirror.service")

	// The mirror job serves latest: same, and doctor passes.
	site.put(t, "el9/repoctl-snapshot.json", map[string]any{"set": testkit.SetName, "target": "el9", "digest": latest, "publish": pub, "pulled": now.Format(time.RFC3339)})
	site.put(t, "el9/repoctl-mirror.json", map[string]any{"set": testkit.SetName, "target": "el9", "checked": now.Format(time.RFC3339),
		"result": "swapped", "served": latest, "latest": latest, "behindSince": nil, "error": nil})
	el9, _, warns = statusOf()
	if el9["served"] != "same" || hasCode(warns, "served.stale") {
		t.Fatalf("same: %v %v", el9, warns)
	}
	env, _ = s.RunJSON("status")
	if open := openSteps(t, env); hasPrefixIn(open, "Served: ") || !hasPrefixIn(open, "First publish: el10") {
		t.Errorf("open setup steps once el9 is served: %v", open)
	}
	r = s.MustRun("status", "-v")
	t.Logf("%s\n%s", r.Stdout, r.Stderr)
	mustContain(t, "status -v", r.Stdout, "store", "local same, served same", "@", "mock        el9 ", "check       never ran on this host, timer off", "staging     ")
	if strings.Contains(r.Stdout, "(-v shows all)") {
		t.Errorf("status -v says (-v shows all):\n%s", r.Stdout)
	}
	r = s.MustRun("doctor", "--only", "registry")
	t.Logf("%s\n%s", r.Stdout, r.Stderr)
	mustContain(t, "doctor", r.Stdout, "✓ served.stale: the mirror job checked el9")
}

// TestStatusSetup: status computes the setup steps of the set from facts
// (lifecycle.html#setup): a local package branch that is not on the remote,
// a package without a lock line, a lock file that is not committed.
func TestStatusSetup(t *testing.T) {
	s := testkit.NewStory(t)
	env, _ := s.RunJSON("status")
	for _, o := range openSteps(t, env) {
		if strings.HasPrefix(o, "Branches pushed") || strings.HasPrefix(o, "All packages pinned") || strings.HasPrefix(o, "Set committed") {
			t.Errorf("open setup step of a set on its remote: %q", o)
		}
	}

	// A new package with a local branch only.
	s.Git(s.SetDir, "branch", "newx/slurm-26.05", "HEAD")
	p := filepath.Join(s.SetDir, "repoctl.yaml")
	writeFile(t, p, readFile(t, p)+"  - id: newx\n    branch: newx/slurm-26.05\n")
	// The lock file leaves the index.
	s.Git(s.SetDir, "rm", "-q", "--cached", "repoctl.lock")

	env, r := s.RunJSON("status")
	if r.Code != 0 {
		t.Fatalf("status: exit %d\n%s", r.Code, r.Stderr)
	}
	cmds := map[string]string{}
	for _, o := range list(items(t, env)[0], "open") {
		m := o.(map[string]any)
		cmd, _ := m["command"].(string)
		cmds[m["step"].(string)] = cmd
	}
	for step, cmd := range map[string]string{
		"Branches pushed: newx/slurm-26.05 is not on origin":         "git push origin newx/slurm-26.05",
		"All packages pinned: newx has no worktree and no lock line": "repoctl checkout newx",
		"Set committed and pushed: repoctl.lock is not committed":    "git add repoctl.yaml repoctl.lock && git commit",
	} {
		if got, ok := cmds[step]; !ok || got != cmd {
			t.Errorf("open step %q: command %q (%v), want %q", step, got, ok, cmd)
		}
	}
	r = s.MustRun("status")
	t.Logf("%s\n%s", r.Stdout, r.Stderr)
	mustContain(t, "status", r.Stdout, "setup       branches pushed: newx/slurm-26.05 is not on origin")
}

// TestCleanGit: clean --git keeps a bare clone with a worktree, keeps one
// with a commit that the remote does not have (git.mirror-unpushed), and
// deletes it once the commit is pushed and the worktree is gone
// (lifecycle.html#clones).
func TestCleanGit(t *testing.T) {
	s := testkit.NewStory(t)
	s.MustRun("checkout", "rocm-smi")
	wt := filepath.Join(s.SetDir, "packages", "rocm-smi")
	clone := s.Git(wt, "rev-parse", "--git-common-dir")

	cleanOf := func(args ...string) (map[string]any, []string, testkit.Result) {
		t.Helper()
		env, r := s.RunJSON(append([]string{"clean", "--git"}, args...)...)
		if r.Code != 0 {
			t.Fatalf("clean --git: exit %d\n%s", r.Code, r.Stderr)
		}
		its := items(t, env)
		if len(its) != 1 || its[0]["kind"] != "git" || its[0]["path"] != clone {
			t.Fatalf("clean --git: %v", its)
		}
		return its[0], warningCodes(env), r
	}
	if c, _, _ := cleanOf(); c["action"] != "kept" || !strings.Contains(c["reason"].(string), "worktree") {
		t.Fatalf("with a worktree: %v", c)
	}

	// A local commit, then the worktree goes.
	s.Git(wt, "commit", "--allow-empty", "-qm", "A local change")
	branch := s.Git(wt, "rev-parse", "--abbrev-ref", "HEAD")
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := cleanOf("--dry-run"); c["action"] != "kept" || !strings.Contains(c["reason"].(string), "1 commit not on the remote") {
		t.Fatalf("dry run with an unpushed commit: %v", c)
	}
	c, warns, _ := cleanOf()
	if c["action"] != "kept" || !hasCode(warns, "git.mirror-unpushed") {
		t.Fatalf("an unpushed commit: %v %v", c, warns)
	}
	r := s.MustRun("clean", "--git")
	t.Logf("%s\n%s", r.Stdout, r.Stderr)
	mustContain(t, "clean --git", r.Stdout, "keep", "1 commit not on the remote")
	mustContain(t, "clean --git stderr", r.Stderr, "keeps 1 commit that the remote does not have", "push origin "+branch)

	// Pushed: the clone goes.
	s.Git(clone, "push", "-q", "origin", branch)
	if c, _, _ := cleanOf("--dry-run"); c["action"] != "would-delete" {
		t.Fatalf("dry run after the push: %v", c)
	}
	if c, _, _ := cleanOf(); c["action"] != "deleted" {
		t.Fatalf("after the push: %v", c)
	}
	if _, err := os.Stat(clone); !os.IsNotExist(err) {
		t.Fatalf("the clone is still there: %v", err)
	}
}
