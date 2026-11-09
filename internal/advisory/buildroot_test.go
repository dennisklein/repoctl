package advisory

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/advisory/rpmmd"
	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/records"
	"github.com/dennisklein/repoctl/internal/sign"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// rootRPM gives a buildroot RPM of name-version-release.
func rootRPM(nvr, arch string) records.BuildrootRPM {
	i := strings.LastIndex(nvr, "-")
	j := strings.LastIndex(nvr[:i], "-")
	return records.BuildrootRPM{Name: nvr[:j], Version: nvr[j+1 : i], Release: nvr[i+1:], Arch: arch}
}

func inRepo(repo string, p *rpmmd.Package) *rpmmd.Package {
	p.Repo = repo
	return p
}

// TestBuildrootCodes: munge@el9 took openssl-devel 3.0.7 (3.2.2 is in
// BaseOS now: buildroot.drift), hwloc-devel of AppStream (the set package
// hwloc builds it now: provider.changed) and libfoo-devel of the set.
func TestBuildrootCodes(t *testing.T) {
	closure := []*rpmmd.Package{
		inRepo("baseos", pkg("openssl-devel-3.0.7-27.el9", "openssl-3.0.7-27.el9.src.rpm", []string{"openssl-devel = 1:3.0.7-27.el9", "pkgconfig(openssl) = 3.0.7"}, nil)),
		inRepo("baseos", pkg("openssl-devel-3.2.2-6.el9", "openssl-3.2.2-6.el9.src.rpm", []string{"openssl-devel = 1:3.2.2-6.el9", "pkgconfig(openssl) = 3.2.2"}, nil)),
		inRepo("appstream", pkg("gcc-11.4.1-3.el9", "gcc-11.4.1-3.el9.src.rpm", []string{"gcc"}, nil)),
		inRepo("appstream", pkg("make-4.3-8.el9", "make-4.3-8.el9.src.rpm", []string{"make"}, nil)),
		inRepo("appstream", pkg("hwloc-devel-2.4.1-5.el9", "hwloc-2.4.1-5.el9.src.rpm", []string{"hwloc-devel = 2.4.1-5.el9", "pkgconfig(hwloc)"}, nil)),
	}
	latest := []*rpmmd.Package{
		pkg("munge-0.5.18-1.el9", "munge-0.5.18-1.el9.src.rpm", []string{"munge"}, nil),
		pkg("hwloc-devel-2.11.2-1.el9", "hwloc-2.11.2-1.el9.src.rpm", []string{"hwloc-devel = 2.11.2-1.el9", "pkgconfig(hwloc)"}, nil),
		pkg("libfoo-devel-1.0-1.el9", "libfoo-1.0-1.el9.src.rpm", []string{"libfoo-devel"}, nil),
	}
	set := &config.Set{Name: "s", Packages: []config.Package{{ID: "munge"}, {ID: "hwloc"}, {ID: "libfoo"}}}
	m := &manifest.Manifest{Builds: []manifest.Build{
		{Package: "hwloc", SRPM: "hwloc-2.11.2-1.el9.src.rpm"}, {Package: "libfoo", SRPM: "libfoo-1.0-1.el9.src.rpm"},
		{Package: "munge", SRPM: "munge-0.5.18-1.el9.src.rpm"},
	}}
	env := NewBuildrootEnv(closure, latest, NewOwner(set, m, nil))
	var brs []rpmmd.Cap
	for _, c := range []string{"pkgconfig(openssl)", "gcc", "make", "hwloc-devel >= 2", "libfoo-devel", "rpmlib(CompressedFileNames) <= 3.0.4-1", "/usr/bin/xsltproc", "gcc"} {
		brs = append(brs, rpmmd.ParseCap(c))
	}
	node := BuildrootNode{Package: "munge", BuildRequires: brs, Buildroot: []records.BuildrootRPM{
		rootRPM("bash-5.1.8-9.el9", "x86_64"), rootRPM("openssl-devel-3.0.7-27.el9", "x86_64"), rootRPM("gcc-11.4.1-3.el9", "x86_64"),
		rootRPM("make-4.3-7.el9", "x86_64"), rootRPM("hwloc-devel-2.4.1-5.el9", "x86_64"), rootRPM("libfoo-devel-1.0-1.el9", "x86_64"),
	}}
	drift := env.Drift(node)
	if len(drift) != 1 || drift[0] != (api.FactsBuildrootDriftDriftItem{Package: "openssl-devel", Recorded: "3.0.7", Available: "3.2.2", Repo: "baseos"}) {
		t.Errorf("Drift: %+v (a release of make alone is no drift)", drift)
	}
	changes := env.Changes(node)
	if len(changes) != 1 || changes[0] != (api.FactsProviderChangedChange{Capability: "hwloc-devel >= 2", Recorded: "hwloc-devel-2.4.1-5.el9.x86_64", Provider: "hwloc"}) {
		t.Errorf("Changes: %+v", changes)
	}
	// The example of the contract gives the same digest.
	a, err := BuildrootDriftAdvisory("munge", "el9", []api.FactsBuildrootDriftDriftItem{{Package: "openssl-devel", Recorded: "3.0.7", Available: "3.2.2", Repo: "baseos"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "buildroot.drift/munge@el9" || a.Level != api.AdvisoryLevelInfo || a.Title != "openssl-devel 3.0.7 in the buildroot, 3.2.2 in the repos" ||
		a.FactsDigest != "sha256:c6a0e9fccde9a7aa79f87842bf755f25cc2f2a37ee1e711545aa65265ad9cafd" ||
		a.Suggestion.Command != `repoctl rebuild munge --reason "openssl 3.2.2"` {
		t.Errorf("BuildrootDriftAdvisory: %+v", a)
	}
	if s := Short(a, "→"); s != "openssl-devel 3.0.7 → 3.2.2" {
		t.Errorf("Short: %q", s)
	}
	p, err := ProviderChangedAdvisory("munge", "el9", changes)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "provider.changed/munge@el9" || p.Level != api.AdvisoryLevelWarn || len(p.Closure) != 0 ||
		p.Suggestion.Command != `repoctl rebuild munge --reason "build against set hwloc"` {
		t.Errorf("ProviderChangedAdvisory: %+v", p)
	}
	if s := Short(p, "→"); s != "hwloc-devel >= 2 hwloc-devel-2.4.1-5.el9 → set hwloc" {
		t.Errorf("Short: %q", s)
	}
	// Without a provider outside the set, nothing changed.
	node.Buildroot = []records.BuildrootRPM{rootRPM("hwloc-devel-2.11.2-1.el9", "x86_64"), rootRPM("openssl-devel-3.2.2-6.el9", "x86_64")}
	if d, c := env.Drift(node), env.Changes(node); d != nil || c != nil {
		t.Errorf("a current buildroot: %v %v", d, c)
	}
}

// TestSignExpiring: a key that signed RPMs and expires inside
// key_expiry_warn is warn, inside key_min_validity required; a key
// without RPMs or without an expiry gives nothing.
func TestSignExpiring(t *testing.T) {
	now := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	key := func(id, fpr string, expires time.Time) *sign.TrustedKey {
		return &sign.TrustedKey{Key: config.Key{ID: id, Fingerprint: fpr, State: config.KeyActive}, Cert: &sign.Cert{Fingerprint: fpr, Expires: expires}}
	}
	fa, fb, fc, fd := strings.Repeat("A", 40), strings.Repeat("B", 40), strings.Repeat("C", 40), strings.Repeat("D", 40)
	keys := []*sign.TrustedKey{
		key("a.admin", fa, now.Add(100*24*time.Hour)), // warn
		key("d.klein", fb, now.Add(10*24*time.Hour)),  // required
		key("hpc-repo", fc, now.Add(400*24*time.Hour)),
		key("idle", fd, now.Add(5*24*time.Hour)), // no RPMs
		{Key: config.Key{ID: "nofile", Fingerprint: strings.Repeat("E", 40)}},
	}
	rpms := map[string]int{fa: 3, fb: 5, fc: 50, strings.Repeat("E", 40): 1}
	advs, err := SignExpiring(keys, rpms, "el9", now, sign.DefaultKeyExpiryWarn, sign.DefaultKeyMinValidity)
	if err != nil {
		t.Fatal(err)
	}
	if len(advs) != 2 {
		t.Fatalf("SignExpiring: %+v", advs)
	}
	a, d := advs[0], advs[1]
	if a.ID != "sign.expiring/a.admin" || a.Level != api.AdvisoryLevelWarn || d.ID != "sign.expiring/d.klein" || d.Level != api.AdvisoryLevelRequired {
		t.Errorf("levels: %s %s, %s %s", a.ID, a.Level, d.ID, d.Level)
	}
	var f api.FactsSignExpiring
	if err := DecodeFacts(d, &f); err != nil || f.Fingerprint != fb || f.Expires != "2026-11-12" {
		t.Errorf("facts: %+v %v", f, err)
	}
	if len(d.Closure) != 0 || d.Suggestion.Command != "repoctl repo resign --signer d.klein" || ClosureLine(d) != "none: it never blocks a publish" {
		t.Errorf("sign.expiring blocks or suggests wrong: %+v", d)
	}
	// The facts validate against the contract.
	for _, x := range append(advs, a) {
		b, err := json.Marshal(x)
		if err != nil {
			t.Fatal(err)
		}
		if err := testkit.Validate("Advisory", b); err != nil {
			t.Errorf("%s: %v", x.ID, err)
		}
	}
}
