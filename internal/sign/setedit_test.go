package sign

import (
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/testkit"
)

const fprA = "C2F9771305ABE4D618C39F2B70DEA5C1E93F52A7"

const withSign = `apiVersion: repoctl/v1alpha1
name: slurm-26.05                  # set id

repo:
  name: slurm
  url: https://repo.example.org/slurm-26.05
sign:                              # trusted keys; "repoctl sign add" writes them
  keys:
    - id: d.klein
      file: keys/d.klein.asc
      fingerprint: 4E7A0C93B85D21F69A0E3C47D1B266F041D80B6E
      state: active
    - id: hpc-repo
      file: keys/hpc-repo.asc
      fingerprint: 9F2C0B71E4A6C8D0F1A3B5C7D9E1F3A57C3A19F2
      state: retiring   # the bash-era key
  release:
    keyfile: RPM-GPG-KEY-slurm     # key file in the release package

# one mock chroot each
targets:
  - id: el9
    mock: rocky+epel-9-x86_64
    plugins: [rpmautospec]
packages:
  - id: munge
    branch: munge/slurm-26.05
    upstream: {github: dun/munge, fedora: munge}
`

func newKey() config.Key {
	return config.Key{ID: "a.admin", File: "keys/a.admin.asc", Fingerprint: fprA, State: config.KeyActive}
}

func TestAddKeyEntryExisting(t *testing.T) {
	out, err := AddKeyEntry([]byte(withSign), newKey(), "RPM-GPG-KEY-slurm")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(withSign, "      state: retiring   # the bash-era key\n",
		"      state: retiring   # the bash-era key\n    - id: a.admin\n      file: keys/a.admin.asc\n      fingerprint: "+fprA+"\n      state: active\n", 1)
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	s, err := config.ParseSet(out, "repoctl.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Sign.Keys) != 3 || s.Sign.Keys[2].ID != "a.admin" {
		t.Errorf("keys %+v", s.Sign.Keys)
	}
}

func TestAddKeyEntryNoSign(t *testing.T) {
	s := &testkit.Story{Pkgs: testkit.StoryPackages(), Root: "/x"}
	in := s.SetYAML()
	out, err := AddKeyEntry([]byte(in), newKey(), "RPM-GPG-KEY-slurm")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "  url: https://repo.example.org/slurm-26.05\n\nsign:\n  keys:\n    - id: a.admin\n") {
		t.Errorf("sign block not after repo:\n%s", out)
	}
	if !strings.Contains(string(out), "  release:\n    keyfile: RPM-GPG-KEY-slurm\n\ntargets:\n") {
		t.Errorf("release block or blank line missing:\n%s", out)
	}
	// Everything else stays.
	rest := strings.Replace(string(out), "sign:\n  keys:\n    - id: a.admin\n      file: keys/a.admin.asc\n      fingerprint: "+fprA+"\n      state: active\n  release:\n    keyfile: RPM-GPG-KEY-slurm\n\n", "", 1)
	if rest != in {
		t.Errorf("the edit changed other lines:\n%s", rest)
	}
}

func TestAddKeyEntryEmptySignAndRelease(t *testing.T) {
	in := strings.Replace(withSign, "  release:\n    keyfile: RPM-GPG-KEY-slurm     # key file in the release package\n", "", 1)
	out, err := AddKeyEntry([]byte(in), newKey(), "RPM-GPG-KEY-slurm")
	if err != nil {
		t.Fatal(err)
	}
	set, err := config.ParseSet(out, "repoctl.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if set.Sign.Release.Keyfile != "RPM-GPG-KEY-slurm" || len(set.Sign.Keys) != 3 {
		t.Errorf("sign %+v\n%s", set.Sign, out)
	}
	flow := strings.Replace(withSign, "  keys:\n    - id: d.klein\n      file: keys/d.klein.asc\n      fingerprint: 4E7A0C93B85D21F69A0E3C47D1B266F041D80B6E\n      state: active\n    - id: hpc-repo\n      file: keys/hpc-repo.asc\n      fingerprint: 9F2C0B71E4A6C8D0F1A3B5C7D9E1F3A57C3A19F2\n      state: retiring   # the bash-era key\n", "  keys: []\n", 1)
	out, err = AddKeyEntry([]byte(flow), newKey(), "")
	if err != nil {
		t.Fatal(err)
	}
	if set, err = config.ParseSet(out, "repoctl.yaml"); err != nil || len(set.Sign.Keys) != 1 {
		t.Fatalf("flow keys: %v\n%s", err, out)
	}
}

func TestSetKeyState(t *testing.T) {
	out, err := SetKeyState([]byte(withSign), "hpc-repo", config.KeyRemoved)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(withSign, "state: retiring   # the bash-era key", "state: removed    # the bash-era key", 1)
	if string(out) != want {
		t.Errorf("got\n%s", out)
	}
	out, err = SetKeyState([]byte(withSign), "d.klein", config.KeyRetiring)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "      fingerprint: 4E7A0C93B85D21F69A0E3C47D1B266F041D80B6E\n      state: retiring\n") {
		t.Errorf("got\n%s", out)
	}
	if _, err := SetKeyState([]byte(withSign), "nobody", config.KeyRetiring); err == nil {
		t.Error("an unknown id passes")
	}
}
