//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/testkit"
)

// TestDoctorStory: repoctl doctor passes on the story host (exit 0) and
// its JSON validates.
func TestDoctorStory(t *testing.T) {
	s := testkit.NewStory(t)
	r := s.Run("doctor")
	if r.Code != 0 {
		t.Fatalf("doctor: exit %d\nstdout:\n%s\nstderr:\n%s", r.Code, r.Stdout, r.Stderr)
	}
	for _, want := range []string{"✓ repoctl ", "✓ git ", "✓ mock 6.1", "✓ mock configs: el9 rocky+epel-9-x86_64, el10 rocky+epel-10-x86_64",
		"✓ set slurm-26.05: repoctl.yaml valid, 0 worktrees, 1 package in tree", "check.never"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("doctor output misses %q:\n%s", want, r.Stdout)
		}
	}
	if !strings.Contains(r.Stdout, " checks: ") {
		t.Errorf("no totals line on stdout:\n%s", r.Stdout)
	}
	env, r := s.RunJSON("doctor")
	if r.Code != 0 {
		t.Fatalf("doctor --json: exit %d\n%s", r.Code, r.Stderr)
	}
	items := env["items"].([]any)
	rep := items[0].(map[string]any)
	if rep["worst"] == "failed" {
		t.Fatalf("doctor --json: a check failed:\n%s", r.Stdout)
	}
	// --strict turns a warning into exit 8.
	if rs := s.Run("doctor", "--strict"); rs.Code != 8 {
		t.Errorf("doctor --strict: exit %d, want 8 (check.never is a warning)\n%s", rs.Code, rs.Stderr)
	}
	// --only narrows the groups.
	env, _ = s.RunJSON("doctor", "--only", "check")
	for _, c := range env["items"].([]any)[0].(map[string]any)["checks"].([]any) {
		if g := c.(map[string]any)["group"]; g != "check" {
			t.Errorf("doctor --only check gave a check of group %v", g)
		}
	}
	if rs := s.Run("doctor", "--only", "nope"); rs.Code != 2 {
		t.Errorf("doctor --only nope: exit %d, want 2", rs.Code)
	}
}

// TestDoctorMissingMockConfig: a missing mock config fails the check and
// gives exit 9.
func TestDoctorMissingMockConfig(t *testing.T) {
	s := testkit.NewStory(t)
	if err := os.Remove(filepath.Join(s.MockConfigDir, "rocky+epel-10-x86_64.cfg")); err != nil {
		t.Fatal(err)
	}
	r := s.Run("doctor")
	if r.Code != 9 {
		t.Fatalf("doctor: exit %d, want 9\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "✗ mock.configs: missing on this host: el10 rocky+epel-10-x86_64") {
		t.Errorf("no failed mock.configs line:\n%s", r.Stdout)
	}
	// The preflight of check does not need the mock configs.
	if rc := s.Run("check", "--code", "closure.broken"); rc.Code != 0 {
		t.Errorf("check: exit %d\n%s", rc.Code, rc.Stderr)
	}
}

// fakeUpstream serves the GitHub and Fedora answers of the story.
func fakeUpstream(t *testing.T) (*httptest.Server, *int) {
	calls := 0
	releases := map[string]string{
		"SchedMD/slurm":                  `[]`,
		"open-mpi/ompi":                  `[{"tag_name":"v5.0.12","html_url":"https://github.com/open-mpi/ompi/releases/tag/v5.0.12","published_at":"2026-10-21T10:00:00Z"},{"tag_name":"v6.0.0rc1","html_url":"x","published_at":"2026-10-25T10:00:00Z","prerelease":true}]`,
		"GSI-HPC/slurm-singularity-exec": `[{"tag_name":"v3.2.1","html_url":"https://github.com/GSI-HPC/slurm-singularity-exec/releases/tag/v3.2.1","published_at":"2026-10-14T10:00:00Z"}]`,
		"openpmix/openpmix":              `[{"tag_name":"v4.2.9","html_url":"https://github.com/openpmix/openpmix/releases/tag/v4.2.9","published_at":"2024-02-01T10:00:00Z"},{"tag_name":"v6.1.0","html_url":"u","published_at":"2026-06-01T10:00:00Z"},{"tag_name":"v5.0.9","html_url":"u","published_at":"2026-05-01T10:00:00Z"}]`,
	}
	tags := map[string]string{
		"SchedMD/slurm":     `[{"name":"slurm-26-05-5-1","commit":{"sha":"aaa"}},{"name":"slurm-26-05-4-1","commit":{"sha":"bbb"}},{"name":"slurm-26-11-0-0rc1","commit":{"sha":"ccc"}}]`,
		"open-mpi/ompi":     `[{"name":"v5.0.12","commit":{"sha":"d1"}},{"name":"v5.0.11","commit":{"sha":"d2"}},{"name":"v4.1.8","commit":{"sha":"d3"}}]`,
		"dun/munge":         `[{"name":"munge-0.5.18","commit":{"sha":"e1"}}]`,
		"openucx/ucx":       `[{"name":"v1.22.0","commit":{"sha":"f1"}}]`,
		"openpmix/prrte":    `[{"name":"v4.0.0","commit":{"sha":"g1"}}]`,
		"ROCm/rocm-systems": `[{"name":"therock-7.14.0","commit":{"sha":"h1"}},{"name":"rocm-7.2.4","commit":{"sha":"h2"}}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/gh/repos/"):
			rest := strings.TrimPrefix(p, "/gh/repos/")
			parts := strings.Split(rest, "/")
			repo := parts[0] + "/" + parts[1]
			switch parts[2] {
			case "releases":
				fmt.Fprint(w, orDefault(releases[repo], "[]"))
			case "tags":
				fmt.Fprint(w, orDefault(tags[repo], "[]"))
			case "commits":
				fmt.Fprint(w, `{"commit":{"committer":{"date":"2026-10-29T12:00:00Z"}}}`)
			default:
				http.NotFound(w, r)
			}
		case strings.HasPrefix(p, "/fedora/rawhide/srcpkg/"):
			name := strings.TrimPrefix(p, "/fedora/rawhide/srcpkg/")
			v := map[string]string{"slurm": "26.05.4", "openmpi": "5.0.11", "munge": "0.5.18", "ucx": "1.22.0", "pmix": "6.1.0", "prrte": "4.0.0"}[name]
			if v == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"basename":%q,"version":%q,"release":"1.fc45"}`, name, v)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// TestUpstreamJSON: repoctl upstream --json against a fake GitHub and Fedora.
func TestUpstreamJSON(t *testing.T) {
	s := testkit.NewStory(t)
	srv, calls := fakeUpstream(t)
	s.Setenv("REPOCTL_TEST_GITHUB_API", srv.URL+"/gh")
	s.Setenv("REPOCTL_TEST_FEDORA_API", srv.URL+"/fedora")
	env, r := s.RunJSON("upstream")
	if r.Code != 0 {
		t.Fatalf("upstream --json: exit %d\n%s", r.Code, r.Stderr)
	}
	got := map[string]map[string]any{}
	for _, it := range env["items"].([]any) {
		m := it.(map[string]any)
		got[m["package"].(string)] = m
	}
	want := map[string]string{
		"slurm": "update-available", "openmpi": "update-available", "slurm-singularity-exec": "update-available",
		"pmix_v4": "frozen", "munge": "up-to-date", "ucx": "up-to-date", "prrte": "up-to-date",
		"slurm-release": "none", "pmix": "up-to-date", "pmix_v5": "up-to-date",
	}
	for pkg, st := range want {
		if got[pkg]["status"] != st {
			t.Errorf("%s: status %v, want %s (%v)", pkg, got[pkg]["status"], st, got[pkg])
		}
	}
	slurm := got["slurm"]
	if slurm["latest"] != "26.05.5" || slurm["current"].(map[string]any)["version"] != "26.05.4" {
		t.Errorf("slurm: %v", slurm)
	}
	if gh := slurm["github"].(map[string]any); gh["tag"] != "slurm-26-05-5-1" || gh["date"] != "2026-10-29T12:00:00.000Z" {
		t.Errorf("slurm github: %v", gh)
	}
	if got["openmpi"]["latest"] != "5.0.12" {
		t.Errorf("openmpi: the pre-release counts: %v", got["openmpi"])
	}
	// rocm-smi has no clone on this host: its current version is unknown.
	if got["rocm-smi"]["status"] != "unknown" {
		t.Errorf("rocm-smi: %v", got["rocm-smi"])
	}
	// The second call reads the cache.
	n := *calls
	r2 := s.Run("upstream", "slurm")
	if r2.Code != 0 || *calls != n {
		t.Errorf("upstream slurm: exit %d, %d new queries (want the cache)\n%s", r2.Code, *calls-n, r2.Stderr)
	}
	if !strings.Contains(strings.Join(strings.Fields(r2.Stdout), " "), "slurm 26.05.4 26.05.5 2026-10-29 26.05.4 update-available") {
		t.Errorf("upstream slurm:\n%s", r2.Stdout)
	}
	// --check gives exit 11 for an update; --refresh asks again.
	r3 := s.Run("upstream", "--check", "--refresh", "openmpi")
	if r3.Code != 11 || *calls == n {
		t.Errorf("upstream --check: exit %d, %d calls\n%s", r3.Code, *calls-n, r3.Stderr)
	}
	r4 := s.Run("upstream")
	if !strings.Contains(r4.Stdout, "11 packages: 3 updates, 1 frozen, 1 unknown, 5 up-to-date, 1 without upstream  (-v shows all)") {
		t.Errorf("upstream totals:\n%s", r4.Stdout)
	}
	// The unknown row gives a short reason; -v adds the detail.
	for _, l := range strings.Split(r4.Stdout, "\n") {
		if strings.HasPrefix(l, "rocm-smi ") && (!strings.HasSuffix(l, "unknown           current version unknown") || len(l) > 120) {
			t.Errorf("upstream: the unknown row %q", l)
		}
	}
	if v := s.Run("upstream", "-v"); !strings.Contains(v.Stderr, "note: rocm-smi: current version unknown: package rocm-smi has no clone on this host") {
		t.Errorf("upstream -v: no detail of rocm-smi:\n%s", v.Stderr)
	}
	if !strings.Contains(r4.Stderr, "run: repoctl upstream slurm openmpi slurm-singularity-exec --releases") {
		t.Errorf("upstream hint:\n%s", r4.Stderr)
	}
}

// TestCheckRetention: at its start, each check keeps the newest
// state.keep_checks check journals (contract.html#journal), and repoctl
// runs lists the check runs.
func TestCheckRetention(t *testing.T) {
	s := testkit.NewStory(t)
	srv, _ := fakeUpstream(t)
	s.Setenv("REPOCTL_TEST_GITHUB_API", srv.URL+"/gh")
	s.Setenv("REPOCTL_TEST_FEDORA_API", srv.URL+"/fedora")
	writeFile(t, filepath.Join(s.Home, ".config", "repoctl", "config.yaml"), "state:\n  keep_checks: 1\n")
	for range 3 {
		if r := s.Run("check", "--code", "upstream.release"); r.Code != 0 {
			t.Fatalf("check: exit %d\n%s", r.Code, r.Stderr)
		}
	}
	// The third check kept the newest of the two before it.
	runs, _ := filepath.Glob(filepath.Join(s.Home, ".local", "state", "repoctl", testkit.SetName, "runs", "r-*.jsonl"))
	if len(runs) != 2 {
		t.Fatalf("check journals after 3 checks with keep_checks 1: %v", runs)
	}
	env, r := s.RunJSON("runs", "--kind", "check")
	if r.Code != 0 {
		t.Fatalf("runs --kind check: exit %d\n%s", r.Code, r.Stderr)
	}
	items, _ := env["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("runs --kind check: %d runs, want 2\n%s", len(items), r.Stdout)
	}
	h := s.MustRun("runs")
	for _, it := range items {
		m := it.(map[string]any)
		if m["kind"] != "check" || m["state"] != "ok" || !strings.Contains(h.Stdout, m["id"].(string)) {
			t.Errorf("run %v %v %v; runs:\n%s", m["id"], m["kind"], m["state"], h.Stdout)
		}
	}
}

// TestCheckNarrowed: without a local copy of latest, repoctl check narrows:
// the target codes do not run, upstream.release does, and dismiss and
// undismiss round-trip.
func TestCheckNarrowed(t *testing.T) {
	s := testkit.NewStory(t)
	srv, _ := fakeUpstream(t)
	s.Setenv("REPOCTL_TEST_GITHUB_API", srv.URL+"/gh")
	s.Setenv("REPOCTL_TEST_FEDORA_API", srv.URL+"/fedora")
	env, r := s.RunJSON("check")
	if r.Code != 0 {
		t.Fatalf("check: exit %d\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	if w, _ := json.Marshal(env["warnings"]); !strings.Contains(string(w), "no local copy of latest of el9") || !strings.Contains(string(w), "check.incomplete") {
		t.Errorf("check --json: no warning on the narrowed targets: %s", w)
	}
	list := env["items"].([]any)[0].(map[string]any)
	ids := map[string]string{}
	for _, a := range list["advisories"].([]any) {
		m := a.(map[string]any)
		ids[m["id"].(string)] = m["state"].(string)
	}
	for _, id := range []string{"upstream.release/slurm", "upstream.release/openmpi", "upstream.release/slurm-singularity-exec"} {
		if ids[id] != "open" {
			t.Errorf("check: %s is %q, want open (%v)", id, ids[id], ids)
		}
	}
	results := list["check"].(map[string]any)["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["code"] != "upstream.release" {
		t.Errorf("check results: %v (want upstream.release only)", results)
	}
	// The journal of the check validates.
	runs, _ := filepath.Glob(filepath.Join(s.Home, ".local", "state", "repoctl", testkit.SetName, "runs", "r-*.jsonl"))
	if len(runs) != 1 {
		t.Fatalf("check journals: %v", runs)
	}
	j, _ := os.ReadFile(runs[0])
	testkit.ValidateJournal(t, j)

	// The human output.
	h := s.Run("check")
	if !strings.Contains(h.Stderr, "note: no local copy of latest of el9") {
		t.Errorf("check: no note on the narrowed targets:\n%s", h.Stderr)
	}
	if !strings.Contains(h.Stdout, "upstream.release/slurm                   info   26.05.4 → 26.05.5") {
		t.Errorf("check table:\n%s", h.Stdout)
	}
	if !strings.Contains(h.Stdout, "3 open: 3 info") {
		t.Errorf("check totals:\n%s", h.Stdout)
	}
	if c := s.Run("check", "--check", "--min-level", "info"); c.Code != 11 {
		t.Errorf("check --check --min-level info: exit %d, want 11", c.Code)
	}
	if c := s.Run("check", "--check"); c.Code != 0 {
		t.Errorf("check --check (warn): exit %d, want 0", c.Code)
	}

	// Dismiss and undismiss.
	if d := s.Run("advisories", "dismiss", "upstream.release/slurm-singularity-exec", "--note", "short"); d.Code != 2 {
		t.Errorf("dismiss with a short note: exit %d, want 2", d.Code)
	}
	d := s.MustRun("advisories", "dismiss", "upstream.release/slurm-singularity-exec", "--note", "3.2.1 only changes the docs")
	if !strings.Contains(d.Stdout, "dismissed upstream.release/slurm-singularity-exec (facts sha256:") || !strings.Contains(d.Stderr, "Next: git add .repoctl/advisories.yaml") {
		t.Errorf("dismiss output:\n%s\n%s", d.Stdout, d.Stderr)
	}
	file, err := os.ReadFile(filepath.Join(s.SetDir, ".repoctl", "advisories.yaml"))
	if err != nil || !strings.Contains(string(file), "note: 3.2.1 only changes the docs") || !strings.Contains(string(file), "by: d.klein@example.org") {
		t.Fatalf("advisories.yaml: %v\n%s", err, file)
	}
	if again := s.Run("advisories", "dismiss", "upstream.release/slurm-singularity-exec", "--note", "once more please"); again.Code != 1 {
		t.Errorf("a second dismiss: exit %d, want 1 (advisory.unknown)", again.Code)
	}
	if unk := s.Run("advisories", "dismiss", "closure.broken/pmix@el9", "--note", "not a real one"); unk.Code != 1 || !strings.Contains(unk.Stderr, "no open advisory") {
		t.Errorf("dismiss of an unknown id: exit %d\n%s", unk.Code, unk.Stderr)
	}
	env, _ = s.RunJSON("advisories", "show", "upstream.release/slurm-singularity-exec")
	adv := env["items"].([]any)[0].(map[string]any)
	if adv["state"] != "dismissed" || adv["dismissal"].(map[string]any)["note"] != "3.2.1 only changes the docs" {
		t.Errorf("show after dismiss: %v", adv)
	}
	ls := s.Run("advisories")
	if strings.Contains(ls.Stdout, "slurm-singularity-exec") || !strings.Contains(ls.Stdout, "2 open  ·  1 dismissed") {
		t.Errorf("advisories ls after dismiss:\n%s\n%s", ls.Stdout, ls.Stderr)
	}
	up := s.Run("upstream")
	if !strings.Contains(up.Stdout, "dismissed: 3.2.1 only changes the docs") {
		t.Errorf("upstream after dismiss:\n%s", up.Stdout)
	}
	// --stale keeps a valid entry; the check ran each code on each target? No:
	// the target codes did not run, so --stale refuses.
	if st := s.Run("advisories", "undismiss", "--stale"); st.Code != 1 || !strings.Contains(st.Stderr, "the last check did not run") {
		t.Errorf("undismiss --stale after a narrowed check: exit %d\n%s", st.Code, st.Stderr)
	}
	u := s.MustRun("advisories", "undismiss", "upstream.release/slurm-singularity-exec")
	if !strings.Contains(u.Stdout, "undismissed upstream.release/slurm-singularity-exec") {
		t.Errorf("undismiss:\n%s", u.Stdout)
	}
	env, _ = s.RunJSON("advisories", "ls")
	for _, a := range env["items"].([]any)[0].(map[string]any)["advisories"].([]any) {
		if m := a.(map[string]any); m["state"] != "open" {
			t.Errorf("after undismiss: %v", m)
		}
	}
	// An invalid file is a warning, and nothing is dismissed.
	if err := os.WriteFile(filepath.Join(s.SetDir, ".repoctl", "advisories.yaml"), []byte("<<<<<<< HEAD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inv := s.Run("advisories", "ls")
	if inv.Code != 0 || !strings.Contains(inv.Stderr, "advisories.yaml") {
		t.Errorf("ls with an invalid file: exit %d\n%s", inv.Code, inv.Stderr)
	}
	if dm := s.Run("advisories", "dismiss", "upstream.release/slurm", "--note", "a good reason"); dm.Code != 1 {
		t.Errorf("dismiss with an invalid file: exit %d, want 1", dm.Code)
	}
}

// closurePrimary is the primary.xml of a closure repo with gcc 2.0.
const closurePrimary = `<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="1">
<package type="rpm">
  <name>gcc</name><arch>x86_64</arch>
  <version epoch="0" ver="2.0" rel="1.el9"/>
  <location href="Packages/g/gcc-2.0-1.el9.x86_64.rpm"/>
  <format>
    <rpm:sourcerpm>gcc-2.0-1.el9.src.rpm</rpm:sourcerpm>
    <rpm:provides><rpm:entry name="gcc" flags="EQ" epoch="0" ver="2.0" rel="1.el9"/></rpm:provides>
  </format>
</package>
</metadata>`

// TestCheckBuildroot: after a publish of el9, check finds the buildroot
// codes and sign.expiring. hello was built against gcc 1.0 of the fake
// build root; the closure repo of el9 has gcc 2.0 (buildroot.drift); the
// set package slurm-release now provides gcc (provider.changed); the
// signer a.admin expires in 100 days, inside key_expiry_warn
// (sign.expiring).
func TestCheckBuildroot(t *testing.T) {
	s := publishStory(t)
	k := testkit.GenKeySpec(t, testkit.KeySpec{ID: "a.admin", Expire: "100d"})
	testkit.ImportKeys(t, s.GnuPGHome(), k)
	s.MustRun("sign", "add", "a.admin", "--fingerprint", k.Fingerprint, "--from-gpg")
	spec := filepath.Join(s.SetDir, "packages/slurm-release/slurm-release.spec")
	writeFile(t, spec, strings.Replace(readFile(t, spec), "Source1:        RPM-GPG-KEY-slurm\n", "Source1:        RPM-GPG-KEY-slurm\nProvides:       gcc\n", 1))
	s.Git(s.SetDir, "commit", "-qam", "Provide gcc")
	s.Git(s.SetDir, "push", "-q", "origin", testkit.SetBranch)
	src := cloneBranch(t, s, "hello/slurm-26.05")
	helloSpec := filepath.Join(src, "hello.spec")
	writeFile(t, helloSpec, strings.Replace(readFile(t, helloSpec), "Source0:        README.hello\n", "Source0:        README.hello\nBuildRequires:  gcc\n", 1))
	hello := fakeBuild(t, s, src, "hello.spec", filepath.Join(s.Root, "b", "hello"))
	rel := fakeBuild(t, s, filepath.Join(s.SetDir, "packages/slurm-release"), "slurm-release.spec", filepath.Join(s.Root, "b", "rel"))
	inTree := s.Git(s.SetDir, "log", "-1", "--format=%H", "--", "packages/slurm-release")
	stage(t, s, map[string]string{"hello": hello, "slurm-release": rel},
		map[string]string{"hello": s.BranchCommit("hello"), "slurm-release": inTree})
	s.Setenv("REPOCTL_SIGN_KEY", "a.admin")
	s.Setenv(publishUnattended, "1")
	s.MustRun("publish", "--target", "el9", "--yes")

	repo := filepath.Join(s.Root, "appstream")
	writeFile(t, filepath.Join(repo, "repodata", "repomd.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<repomd xmlns="http://linux.duke.edu/metadata/repo"><data type="primary"><location href="repodata/primary.xml"/></data></repomd>`)
	writeFile(t, filepath.Join(repo, "repodata", "primary.xml"), closurePrimary)
	yml := filepath.Join(s.SetDir, "repoctl.yaml")
	writeFile(t, yml, strings.Replace(readFile(t, yml), "    path: el9\n", "    path: el9\n    closure_repos:\n      - id: appstream\n        url: file://"+repo+"\n", 1))

	env, r := s.RunJSON("check", "--target", "el9")
	if r.Code != 0 {
		t.Fatalf("check: exit %d\n%s", r.Code, r.Stderr)
	}
	advs := map[string]map[string]any{}
	for _, a := range env["items"].([]any)[0].(map[string]any)["advisories"].([]any) {
		m := a.(map[string]any)
		advs[m["id"].(string)] = m
	}
	facts := func(id string) string {
		b, _ := json.Marshal(advs[id]["facts"])
		return string(b)
	}
	if a := advs["buildroot.drift/hello@el9"]; a == nil || a["level"] != "info" ||
		!strings.Contains(facts("buildroot.drift/hello@el9"), `{"available":"2.0","package":"gcc","recorded":"1.0","repo":"appstream"}`) {
		t.Errorf("buildroot.drift/hello@el9: %v", a)
	}
	if a := advs["provider.changed/hello@el9"]; a == nil || a["level"] != "warn" ||
		!strings.Contains(facts("provider.changed/hello@el9"), `"capability":"gcc","provider":"slurm-release"`) {
		t.Errorf("provider.changed/hello@el9: %v", a)
	}
	if a := advs["sign.expiring/a.admin"]; a == nil || a["level"] != "warn" || !strings.Contains(facts("sign.expiring/a.admin"), k.Fingerprint) {
		t.Errorf("sign.expiring/a.admin: %v", a)
	}
	if _, ok := advs["sign.expiring/d.klein"]; ok {
		t.Error("sign.expiring for d.klein: its key has no expiry")
	}
	h := s.Run("check", "--target", "el9")
	for _, want := range []string{"buildroot.drift/hello@el9", "gcc 1.0 → 2.0", "provider.changed/hello@el9", "sign.expiring/a.admin"} {
		if !strings.Contains(h.Stdout, want) {
			t.Errorf("check: no %q in\n%s", want, h.Stdout)
		}
	}
	// The journals of the checks validate.
	journals, _ := filepath.Glob(filepath.Join(s.Home, ".local", "state", "repoctl", testkit.SetName, "runs", "r-*.jsonl"))
	for _, j := range journals {
		if !strings.HasSuffix(j, ".progress.jsonl") {
			testkit.ValidateJournal(t, []byte(readFile(t, j)))
		}
	}
}

// TestCheckTimer: --install-timer writes the user units and the enable
// link without systemctl on the test host; doctor then sees the timer.
func TestCheckTimer(t *testing.T) {
	s := testkit.NewStory(t)
	r := s.MustRun("check", "--install-timer")
	unitDir := filepath.Join(s.Home, ".config", "systemd", "user")
	for _, f := range []string{"repoctl-check@.service", "repoctl-check@.timer"} {
		b, err := os.ReadFile(filepath.Join(unitDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.Stdout, "wrote ~/.config/systemd/user/"+f) {
			t.Errorf("install-timer output misses %s:\n%s", f, r.Stdout)
		}
		if f == "repoctl-check@.service" && !strings.Contains(string(b), "ExecStart=/usr/bin/repoctl -C %I check --quiet") {
			t.Errorf("service unit:\n%s", b)
		}
	}
	links, _ := filepath.Glob(filepath.Join(unitDir, "timers.target.wants", "repoctl-check@*.timer"))
	if len(links) != 1 || !strings.Contains(r.Stdout, "started repoctl-check@") {
		t.Fatalf("enable link: %v\n%s", links, r.Stdout)
	}
	// A second install writes nothing.
	r2 := s.MustRun("check", "--install-timer")
	if strings.Contains(r2.Stdout, "wrote") {
		t.Errorf("second install:\n%s", r2.Stdout)
	}
	env, _ := s.RunJSON("doctor", "--only", "check")
	for _, c := range env["items"].([]any)[0].(map[string]any)["checks"].([]any) {
		if c.(map[string]any)["name"] == "check.timer-off" {
			t.Errorf("doctor: timer off after install: %v", c)
		}
	}
	rm := s.MustRun("check", "--remove-timer")
	if !strings.Contains(rm.Stdout, "stopped and removed") {
		t.Errorf("remove-timer:\n%s", rm.Stdout)
	}
	if links, _ := filepath.Glob(filepath.Join(unitDir, "timers.target.wants", "repoctl-check@*.timer")); len(links) != 0 {
		t.Errorf("enable link after remove: %v", links)
	}
}
