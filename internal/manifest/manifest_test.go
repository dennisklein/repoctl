package manifest

import (
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	m := New("slurm-26.05", "el9")
	m.Builds = append(m.Builds, Build{Package: "ucx", Commit: "4b1e9c2", Reason: "commit-changed", SRPM: "ucx-1.22.1-1.el9.src.rpm", RPMs: []string{"ucx-1.22.1-1.el9.x86_64.rpm"}})
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if bld, ok := got.Find("4b1e9c2", "ucx"); !ok || bld.SRPM != "ucx-1.22.1-1.el9.src.rpm" {
		t.Fatalf("find: %v %v", bld, ok)
	}
	if _, ok := got.Find("4b1e9c2", "slurm"); ok {
		t.Fatal("find by other name")
	}
	if _, err := Parse([]byte(`{"apiVersion":"repoctl/v9","kind":"RepoManifest"}`)); err == nil {
		t.Fatal("newer apiVersion accepted")
	}
}

func TestNVR(t *testing.T) {
	if n := NameOfSRPM("slurm-singularity-exec-3.2.0-2.el9.src.rpm"); n != "slurm-singularity-exec" {
		t.Fatal(n)
	}
	if nvr := NVR("ucx-devel-1.22.1-1.el9.x86_64.rpm"); nvr != "ucx-devel-1.22.1-1.el9" {
		t.Fatal(nvr)
	}
}

// TestParseValidates checks that Parse refuses a manifest that does not
// agree with the schema manifest, and names the field (model.html#manifest).
func TestParseValidates(t *testing.T) {
	cases := map[string]string{
		"missing commit": `{"apiVersion":"repoctl/v1alpha1","kind":"RepoManifest","set":"s","target":"el9","publish":"p","builds":[{"package":"ucx","url":null,"reason":"new-package","srpm":"ucx-1-1.src.rpm","rpms":[]}],"removed":[],"history":[]}`,
		"bad reason":     `{"apiVersion":"repoctl/v1alpha1","kind":"RepoManifest","set":"s","target":"el9","publish":"p","builds":[{"package":"ucx","url":null,"commit":"4b1e9c2","reason":"because","srpm":"ucx-1-1.src.rpm","rpms":[]}],"removed":[],"history":[]}`,
		"unknown field":  `{"apiVersion":"repoctl/v1alpha1","kind":"RepoManifest","set":"s","target":"el9","publish":"p","builds":[],"removed":[],"history":[],"extra":1}`,
	}
	for name, doc := range cases {
		_, err := Parse([]byte(doc))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "schema manifest") {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err := Parse([]byte(cases["missing commit"]))
	if !strings.Contains(err.Error(), "/builds/0") {
		t.Errorf("the error does not name the field: %v", err)
	}
}
