// Package tree works on the tree of one target: the three rpm-md
// repositories NAME, NAME-source and NAME-debuginfo, the root files, their
// repodata and signatures (docs/design/publish.html#format,
// config.html#set-config). It reads the repodata without dnf, runs
// createrepo_c and modifyrepo_c, queries RPM headers, and verifies a tree
// with a keyring of the set keys.
package tree

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// The kinds of a repository of a target.
const (
	Binary    api.RepoKind = "binary"
	Source    api.RepoKind = "source"
	Debuginfo api.RepoKind = "debuginfo"
)

// Layout gives the repository directories of a target from repo: of
// repoctl.yaml.
type Layout struct {
	// Name is repo.name: the binary repository.
	Name      string
	Source    bool
	Debuginfo bool
}

// KeyfileName gives sign.release.keyfile, by default
// RPM-GPG-KEY-<repo.name>, the key file at the tree root and in the
// release RPM.
func KeyfileName(set *config.Set) string {
	if set.Sign.Release.Keyfile != "" {
		return set.Sign.Release.Keyfile
	}
	return "RPM-GPG-KEY-" + set.Repo.Name
}

// LayoutOf gives the layout of a set.
func LayoutOf(set *config.Set) Layout {
	return Layout{Name: set.Repo.Name, Source: set.Repo.HasSource(), Debuginfo: set.Repo.HasDebuginfo()}
}

// Repo is one repository of a target.
type Repo struct {
	Kind api.RepoKind
	// Dir is the directory below the target: NAME, NAME-source or
	// NAME-debuginfo.
	Dir string
}

// Repos gives the repositories of the layout: binary, source, debuginfo.
func (l Layout) Repos() []Repo {
	out := []Repo{{Kind: Binary, Dir: l.Name}}
	if l.Source {
		out = append(out, Repo{Kind: Source, Dir: l.Name + "-source"})
	}
	if l.Debuginfo {
		out = append(out, Repo{Kind: Debuginfo, Dir: l.Name + "-debuginfo"})
	}
	return out
}

// Dir gives the directory of a repository kind, or "" when the layout has
// none.
func (l Layout) Dir(kind api.RepoKind) string {
	for _, r := range l.Repos() {
		if r.Kind == kind {
			return r.Dir
		}
	}
	return ""
}

// ParseKind reads --repo binary|source|debuginfo; "" is binary.
func ParseKind(s string) (api.RepoKind, bool) {
	switch s {
	case "", "binary":
		return Binary, true
	case "source":
		return Source, true
	case "debuginfo":
		return Debuginfo, true
	}
	return "", false
}

// KindOfFile gives the artifact kind of an RPM file name: srpm, debuginfo,
// debugsource or rpm.
func KindOfFile(file string) string {
	switch {
	case strings.HasSuffix(file, ".src.rpm"):
		return "srpm"
	case strings.Contains(file, "-debuginfo-"):
		return "debuginfo"
	case strings.Contains(file, "-debugsource-"):
		return "debugsource"
	}
	return "rpm"
}

// RepoKindOfFile gives the repository of an RPM file name.
func RepoKindOfFile(file string) api.RepoKind {
	switch KindOfFile(file) {
	case "srpm":
		return Source
	case "debuginfo", "debugsource":
		return Debuginfo
	}
	return Binary
}

// PackagePath gives the path of an RPM in its repository:
// Packages/<first letter>/FILE.
func PackagePath(file string) string {
	first := "_"
	if r, _ := utf8.DecodeRuneInString(file); unicode.IsLetter(r) || unicode.IsDigit(r) {
		first = strings.ToLower(string(r))
	}
	return filepath.Join("Packages", first, file)
}

// PathOf gives the path of an RPM file in a target tree with the layout,
// relative to the tree, or "" when the layout has no repository for it.
func (l Layout) PathOf(file string) string {
	dir := l.Dir(RepoKindOfFile(file))
	if dir == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Join(dir, PackagePath(file)))
}

// Index maps the file names of the RPMs of a tree to their paths below the
// tree.
type Index map[string]string

// IndexRPMs walks a tree and gives the path of each RPM by file name. The
// root files (TREE/*.rpm) are not in the index.
func IndexRPMs(tree string) (Index, error) {
	idx := Index{}
	err := filepath.WalkDir(tree, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != tree && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".rpm") || filepath.Dir(p) == tree {
			return nil
		}
		rel, _ := filepath.Rel(tree, p)
		idx[d.Name()] = filepath.ToSlash(rel)
		return nil
	})
	if os.IsNotExist(err) {
		return idx, nil
	}
	return idx, err
}

// Names gives the file names of the index, sorted.
func (x Index) Names() []string {
	out := make([]string, 0, len(x))
	for n := range x {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
