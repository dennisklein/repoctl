package manifest

import (
	"encoding/json"
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

// An import writes the null fields of adopted manifest entries
// (lifecycle.html#adopted); they survive a round trip.
func TestAdoptedNulls(t *testing.T) {
	in := `{"apiVersion": "repoctl/v1alpha1", "kind": "RepoManifest", "set": "slurm-26.05", "target": "el9",
	  "publish": "p-import-20260828", "removed": [], "history": [], "builds": [
	  {"package": "munge", "commit": "3a244fd4728f94342beba4a7634c463370a7b877", "url": null, "branch": "munge/slurm-26.05",
	   "srpm": "munge-0.5.18-1.el9.src.rpm", "rpms": ["munge-0.5.18-1.el9.x86_64.rpm"], "reason": "adopted",
	   "mockConfig": null, "macros": null, "buildroot": null, "buildrootFile": null, "run": null},
	  {"package": "munge", "commit": null, "url": null, "srpm": "munge-0.5.17-1.el9.src.rpm",
	   "rpms": ["munge-0.5.17-1.el9.x86_64.rpm"], "reason": "adopted-unmapped"},
	  {"package": null, "commit": null, "url": null, "srpm": "old-1.0-1.el9.src.rpm",
	   "rpms": ["old-1.0-1.el9.x86_64.rpm"], "reason": "adopted-unmapped"}]}`
	m, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if m.Builds[1].Commit != "" || m.Builds[2].Package != "" {
		t.Errorf("builds: %+v", m.Builds)
	}
	out, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"commit": null`) || !strings.Contains(string(out), `"package": null`) {
		t.Errorf("Marshal:\n%s", out)
	}
	if _, err := Parse(out); err != nil {
		t.Errorf("Parse of the Marshal output: %v\n%s", err, out)
	}
	// Only adopted-unmapped may lack the commit.
	m.Builds[1].Reason = "adopted"
	if err := m.Validate(); err == nil {
		t.Error("Validate: an adopted build without a commit passes")
	}
}

// MarshalJSON of Build writes the fields of Build, in their order.
func TestBuildMarshalFields(t *testing.T) {
	b := Build{Package: "p", Branch: "b", Commit: "abcdef1", Reason: "adopted", SRPM: "p-1-1.src.rpm", RPMs: []string{"p-1-1.x86_64.rpm"},
		Provides: []string{"x"}, Requires: []string{"y"}, MockConfig: "m", Macros: "m", Buildroot: "m", BuildrootFile: "f", Run: "r",
		Built: "t", Publish: "p", Signer: "s", Records: &Records{}, Sources: map[string]string{"a": "b"}}
	got, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	type plain Build
	want, _ := json.Marshal(plain(b))
	if string(got) != string(want) {
		t.Errorf("MarshalJSON:\n got %s\nwant %s", got, want)
	}
}
