// Package staging is the staging repository of a build run: the RPMs of
// the nodes that ended ok, their repodata, their buildroot files and record
// drafts, and the staged index that tells the plan which nodes an earlier
// run staged (docs/design/build.html#staging).
//
// The staging of a run is $XDG_DATA_HOME/repoctl/SET/staging/RUN/TARGET/,
// with the sub-repos REPO, REPO-source and REPO-debuginfo (repo.name). Each
// sub-repo has the published layout Packages/<letter>/FILE. The binary repo
// also holds buildroot/NVR.json, and the target holds the record draft of
// each node in drafts/PKG.json.
package staging

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/journal"
	"github.com/dennisklein/repoctl/internal/publish"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// Repos gives the sub-repos of a set: the binary repo, the source repo and
// the debuginfo repo, from repo.name.
type Repos struct {
	Binary, Source, Debuginfo string
	// HasSource and HasDebuginfo are repo.source and repo.debuginfo.
	HasSource, HasDebuginfo bool
}

// ReposOf gives the sub-repos of the set.
func ReposOf(set *config.Set) Repos {
	n := set.Repo.Name
	return Repos{Binary: n, Source: n + "-source", Debuginfo: n + "-debuginfo", HasSource: set.Repo.HasSource(), HasDebuginfo: set.Repo.HasDebuginfo()}
}

// Kind gives the artifact kind of an RPM file: srpm, debuginfo,
// debugsource or rpm.
func Kind(file string) string {
	switch {
	case strings.HasSuffix(file, ".src.rpm"):
		return "srpm"
	case strings.Contains(file, "-debugsource-"):
		return "debugsource"
	case strings.Contains(file, "-debuginfo-"):
		return "debuginfo"
	}
	return "rpm"
}

// RepoOf gives the sub-repo of an RPM file, or "" when the set does not
// publish its kind.
func (r Repos) RepoOf(file string) string {
	switch Kind(file) {
	case "srpm":
		if r.HasSource {
			return r.Source
		}
		return ""
	case "debuginfo", "debugsource":
		if r.HasDebuginfo {
			return r.Debuginfo
		}
		return ""
	}
	return r.Binary
}

// Dir is the staging of one run and target.
type Dir struct {
	Paths  config.Paths
	Set    string
	Run    string
	Target string
	Repos  Repos
}

// Path gives the staging of the target: staging/RUN/TARGET.
func (d Dir) Path() string { return d.Paths.StagingTarget(d.Set, d.Run, d.Target) }

// Repo gives the directory of a sub-repo.
func (d Dir) Repo(repo string) string { return filepath.Join(d.Path(), repo) }

// File gives the staged path of a file of a sub-repo.
func (d Dir) File(repo, file string) string {
	return journal.StagedPath(d.Paths, d.Set, d.Run, d.Target, repo, file)
}

// BuildrootFile gives the buildroot file of a build: REPO/buildroot/NVR.json,
// and its path in the repo (the manifest field buildrootFile).
func (d Dir) BuildrootFile(nvr string) (abs, rel string) {
	rel = "buildroot/" + nvr + ".json"
	return filepath.Join(d.Repo(d.Repos.Binary), filepath.FromSlash(rel)), rel
}

// DraftPath gives the record draft of a node: TARGET/drafts/PKG.json
// (publish.DraftPath).
func (d Dir) DraftPath(pkg string) string {
	return filepath.Join(d.Path(), publish.DraftDir, pkg+".json")
}

// WriteDraft writes the record draft of a node, which publish takes
// (supply-chain.html#capture): its manifest entry, the build facts and the
// buildroot file (publish.Draft). A relative buildroot path is relative
// to the drafts directory.
func (d Dir) WriteDraft(pkg string, dr publish.Draft) error {
	data, err := json.MarshalIndent(dr, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(d.DraftPath(pkg), append(data, '\n'), 0o644)
}

// ReadDraft reads the record draft of a node of a run.
func ReadDraft(p config.Paths, set, run, target, pkg string) (*publish.Draft, error) {
	d := Dir{Paths: p, Set: set, Run: run, Target: target}
	data, err := os.ReadFile(d.DraftPath(pkg))
	if err != nil {
		return nil, err
	}
	var dr publish.Draft
	if err := json.Unmarshal(data, &dr); err != nil {
		return nil, fmt.Errorf("record draft of %s@%s in %s: %w", pkg, target, run, err)
	}
	return &dr, nil
}

// Init makes the sub-repos of the staging with empty repodata, so that
// mock can read the staging of the run through --addrepo before the first
// node is staged.
func (d Dir) Init(ctx context.Context) error {
	for _, repo := range []string{d.Repos.Binary, d.Repos.Source, d.Repos.Debuginfo} {
		dir := d.Repo(repo)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(dir, "repodata", "repomd.xml")); err == nil {
			continue
		}
		if _, err := Createrepo(ctx, dir); err != nil {
			return err
		}
	}
	return nil
}

// File is one file that the stage step put into the staging.
type File struct {
	Kind   string
	Repo   string
	NVRA   string
	File   string
	Path   string
	Size   int64
	SHA256 string
}

// locks are the in-process locks of step stage, one per staging target:
// step stage runs for one node of a target at a time (build.html#node-steps).
var locks sync.Map

// Lock takes the stage lock of the staging target and gives its unlock.
func (d Dir) Lock() func() {
	m, _ := locks.LoadOrStore(d.Path(), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Copy copies the RPM files of a node into the staging, each into its
// sub-repo. It gives the staged files. On an error it removes what it
// copied.
func (d Dir) Copy(files []string) ([]File, error) {
	var out []File
	for _, src := range files {
		name := filepath.Base(src)
		repo := d.Repos.RepoOf(name)
		if repo == "" {
			continue
		}
		dst := d.File(repo, name)
		sum, size, err := copyFile(src, dst)
		if err != nil {
			d.Remove(out)
			return nil, err
		}
		out = append(out, File{Kind: Kind(name), Repo: repo, NVRA: strings.TrimSuffix(name, ".rpm"), File: name, Path: dst, Size: size, SHA256: sum})
	}
	return out, nil
}

// Remove deletes staged files, after a failed or canceled stage.
func (d Dir) Remove(files []File) {
	for _, f := range files {
		os.Remove(f.Path)
	}
}

func copyFile(src, dst string) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// SHA256File gives the sha256 and the size of a file.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// WriteFileAtomic writes a file through a temporary file and a rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Repodata is the result of createrepo_c on a repo.
type Repodata struct {
	// Packages is the number of packages in the repodata.
	Packages int
	// Revision is the revision of repomd.xml.
	Revision string
}

var (
	reRevision = regexp.MustCompile(`<revision>([^<]*)</revision>`)
	rePackages = regexp.MustCompile(`packages="(\d+)"`)
	// rePrimary finds the location of the primary metadata in repomd.xml.
	rePrimary = regexp.MustCompile(`(?s)<data type="primary">.*?<location href="([^"]+)"`)
)

// Createrepo runs createrepo_c --update on a repo. createrepo_c writes the
// new repodata into a temporary directory, and repoctl swaps it in with a
// rename: repodata is a link to a versioned directory, which a rename of a
// new link replaces at once. Thus a reader through --addrepo sees the old
// or the new repodata, never a part of it. The new directory also keeps
// the files of the old one, for a reader that read the old repomd.xml.
func Createrepo(ctx context.Context, dir string) (Repodata, error) {
	vers := filepath.Join(dir, ".repodata")
	if err := os.MkdirAll(vers, 0o755); err != nil {
		return Repodata{}, err
	}
	tmp, err := os.MkdirTemp(vers, "new-")
	if err != nil {
		return Repodata{}, err
	}
	defer os.RemoveAll(tmp)
	args := []string{"--quiet", "--no-database", "-o", tmp}
	link := filepath.Join(dir, "repodata")
	if _, err := os.Stat(filepath.Join(link, "repomd.xml")); err == nil {
		args = append(args, "--update", "--update-md-path", dir)
	}
	args = append(args, dir)
	if err := (&run.Cmd{Name: "createrepo_c", Args: args}).Run(ctx); err != nil {
		return Repodata{}, err
	}
	name := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	final := filepath.Join(vers, name)
	if err := os.Rename(filepath.Join(tmp, "repodata"), final); err != nil {
		return Repodata{}, err
	}
	// Keep the files of the previous repodata for a reader that read its
	// repomd.xml.
	old, _ := os.Readlink(link)
	if old != "" {
		oldDir := old
		if !filepath.IsAbs(oldDir) {
			oldDir = filepath.Join(dir, oldDir)
		}
		if des, err := os.ReadDir(oldDir); err == nil {
			for _, de := range des {
				if de.Name() == "repomd.xml" {
					continue
				}
				if _, err := os.Stat(filepath.Join(final, de.Name())); os.IsNotExist(err) {
					_ = os.Link(filepath.Join(oldDir, de.Name()), filepath.Join(final, de.Name()))
				}
			}
		}
	} else if fi, err := os.Lstat(link); err == nil && fi.IsDir() {
		// A plain repodata directory (an older staging): move it aside.
		_ = os.Rename(link, filepath.Join(vers, "old-"+name))
	}
	newLink := link + ".new-" + name
	if err := os.Symlink(filepath.Join(".repodata", name), newLink); err != nil {
		return Repodata{}, err
	}
	if err := os.Rename(newLink, link); err != nil {
		os.Remove(newLink)
		return Repodata{}, err
	}
	prune(vers, name)
	return readRepomd(final)
}

// prune deletes the versioned repodata directories but the newest two.
func prune(vers, keep string) {
	des, err := os.ReadDir(vers)
	if err != nil {
		return
	}
	var names []string
	for _, de := range des {
		if de.IsDir() && !strings.HasPrefix(de.Name(), "new-") {
			names = append(names, de.Name())
		}
	}
	sort.Strings(names)
	for i := 0; i < len(names)-2; i++ {
		if names[i] != keep {
			os.RemoveAll(filepath.Join(vers, names[i]))
		}
	}
}

func readRepomd(dir string) (Repodata, error) {
	b, err := os.ReadFile(filepath.Join(dir, "repomd.xml"))
	if err != nil {
		return Repodata{}, err
	}
	var rd Repodata
	if m := reRevision.FindSubmatch(b); m != nil {
		rd.Revision = string(m[1])
	}
	// The primary metadata that repomd.xml names gives the count. The
	// directory also holds the files of the previous repodata (hard links
	// for readers of the old repomd.xml), so never pick a file by its name.
	if m := rePrimary.FindSubmatch(b); m != nil {
		rd.Packages = countPackages(filepath.Join(dir, filepath.Base(string(m[1]))))
	}
	return rd, nil
}

// countPackages reads the packages attribute of primary.xml.gz.
func countPackages(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return 0
	}
	head := make([]byte, 4096)
	n, _ := io.ReadFull(bufio.NewReader(zr), head)
	if m := rePackages.FindSubmatch(head[:n]); m != nil {
		c, _ := strconv.Atoi(string(m[1]))
		return c
	}
	return 0
}
