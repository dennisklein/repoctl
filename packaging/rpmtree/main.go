// Command rpmtree assembles the RPM repo of repoctl on GitHub Pages
// (docs/design/tool.html#pages) for the release job and the yank job.
//
//	rpmtree assemble -old DIR -new DIR -out DIR -fedora "43 44" -el "9 10" [-yanked FILE] [-keep-minors 3]
//	rpmtree keys
//
// assemble writes OUT/rpm/RELEASEVER/x86_64/ for each Fedora and EL
// release, and a copy of the newest repoctl-release at OUT/rpm/. It takes
// the RPMs of the old tree (OLD/rpm/, from the rpm-repo.tar of the newest
// GitHub Release) and the new signed RPMs of NEW. A file name that the old
// tree has keeps its old file, so the sha256 of a released RPM never
// changes. It drops the versions of the yank list and keeps, in each
// directory, the newest patch release of the newest minor releases of
// repoctl and the newest repoctl-release and repoctl-mirror. It prints the
// names of the new RPMs that the tree takes, one per line.
//
// keys prints the tool repository keys of this source (doctor.ToolKeys),
// one per line: FINGERPRINT STATE EXPIRES.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/doctor"
	"github.com/dennisklein/repoctl/internal/manifest"
)

// The packages of the tree.
const (
	pkgTool    = "repoctl"
	pkgRelease = "repoctl-release"
	pkgMirror  = "repoctl-mirror"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "rpmtree:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: rpmtree assemble|keys [flags]")
	}
	switch args[0] {
	case "keys":
		keys, err := doctor.ParseToolKeys(doctor.ToolKeys)
		if err != nil {
			return err
		}
		for _, k := range keys {
			_, _ = fmt.Fprintf(stdout, "%s %s %s\n", k.Fingerprint, k.State, k.Expires.Format("2006-01-02"))
		}
		return nil
	case "assemble":
		fs := flag.NewFlagSet("assemble", flag.ContinueOnError)
		var o Options
		var fedora, el string
		fs.StringVar(&o.Old, "old", "", "the root of the old tree (with rpm/); empty or missing: no old tree")
		fs.StringVar(&o.New, "new", "", "the directory of the new signed RPMs")
		fs.StringVar(&o.Out, "out", "", "the root of the new tree; it must not exist")
		fs.StringVar(&o.Yanked, "yanked", "", "the yank list (packaging/yanked.txt)")
		fs.StringVar(&fedora, "fedora", "", "the Fedora releases of the tree")
		fs.StringVar(&el, "el", "", "the EL releases of the tree")
		fs.IntVar(&o.KeepMinors, "keep-minors", 3, "the minor releases of repoctl to keep")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		o.Fedora, o.EL = strings.Fields(fedora), strings.Fields(el)
		added, err := Assemble(o)
		if err != nil {
			return err
		}
		for _, n := range added {
			_, _ = fmt.Fprintln(stdout, n)
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// Options are the inputs of Assemble.
type Options struct {
	Old, New, Out, Yanked string
	Fedora, EL            []string
	KeepMinors            int
}

// RPM is one RPM file of the tree.
type RPM struct {
	Name, Version, Release, Arch string
	File                         string // base name
	Path                         string // source path
	New                          bool
}

// ParseFile parses NAME-VERSION-RELEASE.ARCH.rpm.
func ParseFile(base string) (RPM, error) {
	s, ok := strings.CutSuffix(base, ".rpm")
	if !ok {
		return RPM{}, fmt.Errorf("%s: not an .rpm file", base)
	}
	i := strings.LastIndexByte(s, '.')
	if i < 0 {
		return RPM{}, fmt.Errorf("%s: no arch", base)
	}
	r := RPM{Arch: s[i+1:], File: base}
	s = s[:i]
	i = strings.LastIndexByte(s, '-')
	if i < 0 {
		return RPM{}, fmt.Errorf("%s: no release", base)
	}
	r.Release, s = s[i+1:], s[:i]
	i = strings.LastIndexByte(s, '-')
	if i < 0 {
		return RPM{}, fmt.Errorf("%s: no version", base)
	}
	r.Version, r.Name = s[i+1:], s[:i]
	if r.Name == "" || r.Version == "" || r.Release == "" || r.Arch == "" {
		return RPM{}, fmt.Errorf("%s: not NAME-VERSION-RELEASE.ARCH.rpm", base)
	}
	return r, nil
}

// RPMVersion gives the RPM version of a tag or tool version: without the
// leading v, and with ~ for the - of a pre-release (v0.4.0-rc.1 gives
// 0.4.0~rc.1, which sorts before 0.4.0).
func RPMVersion(v string) string {
	return strings.Replace(strings.TrimPrefix(v, "v"), "-", "~", 1)
}

func newer(a, b RPM) bool {
	return manifest.CompareEVR(0, a.Version, a.Release, 0, b.Version, b.Release) > 0
}

// minor gives MAJOR.MINOR of a version.
func minor(v string) string {
	v, _, _ = strings.Cut(v, "~")
	f := strings.SplitN(v, ".", 3)
	if len(f) < 2 {
		return v
	}
	return f[0] + "." + f[1]
}

// Assemble writes the new tree and gives the names of the new RPMs that it
// took.
func Assemble(o Options) ([]string, error) {
	if o.Out == "" || o.New == "" {
		return nil, errors.New("assemble needs -new and -out")
	}
	if len(o.Fedora)+len(o.EL) == 0 {
		return nil, errors.New("assemble needs -fedora or -el")
	}
	if o.KeepMinors < 1 {
		return nil, errors.New("-keep-minors must be 1 or more")
	}
	if _, err := os.Stat(o.Out); err == nil {
		return nil, fmt.Errorf("%s exists", o.Out)
	}
	yanked, err := readYanked(o.Yanked)
	if err != nil {
		return nil, err
	}
	// dirs: RELEASEVER -> the RPMs it can take.
	dirs := map[string][]RPM{}
	order := append(slices.Clone(o.Fedora), o.EL...)
	for _, d := range order {
		dirs[d] = nil
	}
	isFedora := map[string]bool{}
	for _, f := range o.Fedora {
		isFedora[f] = true
	}
	place := func(r RPM) error {
		switch r.Name {
		case pkgTool:
			if r.Arch != "x86_64" {
				return fmt.Errorf("%s: repoctl must be x86_64", r.File)
			}
			_, dist, _ := strings.Cut(r.Release, ".fc")
			if dist == "" {
				return fmt.Errorf("%s: repoctl must have a Fedora dist tag", r.File)
			}
			if isFedora[dist] {
				dirs[dist] = append(dirs[dist], r)
			}
		case pkgRelease, pkgMirror:
			if r.Arch != "noarch" {
				return fmt.Errorf("%s: %s must be noarch", r.File, r.Name)
			}
			for _, d := range order {
				dirs[d] = append(dirs[d], r)
			}
		default:
			return fmt.Errorf("%s: %s is not a package of the tree", r.File, r.Name)
		}
		return nil
	}
	// The old tree: each file once, by name; the newest copy wins when two
	// directories hold the same name (they hold the same bytes).
	have := map[string]bool{}
	if o.Old != "" {
		old, err := filepath.Glob(filepath.Join(o.Old, "rpm", "*", "x86_64", "*.rpm"))
		if err != nil {
			return nil, err
		}
		sort.Strings(old)
		seen := map[string]RPM{}
		for _, p := range old {
			r, err := ParseFile(filepath.Base(p))
			if err != nil {
				return nil, err
			}
			r.Path = p
			if _, ok := seen[r.File]; !ok {
				seen[r.File] = r
			}
		}
		names := make([]string, 0, len(seen))
		for n := range seen {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			have[n] = true
			if err := place(seen[n]); err != nil {
				return nil, err
			}
		}
	}
	news, err := filepath.Glob(filepath.Join(o.New, "*.rpm"))
	if err != nil {
		return nil, err
	}
	sort.Strings(news)
	for _, p := range news {
		r, err := ParseFile(filepath.Base(p))
		if err != nil {
			return nil, err
		}
		if have[r.File] {
			continue
		}
		r.Path, r.New = p, true
		have[r.File] = true
		if err := place(r); err != nil {
			return nil, err
		}
	}
	// Yank, then keep the newest.
	var added []string
	addedSet := map[string]bool{}
	var newestRelease *RPM
	for _, d := range order {
		var kept []RPM
		for _, r := range dirs[d] {
			if (r.Name == pkgTool || r.Name == pkgMirror) && yanked[r.Version] {
				continue
			}
			kept = append(kept, r)
		}
		kept = retain(kept, o.KeepMinors)
		dst := filepath.Join(o.Out, "rpm", d, "x86_64")
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return nil, err
		}
		for _, r := range kept {
			if err := copyFile(r.Path, filepath.Join(dst, r.File)); err != nil {
				return nil, err
			}
			if r.New && !addedSet[r.File] {
				addedSet[r.File] = true
				added = append(added, r.File)
			}
			if r.Name == pkgRelease && (newestRelease == nil || newer(r, *newestRelease)) {
				rr := r
				newestRelease = &rr
			}
		}
	}
	if newestRelease != nil {
		if err := copyFile(newestRelease.Path, filepath.Join(o.Out, "rpm", newestRelease.File)); err != nil {
			return nil, err
		}
	}
	sort.Strings(added)
	return added, nil
}

// retain keeps the newest patch release of the newest KEEP minor releases
// of repoctl, and the newest repoctl-release and repoctl-mirror.
func retain(rs []RPM, keep int) []RPM {
	best := map[string]RPM{} // key: name, or repoctl@MINOR
	for _, r := range rs {
		k := r.Name
		if r.Name == pkgTool {
			k += "@" + minor(r.Version)
		}
		if b, ok := best[k]; !ok || newer(r, b) {
			best[k] = r
		}
	}
	var tools, out []RPM
	for k, r := range best {
		if strings.HasPrefix(k, pkgTool+"@") {
			tools = append(tools, r)
		} else {
			out = append(out, r)
		}
	}
	sort.Slice(tools, func(i, j int) bool { return newer(tools[i], tools[j]) })
	if len(tools) > keep {
		tools = tools[:keep]
	}
	out = append(out, tools...)
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// readYanked reads the yank list: one version per line (v0.4.1 or 0.4.1),
// # starts a comment.
func readYanked(path string) (map[string]bool, error) {
	out := map[string]bool{}
	if path == "" {
		return out, nil
	}
	f, err := os.Open(path) //nolint:gosec // G304: the yank list of the repository
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if v := strings.TrimSpace(line); v != "" {
			out[RPMVersion(v)] = true
		}
	}
	return out, sc.Err()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: an RPM of the tree
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) //nolint:gosec // G304: a file of the new tree
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
