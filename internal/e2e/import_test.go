//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// bashEraTree writes a served tree of the bash-era scripts: the three repos
// with packages/, signed RPMs, signed repodata, the RPM-GPG-KEY file of each
// repo, the root link slurm-release.rpm and an old RPM in packages/ that the
// repodata does not list.
func bashEraTree(t *testing.T, s *testkit.Story, builds ...string) (dir string, srpms, rpms map[string][]string) {
	t.Helper()
	dir = filepath.Join(s.Root, "served", "el9")
	srpms, rpms = map[string][]string{}, map[string][]string{}
	run := func(wd, name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir, cmd.Env = wd, s.Env()
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, b)
		}
	}
	for _, repo := range []string{"slurm", "slurm-source", "slurm-debuginfo"} {
		if err := os.MkdirAll(filepath.Join(dir, repo, "packages"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var signed []string
	for _, b := range builds {
		files, _ := filepath.Glob(filepath.Join(b, "*.rpm"))
		for _, f := range files {
			name := filepath.Base(f)
			repo := "slurm"
			if strings.HasSuffix(name, ".src.rpm") {
				repo = "slurm-source"
			}
			dst := filepath.Join(dir, repo, "packages", name)
			writeFile(t, dst, readFile(t, f))
			signed = append(signed, dst)
			if repo == "slurm-source" {
				srpms[b] = append(srpms[b], name)
			} else {
				rpms[b] = append(rpms[b], name)
			}
		}
	}
	run(dir, "rpmsign", append([]string{"--define", "_gpg_name d.klein", "--addsign"}, signed...)...)
	// An old build that the repodata no longer lists.
	old := filepath.Join(dir, "slurm", "packages", "hello-0.9-1.el9.x86_64.rpm")
	writeFile(t, old, readFile(t, signed[0]))
	for _, repo := range []string{"slurm", "slurm-source", "slurm-debuginfo"} {
		rd := filepath.Join(dir, repo)
		run(rd, "createrepo_c", "-q", "--excludes", "hello-0.9-*", ".")
		run(rd, "gpg", "--batch", "--yes", "-u", "d.klein", "--detach-sign", "--armor", "repodata/repomd.xml")
		run(rd, "sh", "-c", "gpg --armor --export d.klein > RPM-GPG-KEY")
	}
	var rel string
	for _, f := range signed {
		if b := filepath.Base(f); strings.HasPrefix(b, "slurm-release-") && !strings.HasSuffix(b, ".src.rpm") {
			rel = b
		}
	}
	if err := os.Symlink("slurm/packages/"+rel, filepath.Join(dir, "slurm-release.rpm")); err != nil {
		t.Fatal(err)
	}
	return dir, srpms, rpms
}

func TestRepoPushImport(t *testing.T) {
	s := publishStory(t)
	hello := fakeBuild(t, s, cloneBranch(t, s, "hello/slurm-26.05"), "hello.spec", filepath.Join(s.Root, "b", "hello"))
	rel := fakeBuild(t, s, filepath.Join(s.SetDir, "packages/slurm-release"), "slurm-release.spec", filepath.Join(s.Root, "b", "rel"))
	served, srpms, rpms := bashEraTree(t, s, hello, rel)

	// The import copy, as the runbook makes it.
	imp := filepath.Join(s.Root, "import", "el9")
	if err := os.MkdirAll(filepath.Dir(imp), 0o755); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("cp", "-a", served, imp).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v\n%s", err, b)
	}

	inTree := strings.TrimSpace(s.Git(s.SetDir, "log", "-1", "--format=%H", "--", "packages/slurm-release"))
	// hello: adopted-unmapped with commit null, and package null as for a
	// retired name. slurm-release: adopted.
	m := map[string]any{
		"apiVersion": "repoctl/v1alpha1", "kind": "RepoManifest", "set": testkit.SetName, "target": "el9",
		"publish": "p-import-20260828", "removed": []any{}, "history": []any{},
		"builds": []map[string]any{
			{"package": nil, "commit": nil, "url": nil, "reason": "adopted-unmapped",
				"srpm": srpms[hello][0], "rpms": rpms[hello], "built": "2026-06-02T09:41:17Z", "publish": "p-import-20260828",
				"mockConfig": nil, "macros": nil, "buildroot": nil, "buildrootFile": nil, "run": nil},
			{"package": "slurm-release", "commit": inTree, "url": nil, "reason": "adopted",
				"srpm": srpms[rel][0], "rpms": rpms[rel], "built": "2026-06-02T09:41:17Z",
				"mockConfig": nil, "macros": nil, "buildroot": nil, "buildrootFile": nil, "run": nil},
		},
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mf := filepath.Join(s.Root, "import", "el9.manifest.json")
	writeFile(t, mf, string(mb))

	// Gate 2: the release package of the clients holds d.klein.
	out, err := inTerminal(t, s, "d.klein\n", "sign", "deployed", "d.klein", "--release", strings.SplitN(rpms[rel][0], ".el9", 2)[0],
		"--target", "el9", "--path", imp, "--note", "bash-era")
	if err != nil {
		t.Fatalf("sign deployed: %v\n%s", err, out)
	}

	s.Git(s.SetDir, "push", "-q", "origin", testkit.SetBranch)

	s.Setenv(publishUnattended, "1")
	r := s.Run("repo", "push", "--target", "el9", "--from", imp, "--manifest", mf, "--yes")
	t.Logf("repo push --from:\n%s---\n%s", r.Stdout, r.Stderr)
	if r.Code != 0 {
		t.Fatalf("repo push --from: exit %d", r.Code)
	}

	// The snapshot holds the RPMs of the manifest and nothing else.
	paths := config.NewPaths(os.Getenv)
	var got []string
	lc := paths.LocalCopy(testkit.SetName, "el9")
	_ = filepath.WalkDir(lc, func(p string, d os.DirEntry, err error) error {
		// The root files (TREE/*.rpm) are the links of the release package.
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".rpm") && filepath.Dir(p) != lc {
			got = append(got, filepath.Base(p))
		}
		return nil
	})
	var want []string
	for _, b := range []string{hello, rel} {
		want = append(want, srpms[b]...)
		want = append(want, rpms[b]...)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("RPMs of the import snapshot:\n got %v\nwant %v", got, want)
	}
	// A build without publish gets the id of the import.
	mm := repoManifest(t, lc)
	if !strings.Contains(string(mm), `"publish": "p-import-`) || strings.Count(string(mm), `"publish": "p-import-20260828"`) != 1 {
		t.Errorf("RepoManifest of the snapshot:\n%s", mm)
	}
	if r := s.Run("repo", "verify", "--target", "el9"); r.Code != 0 {
		t.Errorf("repo verify: exit %d\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	// The adopted node is current; an adopted-unmapped entry makes no node
	// current.
	if r := s.Run("plan", "--target", "el9"); !strings.Contains(r.Stdout, "1 current: slurm-release") {
		t.Errorf("plan: exit %d\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
}
