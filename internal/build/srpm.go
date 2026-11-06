package build

import (
	"bufio"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/staging"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// SRPMBuilder is step srpm of a node: the source clone, the sources and
// mock --buildsrpm (build.html#source-clone, #sources). Engine implements
// it with the SRPM host of the srpm package; CloneBuilder is a builder
// without the SRPM cache that never downloads.
type SRPMBuilder interface {
	BuildSRPM(ctx context.Context, r SRPMRequest) (*SRPMResult, error)
}

// SRPMRequest is one step srpm.
type SRPMRequest struct {
	Node   api.NodeId
	Set    *config.Set
	Paths  config.Paths
	Source workspace.Source
	// SetDir is the root of the set repository.
	SetDir string
	// Clone is the directory of the source clone; it does not exist yet.
	Clone string
	// Work is a work directory of the step, for builders that make their
	// own source clone and result directory in it.
	Work string
	Mock *mock.Mock
	// Call is the mock call of the step: the wrapper config, the root, the
	// result directory, the repos and the log.
	Call mock.Call
}

// SRPMResult is the result of step srpm.
type SRPMResult struct {
	// SRPM is the path of the SRPM in the result directory.
	SRPM string
	// Sources are the sha256 of the source files of the build, by file
	// name (record draft).
	Sources map[string]string
	// Warnings of the step, for example a source from the cache.
	Warnings []api.Problem
}

// CloneBuilder makes the source clone with git clone --local --no-checkout
// and a detached checkout, puts the sources of the sources file from the
// source cache into it, and runs mock --buildsrpm. It never downloads.
type CloneBuilder struct{}

var reAutorelease = regexp.MustCompile(`%(\{)?auto(release|changelog)\b`)

// BuildSRPM runs step srpm.
func (CloneBuilder) BuildSRPM(ctx context.Context, r SRPMRequest) (*SRPMResult, error) {
	src := r.Source
	if err := ensureRepo(ctx, src); err != nil {
		return nil, srpmError(r.Node, "the package repository cannot be cloned: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.Clone), 0o755); err != nil {
		return nil, err
	}
	_ = os.RemoveAll(r.Clone)
	if _, err := git.New(filepath.Dir(r.Clone)).Run(ctx, "clone", "--quiet", "--local", "--no-checkout", src.Repo, r.Clone); err != nil {
		return nil, srpmError(r.Node, "git clone of %s: %v", src.Repo, err)
	}
	if _, err := git.New(r.Clone).Run(ctx, "checkout", "--quiet", "--detach", src.Commit); err != nil {
		return nil, srpmError(r.Node, "git checkout of %s: %v", short(src.Commit), err)
	}
	dir := r.Clone
	if src.Path != "" {
		dir = filepath.Join(r.Clone, src.Path)
	}
	spec, err := findSpec(dir, src.Package)
	if err != nil {
		return nil, srpmError(r.Node, "no spec file at commit %s", short(src.Commit))
	}
	if src.Path != "" {
		if b, err := os.ReadFile(spec); err == nil && reAutorelease.Match(b) {
			return nil, api.Errorf(api.PSourceInTreeAutorelease, "%s: the in-tree spec %s uses %%autorelease or %%autochangelog; give it a plain Release and %%changelog",
				r.Node, filepath.Base(spec)).WithNode(r.Node)
		}
	}
	if err := fetchSources(r.Paths, r.Node, dir); err != nil {
		return nil, err
	}
	sums, err := sourceSums(dir)
	if err != nil {
		return nil, err
	}
	if err := r.Mock.BuildSRPM(ctx, r.Call, spec, dir); err != nil {
		return nil, err
	}
	matches, _ := filepath.Glob(filepath.Join(r.Call.ResultDir, "*.src.rpm"))
	if len(matches) == 0 {
		return nil, srpmError(r.Node, "mock --buildsrpm made no SRPM")
	}
	sort.Strings(matches)
	return &SRPMResult{SRPM: matches[len(matches)-1], Sources: sums}, nil
}

func srpmError(n api.NodeId, format string, args ...any) error {
	return api.Errorf(api.PNodeFailed, format, args...).WithNode(n)
}

func short(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

// findSpec gives the spec of the package in dir: PKG.spec, else the one
// spec of the directory.
func findSpec(dir, pkg string) (string, error) {
	if p := filepath.Join(dir, pkg+".spec"); fileExists(p) {
		return p, nil
	}
	m, _ := filepath.Glob(filepath.Join(dir, "*.spec"))
	if len(m) == 0 {
		return "", os.ErrNotExist
	}
	sort.Strings(m)
	return m[0], nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

var reSourcesLine = regexp.MustCompile(`^SHA512 \((.+)\) = ([0-9a-f]{128})$`)

// fetchSources puts each file of the sources file that the commit does
// not hold into dir, from the source cache, and checks its SHA-512.
func fetchSources(p config.Paths, n api.NodeId, dir string) error {
	f, err := os.Open(filepath.Join(dir, "sources"))
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := reSourcesLine.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		name, sum := filepath.Base(m[1]), m[2]
		dst := filepath.Join(dir, name)
		if fileExists(dst) {
			continue
		}
		cached := filepath.Join(p.SourceCache(), "sha512", sum, name)
		if !fileExists(cached) {
			return api.Errorf(api.PSourceUnreachable, "%s: the source %s is not in the source cache", n, name).WithNode(n).WithPath(cached)
		}
		got, err := sha512File(cached)
		if err != nil {
			return err
		}
		if got != sum {
			return api.Errorf(api.PSourceMismatch, "%s: the SHA-512 of %s is not the value of the sources file", n, name).WithNode(n).WithPath(cached)
		}
		if err := os.Link(cached, dst); err != nil {
			if err := copyFile(cached, dst); err != nil {
				return err
			}
		}
	}
	return nil
}

func sha512File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// sourceSums gives the sha256 of each regular file of dir, without the
// git files.
func sourceSums(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".git") {
			continue
		}
		sum, _, err := staging.SHA256File(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out[e.Name()] = sum
	}
	return out, nil
}
