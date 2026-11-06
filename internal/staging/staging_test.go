package staging

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/publish"
)

// buildRPMs makes a binary RPM, a debuginfo-like RPM and an SRPM of a tiny
// spec with rpmbuild.
func buildRPMs(t *testing.T, name string) []string {
	t.Helper()
	if _, err := exec.LookPath("rpmbuild"); err != nil {
		t.Skip("rpmbuild is not installed")
	}
	if _, err := exec.LookPath("createrepo_c"); err != nil {
		t.Skip("createrepo_c is not installed")
	}
	top := t.TempDir()
	spec := filepath.Join(top, name+".spec")
	text := "Name: " + name + "\nVersion: 1.0\nRelease: 1\nSummary: s\nLicense: MIT\nBuildArch: noarch\n%description\nd\n" +
		"%package debuginfo\nSummary: d\n%description debuginfo\nd\n%install\nmkdir -p %{buildroot}/usr/share/" + name +
		"\necho x > %{buildroot}/usr/share/" + name + "/x\n%files\n/usr/share/" + name + "\n%files debuginfo\n"
	if err := os.WriteFile(spec, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("rpmbuild", "--define", "_topdir "+top, "--define", "_build_name_fmt %%{NAME}-%%{VERSION}-%%{RELEASE}.%%{ARCH}.rpm", "-ba", spec)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rpmbuild: %v\n%s", err, out)
	}
	var out []string
	_ = filepath.WalkDir(top, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".rpm" {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func testDir(t *testing.T) Dir {
	home := t.TempDir()
	p := config.NewPaths(func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	})
	set := &config.Set{Repo: config.Repo{Name: "slurm"}}
	return Dir{Paths: p, Set: "s", Run: "r-20261102-1750-a3f", Target: "el9", Repos: ReposOf(set)}
}

func TestStageAndIndex(t *testing.T) {
	rpms := buildRPMs(t, "tiny")
	ctx := context.Background()
	d := testDir(t)
	if err := d.Init(ctx); err != nil {
		t.Fatal(err)
	}
	files, err := d.Copy(rpms)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("staged %d files: %+v", len(files), files)
	}
	kinds := map[string]string{}
	for _, f := range files {
		kinds[f.Kind] = f.Repo
		if _, err := os.Stat(f.Path); err != nil {
			t.Fatal(err)
		}
	}
	if kinds["srpm"] != "slurm-source" || kinds["debuginfo"] != "slurm-debuginfo" || kinds["rpm"] != "slurm" {
		t.Fatalf("kinds %v", kinds)
	}
	rd, err := Createrepo(ctx, d.Repo("slurm"))
	if err != nil {
		t.Fatal(err)
	}
	if rd.Packages != 1 || rd.Revision == "" {
		t.Fatalf("repodata %+v", rd)
	}
	// A second update keeps a working link.
	rd2, err := Createrepo(ctx, d.Repo("slurm"))
	if err != nil || rd2.Packages != 1 {
		t.Fatalf("update %+v %v", rd2, err)
	}
	if _, err := os.Stat(filepath.Join(d.Repo("slurm"), "repodata", "repomd.xml")); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteDraft("tiny", publish.Draft{Build: manifest.Build{Package: "tiny", Commit: "c", SRPM: "tiny-1.0-1.src.rpm"}}); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadDraft(d.Paths, d.Set, d.Run, d.Target, "tiny"); err != nil || b.Build.SRPM != "tiny-1.0-1.src.rpm" {
		t.Fatalf("draft %+v %v", b, err)
	}

	var arts []api.Artifact
	for _, f := range files {
		arts = append(arts, api.Artifact{Kind: f.Kind, Repo: f.Repo, File: f.File, Path: f.Path, SizeBytes: int(f.Size), Sha256: f.SHA256})
	}
	run := &api.Run{ID: d.Run, Kind: "build", Selection: &api.Selection{From: "lock"}, Nodes: []api.NodeResult{
		{Node: "tiny@el9", State: api.NodeStateOk, Commit: "c1", SRPM: "tiny-1.0-1.src.rpm", Artifacts: arts, FinishedAt: "2026-11-02T17:55:00.000Z"},
		{Node: "other@el9", State: api.NodeStateFailed, Commit: "c2"},
	}}
	x := NewIndex()
	x.AddRun(run)
	if sb, ok := x.Find("el9", "tiny", "c1"); !ok || sb.Missing || sb.Run != d.Run || sb.SRPM != "tiny-1.0-1.src.rpm" {
		t.Fatalf("find %+v %v", sb, ok)
	}
	if _, ok := x.Find("el9", "other", "c2"); ok {
		t.Fatal("a failed node is staged")
	}
	// An unpinned run never stages.
	y := NewIndex()
	y.AddRun(&api.Run{ID: "r-x", Kind: "build", Unpinned: true, Selection: &api.Selection{From: "worktree"}, Nodes: run.Nodes})
	if _, ok := y.Find("el9", "tiny", "c1"); ok {
		t.Fatal("an unpinned run stages")
	}
	// The staged view links the binary RPMs.
	view := filepath.Join(t.TempDir(), "view")
	e, _ := x.Lookup("el9", "tiny", "c1")
	if ok, err := MakeView(ctx, view, []Entry{e}); err != nil || !ok {
		t.Fatalf("view %v %v", ok, err)
	}
	// A file that is gone makes the build not intact.
	os.Remove(files[0].Path)
	z := NewIndex()
	z.AddRun(run)
	if sb, ok := z.Find("el9", "tiny", "c1"); !ok || !sb.Missing {
		t.Fatal("a staging with a missing file is intact")
	}
	if e, ok := z.Lookup("el9", "tiny", "c1"); !ok || e.Intact() {
		t.Fatal("lookup of a broken staging")
	}
}
