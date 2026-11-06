package sign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// storyView gives the view of the story on 2026-11-05: el9 has a latest
// with slurm-release-26.05-3 (key file a.admin, d.klein, hpc-repo), 3 files
// of hpc-repo and the records of d.klein and a.admin; el10 has no latest.
func storyView(t *testing.T) (*View, *fixture) {
	t.Helper()
	needTools(t, "rpm2cpio")
	f := newFixture(t, map[string]string{"d.klein": "active", "hpc-repo": "retiring", "a.admin": "active"})
	dk, ad, hr := f.keys["d.klein"], f.keys["a.admin"], f.keys["hpc-repo"]
	kf := keyfileOf(ad, dk, hr)
	rel3 := buildRPM(t, "slurm-release", "26.05", "3.el9", map[string]string{"/etc/pki/rpm-gpg/RPM-GPG-KEY-slurm": string(kf)})
	ucx := manifest.Build{Package: "ucx", Commit: strings.Repeat("b", 40), Reason: "commit-changed",
		SRPM: "ucx-1.22.0-1.el9.src.rpm", RPMs: []string{"ucx-1.22.0-1.el9.x86_64.rpm", "ucx-devel-1.22.0-1.el9.x86_64.rpm"}, Signer: hr.Fingerprint}
	tree := f.tree(t, "el9", []manifest.Build{releaseBuild("slurm-release-26.05-3.el9", dk.Fingerprint), ucx},
		kf, map[string]string{"slurm-release-26.05-3.el9.noarch.rpm": rel3})
	d := NewDeployments()
	d.Put(Deployment{Key: "d.klein", Target: "el9", Fingerprint: dk.Fingerprint, Release: "slurm-release-26.05-3.el9", ConfirmedBy: "a.admin", At: "2026-11-05T09:00:00Z"})
	d.Put(Deployment{Key: "a.admin", Target: "el9", Fingerprint: ad.Fingerprint, Release: "slurm-release-26.05-3.el9", ConfirmedBy: "d.klein", At: "2026-11-05T09:12:00Z", Note: "puppet run"})
	// The in-tree key file equals the key file in latest.
	p := filepath.Join(f.dir, "packages", "slurm-release", "RPM-GPG-KEY-slurm")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, kf, 0o644); err != nil {
		t.Fatal(err)
	}
	v := &View{Set: f.set, Dir: f.dir, Trust: f.trust, Deployments: d, Targets: []string{"el9", "el10"},
		Trees:     map[string]*Tree{"el9": tree, "el10": NewTree(filepath.Join(f.dir, "copy", "el10"), f.set, "el10")},
		KeyfileAt: func(context.Context, string) ([]byte, error) { return kf, nil },
		Now:       time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	v.Current = &Signer{ID: "d.klein", FPR: dk.Fingerprint, Key: f.trust.Key("d.klein"), Backend: "gpg", GPG: NewGPG("")}
	return v, f
}

func TestSignerList(t *testing.T) {
	ctx := context.Background()
	v, _ := storyView(t)
	l := v.SignerList(ctx)
	if err := testkit.Validate("SignerList", testkit.MarshalItem(t, l)); err != nil {
		t.Fatalf("SignerList does not agree with the contract: %v", err)
	}
	if l.Current != "d.klein" || len(l.Keys) != 3 {
		t.Fatalf("current %q, %d keys", l.Current, len(l.Keys))
	}
	items := map[string]SignerItem{}
	for _, x := range l.Keys {
		it := x.(SignerItem)
		items[it.KeyID] = it
	}
	if it := items["d.klein"]; !it.Deployed || it.RPMs["el9"] != 2 || it.Backend != "gpg" {
		t.Errorf("d.klein: %+v", it)
	}
	if it := items["a.admin"]; !it.Deployed || it.Deployments["el9"].ConfirmedBy != "d.klein" || it.RPMs["el9"] != 0 {
		t.Errorf("a.admin: %+v", it)
	}
	// hpc-repo has no record for el9, which has a latest.
	if it := items["hpc-repo"]; it.Deployed || it.RPMs["el9"] != 3 || it.State != "retiring" {
		t.Errorf("hpc-repo: %+v", it)
	}
	// One target in scope: el10 has no latest, so each key is deployed.
	v.Targets = []string{"el10"}
	for _, x := range v.SignerList(ctx).Keys {
		if !x.(SignerItem).Deployed {
			t.Errorf("%s is not deployed for el10 without a latest", x.(SignerItem).KeyID)
		}
	}
}

func TestOnboardOffboard(t *testing.T) {
	ctx := context.Background()
	v, _ := storyView(t)
	r := v.Onboard(ctx, "a.admin")
	if err := testkit.Validate("OnboardReport", testkit.MarshalItem(t, r)); err != nil {
		t.Fatalf("OnboardReport: %v", err)
	}
	if len(r.Steps) != 5 {
		t.Fatalf("%d steps", len(r.Steps))
	}
	for _, s := range r.Steps[1:4] {
		if !s.Ok {
			t.Errorf("onboarding step %d %q: %s", s.N, s.Name, s.Detail)
		}
	}
	// The set directory is no git repository: step 1 is open.
	if r.Steps[0].Ok {
		t.Errorf("step 1 without a set commit: %+v", r.Steps[0])
	}
	if r.Steps[4].Ok {
		t.Errorf("a.admin signed no snapshot: %+v", r.Steps[4])
	}
	// An unknown key.
	if r := v.Onboard(ctx, "x.new"); r.Next != "repoctl sign add x.new --fingerprint FPR" || r.Steps[0].Ok {
		t.Errorf("unknown key: %+v", r)
	}

	// hpc-repo is retiring, and 3 files of el9 carry it.
	r, err := v.Offboard(ctx, "hpc-repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := testkit.Validate("OnboardReport", testkit.MarshalItem(t, r)); err != nil {
		t.Fatalf("OnboardReport of offboard: %v", err)
	}
	if len(r.Steps) != 6 || !r.Steps[1].Ok || r.Steps[2].Ok || r.Next != "repoctl repo resign --signer hpc-repo --target el9" {
		t.Errorf("offboard hpc-repo: %+v", r)
	}
	if !strings.Contains(r.Steps[2].Detail, "3 RPMs in latest of el9") {
		t.Errorf("step 3: %s", r.Steps[2].Detail)
	}
	if len(r.ClientCommands) != 3 || !strings.HasPrefix(r.ClientCommands[0].Command, "rpm -e gpg-pubkey-") {
		t.Errorf("client commands: %+v", r.ClientCommands)
	}
	if _, err := v.Offboard(ctx, "x.new"); err == nil {
		t.Error("offboard of an unknown key")
	}
	// A revoked key: the order after a compromise, with the snapshots.
	v.Trust.Key("hpc-repo").State = "revoked"
	r, err = v.Offboard(ctx, "hpc-repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := testkit.Validate("OnboardReport", testkit.MarshalItem(t, r)); err != nil {
		t.Fatalf("OnboardReport of a revoked key: %v", err)
	}
	if len(r.SignedSnapshots) != 1 || r.SignedSnapshots[0].RPMs != 3 || r.ClientCommands[0].OS != "all" {
		t.Errorf("revoked: %+v", r)
	}
	// The in-tree key file still holds the key: step 2 is open.
	if r.Steps[1].Ok || r.Next != "repoctl release write" {
		t.Errorf("revoked step 2: %+v next %q", r.Steps[1], r.Next)
	}
}

func TestDoctorChecks(t *testing.T) {
	ctx := context.Background()
	v, _ := storyView(t)
	in := DoctorInput{View: v, Signer: v.Current,
		ReleaseCheck: func() (bool, []string, error) { return false, []string{"packages/slurm-release/slurm-el10.repo"}, nil }}
	got := map[string]api.DoctorReportCheck{}
	for _, c := range []api.DoctorReportCheck{CheckTrusted(in), CheckKeyfiles(in), CheckExpiry(in), CheckAlgorithm(in), CheckRetiring(in), CheckRepomd(in)} {
		got[c.Name] = c
	}
	for _, c := range CheckDeployed(ctx, in) {
		got[c.Name] = c
	}
	for _, c := range CheckRelease(ctx, in) {
		got[c.Name] = c
	}
	rep := api.DoctorReport{Worst: api.CheckStatusWarning}
	for _, c := range got {
		rep.Checks = append(rep.Checks, c)
		if c.Group != DoctorGroup {
			t.Errorf("%s: group %q", c.Name, c.Group)
		}
	}
	if err := testkit.Validate("DoctorReport", testkit.MarshalItem(t, rep)); err != nil {
		t.Errorf("the checks do not agree with the contract: %v", err)
	}
	want := map[string]api.CheckStatus{
		"sign.trusted":  api.CheckStatusOk,
		"sign.keyfiles": api.CheckStatusOk,
		"sign.expiry":   api.CheckStatusOk,
		"sign.deployed": api.CheckStatusOk,
		"sign.keyfile":  api.CheckStatusOk,
		"sign.release":  api.CheckStatusOk,
		"sign.retiring": api.CheckStatusWarning,
		"release.repo":  api.CheckStatusWarning,
	}
	for name, st := range want {
		c, ok := got[name]
		if !ok {
			t.Errorf("no check %s", name)
			continue
		}
		if c.Status != st {
			t.Errorf("%s: %s (%s), want %s", name, c.Status, c.Detail, st)
		}
	}
	if c := got["sign.retiring"]; !strings.Contains(c.Detail, "3 RPMs in latest of el9") || !strings.Contains(c.Remediation, "repo resign --signer hpc-repo") {
		t.Errorf("sign.retiring: %+v", c)
	}
	// A signer without a record for el9.
	v.Deployments.Withdraw("d.klein", "el9")
	if c := CheckDeployed(ctx, in)[0]; c.Status != api.CheckStatusWarning {
		t.Errorf("sign.deployed without a record: %+v", c)
	}
	// A retiring key of sign.key is not trusted for a publish.
	v.Trust.Key("d.klein").State = "retiring"
	if c := CheckTrusted(in); c.Status == api.CheckStatusOk {
		t.Errorf("sign.trusted of a retiring key: %+v", c)
	}
}

// TestCheckVerifyEL: sign.verify-el finds the verify root CONFIG-EXT of
// each target; a bootstrap root alone does not count.
func TestCheckVerifyEL(t *testing.T) {
	dir := t.TempDir()
	v := &View{Set: &config.Set{Name: "s"}, Targets: []string{"el9", "el10"}, MockDir: dir}
	in := DoctorInput{View: v}
	if c := CheckVerifyEL(in); c.Status != api.CheckStatusWarning || !strings.Contains(c.Detail, "no verify root for el9, el10") ||
		!strings.Contains(c.Remediation, "repoctl mock update") {
		t.Errorf("no roots: %+v", c)
	}
	for _, n := range []string{"rocky+epel-9-x86_64-" + mock.VerifyExt("s", "el9"), "rocky+epel-10-x86_64-bootstrap-" + mock.VerifyExt("s", "el10")} {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if c := CheckVerifyEL(in); c.Status != api.CheckStatusWarning || !strings.HasPrefix(c.Detail, "no verify root for el10 in ") {
		t.Errorf("el10 bootstrap only: %+v", c)
	}
	if err := os.MkdirAll(filepath.Join(dir, "rocky+epel-10-x86_64-"+mock.VerifyExt("s", "el10")), 0o755); err != nil {
		t.Fatal(err)
	}
	if c := CheckVerifyEL(in); c.Status != api.CheckStatusOk || c.Detail != "verify roots for el9, el10 ready" {
		t.Errorf("both roots: %+v", c)
	}
}
