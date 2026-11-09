package publish

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/dennisklein/repoctl/internal/localcopy"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/publish/tree"
	"github.com/dennisklein/repoctl/internal/snapshot"
)

func TestKindRules(t *testing.T) {
	for _, k := range snapshot.Kinds {
		if _, ok := KindRules[k]; !ok {
			t.Errorf("no rules for kind %s", k)
		}
	}
	pub := KindRules[KindPublish]
	if !pub.TakesStaged || !pub.NotReady || !pub.NVR || pub.LockUnpushed != LockAll || pub.Freshness != FreshApplies {
		t.Errorf("publish rules: %+v", pub)
	}
	for _, k := range []string{KindRestore, KindPrune, KindResign, KindImport} {
		r := KindRules[k]
		if r.TakesStaged || r.NotReady || r.NVR {
			t.Errorf("%s takes no node: %+v", k, r)
		}
		if r.LockUnpushed != LockSet {
			t.Errorf("%s checks only the set commit: %+v", k, r)
		}
		if !r.Gate2 {
			t.Errorf("%s runs gate 2", k)
		}
	}
	want := map[string]string{KindPublish: AdvisoryBlocks, KindRestore: AdvisoryBlocks, KindPrune: AdvisoryClosure, KindResign: AdvisorySkip, KindImport: AdvisoryReport}
	for k, a := range want {
		if KindRules[k].Advisory != a {
			t.Errorf("%s advisory gate %s, want %s", k, KindRules[k].Advisory, a)
		}
	}
	if !KindRules[KindRestore].AcceptAdvisory || KindRules[KindPrune].AcceptAdvisory {
		t.Error("--accept-advisory: restore yes, prune no")
	}
	if KindRules[KindImport].Freshness != FreshEmpty || KindRules[KindPrune].Trust != TrustNever || KindRules[KindRestore].Trust != TrustNewest {
		t.Error("import freshness, prune and restore trust")
	}
	if KindRules[KindPrune].SignsRPMs || !KindRules[KindResign].SignsRPMs || !KindRules[KindPrune].Removes {
		t.Error("prune signs no RPM; resign signs; prune removes")
	}
}

// fixture is a set with one target, a test key, an in-process registry
// and RPMs built with rpmbuild.
type fixture struct {
	t       *testing.T
	root    string
	set     *config.Set
	setDir  string
	env     *Env
	stager  *stubStager
	fpr     string
	clock   time.Time
	getenv  func(string) string
	specDir string
}

type stubGit struct{}

func (stubGit) Check(context.Context, Rules, []string) (string, map[string]string, error) {
	return strings.Repeat("a", 40), map[string]string{"hello": strings.Repeat("b", 40), "t-release": strings.Repeat("c", 40)}, nil
}

type stubStager struct{ nodes []StagedNode }

func (s *stubStager) Nodes(_ context.Context, target string, base *manifest.Manifest, _ string) (*TargetNodes, error) {
	out := &TargetNodes{}
	for _, n := range s.nodes {
		if base != nil {
			if b, ok := base.Newest(n.Package, ""); ok && b.SRPM == n.Draft.SRPM {
				out.Current = append(out.Current, n.Node)
				continue
			}
		}
		out.Staged = append(out.Staged, n)
	}
	return out, nil
}

func need(t *testing.T, progs ...string) {
	for _, p := range progs {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("%s is not installed", p)
		}
	}
}

func sh(t *testing.T, env []string, name string, args ...string) string {
	t.Helper()
	c := exec.Command(name, args...)
	c.Env = append(os.Environ(), env...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func newFixture(t *testing.T) *fixture {
	need(t, "rpmbuild", "gpg", "rpmsign", "rpmkeys", "createrepo_c", "modifyrepo_c", "rpm2cpio")
	root, err := os.MkdirTemp("", "pt-")
	if err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(root, "g")
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", gh, "--kill", "all").Run()
		_ = os.RemoveAll(root)
	})
	if err := os.MkdirAll(gh, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GNUPGHOME", gh)
	sh(t, nil, "gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--quick-gen-key", "Test Signer <test@example.org>", "ed25519", "sign", "1y")
	var fpr string
	for _, l := range strings.Split(sh(t, nil, "gpg", "--batch", "--with-colons", "--list-secret-keys"), "\n") {
		if f := strings.Split(l, ":"); f[0] == "fpr" && fpr == "" {
			fpr = f[9]
		}
	}
	setDir := filepath.Join(root, "set")
	if err := os.MkdirAll(filepath.Join(setDir, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	key := sh(t, nil, "gpg", "--batch", "--armor", "--export", fpr)
	if err := os.WriteFile(filepath.Join(setDir, "keys", "test.asc"), []byte(key), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	set := &config.Set{Name: "t-1", Repo: config.Repo{Name: "t"}, Store: config.Store{OCI: host + "/org/t-repo"},
		Sign: config.SignConfig{Keys: []config.Key{{ID: "test", File: "keys/test.asc", Fingerprint: fpr, State: "active"}},
			Release: config.Release{Keyfile: "RPM-GPG-KEY-test"}},
		Targets:  []config.Target{{ID: "el9", Mock: "rocky+epel-9-x86_64"}},
		Packages: []config.Package{{ID: "hello"}, {ID: "t-release", Role: "release"}},
	}
	vars := map[string]string{"HOME": filepath.Join(root, "h")}
	getenv := func(k string) string { return vars[k] }
	f := &fixture{t: t, root: root, set: set, setDir: setDir, fpr: fpr, getenv: getenv, stager: &stubStager{},
		clock: time.Date(2026, 9, 12, 14, 3, 0, 0, time.UTC), specDir: filepath.Join(root, "b")}
	signer, err := NewGPGSigner(context.Background(), set, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	f.env = &Env{Set: set, SetDir: setDir, Paths: config.NewPaths(getenv), Getenv: getenv, User: "tester", Host: "hpc-a1", Repoctl: "0.1.0",
		Now: func() time.Time { return f.clock }, Via: "cli",
		Store: func(target string) (*snapshot.Store, error) {
			return snapshot.Open(set.OCIPackage(target), snapshot.Options{Getenv: getenv})
		},
		Signer: signer, Gates: BasicGates{}, Advisory: NoAdvisoryGate{}, Stager: f.stager, EL: HostEL{}, Git: stubGit{},
		HTTP: &http.Client{Transport: notFound{}},
	}
	return f
}

// notFound answers 404 to each request: repo.url serves nothing.
type notFound struct{}

func (notFound) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
}

const helloSpec = `Name: %s
Version: %s
Release: 1%%{?dist}
Summary: A test package
License: MIT
BuildArch: noarch
%s
%%description
A test package.
%%install
mkdir -p %%{buildroot}/usr/share/%%{name}
echo %%{version} > %%{buildroot}/usr/share/%%{name}/version
%s
%%files
/usr/share/%%{name}/version
%s
`

// build builds a package and stages it as a node.
func (f *fixture) build(pkg, version string) StagedNode {
	f.t.Helper()
	top := filepath.Join(f.specDir, pkg+"-"+version)
	src := filepath.Join(top, "SOURCES")
	if err := os.MkdirAll(src, 0o755); err != nil {
		f.t.Fatal(err)
	}
	var head, inst, files string
	if pkg == "t-release" {
		b, _ := os.ReadFile(filepath.Join(f.setDir, "keys", "test.asc"))
		if err := os.WriteFile(filepath.Join(src, "RPM-GPG-KEY-test"), b, 0o644); err != nil {
			f.t.Fatal(err)
		}
		head = "Source0: RPM-GPG-KEY-test"
		inst = "install -D -m 644 %{SOURCE0} %{buildroot}/etc/pki/rpm-gpg/RPM-GPG-KEY-test"
		files = "/etc/pki/rpm-gpg/RPM-GPG-KEY-test"
	}
	spec := filepath.Join(top, pkg+".spec")
	if err := os.WriteFile(spec, []byte(fmt.Sprintf(helloSpec, pkg, version, head, inst, files)), 0o644); err != nil {
		f.t.Fatal(err)
	}
	sh(f.t, nil, "rpmbuild", "--quiet", "-ba", "--define", "_topdir "+top, "--define", "dist .el9", "--define", "_sourcedir "+src, spec)
	nvr := pkg + "-" + version + "-1.el9"
	n := StagedNode{Node: api.Node(pkg, "el9"), Package: pkg, Target: "el9", Run: "r-20260912-1300-abc", Commit: strings.Repeat("b", 40)}
	if pkg == "t-release" {
		n.Commit = strings.Repeat("c", 40)
	}
	for _, x := range []struct{ kind, repo, file, path string }{
		{"srpm", "t-source", nvr + ".src.rpm", filepath.Join(top, "SRPMS", nvr+".src.rpm")},
		{"rpm", "t", nvr + ".noarch.rpm", filepath.Join(top, "RPMS", "noarch", nvr+".noarch.rpm")},
	} {
		sum, err := tree.SHA256File(x.path)
		if err != nil {
			f.t.Fatal(err)
		}
		fi, _ := os.Stat(x.path)
		n.Files = append(n.Files, StagedFile{Kind: x.kind, Repo: x.repo, NVRA: strings.TrimSuffix(x.file, ".rpm"), File: x.file, Path: x.path, SHA256: sum, Size: fi.Size()})
	}
	n.Draft = manifest.Build{Package: pkg, Commit: n.Commit, Reason: "commit-changed", SRPM: nvr + ".src.rpm", Sources: map[string]string{}}
	n.Facts.Started, n.Facts.Finished = f.clock.Add(-time.Hour), f.clock.Add(-50*time.Minute)
	return n
}

func (f *fixture) publish(kind string, mod func(*Request)) (*Plan, *Result, error) {
	f.t.Helper()
	f.clock = f.clock.Add(time.Hour)
	req := Request{Kind: kind, Targets: []string{"el9"}, Explicit: true}
	if mod != nil {
		mod(&req)
	}
	ctx := context.Background()
	p, err := Prepare(ctx, f.env, req)
	if err != nil {
		return nil, nil, err
	}
	res, err := Execute(ctx, f.env, p)
	return p, res, err
}

func code(err error) api.ProblemCode {
	if e, ok := api.AsError(err); ok {
		return e.Code
	}
	return ""
}

func TestPublishPipeline(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	hello := f.build("hello", "1.0")
	rel := f.build("t-release", "1.0")
	f.stager.nodes = []StagedNode{hello, rel}

	// The first publish.
	p, res, err := f.publish(KindPublish, nil)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if !p.Targets[0].First || p.Targets[0].Sign.New != 4 {
		t.Fatalf("first publish plan: first %v, sign %+v", p.Targets[0].First, p.Targets[0].Sign)
	}
	v := res.Run.Publish["el9"]
	if res.Run.State != api.RunStateOk || v.State != StepOK || len(v.Steps) != 8 || v.Pushable {
		t.Fatalf("run %s, target %+v", res.Run.State, v)
	}
	store, _ := f.env.Store("el9")
	d, ok, err := store.Latest(ctx)
	if err != nil || !ok || d.Digest.String() != v.Snapshot.Digest {
		t.Fatalf("latest %v %v %v, want %s", d.Digest, ok, err, v.Snapshot.Digest)
	}
	local := localcopy.Dir(f.env.Paths, "t-1", "el9")
	snap, err := localcopy.ReadSnapshot(local)
	if err != nil || snap == nil || snap.Digest != v.Snapshot.Digest || snap.Publish != p.ID {
		t.Fatalf("snapshot.json %+v %v", snap, err)
	}
	m, err := manifest.ReadLocal(f.env.Paths, f.set, "el9")
	if err != nil || m == nil || len(m.Builds) != 2 || len(m.History) != 1 {
		t.Fatalf("RepoManifest %+v %v", m, err)
	}
	for _, b := range m.Builds {
		if b.Records == nil || b.Records.Provenance == nil || b.Records.SBOM == nil || b.Publish != p.ID || b.Signer != f.fpr {
			t.Fatalf("build %+v", b)
		}
	}
	if m.Records == nil || m.Records.SBOM == nil {
		t.Fatal("no repository SBOM")
	}
	for _, root := range []string{"RPM-GPG-KEY-test", "t-release.rpm"} {
		if _, err := os.Stat(filepath.Join(local, root)); err != nil {
			t.Fatalf("root file %s: %v", root, err)
		}
	}
	journal, err := os.ReadFile(f.env.Paths.Journal("t-1", p.RunID))
	if err != nil || !strings.Contains(string(journal), `"type":"publish.finished"`) {
		t.Fatalf("journal: %v\n%s", err, journal)
	}

	// Nothing to publish.
	if _, _, err := f.publish(KindPublish, nil); code(err) != api.PPublishNothingToPublish {
		t.Fatalf("second publish without change: %v", err)
	}

	// A new version keeps the base and adds 2 RPMs.
	hello2 := f.build("hello", "1.1")
	f.stager.nodes = []StagedNode{hello2, rel}
	p2, res2, err := f.publish(KindPublish, nil)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if p2.Targets[0].Sign.New != 2 || p2.Targets[0].Sign.Keep != 4 {
		t.Fatalf("second publish sign %+v", p2.Targets[0].Sign)
	}
	v2 := res2.Run.Publish["el9"]
	if v2.Base != v.Snapshot.Digest {
		t.Fatalf("base %s, want %s", v2.Base, v.Snapshot.Digest)
	}
	d2, _, _ := store.Latest(ctx)
	chain, err := store.Chain(ctx, d2, 0)
	if err != nil || len(chain) != 2 || chain[1].Digest() != v.Snapshot.Digest {
		t.Fatalf("chain %d %v", len(chain), err)
	}

	// An older NVR is refused.
	f.stager.nodes = []StagedNode{f.build("hello", "0.9"), rel}
	if _, _, err := f.publish(KindPublish, nil); code(err) != api.PNVROlder {
		t.Fatalf("older NVR: %v", err)
	}

	// Pull into a second home gives the same RepoManifest.
	vars2 := map[string]string{"HOME": filepath.Join(f.root, "h2")}
	p2paths := config.NewPaths(func(k string) string { return vars2[k] })
	pr, err := localcopy.Pull(ctx, localcopy.PullOptions{Store: store, Set: "t-1", Target: "el9", Dir: localcopy.Dir(p2paths, "t-1", "el9"),
		Layout: tree.LayoutOf(f.set), Keys: localcopy.Keys(f.set, f.setDir), Keyfile: "RPM-GPG-KEY-test", Release: "t-release", User: "other"})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if pr.Snapshot.Digest() != v2.Snapshot.Digest {
		t.Fatalf("pulled %s", pr.Snapshot.Digest())
	}
	m1, _ := manifest.ReadLocal(f.env.Paths, f.set, "el9")
	m2, _ := manifest.ReadLocal(p2paths, f.set, "el9")
	b1, _ := m1.Marshal()
	b2, _ := m2.Marshal()
	if string(b1) != string(b2) {
		t.Fatal("the pulled RepoManifest differs")
	}

	// Prune hello 1.0; the newest NVR is protected.
	if _, _, err := f.publish(KindPrune, func(r *Request) { r.Prune.NVRs = []string{"hello-1.1-1.el9"} }); code(err) != api.PPruneUnsafe {
		t.Fatalf("prune of the newest NVR: %v", err)
	}
	_, res3, err := f.publish(KindPrune, func(r *Request) { r.Prune.NVRs = []string{"hello-1.0-1.el9"} })
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	m3, _ := manifest.ReadLocal(f.env.Paths, f.set, "el9")
	if len(m3.Removed) != 1 || m3.Removed[0].NVR != "hello-1.0-1.el9" || len(m3.Builds) != 2 || len(m3.History) != 3 {
		t.Fatalf("after prune: removed %+v builds %d history %d", m3.Removed, len(m3.Builds), len(m3.History))
	}
	if res3.Run.PublishKind != KindPrune {
		t.Fatalf("kind %s", res3.Run.PublishKind)
	}
	// A removed NVR does not come back.
	f.stager.nodes = []StagedNode{hello, rel}
	if _, _, err := f.publish(KindPublish, nil); code(err) != api.PNVRRemoved && code(err) != api.PPublishNothingToPublish {
		t.Fatalf("removed NVR: %v", err)
	}

	// Restore the first snapshot: hello 1.1 leaves.
	_, _, err = f.publish(KindRestore, func(r *Request) { r.Ref = p.ID })
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	m4, _ := manifest.ReadLocal(f.env.Paths, f.set, "el9")
	found := false
	for _, r := range m4.Removed {
		if r.NVR == "hello-1.1-1.el9" && r.Reason == "restore" {
			found = true
		}
	}
	if !found {
		t.Fatalf("restore: removed %+v", m4.Removed)
	}

	// Re-sign with the same key: each RPM gets a new signature.
	_, res5, err := f.publish(KindResign, func(r *Request) { r.SignerID = "test" })
	if err != nil {
		t.Fatalf("resign: %v", err)
	}
	if res5.Run.Publish["el9"].Sign.Resign == 0 {
		t.Fatalf("resign %+v", res5.Run.Publish["el9"].Sign)
	}
}

func TestStopAndComplete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.stager.nodes = []StagedNode{f.build("hello", "1.0"), f.build("t-release", "1.0")}
	good := f.env.Store
	f.env.Store = func(target string) (*snapshot.Store, error) {
		return snapshot.Open("127.0.0.1:1/org/t-repo/t-1-"+target, snapshot.Options{Getenv: f.getenv})
	}
	_, err := Prepare(ctx, f.env, Request{Kind: KindPublish, Targets: []string{"el9"}, Explicit: true})
	if code(err) != api.PRegistryUnreachable {
		t.Fatalf("an unreachable registry stops step 1: %v", err)
	}
	f.env.Store = good
	p, err := Prepare(ctx, f.env, Request{Kind: KindPublish, Targets: []string{"el9"}, Explicit: true})
	if err != nil {
		t.Fatal(err)
	}
	f.env.Store = func(target string) (*snapshot.Store, error) {
		return snapshot.Open("127.0.0.1:1/org/t-repo/t-1-"+target, snapshot.Options{Getenv: f.getenv})
	}
	res, err := Execute(ctx, f.env, p)
	if err == nil || res.Run.State != api.RunStateFailed {
		t.Fatalf("the push must fail: %v", err)
	}
	st, err := ReadState(f.env.Paths, "t-1", p.ID)
	if err != nil || !IsPushable(st.Publish["el9"]) {
		t.Fatalf("state %+v %v", st, err)
	}
	f.env.Store = good
	if pf := PushableFor(f.env.Paths, "t-1", "el9"); len(pf) != 1 || pf[0].Publish != p.ID {
		t.Fatalf("pushable %+v", pf)
	}
	out, err := Complete(ctx, f.env, "", "")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if out.Run.State != api.RunStateOk || IsPushable(out.Run.Publish["el9"]) {
		t.Fatalf("after complete: %s %+v", out.Run.State, out.Run.Publish["el9"])
	}
	if _, err := Complete(ctx, f.env, p.ID, ""); code(err) != api.PPublishNothingToPublish {
		t.Fatalf("a second complete: %v", err)
	}
	var ue *UsageError
	if _, err := Prepare(ctx, f.env, Request{Kind: KindPublish, Targets: []string{"el9"}, Hold: []string{"hello"}, Reason: "short"}); !errors.As(err, &ue) {
		t.Fatalf("--hold needs a reason: %v", err)
	}
}
