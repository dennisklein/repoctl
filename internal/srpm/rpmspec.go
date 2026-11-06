package srpm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/tools/run"
)

// rpmspec runs rpmspec on the host with the macros of the chroot of the
// target, so that %if 0%{?rhel} >= 10 in a spec reads as in the chroot: the
// macros of the mock config (dist, rhel, elN), the macro file of the
// target, and the fallbacks of the rpmautospec macros (the release does
// not change a Provides name). %fedora of a Fedora host is undefined.
func rpmspec(ctx context.Context, t *Target, pkgDir string, args ...string) ([]byte, error) {
	a := []string{"--undefine", "fedora"}
	if t.Arch != "" {
		a = append(a, "--target", t.Arch)
	}
	for _, kv := range t.Macros {
		a = append(a, "--define", kv[0]+" "+kv[1])
	}
	if t.Files.Macros != "" {
		a = append(a, "--load", t.Files.Macros)
	}
	a = append(a,
		"--define", "_sourcedir "+pkgDir,
		"--define", "_specdir "+pkgDir,
		"--define", "autorelease 1%{?dist}",
		"--define", "autochangelog %{nil}",
	)
	a = append(a, args...)
	return (&run.Cmd{Name: "rpmspec", Args: a, Dir: pkgDir}).Output(ctx)
}

// FindSpec gives the spec file of a package directory: the only *.spec, or
// PKG.spec when there are several.
func FindSpec(pkgDir, pkg string) (string, error) {
	m, _ := filepath.Glob(filepath.Join(pkgDir, "*.spec"))
	switch len(m) {
	case 0:
		return "", fmt.Errorf("%s has no spec file", pkgDir)
	case 1:
		return m[0], nil
	}
	for _, s := range m {
		if filepath.Base(s) == pkg+".spec" {
			return s, nil
		}
	}
	names := make([]string, len(m))
	for i, s := range m {
		names[i] = filepath.Base(s)
	}
	return "", fmt.Errorf("%s has %d spec files (%s) and none is %s.spec", pkgDir, len(m), strings.Join(names, ", "), pkg)
}

var reAutospec = regexp.MustCompile(`%(\{\??)?(autorelease|autochangelog)\b`)

// UsesAutospec reports whether a spec text uses %autorelease or
// %autochangelog.
func UsesAutospec(spec []byte) bool { return reAutospec.Match(spec) }

// specFacts are the facts of a spec that rpmspec gives in the chroot of a
// target.
type specFacts struct {
	Name     string   // Name of the source package
	Version  string   // Version
	RPMs     []string // the binary packages that the spec builds
	Provides []string // the capabilities of the binary packages, without version
	Sources  []SpecSource
	Dynamic  bool // %generate_buildrequires
}

// readSpec reads the sources and the provides of a spec with rpmspec on the
// host (build.html#graph: rpmspec reads the Provides with the macros of the
// target).
func readSpec(ctx context.Context, t *Target, pkgDir, spec string) (*specFacts, error) {
	parsed, err := rpmspec(ctx, t, pkgDir, "-P", spec)
	if err != nil {
		return nil, fmt.Errorf("rpmspec -P %s: %w", filepath.Base(spec), err)
	}
	f := &specFacts{Sources: ParseSpecSources(string(parsed))}
	for _, l := range strings.Split(string(parsed), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "%generate_buildrequires") {
			f.Dynamic = true
		}
	}
	src, err := rpmspec(ctx, t, pkgDir, "-q", "--srpm", "--qf", "%{NAME}\t%{VERSION}\n", spec)
	if err != nil {
		return nil, fmt.Errorf("rpmspec -q --srpm %s: %w", filepath.Base(spec), err)
	}
	if fs := strings.Split(strings.TrimSpace(string(src)), "\t"); len(fs) == 2 {
		f.Name, f.Version = fs[0], fs[1]
	}
	out, err := rpmspec(ctx, t, pkgDir, "-q", "--builtrpms", "--qf", "P\t%{NAME}\n[C\t%{PROVIDENAME}\n]", spec)
	if err != nil {
		return nil, fmt.Errorf("rpmspec -q --provides %s: %w", filepath.Base(spec), err)
	}
	prov := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(l, "\t")
		if !ok || v == "" {
			continue
		}
		switch k {
		case "P":
			f.RPMs = append(f.RPMs, v)
			prov[v] = true
		case "C":
			prov[v] = true
		}
	}
	f.Provides = sortedKeys(prov)
	return f, nil
}

// srpmFacts are the facts of the header of an SRPM.
type srpmFacts struct {
	Name, Epoch, Version, Release string
	Requires                      []string
	Dynamic                       bool
}

// readSRPM reads the header and the requires of an SRPM (rpm -qp).
func readSRPM(ctx context.Context, path string) (*srpmFacts, error) {
	out, err := (&run.Cmd{Name: "rpm", Args: []string{"-qp", "--nosignature", "--nodigest", "--qf",
		"H\t%{NAME}\t%{EPOCHNUM}\t%{VERSION}\t%{RELEASE}\n[R\t%{REQUIRENAME}\n]", path}}).Output(ctx)
	if err != nil {
		return nil, err
	}
	f := &srpmFacts{}
	req := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		fs := strings.Split(l, "\t")
		switch {
		case len(fs) == 5 && fs[0] == "H":
			f.Name, f.Epoch, f.Version, f.Release = fs[1], fs[2], fs[3], fs[4]
		case len(fs) == 2 && fs[0] == "R":
			r := fs[1]
			if strings.HasPrefix(r, "rpmlib(") {
				if r == "rpmlib(DynamicBuildRequires)" {
					f.Dynamic = true
				}
				continue
			}
			req[r] = true
		}
	}
	if f.Name == "" {
		return nil, fmt.Errorf("rpm -qp %s: no header", filepath.Base(path))
	}
	f.Requires = sortedKeys(req)
	return f, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
