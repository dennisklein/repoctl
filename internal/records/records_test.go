package records

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/manifest"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestLicenses(t *testing.T) {
	for tag, spdx := range map[string]bool{
		"MIT": true, "GPL-2.0-or-later AND MIT": true, "(MIT OR Apache-2.0)": true, "GPL-2.0-only WITH Classpath-exception-2.0": true,
		"GPLv2+ and BSD": false, "GPLv2+": false, "LGPLv3": false, "MIT AND": false, "": false, "Public Domain": false,
	} {
		if got := IsSPDX(tag); got != spdx {
			t.Errorf("IsSPDX(%q) = %v", tag, got)
		}
	}
	if l := Licenses("MIT"); len(l) != 1 || l[0].Expression != "MIT" {
		t.Errorf("Licenses(MIT) = %+v", l)
	}
	if l := Licenses("GPLv2+"); len(l) != 1 || l[0].License == nil || l[0].License.Name != "GPLv2+" {
		t.Errorf("Licenses(GPLv2+) = %+v", l)
	}
	if Namespace("ghcr.io/GSI-HPC/slurm-repo") != "gsi-hpc" || Namespace("localhost") != "repoctl" {
		t.Error("Namespace")
	}
}

func sbomInput() SBOMInput {
	return SBOMInput{
		Set: "slurm-26.05", Target: "el9", Namespace: "gsi-hpc", Publish: "p-20260912-1403-5e1",
		Created: time.Date(2026, 9, 12, 14, 3, 0, 0, time.UTC), Repoctl: "0.1.0",
		Build: manifest.Build{Package: "munge", Commit: strings.Repeat("a", 40), SRPM: "munge-0.5.18-1.el9.src.rpm"},
		SRPM:  RPMInfo{File: "munge-0.5.18-1.el9.src.rpm", SHA256: strings.Repeat("1", 64), Name: "munge", Version: "0.5.18", Release: "1.el9", Arch: "src", License: "GPL-3.0-or-later", Kind: "srpm"},
		RPMs: []RPMInfo{
			{File: "munge-0.5.18-1.el9.x86_64.rpm", SHA256: strings.Repeat("2", 64), Name: "munge", Version: "0.5.18", Release: "1.el9", Arch: "x86_64", License: "GPLv3+", Kind: "rpm",
				Provides: []string{"munge = 0.5.18-1.el9", "bundled(libfoo) = 1.2"}},
		},
		SourceURIs: map[string]string{"munge-0.5.18.tar.xz": "https://github.com/dun/munge/releases/download/munge-0.5.18/munge-0.5.18.tar.xz"},
	}
}

func TestBuildSBOM(t *testing.T) {
	a, err := Marshal(NewBuildSBOM(sbomInput()))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Marshal(NewBuildSBOM(sbomInput()))
	if !bytes.Equal(a, b) {
		t.Fatal("the SBOM of the same input differs")
	}
	bom, err := ParseBOM(a)
	if err != nil {
		t.Fatal(err)
	}
	if bom.SpecVersion != SpecVersion || !strings.HasPrefix(bom.SerialNumber, "urn:uuid:") {
		t.Errorf("header: %s %s", bom.SpecVersion, bom.SerialNumber)
	}
	main := bom.Metadata.Component
	if main == nil || main.Prop(PropType) != "srpm" || main.Prop(PropFile) != "munge-0.5.18-1.el9.src.rpm" || main.SHA256() != strings.Repeat("1", 64) {
		t.Fatalf("main component: %+v", main)
	}
	types := map[string]Component{}
	for _, c := range bom.Components {
		types[c.Prop(PropType)] = c
	}
	rpm, ok := types["rpm"]
	if !ok || rpm.Prop(PropFile) != "munge-0.5.18-1.el9.x86_64.rpm" || rpm.LicenseText() != "GPLv3+" {
		t.Errorf("rpm component: %+v", rpm)
	}
	if len(rpm.Components) != 1 || rpm.Components[0].Prop(PropType) != "bundled" || rpm.Components[0].Name != "libfoo" || rpm.Components[0].Version != "1.2" {
		t.Errorf("bundled component: %+v", rpm.Components)
	}
	in := sbomInput()
	in.Publish = "p-20260913-0900-aaa"
	c, _ := Marshal(NewBuildSBOM(in))
	if bytes.Equal(a, c) {
		t.Error("the SBOM does not change with the publish")
	}
	if _, err := ParseBOM([]byte(`{"bomFormat":"SPDX"}`)); err == nil {
		t.Error("ParseBOM takes a document that is not CycloneDX")
	}
}

func TestRepositorySBOM(t *testing.T) {
	bom := NewRepositorySBOM(RepoSBOMInput{Set: "slurm-26.05", Target: "el9", Namespace: "gsi-hpc", Store: "ghcr.io/gsi-hpc/slurm-repo",
		Publish: "p-20260912-1403-5e1", Repoctl: "0.1.0", Builds: []BuildSBOMRef{
			{NVR: "slurm-26.05.4-1.el9", Path: "records/slurm-26.05.4-1.el9/sbom.cdx.json", SHA256: strings.Repeat("b", 64)},
			{NVR: "munge-0.5.18-1.el9", Path: "records/munge-0.5.18-1.el9/sbom.cdx.json", SHA256: strings.Repeat("a", 64)},
		}})
	if p := bom.Metadata.Component.Purl; !strings.HasPrefix(p, "pkg:oci/slurm-26.05-el9?") || !strings.Contains(p, "tag=p-20260912-1403-5e1") {
		t.Errorf("purl %s", p)
	}
	if len(bom.Components) != 2 || bom.Components[0].Version != "munge-0.5.18-1.el9" || bom.Components[0].SHA256() != strings.Repeat("a", 64) {
		t.Errorf("components sorted by path: %+v", bom.Components)
	}
}

func TestProvenance(t *testing.T) {
	st := NewProvenance(BuildInput{Set: "slurm-26.05", Target: "el9", Namespace: "gsi-hpc", Repo: "https://github.com/gsi-hpc/slurm-repo",
		Build: manifest.Build{Package: "munge", Branch: "munge/slurm-26.05", Commit: strings.Repeat("a", 40), Run: "r-20260912-1300-abc", SRPM: "munge-0.5.18-1.el9.src.rpm"},
		Facts: Facts{Started: time.Date(2026, 9, 12, 13, 0, 0, 0, time.UTC), Finished: time.Date(2026, 9, 12, 13, 5, 0, 0, time.UTC), Mock: "6.1",
			MockConfig: "rocky+epel-9-x86_64", Repoctl: "0.1.0", User: "d.klein", Host: "hpc-a1"},
		Subjects: []Subject{{Name: "munge-0.5.18-1.el9.src.rpm", SHA256: strings.Repeat("1", 64)}, {Name: "munge-0.5.18-1.el9.x86_64.rpm", SHA256: strings.Repeat("2", 64)}},
	})
	raw, err := Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseStatement(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Subject) != 2 || got.Subject[1].Digest["sha256"] != strings.Repeat("2", 64) {
		t.Errorf("subjects: %+v", got.Subject)
	}
	if got.Predicate.BuildDefinition.BuildType != BuildType || got.Predicate.RunDetails.Builder.ID != BuilderID {
		t.Errorf("build type or builder: %+v", got.Predicate)
	}
	if !strings.HasPrefix(got.Predicate.RunDetails.Metadata.InvocationID, "r-20260912-1300-abc") {
		t.Errorf("invocation id %s", got.Predicate.RunDetails.Metadata.InvocationID)
	}
	if _, err := ParseStatement([]byte(`{"_type":"x"}`)); err == nil {
		t.Error("ParseStatement takes a document that is not a Statement")
	}
}

// TestCheck: the records of a binary repository agree with the
// RepoManifest and the files; a changed record or file is a
// records.mismatch.
func TestCheck(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{"munge-0.5.18-1.el9.src.rpm": strings.Repeat("1", 64), "munge-0.5.18-1.el9.x86_64.rpm": strings.Repeat("2", 64)}
	fileSums := func(base string) (string, bool) { s, ok := files[base]; return s, ok }
	write := func(rel string, b []byte) *manifest.Record {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return &manifest.Record{Path: rel, SHA256: sum(b)}
	}
	nvr := "munge-0.5.18-1.el9"
	prov, _ := Marshal(NewProvenance(BuildInput{Set: "s", Target: "el9", Namespace: "gsi-hpc", Build: manifest.Build{Package: "munge", SRPM: nvr + ".src.rpm", Run: "r-20260912-1300-abc"},
		Subjects: []Subject{{Name: nvr + ".src.rpm", SHA256: files[nvr+".src.rpm"]}, {Name: nvr + ".x86_64.rpm", SHA256: files[nvr+".x86_64.rpm"]}}}))
	sbom, _ := Marshal(NewBuildSBOM(sbomInput()))
	pr, sr := write(ProvenancePath(nvr), prov), write(SBOMPath(nvr), sbom)
	repo, _ := Marshal(NewRepositorySBOM(RepoSBOMInput{Set: "s", Target: "el9", Publish: "p-20260912-1403-5e1", Builds: []BuildSBOMRef{{NVR: nvr, Path: sr.Path, SHA256: sr.SHA256}}}))
	rr := write(RepositorySBOMPath, repo)
	m := &manifest.Manifest{Target: "el9", Builds: []manifest.Build{
		{Package: "munge", SRPM: nvr + ".src.rpm", Records: &manifest.Records{Provenance: pr, SBOM: sr}},
		{Package: "adopted", SRPM: "adopted-1-1.el9.src.rpm", Records: &manifest.Records{SBOM: sr}},
	}, Records: &manifest.Records{SBOM: rr}}
	res := Check(dir, m, fileSums)
	if err := res.Err(); err != nil || res.Records != 4 || res.Adopted != 1 {
		t.Fatalf("Check: %v, %d records, %d adopted, %+v", err, res.Records, res.Adopted, res.Problems)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != api.PRecordsMissing {
		t.Errorf("warnings: %+v", res.Warnings)
	}

	// A subject whose file changed.
	files[nvr+".x86_64.rpm"] = strings.Repeat("3", 64)
	res = Check(dir, m, fileSums)
	if len(res.Problems) < 2 || !strings.Contains(res.Problems[0].Message, "does not have the sha256 of its file") {
		t.Errorf("changed file: %+v", res.Problems)
	}
	e, ok := api.AsError(res.Err())
	if !ok || e.Code != api.PRecordsMismatch {
		t.Errorf("Err: %v", res.Err())
	}
	files[nvr+".x86_64.rpm"] = strings.Repeat("2", 64)

	// A changed record.
	write(ProvenancePath(nvr), append(prov, ' '))
	res = Check(dir, m, fileSums)
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0].Message, "does not have the sha256 of the RepoManifest") {
		t.Errorf("changed record: %+v", res.Problems)
	}
}
