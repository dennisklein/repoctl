// Package release is the in-tree release package of a set
// (docs/design/signing.html#release): repoctl release write writes its key
// file from sign.keys and the .repo file of each target, and raises the
// Release of its spec with a %changelog entry when a file changes. The stale
// checks find a key file or a .repo file that differs from what release
// write writes.
package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/sign"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// DefaultMessage is the %changelog entry when the command gives none.
const DefaultMessage = "Update the key file and the repo files"

// File is one file that release write owns.
type File struct {
	// Path is relative to the set directory.
	Path string
	// Kind is keyfile or repofile.
	Kind string
	// Target is the target of a .repo file.
	Target string
	Data   []byte
}

// Package gives the release package of the set, or config.invalid.
func Package(set *config.Set) (*config.Package, error) {
	p, ok := set.ReleasePackage()
	if !ok {
		return nil, api.Errorf(api.PConfigInvalid, "repoctl.yaml: no package has role: release").
			WithHint("add the in-tree release package with role: release", "repoctl help new-set")
	}
	return p, nil
}

// KeyfilePath gives the path of the key file in the release package.
func KeyfilePath(set *config.Set, p *config.Package) string {
	return filepath.Join(p.Path, sign.KeyfileName(set))
}

// RepoFilePath gives the path of the .repo file of a target:
// <path>/<repo.name>-<target>.repo.
func RepoFilePath(set *config.Set, p *config.Package, target string) string {
	return filepath.Join(p.Path, set.Repo.Name+"-"+target+".repo")
}

// Expected gives the files that release write writes: the key file, then
// the .repo file of each active target.
func Expected(set *config.Set, trust *sign.Trust) ([]File, error) {
	p, err := Package(set)
	if err != nil {
		return nil, err
	}
	kf, err := trust.Keyfile()
	if err != nil {
		return nil, err
	}
	out := []File{{Path: KeyfilePath(set, p), Kind: "keyfile", Data: kf}}
	if set.Repo.URL == "" {
		return nil, api.Errorf(api.PConfigInvalid, "repoctl.yaml: repo.url is missing; the .repo files need it").
			WithHint("set repo.url, then run release write again", "repoctl release write")
	}
	for _, t := range set.ActiveTargets() {
		out = append(out, File{Path: RepoFilePath(set, p, t.ID), Kind: "repofile", Target: t.ID, Data: RepoFile(set, t)})
	}
	return out, nil
}

// RepoFile gives the .repo file of a target (signing.html#repo-file).
// release write owns each line.
func RepoFile(set *config.Set, t config.Target) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s %s. Written by \"repoctl release write\". Do not edit.\n", set.Name, t.ID)
	base := strings.TrimSuffix(set.Repo.URL, "/") + "/" + t.ClientPath() + "/"
	gpgkey := "file://" + sign.KeyfileInstallDir + "/" + sign.KeyfileName(set)
	section := func(id, name string, enabled int) {
		fmt.Fprintf(&b, "[%s]\nname=%s\nbaseurl=%s%s/\nenabled=%d\ngpgcheck=1\nrepo_gpgcheck=1\ngpgkey=%s\nmetadata_expire=1h\n",
			id, name, base, id, enabled, gpgkey)
	}
	name := set.Repo.Name
	section(name, set.Name+" "+t.ID, 1)
	if set.Repo.HasSource() {
		b.WriteString("\n")
		section(name+"-source", set.Name+" "+t.ID+" source", 0)
	}
	if set.Repo.HasDebuginfo() {
		b.WriteString("\n")
		section(name+"-debuginfo", set.Name+" "+t.ID+" debuginfo", 0)
	}
	return b.Bytes()
}

// Options are the options of Write.
type Options struct {
	// Message is the %changelog entry; "" gives DefaultMessage.
	Message string
	// Now is the date of the %changelog entry.
	Now time.Time
	// Author is "Name <email>" of the entry; "" reads git var
	// GIT_AUTHOR_IDENT in the set.
	Author string
}

// Result is what Write did.
type Result struct {
	// Files are the key file, the .repo files and the spec, each with its
	// action.
	Files []api.File
	// Changed are the paths that Write changed.
	Changed []string
	// Release is the new Release of the spec, "" when it did not change.
	Release string
	// Keys are the ids of the keys in the key file.
	Keys []string
}

// Write writes the key file and the .repo files of the release package.
// When a file changes, it raises the Release of the spec by one and adds a
// %changelog entry. It does not commit.
func Write(ctx context.Context, set *config.Set, setDir string, trust *sign.Trust, opts Options) (*Result, error) {
	files, err := Expected(set, trust)
	if err != nil {
		return nil, err
	}
	p, _ := Package(set)
	res := &Result{}
	for _, k := range trust.KeyfileKeys() {
		res.Keys = append(res.Keys, k.ID)
	}
	changed := false
	for _, f := range files {
		full := filepath.Join(setDir, f.Path)
		old, err := os.ReadFile(full)
		action := "unchanged"
		switch {
		case errors.Is(err, os.ErrNotExist):
			action = "created"
		case err != nil:
			return nil, err
		case !bytes.Equal(old, f.Data):
			action = "updated"
		}
		if action != "unchanged" {
			if err := writeFile(full, f.Data); err != nil {
				return nil, err
			}
			changed = true
			res.Changed = append(res.Changed, f.Path)
		}
		res.Files = append(res.Files, api.File{Path: f.Path, Action: action, Kind: f.Kind, Bytes: len(f.Data), Sha256: sha256Hex(f.Data)})
	}
	spec, err := SpecPath(setDir, p)
	if err != nil {
		return nil, err
	}
	if !changed {
		res.Files = append(res.Files, api.File{Path: spec, Action: "unchanged", Kind: "other"})
		return res, nil
	}
	full := filepath.Join(setDir, spec)
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	msg := opts.Message
	if msg == "" {
		msg = DefaultMessage
	}
	author := opts.Author
	if author == "" {
		author = gitAuthor(ctx, setDir)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	out, rel, err := BumpSpec(data, msg, author, now)
	if err != nil {
		if e, ok := api.AsError(err); ok {
			e.Path = spec
		}
		return nil, err
	}
	if err := writeFile(full, out); err != nil {
		return nil, err
	}
	res.Release = rel
	res.Changed = append(res.Changed, spec)
	res.Files = append(res.Files, api.File{Path: spec, Action: "updated", Kind: "other", Bytes: len(out)})
	return res, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SpecPath gives the spec of the release package, relative to the set:
// <path>/<id>.spec, else the only .spec file in its directory.
func SpecPath(setDir string, p *config.Package) (string, error) {
	want := filepath.Join(p.Path, p.ID+".spec")
	if _, err := os.Stat(filepath.Join(setDir, want)); err == nil {
		return want, nil
	}
	m, _ := filepath.Glob(filepath.Join(setDir, p.Path, "*.spec"))
	if len(m) == 1 {
		return filepath.Rel(setDir, m[0])
	}
	return "", api.Errorf(api.PConfigInvalid, "%s: the release package has %d spec files; want one", p.Path, len(m)).WithPath(p.Path)
}

var (
	releaseRE   = regexp.MustCompile(`(?m)^(Release:[ \t]*)(\d+)(.*)$`)
	releaseAny  = regexp.MustCompile(`(?m)^Release:[ \t]*(\S.*)$`)
	versionRE   = regexp.MustCompile(`(?m)^Version:[ \t]*(\S+)`)
	changelogRE = regexp.MustCompile(`(?m)^%changelog[ \t]*$`)
)

// BumpSpec raises the plain Release of a spec by one and adds a %changelog
// entry. It gives the new spec and the new release number.
func BumpSpec(spec []byte, message, author string, now time.Time) ([]byte, string, error) {
	m := releaseRE.FindSubmatchIndex(spec)
	if m == nil {
		val := ""
		if a := releaseAny.FindSubmatch(spec); a != nil {
			val = string(a[1])
		}
		return nil, "", api.Errorf(api.PSourceInTreeAutorelease, "the spec of the release package has Release: %s; release write needs a plain number", val).
			WithHint("write a plain Release, for example 1%{?dist}, and a plain %changelog", "")
	}
	n, err := strconv.Atoi(string(spec[m[4]:m[5]]))
	if err != nil {
		return nil, "", api.Errorf(api.PConfigInvalid, "the Release of the spec is not a number: %v", err)
	}
	rel := strconv.Itoa(n + 1)
	var out bytes.Buffer
	out.Write(spec[:m[4]])
	out.WriteString(rel)
	out.Write(spec[m[5]:])
	spec = out.Bytes()
	if changelogRE.FindIndex(spec) == nil {
		return nil, "", api.Errorf(api.PSourceInTreeAutorelease, "the spec of the release package has no %%changelog").
			WithHint("add a plain %changelog section", "")
	}
	version := ""
	if v := versionRE.FindSubmatch(spec); v != nil {
		version = string(v[1])
	}
	cl := changelogRE.FindIndex(spec)
	entry := fmt.Sprintf("* %s %s - %s-%s\n- %s\n\n", now.UTC().Format("Mon Jan 02 2006"), author, version, rel, message)
	var b bytes.Buffer
	b.Write(spec[:cl[1]])
	b.WriteString("\n")
	b.WriteString(entry)
	rest := bytes.TrimLeft(spec[cl[1]:], "\n")
	b.Write(rest)
	return b.Bytes(), rel, nil
}

// gitAuthor gives "Name <email>" of git var GIT_AUTHOR_IDENT in dir.
func gitAuthor(ctx context.Context, dir string) string {
	out, err := git.New(dir).Run(ctx, "var", "GIT_AUTHOR_IDENT")
	if err != nil {
		return "repoctl <repoctl@localhost>"
	}
	if i := strings.LastIndex(out, ">"); i > 0 {
		return out[:i+1]
	}
	return out
}

// Check is the stale check of the publish and of doctor: the key file and
// each .repo file on disk equal what release write writes. A difference
// gives sign.keyfile-stale or sign.repofile-stale (exit 5).
func Check(set *config.Set, setDir string, trust *sign.Trust) error {
	files, err := Expected(set, trust)
	if err != nil {
		return err
	}
	var stale []File
	for _, f := range files {
		old, err := os.ReadFile(filepath.Join(setDir, f.Path))
		if err != nil || !bytes.Equal(old, f.Data) {
			stale = append(stale, f)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	sort.SliceStable(stale, func(i, j int) bool { return stale[i].Kind == "keyfile" && stale[j].Kind != "keyfile" })
	f := stale[0]
	if f.Kind == "keyfile" {
		return api.Errorf(api.PSignKeyfileStale, "%s differs from sign.keys (active and retiring keys)", f.Path).WithPath(f.Path).
			WithHint("write the key file, then commit it", "repoctl release write")
	}
	return api.Errorf(api.PSignRepofileStale, "%s differs from the file that release write writes", f.Path).WithPath(f.Path).
		WithHint("write the .repo files, then commit them", "repoctl release write")
}

// Diff gives the files whose content on disk differs from what release write
// writes, by kind.
func Diff(set *config.Set, setDir string, trust *sign.Trust) (keyfile bool, repofiles []string, err error) {
	files, err := Expected(set, trust)
	if err != nil {
		return false, nil, err
	}
	for _, f := range files {
		old, rerr := os.ReadFile(filepath.Join(setDir, f.Path))
		if rerr == nil && bytes.Equal(old, f.Data) {
			continue
		}
		if f.Kind == "keyfile" {
			keyfile = true
		} else {
			repofiles = append(repofiles, f.Path)
		}
	}
	return keyfile, repofiles, nil
}
