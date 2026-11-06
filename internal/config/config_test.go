package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
)

const storyYAML = `apiVersion: repoctl/v1alpha1
name: slurm-26.05
repo:
  name: slurm
targets:
  - id: el9
    mock: rocky+epel-9-x86_64
    plugins: [rpmautospec]
  - id: el10
    mock: rocky+epel-10-x86_64
    plugins: [rpmautospec]
packages:
  - id: munge
    branch: munge/slurm-26.05
  - id: rocm-smi
    git: {url: https://git.gsi.de/SDE/packages/rocm-smi.git}
    branch: slurm-26.05
    targets: [el9]
  - id: slurm-release
    path: packages/slurm-release
    role: release
`

func TestParseSet(t *testing.T) {
	s, err := ParseSet([]byte(storyYAML), "repoctl.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "slurm-26.05" || len(s.Targets) != 2 || len(s.Packages) != 3 {
		t.Fatalf("parsed %+v", s)
	}
	nodes := s.Nodes()
	if len(nodes) != 5 {
		t.Fatalf("nodes %v, want 5 (rocm-smi is el9 only)", nodes)
	}
	rs, _ := s.Package("rocm-smi")
	if got := s.PackageTargets(*rs); !rs.External() || len(got) != 1 || got[0] != "el9" {
		t.Fatalf("rocm-smi: %+v", rs)
	}
	if rel, ok := s.ReleasePackage(); !ok || rel.ID != "slurm-release" || !rel.InTree() {
		t.Fatalf("release package: %+v", rel)
	}
}

func TestParseSetErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml string
		code api.ProblemCode
	}{
		"unknown key":   {strings.Replace(storyYAML, "repo:\n", "colour: red\nrepo:\n", 1), api.PConfigUnknownKey},
		"newer api":     {strings.Replace(storyYAML, "v1alpha1", "v9", 1), api.PContractNewerFile},
		"no autospec":   {strings.Replace(storyYAML, "plugins: [rpmautospec]\n  - id: el10", "plugins: []\n  - id: el10", 1), api.PConfigInvalid},
		"non x86_64":    {strings.Replace(storyYAML, "rocky+epel-10-x86_64", "rocky+epel-10-aarch64", 1), api.PConfigUnsupported},
		"bad name":      {strings.Replace(storyYAML, "name: slurm-26.05", "name: Slurm_26", 1), api.PConfigInvalid},
		"min repoctl":   {strings.Replace(storyYAML, "name: slurm-26.05", "name: slurm-26.05\nminRepoctl: 99.0.0", 1), api.PContractNewerFile},
		"branch + path": {strings.Replace(storyYAML, "    path: packages/slurm-release", "    path: packages/slurm-release\n    branch: x", 1), api.PConfigInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSet([]byte(tc.yaml), "repoctl.yaml")
			e, ok := api.AsError(err)
			if !ok {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
			if e.Code != tc.code {
				t.Fatalf("code = %s, want %s (%v)", e.Code, tc.code, err)
			}
		})
	}
}

func TestDurations(t *testing.T) {
	for in, want := range map[string]time.Duration{"0": 0, "90d": 90 * 24 * time.Hour, "1y": 365 * 24 * time.Hour, "2h": 2 * time.Hour, "30m": 30 * time.Minute, "1w": 7 * 24 * time.Hour} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseDuration("soon"); err == nil {
		t.Error("ParseDuration accepts soon")
	}
}

func TestPaths(t *testing.T) {
	env := map[string]string{"HOME": "/home/u", "XDG_RUNTIME_DIR": "/run/user/1000", "XDG_STATE_HOME": "relative"}
	p := NewPaths(func(k string) string { return env[k] })
	if p.ConfigHome != "/home/u/.config/repoctl" || p.StateHome != "/home/u/.local/state/repoctl" {
		t.Fatalf("paths %+v (a relative XDG value is ignored)", p)
	}
	if got := p.SetLock("slurm-26.05"); got != "/run/user/1000/repoctl/slurm-26.05.lock" {
		t.Fatal(got)
	}
	if got := p.StagingTarget("s", "r-1", "el9"); got != "/home/u/.local/share/repoctl/s/staging/r-1/el9" {
		t.Fatal(got)
	}
	if p.HostMockLock() != "/var/lib/mock/.repoctl.lock" {
		t.Fatal(p.HostMockLock())
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	set := filepath.Join(root, "set")
	deep := filepath.Join(set, "packages", "ucx", "src")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(set, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(set, "repoctl.yaml"), []byte(storyYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(deep)
	if err != nil || got != set {
		t.Fatalf("Discover = %q, %v; want %q", got, err, set)
	}
	if _, err := Discover(root); !IsNoSet(err) {
		t.Fatalf("Discover outside a set: %v", err)
	}
}

func TestResolveLayers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("jobs: 3\nsign:\n  key: d.klein\nsets:\n  slurm-26.05:\n    jobs: 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uf, err := LoadUser(path, true)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"REPOCTL_LOCK_WAIT": "2h"}
	e, err := Resolve(uf, "slurm-26.05", func(k string) string { return env[k] }, map[string]string{"progress": "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Int("jobs") != 4 || e.String("sign.key") != "d.klein" || e.Duration("lock_wait") != 2*time.Hour || e.String("progress") != "plain" {
		t.Fatalf("effective: jobs=%d key=%s wait=%v progress=%s", e.Int("jobs"), e.String("sign.key"), e.Duration("lock_wait"), e.String("progress"))
	}
	if e.Get("progress").Layer != 1 || e.Get("lock_wait").Layer != 2 || e.Get("sign.key").Layer != 3 || e.Get("ui.open").Layer != 5 {
		t.Fatal("layers")
	}
	if err := os.WriteFile(path, []byte("packages: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUser(path, true); err == nil {
		t.Fatal("a set key in the user config is accepted")
	}
}
