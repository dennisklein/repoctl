package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// SpecFile gives the spec of a package in dir: PKG.spec, else the one
// *.spec file of the directory.
func SpecFile(dir, pkg string) (string, error) {
	p := filepath.Join(dir, pkg+".spec")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	list, _ := filepath.Glob(filepath.Join(dir, "*.spec"))
	switch len(list) {
	case 1:
		return list[0], nil
	case 0:
		return "", fmt.Errorf("%s has no spec file", dir)
	}
	return "", fmt.Errorf("%s has %d spec files and none is %s.spec", dir, len(list), pkg)
}

// SpecInfo is what rebuild needs of a spec: its Name, its Version and its
// Release without the dist tag.
type SpecInfo struct {
	Name, Version, Release string
	// AutoRelease reports Release: %autorelease. Without it, a rebuild
	// commit gives the same NVR.
	AutoRelease bool
}

var tagRE = regexp.MustCompile(`(?i)^\s*(name|version|release)\s*:\s*(.*?)\s*$`)

// specTags gives the Name, Version and Release lines of the spec text, as
// written.
func specTags(text string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(text, "\n") {
		if m := tagRE.FindStringSubmatch(l); m != nil {
			k := strings.ToLower(m[1])
			if _, ok := out[k]; !ok {
				out[k] = m[2]
			}
		}
	}
	return out
}

func isAutoRelease(release string) bool {
	return strings.Contains(release, "%autorelease") || strings.Contains(release, "%{autorelease}")
}

// ReadSpec evaluates Name, Version and Release of the spec with rpmspec
// and the macro files, as rpmspec -q --srpm --qf '%{NAME}-%{VERSION}' does.
// %autorelease is 1 and %{dist} is empty here. Without rpmspec, or when it
// fails, it reads the tag lines as written.
func ReadSpec(ctx context.Context, spec string, macroFiles []string) (SpecInfo, error) {
	text, err := os.ReadFile(spec)
	if err != nil {
		return SpecInfo{}, err
	}
	tags := specTags(string(text))
	info := SpecInfo{Name: tags["name"], Version: tags["version"], Release: tags["release"], AutoRelease: isAutoRelease(tags["release"])}
	if _, ok := run.LookPath("rpmspec"); ok {
		args := []string{"-q", "--srpm", "--qf", "%{NAME}\t%{VERSION}\t%{RELEASE}\n",
			"--define", "autorelease 1", "--define", "autochangelog %{nil}", "--define", "dist %{nil}"}
		for _, m := range macroFiles {
			if _, err := os.Stat(m); err == nil {
				args = append(args, "--load", m)
			}
		}
		out, err := (&run.Cmd{Name: "rpmspec", Args: append(args, spec)}).Output(ctx)
		if err == nil {
			first, _, _ := strings.Cut(string(out), "\n")
			if f := strings.Split(first, "\t"); len(f) == 3 {
				info.Name, info.Version, info.Release = f[0], f[1], f[2]
			}
		}
	}
	if info.Name == "" || info.Version == "" {
		return info, fmt.Errorf("%s: no Name or Version", spec)
	}
	return info, nil
}

// AutoReleaseAt gives the %autorelease number of a commit, as rpmautospec
// counts it: the number of commits since the last change of the Version
// line of the spec, the commit of the change included. specPath is the
// path of the spec in the repository.
func AutoReleaseAt(ctx context.Context, dir, rev, specPath string) (int, error) {
	g := git.New(dir)
	commits, err := g.Lines(ctx, "log", "--format=%H", rev)
	if err != nil {
		return 0, err
	}
	version := func(c string) string {
		b, err := g.Show(ctx, c, specPath)
		if err != nil {
			return ""
		}
		return specTags(string(b))["version"]
	}
	if len(commits) == 0 {
		return 0, nil
	}
	cur := version(commits[0])
	n := 0
	for _, c := range commits {
		if version(c) != cur {
			break
		}
		n++
	}
	return n, nil
}
