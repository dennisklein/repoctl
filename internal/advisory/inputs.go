package advisory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/repoctl/internal/advisory/rpmmd"
	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/sign"
	"github.com/dennisklein/repoctl/internal/tools/run"
	"github.com/dennisklein/repoctl/internal/upstream"
)

// Inputs are the inputs of the checks of one set: latest of each target
// (the local copy), the closure repos, the upstream release feeds and the
// mock config of this host. Each input is read once and shared by the
// codes.
type Inputs struct {
	Paths  config.Paths
	Set    *config.Set
	SetDir string
	// Lock is repoctl.lock; nil without one.
	Lock *lock.File
	// Mock runs mock --debug-config for the mock config hash and the
	// default closure repos.
	Mock *mock.Mock
	// Upstream answers upstream.release; nil skips the code.
	Upstream *upstream.Client
	// Repos reads the repodata of the closure repos.
	Repos *rpmmd.Reader
	// HTTP is the client of the mirror lists; nil is http.DefaultClient.
	HTTP *http.Client
	Now  func() time.Time

	mu      sync.Mutex
	latest  map[string]*latestEntry
	debug   map[string]*debugEntry
	closure map[string]*closureEntry
	ups     *upstreamEntry
	brs     map[string]*buildrootEntry
	trust   *sign.Trust
}

// Latest is latest of one target as the local copy holds it.
type Latest struct {
	Target   string
	Dir      string
	Manifest *manifest.Manifest
	// Packages are the binary RPMs of the repodata of the local copy.
	Packages []*rpmmd.Package
	// Snapshot is snapshot.json of the local copy, nil without one.
	Snapshot *api.Snapshot
}

// Digest gives the digest of latest that a result used.
func (l *Latest) Digest() api.Digest {
	if l.Snapshot != nil {
		return l.Snapshot.Digest
	}
	return ""
}

type latestEntry struct {
	l   *Latest
	err error
}

type debugEntry struct {
	out []byte
	err error
}

type closureEntry struct {
	pkgs []*rpmmd.Package
	err  error
}

type upstreamEntry struct {
	rep *upstream.Report
	err error
}

// SkipError tells that a code did not run on a target: the check is
// narrowed there. The last result of the code and target stays, and no
// advisory.gone is written.
type SkipError struct{ Reason string }

func (e *SkipError) Error() string { return e.Reason }

// IsSkip reports a SkipError.
func IsSkip(err error) (*SkipError, bool) {
	var s *SkipError
	ok := errors.As(err, &s)
	return s, ok
}

func (in *Inputs) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

// ReadSnapshotJSON reads snapshot.json of the local copy of a target; nil
// without one.
func ReadSnapshotJSON(p config.Paths, set, target string) *api.Snapshot {
	b, err := os.ReadFile(filepath.Join(p.LocalCopy(set, target), "snapshot.json"))
	if err != nil {
		return nil
	}
	var s api.Snapshot
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

// Latest gives latest of a target from its local copy. Without a local
// copy it gives a SkipError.
func (in *Inputs) Latest(ctx context.Context, target string) (*Latest, error) {
	in.mu.Lock()
	if in.latest == nil {
		in.latest = map[string]*latestEntry{}
	}
	if e, ok := in.latest[target]; ok {
		in.mu.Unlock()
		return e.l, e.err
	}
	in.mu.Unlock()
	l, err := in.readLatest(ctx, target)
	in.mu.Lock()
	in.latest[target] = &latestEntry{l: l, err: err}
	in.mu.Unlock()
	return l, err
}

func (in *Inputs) readLatest(ctx context.Context, target string) (*Latest, error) {
	dir := manifest.LocalRepo(in.Paths, in.Set, target)
	if !rpmmd.HasRepodata(dir) {
		return nil, &SkipError{Reason: "no local copy of latest of " + target}
	}
	m, err := manifest.ReadRepo(dir)
	if err != nil {
		return nil, err
	}
	pkgs, err := in.reader().Read(ctx, dir, "latest")
	if err != nil {
		return nil, err
	}
	var bin []*rpmmd.Package
	for _, p := range pkgs {
		if p.Arch != "src" {
			bin = append(bin, p)
		}
	}
	return &Latest{Target: target, Dir: dir, Manifest: m, Packages: bin, Snapshot: ReadSnapshotJSON(in.Paths, in.Set.Name, target)}, nil
}

func (in *Inputs) reader() *rpmmd.Reader {
	if in.Repos != nil {
		return in.Repos
	}
	return &rpmmd.Reader{Client: in.HTTP}
}

// DebugConfig gives the output of mock -r CONFIG --debug-config of the
// target.
func (in *Inputs) DebugConfig(ctx context.Context, target string) ([]byte, error) {
	in.mu.Lock()
	if in.debug == nil {
		in.debug = map[string]*debugEntry{}
	}
	if e, ok := in.debug[target]; ok {
		in.mu.Unlock()
		return e.out, e.err
	}
	in.mu.Unlock()
	t, ok := in.Set.Target(target)
	var out []byte
	var err error
	if !ok {
		err = fmt.Errorf("no target %s", target)
	} else if _, statErr := os.Stat(in.Paths.MockConfig(t.Mock)); statErr != nil {
		err = api.Errorf(api.PMockConfigMissing, "the mock config %s of %s is not on this host", t.Mock, target).
			WithTarget(target).WithHint("install it, or correct targets[].mock", "repoctl doctor")
	} else {
		m := in.Mock
		if m == nil {
			m = mock.New(in.Paths, mock.Options{})
		}
		out, err = (&run.Cmd{Name: m.Program, Args: []string{"-r", t.Mock, "--debug-config"}}).Output(ctx)
		if err != nil {
			err = api.Errorf(api.PMockUnavailable, "mock -r %s --debug-config: %v", t.Mock, err).WithTarget(target).WithCause(err)
		}
	}
	in.mu.Lock()
	in.debug[target] = &debugEntry{out: out, err: err}
	in.mu.Unlock()
	return out, err
}

// HostMockHash gives the mock config hash and the sha256 of the macro file
// of a target on this host, both as sha256:HEX.
func (in *Inputs) HostMockHash(ctx context.Context, target string) (cfg, macros api.Digest, err error) {
	out, err := in.DebugConfig(ctx, target)
	if err != nil {
		return "", "", err
	}
	t, _ := in.Set.Target(target)
	mt, err := mock.MacroText(mock.FromConfig(*t, in.SetDir))
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(mt)
	return "sha256:" + mock.HashDebugConfig(out), "sha256:" + hex.EncodeToString(sum[:]), nil
}

// DriftHash gives the one hash of the mock config and the macros that
// mock.drift compares: sha256 of "CONFIG\nMACROS\n", both as sha256:HEX.
func DriftHash(cfg, macros string) api.Digest {
	sum := sha256.Sum256([]byte(normDigest(cfg) + "\n" + normDigest(macros) + "\n"))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normDigest(d string) string {
	if d == "" || strings.HasPrefix(d, "sha256:") {
		return d
	}
	return "sha256:" + d
}

// ClosureRepo is one closure repo of a target.
type ClosureRepo struct {
	ID  string
	URL string
}

// ClosureRepos gives the closure repos of a target: targets[].closure_repos,
// else the enabled repos of the mock config of the target, plus
// targets[].repos.
func (in *Inputs) ClosureRepos(ctx context.Context, target string) ([]ClosureRepo, error) {
	t, ok := in.Set.Target(target)
	if !ok {
		return nil, fmt.Errorf("no target %s", target)
	}
	if len(t.ClosureRepos) > 0 {
		var out []ClosureRepo
		for _, r := range t.ClosureRepos {
			out = append(out, ClosureRepo{ID: r.ID, URL: r.URL})
		}
		return out, nil
	}
	out, err := in.DebugConfig(ctx, target)
	if err != nil {
		return nil, err
	}
	repos := MockRepos(out, t.Arch())
	for i, u := range t.Repos {
		repos = append(repos, ClosureRepo{ID: fmt.Sprintf("repos-%d", i+1), URL: u})
	}
	return repos, nil
}

// ClosurePackages gives the packages of the closure repos of a target.
func (in *Inputs) ClosurePackages(ctx context.Context, target string) ([]*rpmmd.Package, error) {
	in.mu.Lock()
	if in.closure == nil {
		in.closure = map[string]*closureEntry{}
	}
	if e, ok := in.closure[target]; ok {
		in.mu.Unlock()
		return e.pkgs, e.err
	}
	in.mu.Unlock()
	var pkgs []*rpmmd.Package
	repos, err := in.ClosureRepos(ctx, target)
	for _, r := range repos {
		if err != nil {
			break
		}
		base := r.URL
		if strings.HasPrefix(base, "mirrorlist=") || strings.HasPrefix(base, "metalink=") {
			base, err = rpmmd.ResolveMirror(ctx, in.HTTP, base)
			if err != nil {
				err = api.Errorf(api.PUpstreamUnreachable, "%s: closure repo %s: %v", target, r.ID, err).WithTarget(target)
				break
			}
		}
		var p []*rpmmd.Package
		p, err = in.reader().Read(ctx, base, r.ID)
		if err != nil {
			err = api.Errorf(api.PUpstreamUnreachable, "%s: closure repo %s does not answer: %v", target, r.ID, err).WithTarget(target).WithCause(err)
			break
		}
		for _, x := range p {
			if x.Arch != "src" {
				pkgs = append(pkgs, x)
			}
		}
	}
	if err != nil {
		pkgs = nil
	}
	in.mu.Lock()
	in.closure[target] = &closureEntry{pkgs: pkgs, err: err}
	in.mu.Unlock()
	return pkgs, err
}

// MockRepos reads the enabled repos of the dnf.conf of a mock config from
// the output of mock --debug-config. A repo with only a mirror list or a
// metalink gives the URL mirrorlist=URL or metalink=URL.
func MockRepos(debug []byte, arch string) []ClosureRepo {
	vars := map[string]string{"basearch": arch, "arch": arch}
	var dnfConf string
	sc := bufio.NewScanner(bytes.NewReader(debug))
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "config_opts['releasever'] = "); ok {
			vars["releasever"] = unquotePy(v)
		}
		for _, key := range []string{"config_opts['dnf.conf'] = ", "config_opts['yum.conf'] = "} {
			if v, ok := strings.CutPrefix(line, key); ok {
				dnfConf = unquotePy(v)
			}
		}
	}
	type section struct {
		id, baseurl, mirror string
		enabled             bool
	}
	var secs []*section
	var cur *section
	for _, l := range strings.Split(dnfConf, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			cur = &section{id: strings.Trim(l, "[]"), enabled: true}
			if cur.id != "main" {
				secs = append(secs, cur)
			}
			continue
		}
		if cur == nil {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "enabled":
			cur.enabled = v == "1" || v == "True" || v == "true"
		case "baseurl":
			cur.baseurl = strings.Fields(strings.ReplaceAll(v, ",", " "))[0]
		case "mirrorlist":
			if cur.mirror == "" {
				cur.mirror = "mirrorlist=" + v
			}
		case "metalink":
			cur.mirror = "metalink=" + v
		}
	}
	var out []ClosureRepo
	for _, s := range secs {
		if !s.enabled {
			continue
		}
		u := s.baseurl
		if u == "" {
			u = s.mirror
		}
		if u == "" {
			continue
		}
		for k, v := range vars {
			u = strings.ReplaceAll(u, "$"+k, v)
		}
		out = append(out, ClosureRepo{ID: s.id, URL: u})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// unquotePy reads a Python string literal of --debug-config.
func unquotePy(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	r := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\'`, "'", `\"`, `"`, `\\`, `\`)
	return r.Replace(s)
}

// UpstreamReport gives the upstream state of each package of the set, once
// per check.
func (in *Inputs) UpstreamReport(ctx context.Context) (*upstream.Report, error) {
	in.mu.Lock()
	if in.ups != nil {
		e := in.ups
		in.mu.Unlock()
		return e.rep, e.err
	}
	in.mu.Unlock()
	e := &upstreamEntry{}
	if in.Upstream == nil {
		e.err = &SkipError{Reason: "no upstream client"}
	} else {
		e.rep, e.err = in.Upstream.Query(ctx, in.Set, in.SetDir, in.Lock, nil)
	}
	in.mu.Lock()
	in.ups = e
	in.mu.Unlock()
	return e.rep, e.err
}
