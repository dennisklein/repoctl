package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func touch(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestParseFile(t *testing.T) {
	r, err := ParseFile("repoctl-mirror-0.4.0~rc.1-1.noarch.rpm")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "repoctl-mirror" || r.Version != "0.4.0~rc.1" || r.Release != "1" || r.Arch != "noarch" {
		t.Errorf("got %+v", r)
	}
	for _, bad := range []string{"repoctl.tar", "repoctl.rpm", "repoctl-1.x86_64.rpm"} {
		if _, err := ParseFile(bad); err == nil {
			t.Errorf("ParseFile(%q) gives no error", bad)
		}
	}
	if got := RPMVersion("v0.4.0-rc.1"); got != "0.4.0~rc.1" {
		t.Errorf("RPMVersion = %s", got)
	}
}

func TestAssemble(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old")
	// The old tree: Fedora 42 is end of life now, 43 stays; 9 is EL.
	for _, f := range []string{
		"rpm/42/x86_64/repoctl-0.3.0-1.fc42.x86_64.rpm",
		"rpm/43/x86_64/repoctl-0.1.4-1.fc43.x86_64.rpm",
		"rpm/43/x86_64/repoctl-0.2.0-1.fc43.x86_64.rpm",
		"rpm/43/x86_64/repoctl-0.2.1-1.fc43.x86_64.rpm",
		"rpm/43/x86_64/repoctl-0.3.0-1.fc43.x86_64.rpm",
		"rpm/43/x86_64/repoctl-release-1-1.noarch.rpm",
		"rpm/43/x86_64/repoctl-mirror-0.3.0-1.noarch.rpm",
		"rpm/9/x86_64/repoctl-release-1-1.noarch.rpm",
		"rpm/9/x86_64/repoctl-mirror-0.3.0-1.noarch.rpm",
		"rpm/repoctl-release-1-1.noarch.rpm",
	} {
		touch(t, filepath.Join(old, f), "old "+filepath.Base(f))
	}
	// The new RPMs of 0.4.0: repoctl-release has the same NVR, so the old
	// file stays.
	news := filepath.Join(dir, "new")
	for _, f := range []string{
		"repoctl-0.4.0-1.fc43.x86_64.rpm",
		"repoctl-0.4.0-1.fc44.x86_64.rpm",
		"repoctl-release-1-1.noarch.rpm",
		"repoctl-mirror-0.4.0-1.noarch.rpm",
	} {
		touch(t, filepath.Join(news, f), "new "+f)
	}
	out := filepath.Join(dir, "out")
	added, err := Assemble(Options{Old: old, New: news, Out: out, Fedora: []string{"43", "44"}, EL: []string{"9", "10"}, KeepMinors: 3})
	if err != nil {
		t.Fatal(err)
	}
	wantAdded := []string{"repoctl-0.4.0-1.fc43.x86_64.rpm", "repoctl-0.4.0-1.fc44.x86_64.rpm", "repoctl-mirror-0.4.0-1.noarch.rpm"}
	if !slices.Equal(added, wantAdded) {
		t.Errorf("added = %v, want %v", added, wantAdded)
	}
	want := []string{
		"rpm/10/x86_64/repoctl-mirror-0.4.0-1.noarch.rpm",
		"rpm/10/x86_64/repoctl-release-1-1.noarch.rpm",
		"rpm/43/x86_64/repoctl-0.2.1-1.fc43.x86_64.rpm",
		"rpm/43/x86_64/repoctl-0.3.0-1.fc43.x86_64.rpm",
		"rpm/43/x86_64/repoctl-0.4.0-1.fc43.x86_64.rpm",
		"rpm/43/x86_64/repoctl-mirror-0.4.0-1.noarch.rpm",
		"rpm/43/x86_64/repoctl-release-1-1.noarch.rpm",
		"rpm/44/x86_64/repoctl-0.4.0-1.fc44.x86_64.rpm",
		"rpm/44/x86_64/repoctl-mirror-0.4.0-1.noarch.rpm",
		"rpm/44/x86_64/repoctl-release-1-1.noarch.rpm",
		"rpm/9/x86_64/repoctl-mirror-0.4.0-1.noarch.rpm",
		"rpm/9/x86_64/repoctl-release-1-1.noarch.rpm",
		"rpm/repoctl-release-1-1.noarch.rpm",
	}
	if got := listTree(t, out); !slices.Equal(got, want) {
		t.Errorf("tree:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, f := range []string{"rpm/44/x86_64/repoctl-release-1-1.noarch.rpm", "rpm/repoctl-release-1-1.noarch.rpm"} {
		b, err := os.ReadFile(filepath.Join(out, f))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "old repoctl-release-1-1.noarch.rpm" {
			t.Errorf("%s = %q: a released file must keep its bytes", f, b)
		}
	}

	// The yank job: 0.4.0 is bad; the tree goes back to 0.3.0.
	yank := filepath.Join(dir, "yanked.txt")
	touch(t, yank, "# bad releases\nv0.4.0  # broke the publish\n")
	out2 := filepath.Join(dir, "out2")
	added, err = Assemble(Options{Old: out, New: filepath.Join(dir, "empty"), Out: out2, Yanked: yank, Fedora: []string{"43", "44"}, EL: []string{"9", "10"}, KeepMinors: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Errorf("the yank job added %v", added)
	}
	got := listTree(t, out2)
	for _, f := range got {
		if strings.Contains(f, "0.4.0") {
			t.Errorf("the yanked %s is in the tree", f)
		}
	}
	// 0.1.4 left the tree before; a yank does not bring it back.
	if slices.Contains(got, "rpm/43/x86_64/repoctl-0.1.4-1.fc43.x86_64.rpm") {
		t.Error("the yank brought back 0.1.4")
	}
	if slices.Contains(got, "rpm/44/x86_64/repoctl-mirror-0.4.0-1.noarch.rpm") || !slices.Contains(got, "rpm/43/x86_64/repoctl-0.3.0-1.fc43.x86_64.rpm") {
		t.Errorf("tree after the yank:\n%s", strings.Join(got, "\n"))
	}

	if _, err := Assemble(Options{Old: old, New: news, Out: out, Fedora: []string{"43"}, KeepMinors: 3}); err == nil {
		t.Error("Assemble into an existing directory gives no error")
	}
	touch(t, filepath.Join(dir, "bad", "slurm-24.11-1.el9.x86_64.rpm"), "")
	if _, err := Assemble(Options{New: filepath.Join(dir, "bad"), Out: filepath.Join(dir, "out3"), Fedora: []string{"43"}, KeepMinors: 3}); err == nil {
		t.Error("Assemble takes a package that is not repoctl")
	}
}

func TestKeys(t *testing.T) {
	var buf bytes.Buffer
	if err := run([]string{"keys"}, &buf); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"nope"}, &buf); err == nil {
		t.Error("an unknown command gives no error")
	}
}

// TestScripts checks the shell scripts of packaging/: bash -n always,
// shellcheck when it is on the host (CI has it).
func TestScripts(t *testing.T) {
	scripts, err := filepath.Glob(filepath.Join("..", "*.sh"))
	if err != nil || len(scripts) == 0 {
		t.Fatalf("no scripts: %v", err)
	}
	for _, s := range scripts {
		if out, err := exec.Command("bash", "-n", s).CombinedOutput(); err != nil {
			t.Errorf("bash -n %s: %v\n%s", s, err, out)
		}
	}
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Log("shellcheck is not installed")
		return
	}
	// From the repository root: the "# shellcheck source=" paths start there.
	args := []string{"-x"}
	for _, s := range scripts {
		args = append(args, filepath.Join("packaging", filepath.Base(s)))
	}
	cmd := exec.Command("shellcheck", args...)
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}
