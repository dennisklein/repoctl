package sign

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// SnapshotFile is the cache of the annotations of the local copy.
const SnapshotFile = "snapshot.json"

// KeyfileInstallDir is the directory of the key file on a client.
const KeyfileInstallDir = "/etc/pki/rpm-gpg"

// Tree is a snapshot tree on disk: the local copy of latest of a target,
// the base of a publish, or the signed tree of an import (--path). It has
// the repositories (REPO, REPO-source, REPO-debuginfo) and the root files
// (the key file and <release>.rpm).
type Tree struct {
	Dir    string
	Target string
	Set    *config.Set

	loaded bool
	man    *manifest.Manifest
	manErr error
}

// NewTree gives the tree in dir.
func NewTree(dir string, set *config.Set, target string) *Tree {
	return &Tree{Dir: dir, Set: set, Target: target}
}

// LocalTree gives the local copy of latest of a target.
func LocalTree(p config.Paths, set *config.Set, target string) *Tree {
	return NewTree(p.LocalCopy(set.Name, target), set, target)
}

// Exists reports whether the tree has a binary repository with repodata.
func (t *Tree) Exists() bool {
	_, err := os.Stat(filepath.Join(t.Dir, t.Set.Repo.Name, "repodata", "repomd.xml"))
	return err == nil
}

// Manifest gives the RepoManifest of the binary repository; nil without an
// error when the tree has none.
func (t *Tree) Manifest() (*manifest.Manifest, error) {
	if !t.loaded {
		t.loaded = true
		t.man, t.manErr = manifest.ReadRepo(filepath.Join(t.Dir, t.Set.Repo.Name))
	}
	return t.man, t.manErr
}

// Snapshot gives snapshot.json of the local copy, or nil.
func (t *Tree) Snapshot() *api.Snapshot {
	data, err := os.ReadFile(filepath.Join(t.Dir, SnapshotFile))
	if err != nil {
		return nil
	}
	var s api.Snapshot
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	return &s
}

// PublishID gives the publish id of the tree: snapshot.json, else the
// manifest.
func (t *Tree) PublishID() string {
	if s := t.Snapshot(); s != nil && s.Publish != "" {
		return s.Publish
	}
	if m, _ := t.Manifest(); m != nil {
		return m.Publish
	}
	return ""
}

// releaseBuilds gives the builds of the release package.
func (t *Tree) releaseBuilds() []manifest.Build {
	rp, ok := t.Set.ReleasePackage()
	if !ok {
		return nil
	}
	m, _ := t.Manifest()
	if m == nil {
		return nil
	}
	var out []manifest.Build
	for _, b := range m.Builds {
		if b.Package == rp.ID {
			out = append(out, b)
		}
	}
	return out
}

// Releases gives the NVRs of the release package in the tree, oldest
// first.
func (t *Tree) Releases() []string {
	var out []string
	for _, b := range t.releaseBuilds() {
		out = append(out, manifest.NVR(b.SRPM))
	}
	sort.Slice(out, func(i, j int) bool { return CompareNVR(out[i], out[j]) < 0 })
	return out
}

// NewestRelease gives the release NVR with the highest version, or "".
func (t *Tree) NewestRelease() string {
	r := t.Releases()
	if len(r) == 0 {
		return ""
	}
	return r[len(r)-1]
}

// HasRelease finds a release NVR in the tree. nvr may lack the dist tag:
// slurm-release-26.05-3 finds slurm-release-26.05-3.el9.
func (t *Tree) HasRelease(nvr string) (string, bool) {
	for _, r := range t.Releases() {
		if r == nvr || strings.HasPrefix(r, nvr+".") {
			return r, true
		}
	}
	return "", false
}

// RootKeyfile gives the root key file of the tree (sign.release.keyfile).
func (t *Tree) RootKeyfile() ([]byte, error) {
	return os.ReadFile(filepath.Join(t.Dir, KeyfileName(t.Set)))
}

// ReleaseKeyfile gives the key file that the release RPM nvr installs. It
// reads the RPM with rpm2cpio; when the RPM is not in the tree and nvr is
// the newest release, it gives the root key file.
func (t *Tree) ReleaseKeyfile(ctx context.Context, nvr string) ([]byte, error) {
	full, ok := t.HasRelease(nvr)
	if !ok {
		return nil, fmt.Errorf("the release %s is not in the tree of %s", nvr, t.Target)
	}
	if f := t.findRPM(full); f != "" {
		data, err := ExtractFile(ctx, f, filepath.Join(KeyfileInstallDir, KeyfileName(t.Set)))
		if err == nil {
			return data, nil
		}
		if full != t.NewestRelease() {
			return nil, err
		}
	}
	if full == t.NewestRelease() {
		return t.RootKeyfile()
	}
	return nil, fmt.Errorf("the RPM of the release %s is not in the tree of %s", full, t.Target)
}

// findRPM gives the binary RPM file of the release NVR in the tree.
func (t *Tree) findRPM(nvr string) string {
	var want string
	for _, b := range t.releaseBuilds() {
		if manifest.NVR(b.SRPM) != nvr {
			continue
		}
		for _, r := range b.RPMs {
			if manifest.NVR(r) == nvr {
				want = filepath.Base(r)
			}
		}
	}
	if want == "" {
		return ""
	}
	found := ""
	_ = filepath.WalkDir(filepath.Join(t.Dir, t.Set.Repo.Name), func(p string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() && d.Name() == want {
			found = p
		}
		return nil
	})
	return found
}

// SignerCounts gives the number of RPMs (binary and source) in the tree
// per signer fingerprint, from the signer of each build of the manifest.
func (t *Tree) SignerCounts() map[string]int {
	out := map[string]int{}
	m, _ := t.Manifest()
	if m == nil {
		return out
	}
	for _, b := range m.Builds {
		if b.Signer == "" {
			continue
		}
		n := len(b.RPMs)
		if b.SRPM != "" {
			n++
		}
		out[NormalizeFingerprint(b.Signer)] += n
	}
	return out
}

// Signers gives the fingerprints of the history of the tree: each key that
// signed a publish of its chain.
func (t *Tree) Signers() map[string]bool {
	out := map[string]bool{}
	m, _ := t.Manifest()
	if m == nil {
		return out
	}
	for _, h := range m.History {
		if h.Signer != "" {
			out[NormalizeFingerprint(h.Signer)] = true
		}
	}
	return out
}

// ExtractFile gives the content of one file of an RPM: rpm2cpio, then the
// cpio archive in this process. name is the path on the client, for example
// /etc/pki/rpm-gpg/RPM-GPG-KEY-slurm.
func ExtractFile(ctx context.Context, rpmFile, name string) ([]byte, error) {
	out, err := (&run.Cmd{Name: "rpm2cpio", Args: []string{rpmFile}}).Output(ctx)
	if err != nil {
		return nil, err
	}
	data, ok, err := cpioFile(out, name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(rpmFile), err)
	}
	if !ok {
		return nil, fmt.Errorf("%s has no file %s", filepath.Base(rpmFile), name)
	}
	return data, nil
}

// cpioFile reads one file of a cpio archive of the newc form.
func cpioFile(a []byte, name string) ([]byte, bool, error) {
	want := strings.TrimPrefix(name, "/")
	for len(a) >= 110 {
		magic := string(a[:6])
		if magic != "070701" && magic != "070702" {
			return nil, false, errors.New("cpio: not a newc archive")
		}
		field := func(i int) (int, error) {
			v, err := strconv.ParseUint(string(a[6+8*i:6+8*i+8]), 16, 32)
			return int(v), err
		}
		size, err1 := field(6)
		nameSize, err2 := field(11)
		if err1 != nil || err2 != nil || 110+nameSize > len(a) {
			return nil, false, errors.New("cpio: bad header")
		}
		fname := string(bytes.TrimRight(a[110:110+nameSize], "\x00"))
		off := align4(110 + nameSize)
		if off+size > len(a) {
			return nil, false, errors.New("cpio: truncated")
		}
		if fname == "TRAILER!!!" {
			return nil, false, nil
		}
		if strings.TrimPrefix(strings.TrimPrefix(fname, "."), "/") == want {
			return a[off : off+size], true, nil
		}
		a = a[align4(off+size):]
	}
	return nil, false, nil
}

func align4(n int) int { return (n + 3) &^ 3 }

// CompareNVR compares two N-V-R strings with the rpm version order of the
// version and the release (the names are compared as text).
func CompareNVR(a, b string) int {
	na, va, ra := manifest.SplitNVR(a)
	nb, vb, rb := manifest.SplitNVR(b)
	if na != nb {
		return strings.Compare(na, nb)
	}
	if c := rpmvercmp(va, vb); c != 0 {
		return c
	}
	return rpmvercmp(ra, rb)
}

// rpmvercmp is the version comparison of rpm (rpmio/rpmvercmp.c).
func rpmvercmp(a, b string) int {
	if a == b {
		return 0
	}
	isAlnum := func(r byte) bool { return r < 128 && (unicode.IsLetter(rune(r)) || unicode.IsDigit(rune(r))) }
	isDigit := func(r byte) bool { return r >= '0' && r <= '9' }
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for i < len(a) && !isAlnum(a[i]) && a[i] != '~' && a[i] != '^' {
			i++
		}
		for j < len(b) && !isAlnum(b[j]) && b[j] != '~' && b[j] != '^' {
			j++
		}
		// Tilde sorts before everything, even the end.
		if i < len(a) && a[i] == '~' || j < len(b) && b[j] == '~' {
			if i >= len(a) || a[i] != '~' {
				return 1
			}
			if j >= len(b) || b[j] != '~' {
				return -1
			}
			i++
			j++
			continue
		}
		// Caret sorts after the end, but before everything else.
		if i < len(a) && a[i] == '^' || j < len(b) && b[j] == '^' {
			if i >= len(a) {
				return -1
			}
			if j >= len(b) {
				return 1
			}
			if a[i] != '^' {
				return 1
			}
			if b[j] != '^' {
				return -1
			}
			i++
			j++
			continue
		}
		if i >= len(a) || j >= len(b) {
			break
		}
		si, sj := i, j
		num := isDigit(a[i])
		if num {
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
		} else {
			for i < len(a) && isAlnum(a[i]) && !isDigit(a[i]) {
				i++
			}
			for j < len(b) && isAlnum(b[j]) && !isDigit(b[j]) {
				j++
			}
		}
		sa, sb := a[si:i], b[sj:j]
		if sb == "" {
			if num {
				return 1
			}
			return -1
		}
		if num {
			sa = strings.TrimLeft(sa, "0")
			sb = strings.TrimLeft(sb, "0")
			if len(sa) != len(sb) {
				if len(sa) > len(sb) {
					return 1
				}
				return -1
			}
		}
		if c := strings.Compare(sa, sb); c != 0 {
			return c
		}
	}
	switch {
	case i >= len(a) && j >= len(b):
		return 0
	case i >= len(a):
		return -1
	default:
		return 1
	}
}
