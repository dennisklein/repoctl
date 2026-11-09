package sign

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// fixture is a set directory with keys/ and a local copy per target.
type fixture struct {
	dir   string
	set   *config.Set
	trust *Trust
	keys  map[string]*testkit.TestKey
}

func newFixture(t *testing.T, states map[string]string) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir(), keys: map[string]*testkit.TestKey{}}
	var b strings.Builder
	b.WriteString("apiVersion: repoctl/v1alpha1\nname: slurm-26.05\nrepo:\n  name: slurm\n  url: https://repo.example.org/slurm-26.05\nsign:\n  keys:\n")
	for _, id := range []string{"a.admin", "d.klein", "hpc-repo"} {
		st, ok := states[id]
		if !ok {
			continue
		}
		k := testkit.GenKey(t, id)
		f.keys[id] = k
		p := filepath.Join(f.dir, "keys", id+".asc")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, k.Public, 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "    - {id: %s, file: keys/%s.asc, fingerprint: %s, state: %s}\n", id, id, k.Fingerprint, st)
	}
	b.WriteString("  release: {keyfile: RPM-GPG-KEY-slurm}\ntargets:\n")
	b.WriteString("  - {id: el9, mock: rocky+epel-9-x86_64, plugins: [rpmautospec]}\n")
	b.WriteString("  - {id: el10, mock: rocky+epel-10-x86_64, plugins: [rpmautospec]}\n")
	b.WriteString("packages:\n  - {id: ucx, branch: ucx/slurm-26.05}\n  - {id: slurm-release, path: packages/slurm-release, role: release}\n")
	set, err := config.ParseSet([]byte(b.String()), "repoctl.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.set = set
	f.trust = LoadTrust(set, f.dir)
	return f
}

// tree writes a local copy of a target: a manifest with the builds, the
// release RPMs and the root key file.
func (f *fixture) tree(t *testing.T, target string, builds []manifest.Build, rootKeyfile []byte, rpms map[string]string) *Tree {
	t.Helper()
	dir := filepath.Join(f.dir, "copy", target)
	repo := filepath.Join(dir, "slurm")
	if err := os.MkdirAll(filepath.Join(repo, "repodata"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := manifest.New(f.set.Name, target)
	m.Publish = "p-20261103-1412-5e1"
	m.Builds = builds
	for _, b := range builds {
		if b.Signer != "" {
			m.History = append(m.History, manifest.History{Publish: m.Publish, Kind: "publish", User: "d.klein", Host: "hpc-a1", Signer: b.Signer, Created: "2026-11-03T14:13:33Z"})
		}
	}
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "repodata", "repoctl.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	repomd := `<?xml version="1.0" encoding="UTF-8"?>
<repomd xmlns="http://linux.duke.edu/metadata/repo"><data type="repoctl"><location href="repodata/repoctl.json"/></data></repomd>
`
	if err := os.WriteFile(filepath.Join(repo, "repodata", "repomd.xml"), []byte(repomd), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, src := range rpms {
		p := filepath.Join(repo, "Packages", "s", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if rootKeyfile != nil {
		if err := os.WriteFile(filepath.Join(dir, "RPM-GPG-KEY-slurm"), rootKeyfile, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snap := api.Snapshot{Target: target, Publish: m.Publish, Digest: "sha256:" + strings.Repeat("9d07", 16), Kind: "publish", User: "d.klein", Host: "hpc-a1"}
	sb, _ := json.Marshal(snap)
	if err := os.WriteFile(filepath.Join(dir, SnapshotFile), sb, 0o644); err != nil {
		t.Fatal(err)
	}
	return NewTree(dir, f.set, target)
}

func releaseBuild(nvr, signer string) manifest.Build {
	return manifest.Build{Package: "slurm-release", Commit: strings.Repeat("a", 40), Reason: "commit-changed",
		SRPM: nvr + ".src.rpm", RPMs: []string{nvr + ".noarch.rpm"}, Signer: signer}
}

func keyfileOf(keys ...*testkit.TestKey) []byte {
	var out []byte
	for _, k := range keys {
		out = append(out, k.Public...)
	}
	return out
}

func TestTrustAndKeyfile(t *testing.T) {
	f := newFixture(t, map[string]string{"d.klein": "active", "hpc-repo": "retiring", "a.admin": "removed"})
	for _, k := range f.trust.Keys {
		if k.Err != nil || k.Cert == nil {
			t.Fatalf("%s: %v", k.ID, k.Err)
		}
	}
	if k := f.trust.Resolve(f.keys["d.klein"].Fingerprint); k == nil || k.ID != "d.klein" {
		t.Error("Resolve by fingerprint")
	}
	if k := f.trust.ForIssuer(f.keys["hpc-repo"].Fingerprint[24:]); k == nil || k.ID != "hpc-repo" {
		t.Error("ForIssuer by key id")
	}
	kf, err := f.trust.Keyfile()
	if err != nil {
		t.Fatal(err)
	}
	fprs, err := KeysOfKeyfile(kf)
	if err != nil {
		t.Fatal(err)
	}
	// Active and retiring keys, sorted by id; not the removed key.
	want := []string{f.keys["d.klein"].Fingerprint, f.keys["hpc-repo"].Fingerprint}
	if strings.Join(fprs, ",") != strings.Join(want, ",") {
		t.Errorf("key file holds %v, want %v", fprs, want)
	}
	// A fingerprint that does not match keys/ID.asc.
	if err := os.WriteFile(filepath.Join(f.dir, "keys", "d.klein.asc"), f.keys["hpc-repo"].Public, 0o644); err != nil {
		t.Fatal(err)
	}
	tr := LoadTrust(f.set, f.dir)
	if tr.Key("d.klein").Err == nil {
		t.Error("a key file with another fingerprint passes")
	}
	if _, err := tr.Keyfile(); err == nil {
		t.Error("Keyfile with a bad key passes")
	}
}

func TestDeploymentsRoundTrip(t *testing.T) {
	d := NewDeployments()
	at := time.Date(2026, 11, 5, 9, 12, 0, 0, time.UTC)
	d.Put(Deployment{Key: "d.klein", Target: "el9", Fingerprint: "4E7A0C93B85D21F69A0E3C47D1B266F041D80B6E", Release: "slurm-release-26.05-2.el9", ConfirmedBy: "d.klein", At: FormatAt(at.Add(-time.Hour))})
	d.Put(Deployment{Key: "a.admin", Target: "el9", Fingerprint: fprA, Release: "slurm-release-26.05-3.el9", ConfirmedBy: "d.klein", At: FormatAt(at), Note: "puppet run"})
	d.Put(Deployment{Key: "d.klein", Target: "el10", Fingerprint: "4E7A0C93B85D21F69A0E3C47D1B266F041D80B6E", Release: "slurm-release-26.05-2.el10", ConfirmedBy: "d.klein", At: FormatAt(at)})
	// A new record replaces the old entry.
	d.Put(Deployment{Key: "d.klein", Target: "el9", Fingerprint: "4E7A0C93B85D21F69A0E3C47D1B266F041D80B6E", Release: "slurm-release-26.05-3.el9", ConfirmedBy: "a.admin", At: FormatAt(at)})
	d.PutRemoval(Removal{Key: "hpc-repo", Target: "el9", Fingerprint: "9F2C0B71E4A6C8D0F1A3B5C7D9E1F3A57C3A19F2", ConfirmedBy: "d.klein", At: FormatAt(at)})
	data, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(doc)
	if err := testkit.Validate("KeyDeployments", js); err != nil {
		t.Fatalf("schema: %v\n%s", err, data)
	}
	back, err := ParseDeployments(data, DeployedFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Deployments) != 3 || back.Deployments[0].Key != "a.admin" || back.Deployments[1].Target != "el10" {
		t.Errorf("order: %+v", back.Deployments)
	}
	if r := back.Find("d.klein", "el9"); r == nil || r.Release != "slurm-release-26.05-3.el9" {
		t.Errorf("replace: %+v", r)
	}
	// Two entries of one key and target after a merge: the newest at wins.
	merged := strings.Replace(string(data), "deployments:\n", "deployments:\n  - key: a.admin\n    target: el9\n    fingerprint: "+fprA+"\n    release: slurm-release-26.05-1.el9\n    confirmedBy: d.klein\n    at: 2026-01-01T00:00:00Z\n", 1)
	back, err = ParseDeployments([]byte(merged), DeployedFile)
	if err != nil {
		t.Fatal(err)
	}
	if r := back.Find("a.admin", "el9"); r == nil || r.Release != "slurm-release-26.05-3.el9" || len(back.Deployments) != 3 {
		t.Errorf("merge: %+v", back.Deployments)
	}
	if !back.Withdraw("a.admin", "el9") || back.Withdraw("a.admin", "el9") {
		t.Error("Withdraw")
	}
	if _, err := ParseDeployments([]byte("apiVersion: repoctl/v1alpha1\nkind: KeyDeployments\ndeployments:\n  - {key: x, target: el9, fingerprint: AB, release: r, confirmedBy: x, at: 2026-01-01T00:00:00Z}\n"), DeployedFile); err == nil {
		t.Error("a short fingerprint passes")
	}
	out := back.API()
	if len(out.Removals) != 1 {
		t.Errorf("API removals %+v", out.Removals)
	}
	// A key file record has its sum and commit in the output, and both
	// forms validate against KeyDeployments.
	back.Put(Deployment{Key: "a.admin", Target: "el10", Fingerprint: fprA, Keyfile: strings.Repeat("ab", 32), Commit: strings.Repeat("7e", 20),
		ConfirmedBy: "d.klein", At: "2026-11-05T09:30:00Z"})
	out = back.API()
	var kf *api.KeyDeploymentEntry
	for i, e := range out.Deployments {
		if e.Target == "el10" && e.Key == "a.admin" {
			kf = &out.Deployments[i]
		}
	}
	if kf == nil || kf.Release != "" || kf.Keyfile != strings.Repeat("ab", 32) || kf.Commit != strings.Repeat("7e", 20) {
		t.Errorf("key file record: %+v", kf)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := testkit.Validate("KeyDeployments", b); err != nil {
		t.Errorf("KeyDeployments: %v\n%s", err, b)
	}
	kf.Release = "slurm-release-26.05-3.el10"
	if b, _ := json.Marshal(out); testkit.Validate("KeyDeployments", b) == nil {
		t.Error("an entry with a release and a key file validates")
	}
}

func TestGates(t *testing.T) {
	needTools(t, "rpm2cpio")
	ctx := context.Background()
	f := newFixture(t, map[string]string{"d.klein": "active", "hpc-repo": "retiring", "a.admin": "active"})
	dk, ad, hr := f.keys["d.klein"], f.keys["a.admin"], f.keys["hpc-repo"]
	rel2 := buildRPM(t, "slurm-release", "26.05", "2.el9", map[string]string{"/etc/pki/rpm-gpg/RPM-GPG-KEY-slurm": string(keyfileOf(dk, hr))})
	rel3 := buildRPM(t, "slurm-release", "26.05", "3.el9", map[string]string{"/etc/pki/rpm-gpg/RPM-GPG-KEY-slurm": string(keyfileOf(ad, dk, hr))})
	tree := f.tree(t, "el9", []manifest.Build{releaseBuild("slurm-release-26.05-2.el9", dk.Fingerprint), releaseBuild("slurm-release-26.05-3.el9", dk.Fingerprint)},
		keyfileOf(ad, dk, hr), map[string]string{"slurm-release-26.05-2.el9.noarch.rpm": rel2, "slurm-release-26.05-3.el9.noarch.rpm": rel3})
	if got := tree.NewestRelease(); got != "slurm-release-26.05-3.el9" {
		t.Fatalf("newest release %s", got)
	}
	if nvr, ok := tree.HasRelease("slurm-release-26.05-2"); !ok || nvr != "slurm-release-26.05-2.el9" {
		t.Fatalf("HasRelease %s %v", nvr, ok)
	}
	d := NewDeployments()
	d.Put(Deployment{Key: "d.klein", Target: "el9", Fingerprint: dk.Fingerprint, Release: "slurm-release-26.05-2.el9", ConfirmedBy: "d.klein", At: "2026-09-10T08:41:17Z"})
	signer := func(id string) *Signer {
		k := f.trust.Key(id)
		return &Signer{ID: id, FPR: k.Fingerprint, Key: k}
	}
	in := GateInput{Trust: f.trust, Deployments: d, Signer: signer("d.klein"), Targets: []string{"el9", "el10"},
		Bases: map[string]TargetBase{"el9": tree}, Now: time.Now()}
	if err := CheckGates(ctx, in); err != nil {
		t.Fatalf("d.klein: %v", err)
	}
	code := func(err error) api.ProblemCode {
		if e, ok := api.AsError(err); ok {
			return e.Code
		}
		return ""
	}
	// a.admin is active but has no record: gate 2.
	in.Signer = signer("a.admin")
	if err := CheckGates(ctx, in); code(err) != api.PSignNotDeployed {
		t.Errorf("a.admin without a record: %v", err)
	}
	// A record of the older release, whose key file lacks a.admin, fails.
	d.Put(Deployment{Key: "a.admin", Target: "el9", Fingerprint: ad.Fingerprint, Release: "slurm-release-26.05-2.el9", ConfirmedBy: "d.klein", At: "2026-11-05T09:12:00Z"})
	if err := CheckGates(ctx, in); code(err) != api.PSignNotDeployed || !strings.Contains(err.Error(), "does not hold") {
		t.Errorf("a.admin with release 2: %v", err)
	}
	d.Put(Deployment{Key: "a.admin", Target: "el9", Fingerprint: ad.Fingerprint, Release: "slurm-release-26.05-3.el9", ConfirmedBy: "d.klein", At: "2026-11-05T09:12:00Z"})
	if err := CheckGates(ctx, in); err != nil {
		t.Errorf("a.admin with release 3: %v", err)
	}
	// A first publish (no base) needs no record.
	in.Bases = map[string]TargetBase{}
	in.Signer = signer("hpc-repo")
	if err := CheckGates(ctx, in); code(err) != api.PSignNotActive {
		t.Errorf("hpc-repo is retiring: %v", err)
	}
	// Gate 1: the key expires within key_min_validity.
	in.Signer = signer("d.klein")
	in.Now = time.Now().AddDate(0, 0, 1)
	f.trust.Key("d.klein").Cert.Expires = time.Now().AddDate(0, 0, 10)
	if err := CheckGates(ctx, in); code(err) != api.PSignKeyExpiring {
		t.Errorf("expiring key: %v", err)
	}
	f.trust.Key("d.klein").Cert.Expires = time.Time{}
	// Gate 3: an RSA 1024 key.
	small := testkit.GenKeySpec(t, testkit.KeySpec{ID: "small", Algo: "rsa1024"})
	c, _ := ParseCert(small.Public)
	in.Signer = &Signer{ID: "small", FPR: small.Fingerprint, Key: &TrustedKey{Key: config.Key{ID: "small", State: "active", Fingerprint: small.Fingerprint}, Cert: c}}
	if err := Gate3(in.Signer); code(err) != api.PSignKeyFormat {
		t.Errorf("gate 3: %v", err)
	}
	// A key file record: the key file at the commit holds the key.
	kf := keyfileOf(ad, dk)
	rec := &Deployment{Key: "a.admin", Target: "el10", Fingerprint: ad.Fingerprint, Keyfile: Sha256Hex(kf), Commit: strings.Repeat("7", 40), ConfirmedBy: "d.klein", At: "2026-11-05T09:30:00Z"}
	at := func(context.Context, string) ([]byte, error) { return kf, nil }
	if ok, reason := RecordPasses(ctx, rec, ad.Fingerprint, nil, at); !ok {
		t.Errorf("key file record: %s", reason)
	}
	rec.Keyfile = strings.Repeat("0", 64)
	if ok, _ := RecordPasses(ctx, rec, ad.Fingerprint, nil, at); ok {
		t.Error("a key file record with another sum passes")
	}
}

func TestClassifyBase(t *testing.T) {
	f := newFixture(t, map[string]string{"d.klein": "active", "hpc-repo": "retiring", "a.admin": "removed"})
	dk, ad, hr := f.keys["d.klein"], f.keys["a.admin"], f.keys["hpc-repo"]
	now := time.Now()
	sig := func(fpr string) *Signature { return &Signature{IssuerFingerprint: fpr, Created: now.Add(-time.Hour)} }
	sigs := map[string]*Signature{"a.rpm": sig(dk.Fingerprint), "b.rpm": sig(hr.Fingerprint), "c.rpm": sig(hr.Fingerprint)}
	res, err := ClassifyBase(f.trust, sigs, now, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Retiring["hpc-repo"] != 2 || len(res.Files["d.klein"]) != 1 {
		t.Errorf("%+v", res)
	}
	sigs["d.rpm"] = sig(ad.Fingerprint)
	if _, err := ClassifyBase(f.trust, sigs, now, ""); err == nil || !strings.Contains(err.Error(), "removed key a.admin") {
		t.Errorf("removed signer: %v", err)
	}
	res, err = ClassifyBase(f.trust, sigs, now, "a.admin")
	if err != nil || len(res.Resign) != 1 {
		t.Errorf("resign of a.admin: %v %+v", err, res)
	}
	sigs["e.rpm"] = nil
	_, err = ClassifyBase(f.trust, sigs, now, "a.admin")
	if e, ok := api.AsError(err); !ok || e.Code != api.PSignUnknownSigner || !strings.Contains(e.Message, "e.rpm") {
		t.Errorf("unsigned RPM: %v", err)
	}
}

func TestLifecycle(t *testing.T) {
	for _, tc := range []struct {
		cmd, from string
		ok        bool
	}{
		{CmdRetire, "active", true}, {CmdRetire, "retiring", false},
		{CmdActivate, "retiring", true}, {CmdActivate, "active", false},
		{CmdRemove, "retiring", true}, {CmdRemove, "active", false},
		{CmdRevoke, "active", true}, {CmdRevoke, "retiring", true}, {CmdRevoke, "removed", true}, {CmdRevoke, "revoked", false},
		{CmdActivate, "removed", false}, {CmdRetire, "revoked", false},
	} {
		err := CheckTransition(tc.cmd, &TrustedKey{Key: config.Key{ID: "x", State: tc.from}})
		if (err == nil) != tc.ok {
			t.Errorf("%s from %s: %v", tc.cmd, tc.from, err)
		}
		if err != nil {
			if e, ok := api.AsError(err); !ok || e.Code != api.PSignWrongState {
				t.Errorf("%s from %s: %v", tc.cmd, tc.from, err)
			}
		}
	}
}

func TestTrustChange(t *testing.T) {
	f := newFixture(t, map[string]string{"d.klein": "active", "hpc-repo": "retiring", "a.admin": "active"})
	dk, ad, hr := f.keys["d.klein"], f.keys["a.admin"], f.keys["hpc-repo"]
	c, err := ComputeTrustChange(context.Background(), f.trust, "", "slurm-release-26.05-3", keyfileOf(dk, hr), keyfileOf(ad, dk, hr))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Added) != 1 || c.Added[0].ID != "a.admin" || len(c.Removed) != 0 {
		t.Fatalf("%+v", c)
	}
	lines := strings.Join(c.Lines(), "\n")
	for _, want := range []string{"trust    slurm-release-26.05-3 changes the key file of all clients", "+ a.admin", "- none", "a.admin cannot sign until"} {
		if !strings.Contains(lines, want) {
			t.Errorf("trust block lacks %q:\n%s", want, lines)
		}
	}
	if ids := c.AddedIDs(); len(ids) != 1 || ids[0] != "a.admin" {
		t.Errorf("AddedIDs %v", ids)
	}
	if d := DiffLines([]byte("a\nb\n"), []byte("a\nc\n")); strings.Join(d, "|") != "+ c|- b" {
		t.Errorf("DiffLines %v", d)
	}
}
