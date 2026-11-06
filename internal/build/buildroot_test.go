package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/records"
)

// The buildroot file of a staged build carries the header digest and the
// signer of each RPM, and the provenance record built from it has them
// (build.html#buildroot-facts, supply-chain.html#capture).
func TestBuildrootFileRecord(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "buildroot_lock.json")
	lockJSON := `{"version": "1.1", "buildroot": {"rpms": [
  {"name": "glibc", "epoch": null, "version": "2.39", "release": "17.el10", "arch": "x86_64", "license": "LGPL-2.1-or-later", "sigmd5": "aa", "url": "https://dl.rockylinux.org/glibc.rpm"},
  {"name": "munge-devel", "epoch": null, "version": "0.5.18", "release": "1.el10", "arch": "x86_64", "license": "GPL-3.0-or-later", "sigmd5": "bb", "url": "file:///staging/munge-devel.rpm"},
  {"name": "gpg-pubkey", "epoch": null, "version": "6d745a60", "release": "60287f36", "arch": "noarch", "license": "pubkey", "sigmd5": "cc", "url": ""}
]}, "bootstrap": {"image_digest": "sha256:0123"}}`
	if err := os.WriteFile(lockPath, []byte(lockJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	glibc, devel := strings.Repeat("1", 64), strings.Repeat("2", 64)
	facts := []mock.BuildrootRPM{
		{NEVRA: "glibc-0:2.39-17.el10.x86_64", HeaderSHA256: glibc, Signer: "702d426d350d275d"},
		{NEVRA: "munge-devel-0:0.5.18-1.el10.x86_64", HeaderSHA256: devel},
	}
	b, err := buildrootFile(lockPath, facts)
	if err != nil {
		t.Fatal(err)
	}
	bf, err := records.ParseBuildroot(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(bf.Buildroot.RPMs) != 3 || bf.Buildroot.RPMs[0].Header() != glibc || bf.Buildroot.RPMs[1].Header() != devel {
		t.Fatalf("buildroot file: %s", b)
	}
	if s := bf.Buildroot.RPMs[0].Signer; s == nil || *s != "702d426d350d275d" {
		t.Fatalf("signer of glibc: %v", s)
	}

	st := records.NewProvenance(records.BuildInput{Set: "slurm-26.05", Target: "el10", Namespace: "gsi-hpc", Distro: "rocky",
		Build: manifest.Build{Package: "slurm", Commit: strings.Repeat("d", 40)}, Buildroot: bf,
		SetRPM: func(nevra string) string {
			if strings.HasPrefix(nevra, "munge-devel-") {
				return "staging r-20261102-1750-a3f"
			}
			return ""
		}})
	got := map[string]string{}
	for _, r := range st.Predicate.BuildDefinition.ResolvedDependencies {
		if r.Annotations["digest"] == "SHA256HEADER" {
			got[r.URI] = r.Digest["sha256"]
		}
	}
	if len(got) != 2 {
		t.Fatalf("buildroot dependencies with a header digest: %v", got)
	}
	for uri, d := range got {
		want := glibc
		if strings.Contains(uri, "munge-devel") {
			want = devel
		}
		if d != want {
			t.Errorf("%s: digest %q, want %q", uri, d, want)
		}
	}
}
