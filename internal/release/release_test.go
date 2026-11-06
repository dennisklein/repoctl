package release

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/sign"
	"github.com/dennisklein/repoctl/internal/testkit"
)

const spec = `Name:           slurm-release
Version:        26.05
Release:        1%{?dist}
Summary:        slurm-release for the repoctl tests
License:        MIT

%description
The release package.

%files

%changelog
* Tue Sep 01 2026 Dennis Klein <d.klein@example.org> - 26.05-1
- First release
`

func setup(t *testing.T) (*config.Set, string, *sign.Trust) {
	t.Helper()
	dir := t.TempDir()
	k := testkit.GenKey(t, "d.klein")
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "d.klein.asc"), k.Public, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "packages", "slurm-release"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "packages", "slurm-release", "slurm-release.spec"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	yml := fmt.Sprintf(`apiVersion: repoctl/v1alpha1
name: slurm-26.05
repo:
  name: slurm
  url: https://repo.example.org/slurm-26.05
sign:
  keys:
    - {id: d.klein, file: keys/d.klein.asc, fingerprint: %s, state: active}
  release: {keyfile: RPM-GPG-KEY-slurm}
targets:
  - {id: el9, mock: rocky+epel-9-x86_64, plugins: [rpmautospec], path: el9}
  - {id: el10, mock: rocky+epel-10-x86_64, plugins: [rpmautospec], path: el10}
  - {id: el8, mock: rocky+epel-8-x86_64, plugins: [rpmautospec], state: retired}
packages:
  - {id: slurm-release, path: packages/slurm-release, role: release}
`, k.Fingerprint)
	set, err := config.ParseSet([]byte(yml), "repoctl.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return set, dir, sign.LoadTrust(set, dir)
}

const repoEL9 = `# slurm-26.05 el9. Written by "repoctl release write". Do not edit.
[slurm]
name=slurm-26.05 el9
baseurl=https://repo.example.org/slurm-26.05/el9/slurm/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-slurm
metadata_expire=1h

[slurm-source]
name=slurm-26.05 el9 source
baseurl=https://repo.example.org/slurm-26.05/el9/slurm-source/
enabled=0
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-slurm
metadata_expire=1h

[slurm-debuginfo]
name=slurm-26.05 el9 debuginfo
baseurl=https://repo.example.org/slurm-26.05/el9/slurm-debuginfo/
enabled=0
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-slurm
metadata_expire=1h
`

func TestRepoFile(t *testing.T) {
	set, _, _ := setup(t)
	if got := string(RepoFile(set, set.Targets[0])); got != repoEL9 {
		t.Errorf("got\n%s\nwant\n%s", got, repoEL9)
	}
}

func TestWrite(t *testing.T) {
	ctx := context.Background()
	set, dir, trust := setup(t)
	err := Check(set, dir, trust)
	if e, ok := api.AsError(err); !ok || e.Code != api.PSignKeyfileStale || e.ExitCode != 5 {
		t.Fatalf("Check before the first write: %v", err)
	}
	now := time.Date(2026, 11, 3, 14, 40, 0, 0, time.UTC)
	res, err := Write(ctx, set, dir, trust, Options{Message: "Trust key d.klein X", Now: now, Author: "A. Admin <a.admin@example.org>"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Release != "2" || len(res.Changed) != 4 {
		t.Errorf("release %q, changed %v", res.Release, res.Changed)
	}
	for _, p := range []string{"packages/slurm-release/RPM-GPG-KEY-slurm", "packages/slurm-release/slurm-el9.repo", "packages/slurm-release/slurm-el10.repo"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "packages/slurm-release/slurm-el8.repo")); err == nil {
		t.Error("a retired target got a .repo file")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "packages/slurm-release/slurm-release.spec"))
	if !strings.Contains(string(got), "Release:        2%{?dist}\n") {
		t.Errorf("Release not raised:\n%s", got)
	}
	if !strings.Contains(string(got), "%changelog\n* Tue Nov 03 2026 A. Admin <a.admin@example.org> - 26.05-2\n- Trust key d.klein X\n\n* Tue Sep 01 2026") {
		t.Errorf("changelog entry:\n%s", got)
	}
	if err := Check(set, dir, trust); err != nil {
		t.Errorf("Check after the write: %v", err)
	}
	// A second write changes nothing.
	res, err = Write(ctx, set, dir, trust, Options{Now: now, Author: "x <x@y>"})
	if err != nil || len(res.Changed) != 0 || res.Release != "" {
		t.Errorf("second write: %v %+v", err, res)
	}
	// A hand edit of a .repo file makes it stale.
	p := filepath.Join(dir, "packages/slurm-release/slurm-el10.repo")
	if err := os.WriteFile(p, []byte("[slurm]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = Check(set, dir, trust)
	if e, ok := api.AsError(err); !ok || e.Code != api.PSignRepofileStale {
		t.Errorf("stale .repo: %v", err)
	}
	kf, repos, err := Diff(set, dir, trust)
	if err != nil || kf || len(repos) != 1 {
		t.Errorf("Diff %v %v %v", kf, repos, err)
	}
}

func TestBumpSpecAutorelease(t *testing.T) {
	_, _, err := BumpSpec([]byte("Release: %autorelease\n%changelog\n%autochangelog\n"), "m", "a <b>", time.Now())
	if e, ok := api.AsError(err); !ok || e.Code != api.PSourceInTreeAutorelease {
		t.Errorf("%v", err)
	}
}
