// Package srpm builds the source RPM of a node in the mock chroot of its
// target, from a source clone of the node commit, and keeps the result in
// the SRPM cache (docs/design/build.html#graph, #sources, #source-clone,
// #mock, #mock-config).
//
// The API has three parts:
//
//   - Host is the SRPM side of one repoctl process: the mock adapter, the
//     XDG paths, the source fetcher and the versions of mock and its
//     plugins. NewHost gives one.
//   - Host.PrepareTarget writes the wrapper config and the macro file of a
//     target into a work directory and reads the facts of its mock config:
//     the mock config hash and the macros of the chroot. It gives a Target.
//   - Host.Build makes the source clone of a node, puts its sources into
//     it, runs mock --buildsrpm and reads the SRPM: NVR, Name, the requires
//     (rpm -qp --requires) and the provides of the binary packages (rpmspec
//     -q --provides with the macros of the target). Host.Inspect reads only
//     the spec (no mock), for the provides of a node of another target.
//
// The plan, graph, list and show commands and the step plan of a build use
// the plan root (mock.PlanExt) and the SRPM cache. Step srpm of a build
// calls Build with its build root and Cache false, and keeps the SRPM of
// its work directory.
package srpm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// Host is the SRPM side of one repoctl process. Its methods are safe for
// concurrent use.
type Host struct {
	Mock  *mock.Mock
	Paths config.Paths
	Fetch *Fetcher

	verOnce sync.Once
	mockVer string
	plugVer string
	verErr  error

	mu    sync.Mutex
	roots map[string]mock.Call // roots that Build used, by config and uniqueext
}

// NewHost gives a Host with a fetcher on the source cache of p.
func NewHost(m *mock.Mock, p config.Paths) *Host {
	return &Host{Mock: m, Paths: p, Fetch: NewFetcher(p.SourceCache())}
}

// versions gives the mock version and the versions of the plugins that do
// not ship with mock, once per process.
func (h *Host) versions(ctx context.Context) (string, string, error) {
	h.verOnce.Do(func() {
		out, err := (&run.Cmd{Name: h.Mock.Program, Args: []string{"--version"}}).Output(ctx)
		if err != nil {
			h.verErr = err
			return
		}
		h.mockVer = strings.TrimSpace(string(out))
		// The rpmautospec plugin of mock needs rpmautospec-core; the other
		// plugins of repoctl ship with mock.
		q, _ := (&run.Cmd{Name: "rpm", Args: []string{"-q", "--qf", "%{NAME}-%{VERSION}-%{RELEASE}\n", "python3-rpmautospec-core", "python3-rpmautospec"}}).Output(ctx)
		h.plugVer = strings.TrimSpace(string(q))
	})
	return h.mockVer, h.plugVer, h.verErr
}

// Target is one target, ready for SRPM builds: its wrapper config and macro
// file in a work directory, and the facts of its mock config.
type Target struct {
	Mock  mock.Target
	Files mock.Files
	// ConfigHash is the mock config hash: the sha256 of mock -r CONFIG
	// --debug-config without the volatile keys (hex).
	ConfigHash string
	// Dist is config_opts['dist'], for example el9.
	Dist string
	// RHEL is the major release of the chroot, for example 9 (%rhel).
	RHEL string
	// Macros are the macros that rpmspec on the host gets, so that it reads
	// the spec as the chroot does: dist, rhel, elN and the macros of the
	// mock config, in this order.
	Macros [][2]string
	// Arch is the architecture of the chroot, for example x86_64.
	Arch string
}

// ID gives the target id.
func (t *Target) ID() string { return t.Mock.ID }

// PrepareTarget writes the wrapper config WORK/TARGET.cfg and the macro file
// WORK/TARGET.macros of the target (mock.WriteFiles) and reads the mock
// config hash and the macros of the chroot from one mock --debug-config.
func (h *Host) PrepareTarget(ctx context.Context, work string, t mock.Target, arch string) (*Target, error) {
	files, err := h.Mock.WriteFiles(work, t)
	if err != nil {
		return nil, err
	}
	out, err := (&run.Cmd{Name: h.Mock.Program, Args: []string{"-r", t.Config, "--debug-config"}}).Output(ctx)
	if err != nil {
		return nil, fmt.Errorf("mock config %s of target %s: %w", t.Config, t.ID, err)
	}
	tg := &Target{Mock: t, Files: files, ConfigHash: mock.HashDebugConfig(out), Arch: arch}
	tg.Dist, tg.RHEL, tg.Macros = chrootMacros(string(out))
	if tg.RHEL == "" {
		tg.RHEL = digits(t.ID)
	}
	return tg, nil
}

var (
	reDist       = regexp.MustCompile(`(?m)^config_opts\['dist'\]\s*=\s*['"]([^'"]*)['"]`)
	reReleasever = regexp.MustCompile(`(?m)^config_opts\['releasever'\]\s*=\s*['"]([^'"]*)['"]`)
	reMacroLine  = regexp.MustCompile(`(?m)^config_opts\['macros'\]\['(%[A-Za-z_][A-Za-z0-9_]*)'\]\s*=\s*['"]([^'"\n]*)['"]`)
	reMacroDict  = regexp.MustCompile(`(?m)^config_opts\['macros'\]\s*=\s*\{(.*)\}\s*$`)
	reDictItem   = regexp.MustCompile(`'(%[A-Za-z_][A-Za-z0-9_]*)'\s*:\s*'([^'\n]*)'`)
	reDigits     = regexp.MustCompile(`[0-9]+`)
)

// chrootMacros reads dist, the release and the macros of the output of mock
// --debug-config. mock prints the macros as one dict or one line per macro.
func chrootMacros(out string) (dist, rhel string, macros [][2]string) {
	if m := reDist.FindStringSubmatch(out); m != nil {
		dist = strings.TrimPrefix(m[1], ".")
	}
	var cfg [][2]string
	for _, m := range reMacroLine.FindAllStringSubmatch(out, -1) {
		cfg = append(cfg, [2]string{m[1][1:], m[2]})
	}
	if m := reMacroDict.FindStringSubmatch(out); m != nil {
		for _, it := range reDictItem.FindAllStringSubmatch(m[1], -1) {
			cfg = append(cfg, [2]string{it[1][1:], it[2]})
		}
	}
	for _, kv := range cfg {
		if kv[0] == "rhel" {
			rhel = kv[1]
		}
	}
	if rhel == "" {
		if m := reReleasever.FindStringSubmatch(out); m != nil {
			rhel = digits(m[1])
		}
	}
	if rhel == "" && strings.HasPrefix(dist, "el") {
		rhel = digits(dist)
	}
	if dist != "" {
		macros = append(macros, [2]string{"dist", "." + dist})
		if strings.HasPrefix(dist, "el") {
			macros = append(macros, [2]string{dist, "1"})
		}
	}
	if rhel != "" {
		macros = append(macros, [2]string{"rhel", rhel})
	}
	for _, kv := range cfg {
		if kv[0] != "dist" && kv[0] != "rhel" {
			macros = append(macros, kv)
		}
	}
	return dist, rhel, macros
}

func digits(s string) string { return reDigits.FindString(s) }

// rootKey names a root: the mock config and the uniqueext.
func rootKey(c mock.Call) string { return c.Files.Config + "\x00" + c.Uniqueext }

func (h *Host) noteRoot(c mock.Call) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.roots == nil {
		h.roots = map[string]mock.Call{}
	}
	h.roots[rootKey(c)] = mock.Call{Files: c.Files, Uniqueext: c.Uniqueext}
}

// Cleanup removes the roots that Build made: mock --clean and
// --scrub=bootstrap on each (the plan root ends with its command,
// build.html#mock). A build run does not call it for its build roots: it
// cleans each root after the buildroot facts.
func (h *Host) Cleanup(ctx context.Context) {
	h.mu.Lock()
	roots := h.roots
	h.roots = nil
	h.mu.Unlock()
	for _, c := range roots {
		_ = h.Mock.Clean(ctx, c)
		_ = h.Mock.Scrub(ctx, c, "bootstrap")
	}
}

// removeAll removes a directory tree that may hold read-only directories
// (git objects).
func removeAll(dir string) {
	if err := os.RemoveAll(dir); err == nil {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}
