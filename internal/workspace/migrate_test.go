package workspace

import (
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/config"
)

// commonSH is the head of scripts/common.sh of the bash scripts.
const commonSH = `#!/bin/bash
__DIR__="$( cd "$( dirname "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"
set -e
pkgdir=$(readlink -f ${__DIR__}/../packages)
repobasename=slurm
reponame=${repobasename}
os=rocky+epel
releasever=9
arch=$(rpm --eval "%{_arch}") # x86_64
mockroot=${os}-${releasever}-${arch}
releasedir=el${releasever}
function status() {
  local releasedir=nope
}
`

func TestShellVarsAndTarget(t *testing.T) {
	v := shellVars([]byte(commonSH))
	for k, want := range map[string]string{"reponame": "slurm", "mockroot": "rocky+epel-9-x86_64", "releasedir": "el9", "pkgdir": ""} {
		if v[k] != want {
			t.Errorf("%s = %q, want %q", k, v[k], want)
		}
	}
	tg := migrateTarget(v)
	if tg.ID != "el9" || tg.Mock != "rocky+epel-9-x86_64" || tg.Path != "el9" {
		t.Errorf("target %+v", tg)
	}
	// Values that repoctl.yaml does not take give the default target.
	tg = migrateTarget(map[string]string{"releasedir": "/srv/x", "mockroot": "a b"})
	if tg.ID != DefaultTarget.ID || tg.Mock != DefaultTarget.Mock {
		t.Errorf("default target %+v", tg)
	}
}

func TestParseBashLock(t *testing.T) {
	lines := parseBashLock([]byte("# dir   branch   sha\nmunge  munge/slurm-26.05  " + strings.Repeat("a", 40) +
		"\n\npackages/ucx/ ucx/slurm-26.05 " + strings.Repeat("b", 40) + "\nbroken line\n"))
	if len(lines) != 2 || lines[0].dir != "munge" || lines[1].dir != "ucx" || lines[1].branch != "ucx/slurm-26.05" {
		t.Errorf("lines %+v", lines)
	}
}

func TestBuildLines(t *testing.T) {
	order, commented := buildLines([]byte(`#!/bin/bash
source common.sh
function build() {
  build_one $1
}
build munge
# build pmix_v4
build ucx
`))
	if strings.Join(order, " ") != "munge ucx" || strings.Join(commented, " ") != "pmix_v4" {
		t.Errorf("order %v, commented %v", order, commented)
	}
}

func TestParseFrom(t *testing.T) {
	for in, want := range map[string][3]string{
		"fedora":           {"", "true", "rawhide"},
		"fedora:f43":       {"", "true", "f43"},
		"pmix/slurm-26.05": {"pmix/slurm-26.05", "false", ""},
	} {
		ref, fedora, branch := ParseFrom(in)
		if ref != want[0] || (fedora && want[1] != "true") || (!fedora && want[1] != "false") || branch != want[2] {
			t.Errorf("ParseFrom(%q) = %q %v %q", in, ref, fedora, branch)
		}
	}
}

func TestRenderParses(t *testing.T) {
	d := Draft{
		Name: "slurm-26.05", Repo: "slurm", Remote: "origin",
		Targets: []config.Target{DefaultTarget},
		Packages: []config.Package{
			{ID: "munge", Branch: "munge/slurm-26.05"},
			{ID: "slurm-release", Path: "packages/slurm-release", Role: "release"},
		},
	}
	data := d.Render()
	set, err := config.ParseSet(data, "repoctl.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if set.Name != "slurm-26.05" || len(set.Packages) != 2 || set.Packages[1].Role != "release" {
		t.Errorf("parsed %+v\n%s", set, data)
	}
	// Without packages the file has an empty list and parses.
	d.Packages = nil
	if _, err := config.ParseSet(d.Render(), "repoctl.yaml"); err != nil {
		t.Errorf("empty packages: %v\n%s", err, d.Render())
	}
	for in, want := range map[string]string{"el9": "el9", "yes": "'yes'", "1.0": "'1.0'", "a b": "'a b'", "it's": "'it''s'"} {
		if got := scalar(in); got != want {
			t.Errorf("scalar(%q) = %s, want %s", in, got, want)
		}
	}
}
