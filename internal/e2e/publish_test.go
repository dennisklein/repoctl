//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/publish"
	"github.com/dennisklein/repoctl/internal/publish/tree"
	"github.com/dennisklein/repoctl/internal/records"
	"github.com/dennisklein/repoctl/internal/testkit"
)

const (
	publishRun = "r-20260901-0915-e2e"
	mockEL9    = "rocky+epel-9-x86_64"
)

// publishStory is the story with two packages, a test key in sign.keys,
// store.oci on an in-process registry and repo.url on a server that
// answers 404.
func publishStory(t *testing.T) *testkit.Story {
	t.Helper()
	needTools(t, "createrepo_c", "modifyrepo_c", "rpmsign", "rpmkeys", "rpm2cpio", "gpg")
	testkit.Binaries(t)
	s := testkit.NewStory(t, testkit.WithPackages(
		&testkit.Pkg{ID: "hello", Version: "1.0", Branch: "hello/slurm-26.05"},
		&testkit.Pkg{ID: "slurm-release", Version: "26.05", Path: "packages/slurm-release", Role: "release"},
	))
	reg := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(reg.Close)
	web := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(web.Close)

	yml := readFile(t, filepath.Join(s.SetDir, "repoctl.yaml"))
	yml = strings.Replace(yml, "url: https://repo.example.org/slurm-26.05", "url: "+web.URL+"/slurm-26.05", 1)
	yml += "\nstore:\n  oci: " + strings.TrimPrefix(reg.URL, "http://") + "/gsi-hpc\n"
	writeFile(t, filepath.Join(s.SetDir, "repoctl.yaml"), yml)
	s.Git(s.SetDir, "commit", "-qam", "Add the store")

	k := testkit.GPGKeys(t, s, "d.klein")["d.klein"]
	s.Setenv("REPOCTL_SIGN_KEY", "d.klein")
	s.MustRun("sign", "add", "d.klein", "--fingerprint", k.Fingerprint, "--from-gpg")

	// The release RPM carries the key file.
	spec := filepath.Join(s.SetDir, "packages/slurm-release/slurm-release.spec")
	text := readFile(t, spec)
	text = strings.Replace(text, "Source0:        README.slurm-release\n", "Source0:        README.slurm-release\nSource1:        RPM-GPG-KEY-slurm\n", 1)
	text = strings.Replace(text, "%install\n", "%install\ninstall -D -m 0644 %{SOURCE1} %{buildroot}%{_sysconfdir}/pki/rpm-gpg/RPM-GPG-KEY-slurm\n", 1)
	text = strings.Replace(text, "%files\n", "%files\n%{_sysconfdir}/pki/rpm-gpg/RPM-GPG-KEY-slurm\n", 1)
	writeFile(t, spec, text)
	s.Git(s.SetDir, "commit", "-qam", "Install the key file")
	s.Git(s.SetDir, "push", "-q", "origin", testkit.SetBranch)
	return s
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeBuild builds a package with the fake mock: the SRPM, then the
// binary RPMs. It gives the result directory of the rebuild.
func fakeBuild(t *testing.T, s *testkit.Story, srcDir, spec, out string) string {
	t.Helper()
	mock := func(args ...string) {
		cmd := exec.Command(filepath.Join(s.Bin, "mock"), args...)
		cmd.Env = s.Env()
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("mock %s: %v\n%s", strings.Join(args, " "), err, b)
		}
	}
	mock("-r", mockEL9, "--buildsrpm", "--spec", filepath.Join(srcDir, spec), "--sources", srcDir, "--resultdir", filepath.Join(out, "srpm"))
	srpms, _ := filepath.Glob(filepath.Join(out, "srpm", "*.src.rpm"))
	if len(srpms) != 1 {
		t.Fatalf("buildsrpm gave %v", srpms)
	}
	mock("-r", mockEL9, "--rebuild", srpms[0], "--resultdir", filepath.Join(out, "rpm"))
	return filepath.Join(out, "rpm")
}

// stage writes the staging of a build run of el9 as the build step does:
// the RPMs by repository with repodata, the record drafts and the run
// journal with plan.ready, node.state and artifact.added.
func stage(t *testing.T, s *testkit.Story, builds map[string]string, commits map[string]string) {
	t.Helper()
	paths := config.NewPaths(os.Getenv)
	host, _ := os.Hostname()
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	clock := time.Date(2026, 9, 1, 9, 15, 0, 0, time.UTC)
	now := func() time.Time { clock = clock.Add(time.Second); return clock }
	j, err := publish.OpenJournal(paths.Journal(testkit.SetName, publishRun), publishRun, now)
	if err != nil {
		t.Fatal(err)
	}
	app := func(typ api.EventType, ev any) {
		if err := j.Append(typ, ev); err != nil {
			t.Fatal(err)
		}
	}
	app(api.EventTypeRunStarted, api.RunStarted{Set: testkit.SetName, Kind: "build", Trace: strings.Repeat("ab", 16),
		Origin: api.RunStartedOrigin{Via: "cli", User: "tester", Host: host}})
	pr := api.PlanReady{Summary: api.PlanReadySummary{Build: len(builds)}, Current: []api.NodeId{}, Commits: map[string]api.Commit{}}
	for pkg := range builds {
		n := api.Node(pkg, "el9")
		pr.Build = append(pr.Build, api.PlanReadyBuildItem{Node: n, Reasons: []api.PlanReasonCode{api.PlanReasonCodeNewPackage}})
		pr.Commits[n] = commits[pkg]
	}
	app(api.EventTypePlanReady, pr)
	dir := paths.StagingTarget(testkit.SetName, publishRun, "el9")
	for pkg, result := range builds {
		n := api.Node(pkg, "el9")
		started := clock
		files, _ := filepath.Glob(filepath.Join(result, "*.rpm"))
		srpm := ""
		for _, f := range files {
			kind, repo := "rpm", "slurm"
			if strings.HasSuffix(f, ".src.rpm") {
				kind, repo, srpm = "srpm", "slurm-source", filepath.Base(f)
			}
			dst := filepath.Join(dir, repo, filepath.Base(f))
			writeFile(t, dst, readFile(t, f))
			sum, err := tree.SHA256File(dst)
			if err != nil {
				t.Fatal(err)
			}
			fi, _ := os.Stat(dst)
			app(api.EventTypeArtifactAdded, api.ArtifactAdded{EventHeader: api.EventHeader{Node: n}, Kind: kind, Repo: repo,
				NVRA: strings.TrimSuffix(filepath.Base(f), ".rpm"), File: filepath.Base(f), SizeBytes: int(fi.Size()), Sha256: sum})
		}
		d := publish.Draft{Build: manifest.Build{SRPM: srpm, Sources: map[string]string{}},
			Facts:     records.Facts{Started: started, Finished: clock, Mock: "6.1", MockConfig: mockEL9, Repoctl: config.Version, User: "tester", Host: host},
			Buildroot: filepath.Join(result, "buildroot_lock.json")}
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, publish.DraftPath(paths, testkit.SetName, publishRun, "el9", pkg), string(b))
		app(api.EventTypeNodeState, api.NodeStateEvent{EventHeader: api.EventHeader{Node: n}, State: api.NodeStateOk, Prev: api.NodeStateRunning})
	}
	app(api.EventTypeRunFinished, api.RunFinished{State: api.RunStateOk, Counts: api.StateCounts{Ok: len(builds)}, Publishable: []api.TargetId{"el9"}})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"slurm", "slurm-source"} {
		cmd := exec.Command("createrepo_c", "--quiet", filepath.Join(dir, repo))
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("createrepo_c: %v\n%s", err, b)
		}
	}
	testkit.ValidateJournal(t, []byte(readFile(t, paths.Journal(testkit.SetName, publishRun))))
}

// runEnv runs repoctl in the set directory with more environment.
func runEnv(t *testing.T, s *testkit.Story, extra []string, args ...string) testkit.Result {
	t.Helper()
	cmd := exec.Command(filepath.Join(s.Bin, "repoctl"), args...)
	cmd.Dir = s.SetDir
	cmd.Env = append(s.Env(), extra...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := testkit.Result{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		r.Code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestPublishStory: two packages built with the fake mock and staged by a
// build run are published to el9; latest moves; a second home pulls the
// same RepoManifest; repo verify passes, and fails with exit 10 after a
// changed byte.
func TestPublishStory(t *testing.T) {
	s := publishStory(t)
	hello := fakeBuild(t, s, cloneBranch(t, s, "hello/slurm-26.05"), "hello.spec", filepath.Join(s.Root, "b", "hello"))
	rel := fakeBuild(t, s, filepath.Join(s.SetDir, "packages/slurm-release"), "slurm-release.spec", filepath.Join(s.Root, "b", "rel"))
	inTree := s.Git(s.SetDir, "log", "-1", "--format=%H", "--", "packages/slurm-release")
	stage(t, s, map[string]string{"hello": hello, "slurm-release": rel},
		map[string]string{"hello": s.BranchCommit("hello"), "slurm-release": inTree})

	// The dry run shows the summary and signs nothing.
	r := s.MustRun("publish", "--target", "el9", "--dry-run")
	t.Logf("publish --dry-run:\n%s---\n%s", r.Stdout, r.Stderr)
	for _, want := range []string{"publish ", "el9 of slurm-26.05", "none: the first snapshot of el9", "hello", "slurm-release", "dry run"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("dry run: no %q in\n%s", want, r.Stdout)
		}
	}
	env, r := s.RunJSON("publish", "--target", "el9", "--dry-run")
	if r.Code != 0 || env["kind"] != "Run" {
		t.Fatalf("dry run --json: exit %d kind %v\n%s", r.Code, env["kind"], r.Stderr)
	}

	// Without a terminal: --yes and the opt-in of the environment.
	if code, p := problem(s, "publish", "--target", "el9"); code != 6 || p != "publish.needs-confirmation" {
		t.Errorf("publish without a terminal: exit %d %s", code, p)
	}
	if code, p := problem(s, "publish", "--target", "el9", "--yes"); code != 6 || p != "publish.unattended-refused" {
		t.Errorf("publish --yes without the opt-in: exit %d %s", code, p)
	}
	// In a terminal, a wrong set name stops the publish with exit 6.
	if _, err := exec.LookPath("script"); err == nil {
		cmd := exec.Command("script", "-qec", filepath.Join(s.Bin, "repoctl")+" publish --target el9", "/dev/null")
		cmd.Dir, cmd.Env, cmd.Stdin = s.SetDir, s.Env(), strings.NewReader("slurm-26.04\n")
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 6 || !strings.Contains(string(out), "Type the set name to sign 5 RPMs and publish el9 of slurm-26.05") {
			t.Errorf("publish with a wrong answer: %v\n%s", err, out)
		}
	}
	s.Setenv(publishUnattended, "1")
	r = s.MustRun("publish", "--target", "el9", "--yes")
	t.Logf("publish:\n%s---\n%s", r.Stdout, r.Stderr)
	if !strings.Contains(r.Stdout, "published  el9  p-") || !strings.Contains(r.Stdout, "latest was none") {
		t.Errorf("publish: no published line in\n%s", r.Stdout)
	}
	if !strings.Contains(r.Stderr, "Next:") {
		t.Errorf("publish: no Next: line in\n%s", r.Stderr)
	}
	paths := config.NewPaths(os.Getenv)
	journals, _ := filepath.Glob(filepath.Join(paths.Runs(testkit.SetName), "p-*.jsonl"))
	if len(journals) != 1 {
		t.Fatalf("publish journals: %v", journals)
	}
	jtext := readFile(t, journals[0])
	testkit.ValidateJournal(t, []byte(jtext))
	steps := 0
	var digest any
	for _, line := range strings.Split(strings.TrimSpace(jtext), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		switch ev["type"] {
		case "publish.step":
			if ev["phase"] == "ended" && ev["status"] == "ok" {
				steps++
			}
		case "publish.finished":
			snaps, _ := ev["snapshots"].(map[string]any)
			if s, ok := snaps["el9"].(map[string]any); ok {
				digest = s["digest"]
			}
		}
	}
	if steps != 8 || digest == nil {
		t.Errorf("publish journal: %d steps ok, digest %v; want 8 and a digest\n%s", steps, digest, jtext)
	}

	// latest moved to the new snapshot.
	env, _ = s.RunJSON("repo", "snapshots", "--target", "el9")
	snaps := list(items(t, env)[0], "snapshots")
	if len(snaps) != 1 {
		t.Fatalf("repo snapshots: %v", snaps)
	}
	latest := snaps[0].(map[string]any)
	if latest["digest"] != digest || list(latest, "tags")[0] != "latest" || latest["local"] != "same" {
		t.Errorf("latest: %v, want digest %v", latest, digest)
	}

	// Nothing new to publish: exit 4.
	if r := s.Run("publish", "--target", "el9", "--yes"); r.Code != 4 {
		t.Errorf("second publish: exit %d, want 4\n%s", r.Code, r.Stderr)
	}

	// repo ls lists the RPMs of the local copy.
	env, _ = s.RunJSON("repo", "ls", "--target", "el9")
	names := map[string]bool{}
	for _, p := range list(items(t, env)[0], "packages") {
		names[p.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"hello", "hello-devel", "slurm-release"} {
		if !names[n] {
			t.Errorf("repo ls: no %s in %v", n, names)
		}
	}
	s.RunJSON("repo", "sbom", "--target", "el9")
	s.RunJSON("repo", "sbom", "--target", "el9", "--build", "hello")
	s.RunJSON("repo", "provenance", "hello", "--target", "el9")
	if r := s.Run("repo", "verify", "--target", "el9"); r.Code != 0 {
		t.Errorf("repo verify: exit %d\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}

	// A second home pulls the same RepoManifest.
	h2 := filepath.Join(s.Root, "h2")
	for _, d := range []string{h2, filepath.Join(h2, "run"), filepath.Join(h2, ".gnupg")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	home2 := []string{"HOME=" + h2, "XDG_CONFIG_HOME=" + filepath.Join(h2, ".config"), "XDG_CACHE_HOME=" + filepath.Join(h2, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(h2, ".local", "share"), "XDG_STATE_HOME=" + filepath.Join(h2, ".local", "state"),
		"XDG_RUNTIME_DIR=" + filepath.Join(h2, "run"), "GNUPGHOME=" + filepath.Join(h2, ".gnupg"), "REPOCTL_SIGN_KEY="}
	r = runEnv(t, s, home2, "repo", "pull", "--target", "el9", "--json")
	if r.Code != 0 {
		t.Fatalf("repo pull in a second home: exit %d\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	testkit.ValidateEnvelope(t, []byte(r.Stdout))
	paths2 := config.NewPaths(func(k string) string {
		for _, e := range home2 {
			if v, ok := strings.CutPrefix(e, k+"="); ok {
				return v
			}
		}
		return os.Getenv(k)
	})
	m1 := repoManifest(t, paths.LocalCopy(testkit.SetName, "el9"))
	m2 := repoManifest(t, paths2.LocalCopy(testkit.SetName, "el9"))
	if !bytes.Equal(m1, m2) {
		t.Errorf("the RepoManifest of the second home differs:\n%s\n%s", m1, m2)
	}

	// A changed byte fails repo verify with exit 10.
	rpms, _ := filepath.Glob(filepath.Join(paths2.LocalCopy(testkit.SetName, "el9"), "slurm", "Packages", "*", "hello-1.0*.rpm"))
	if len(rpms) == 0 {
		t.Fatalf("no hello RPM in the local copy of the second home")
	}
	b := []byte(readFile(t, rpms[0]))
	b[len(b)/2] ^= 0xff
	if err := os.Remove(rpms[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rpms[0], b, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := runEnv(t, s, home2, "repo", "verify", "--target", "el9"); r.Code != 10 {
		t.Errorf("repo verify after a changed byte: exit %d, want 10\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	if r := s.Run("repo", "verify", "--target", "el9"); r.Code != 0 {
		t.Errorf("repo verify of the first home: exit %d\n%s", r.Code, r.Stderr)
	}
}

const publishUnattended = "REPOCTL_ALLOW_UNATTENDED_PUBLISH"

func cloneBranch(t *testing.T, s *testkit.Story, branch string) string {
	t.Helper()
	dir := filepath.Join(s.Root, "src", strings.ReplaceAll(branch, "/", "_"))
	s.Git(s.Root, "clone", "-q", "--branch", branch, s.Origin, dir)
	return dir
}

// repoManifest gives the RepoManifest of a local copy, encoded again.
func repoManifest(t *testing.T, dir string) []byte {
	t.Helper()
	m, err := manifest.ReadRepo(filepath.Join(dir, "slurm"))
	if err != nil || m == nil {
		t.Fatalf("RepoManifest of %s: %v", dir, err)
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
