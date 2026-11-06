package sign

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/testkit"
)

// gnupgHome gives a short temporary GnuPG home with the secret keys.
func gnupgHome(t *testing.T, keys ...*testkit.TestKey) string {
	t.Helper()
	home, err := os.MkdirTemp("", "sg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run()
		_ = os.RemoveAll(home)
	})
	testkit.ImportKeys(t, home, keys...)
	return home
}

// buildRPM builds a noarch RPM with the files (client path -> content) and
// gives its path.
func buildRPM(t *testing.T, name, version, release string, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("rpmbuild"); err != nil {
		t.Skip("rpmbuild is not installed")
	}
	top := t.TempDir()
	for _, d := range []string{"SPECS", "SOURCES", "BUILD", "RPMS", "SRPMS"} {
		if err := os.MkdirAll(filepath.Join(top, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var install, list strings.Builder
	i := 0
	for path, content := range files {
		src := fmt.Sprintf("src%d", i)
		i++
		if err := os.WriteFile(filepath.Join(top, "SOURCES", src), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&install, "install -D -m 0644 %%{_sourcedir}/%s %%{buildroot}%s\n", src, path)
		fmt.Fprintf(&list, "%s\n", path)
	}
	spec := fmt.Sprintf(`Name: %s
Version: %s
Release: %s
Summary: test
License: MIT
BuildArch: noarch
%%description
test
%%install
%s
%%files
%s
%%changelog
`, name, version, release, install.String(), list.String())
	sp := filepath.Join(top, "SPECS", name+".spec")
	if err := os.WriteFile(sp, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("rpmbuild", "-bb", "--define", "_topdir "+top, sp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rpmbuild: %v\n%s", err, out)
	}
	m, _ := filepath.Glob(filepath.Join(top, "RPMS", "noarch", "*.rpm"))
	if len(m) != 1 {
		t.Fatalf("rpmbuild made %d RPMs", len(m))
	}
	return m[0]
}
