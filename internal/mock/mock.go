// Package mock runs mock(1) for repoctl: the wrapper config and the macro
// file of a target, the mock config hash, the names of the roots and the
// calls --buildsrpm, --rebuild, --chroot, --clean, --scrub, --orphanskill
// and --update (docs/design/build.html#mock, #mock-config).
//
// repoctl uses only the mock CLI and the plugins that ship with mock.
package mock

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// Exit codes of mock that repoctl reads (build.html#failures).
const (
	ExitBuildDep = 30 // dnf builddep failed
	ExitTimeout  = 11 // --rpmbuild_timeout stopped rpmbuild
)

// Target is the mock side of one target of the set.
type Target struct {
	ID string
	// Config is the mock config name, for example rocky+epel-9-x86_64.
	Config  string
	Plugins []string
	// Macros are the macro files of targets[].macros, absolute, in list
	// order.
	Macros []string
	// Repos are targets[].repos.
	Repos []string
}

// FromConfig gives the mock target of a set target. setDir resolves the
// macro files.
func FromConfig(t config.Target, setDir string) Target {
	mt := Target{ID: t.ID, Config: t.Mock, Plugins: t.Plugins, Repos: t.Repos}
	for _, m := range t.Macros {
		if !filepath.IsAbs(m) {
			m = filepath.Join(setDir, m)
		}
		mt.Macros = append(mt.Macros, m)
	}
	return mt
}

// Options are the host settings that change how mock runs, not what it
// builds (user config mock.*). The mock config hash does not cover them.
type Options struct {
	NoSync bool
	TmpFS  string
}

// Mock runs mock for one host.
type Mock struct {
	Paths config.Paths
	Opts  Options
	// Program is the mock binary, default "mock".
	Program string
}

// New gives a Mock.
func New(p config.Paths, o Options) *Mock { return &Mock{Paths: p, Opts: o, Program: "mock"} }

// WrapperConfig gives the text of the wrapper config WORK/TARGET.cfg: it
// includes the host config and adds the plugins of repoctl.
func (m *Mock) WrapperConfig(t Target) string {
	var b strings.Builder
	fmt.Fprintf(&b, "include('%s')\n", m.Paths.MockConfig(t.Config))
	b.WriteString("config_opts['cleanup_on_success'] = False   # repoctl reads the buildroot facts, then cleans\n")
	b.WriteString("config_opts['cleanup_on_failure'] = False   # a failed root stays for a look\n")
	for _, p := range t.Plugins {
		fmt.Fprintf(&b, "config_opts['plugin_conf']['%s_enable'] = True\n", p)
	}
	b.WriteString("config_opts['plugin_conf']['buildroot_lock_enable'] = True\n")
	b.WriteString("config_opts['plugin_conf']['chroot_scan_enable'] = True\n")
	b.WriteString("config_opts['plugin_conf']['chroot_scan_opts']['only_failed'] = True\n")
	b.WriteString("config_opts['plugin_conf']['chroot_scan_opts']['regexes'] = [\n")
	b.WriteString("    r'config\\.log$', r'test-suite\\.log$', r'CMakeError\\.log$', r'/core(\\.\\d+)?$']\n")
	if m.Opts.NoSync {
		b.WriteString("config_opts['nosync'] = True\n")
	}
	if m.Opts.TmpFS != "" {
		b.WriteString("config_opts['plugin_conf']['tmpfs_enable'] = True\n")
		fmt.Fprintf(&b, "config_opts['plugin_conf']['tmpfs_opts']['max_fs_size'] = '%s'\n", m.Opts.TmpFS)
	}
	return b.String()
}

// MacroText joins the macro files of the target in list order and adds
// the line %repoctl_target TARGET.
func MacroText(t Target) ([]byte, error) {
	var b bytes.Buffer
	for _, f := range t.Macros {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		b.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	fmt.Fprintf(&b, "%%repoctl_target %s\n", t.ID)
	return b.Bytes(), nil
}

// Files are the wrapper config and the macro file of a target in a work
// directory.
type Files struct {
	Config string // WORK/TARGET.cfg
	Macros string // WORK/TARGET.macros
	// MacrosSHA256 is the sha256 of the macro file (manifest field macros).
	MacrosSHA256 string
}

// WriteFiles writes the wrapper config and the macro file of the target
// into work.
func (m *Mock) WriteFiles(work string, t Target) (Files, error) {
	if err := os.MkdirAll(work, 0o755); err != nil {
		return Files{}, err
	}
	f := Files{Config: filepath.Join(work, t.ID+".cfg"), Macros: filepath.Join(work, t.ID+".macros")}
	if err := os.WriteFile(f.Config, []byte(m.WrapperConfig(t)), 0o644); err != nil {
		return Files{}, err
	}
	mt, err := MacroText(t)
	if err != nil {
		return Files{}, err
	}
	if err := os.WriteFile(f.Macros, mt, 0o644); err != nil {
		return Files{}, err
	}
	sum := sha256.Sum256(mt)
	f.MacrosSHA256 = hex.EncodeToString(sum[:])
	return f, nil
}

// volatileKeys change from call to call; ConfigHash removes them.
var volatileKeys = []string{"mock_run_uuid", "config_paths", "print_main_output", "verbose"}

// ConfigHash gives the mock config hash of a host config: the sha256 of
// mock -r CONFIG --debug-config without the volatile keys.
func (m *Mock) ConfigHash(ctx context.Context, configName string) (string, error) {
	out, err := (&run.Cmd{Name: m.Program, Args: []string{"-r", configName, "--debug-config"}}).Output(ctx)
	if err != nil {
		return "", err
	}
	return HashDebugConfig(out), nil
}

// HashDebugConfig hashes the output of --debug-config without the
// volatile keys.
func HashDebugConfig(out []byte) string {
	h := sha256.New()
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		skip := false
		for _, k := range volatileKeys {
			if strings.HasPrefix(line, "config_opts['"+k+"']") {
				skip = true
			}
		}
		if !skip {
			h.Write([]byte(line))
			h.Write([]byte{'\n'})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Uniqueext names: build, plan, shell and verify roots (build.html#mock).
func BuildExt(set, runID string, slot int) string {
	return fmt.Sprintf("%s-%s-%s-%d", config.MockRootPrefix(), set, runID, slot)
}

// PlanExt is the root of plan, graph, list and show.
func PlanExt() string { return fmt.Sprintf("%s-plan-%d", config.MockRootPrefix(), os.Getpid()) }

// ShellExt is the root of repoctl mock shell.
func ShellExt() string { return fmt.Sprintf("%s-shell-%d", config.MockRootPrefix(), os.Getpid()) }

// VerifyExt is the verify root of a set and target.
func VerifyExt(set, target string) string {
	return fmt.Sprintf("%s-%s-verify-%s", config.MockRootPrefix(), set, target)
}

// Call is the common part of a mock call.
type Call struct {
	Files     Files
	Uniqueext string
	ResultDir string
	// AddRepos are the --addrepo values: the local copy of latest, the
	// staging of the run, the staged view and targets[].repos.
	AddRepos []string
	// Log gets stdout and stderr of mock.
	Log io.Writer
	// Timeout is --rpmbuild_timeout, for Rebuild.
	Timeout time.Duration
}

func (m *Mock) args(c Call) []string {
	args := []string{"-r", c.Files.Config}
	if c.Uniqueext != "" {
		args = append(args, "--uniqueext", c.Uniqueext)
	}
	if c.ResultDir != "" {
		args = append(args, "--resultdir", c.ResultDir)
	}
	if c.Files.Macros != "" {
		args = append(args, "--macro-file", c.Files.Macros)
	}
	for _, r := range c.AddRepos {
		args = append(args, "--addrepo", r)
	}
	return args
}

func (m *Mock) run(ctx context.Context, c Call, extra ...string) error {
	cmd := &run.Cmd{Name: m.Program, Args: append(m.args(c), extra...), Stdout: c.Log, Stderr: c.Log, Group: true}
	return cmd.Run(ctx)
}

// BuildSRPM runs mock --buildsrpm --spec SPEC --sources DIR.
func (m *Mock) BuildSRPM(ctx context.Context, c Call, spec, sources string) error {
	return m.run(ctx, c, "--buildsrpm", "--spec", spec, "--sources", sources)
}

// Rebuild runs mock --rebuild SRPM.
func (m *Mock) Rebuild(ctx context.Context, c Call, srpm string) error {
	var extra []string
	if c.Timeout > 0 {
		extra = append(extra, "--rpmbuild_timeout", strconv.Itoa(int(c.Timeout/time.Second)))
	}
	return m.run(ctx, c, append(extra, "--rebuild", srpm)...)
}

// Chroot runs a command in the root and gives its stdout.
func (m *Mock) Chroot(ctx context.Context, c Call, command ...string) ([]byte, error) {
	cmd := &run.Cmd{Name: m.Program, Args: append(append(m.args(c), "--chroot", "--"), command...), Group: true}
	return cmd.Output(ctx)
}

// Clean runs mock --clean on the root.
func (m *Mock) Clean(ctx context.Context, c Call) error {
	return (&run.Cmd{Name: m.Program, Args: append(m.args(Call{Files: c.Files, Uniqueext: c.Uniqueext}), "--clean")}).Run(ctx)
}

// Scrub runs mock --scrub=WHAT on the root.
func (m *Mock) Scrub(ctx context.Context, c Call, what string) error {
	return (&run.Cmd{Name: m.Program, Args: append(m.args(Call{Files: c.Files, Uniqueext: c.Uniqueext}), "--scrub="+what)}).Run(ctx)
}

// OrphanSkill runs mock --orphanskill on the root: it stops each process in
// the root, and the mock of the build ends with exit 1.
func (m *Mock) OrphanSkill(ctx context.Context, c Call) error {
	return (&run.Cmd{Name: m.Program, Args: append(m.args(Call{Files: c.Files, Uniqueext: c.Uniqueext}), "--orphanskill")}).Run(ctx)
}

// Update runs mock --update.
func (m *Mock) Update(ctx context.Context, c Call) error {
	return m.run(ctx, c, "--update")
}

// BuildrootQuery is the command of the buildroot facts (build.html#buildroot-facts).
var BuildrootQuery = []string{"rpm", "-qa", "--qf", `PKG %{NEVRA}\t%{SHA256HEADER}\t%{RSAHEADER:pgpsig}\n`}

// BuildrootRPM is one line of the buildroot query.
type BuildrootRPM struct {
	NEVRA        string
	HeaderSHA256 string
	// Signer is the key id of the signature, "" for none.
	Signer string
}

// ParseBuildroot reads the lines that start with PKG.
func ParseBuildroot(out []byte) []BuildrootRPM {
	var res []BuildrootRPM
	for _, line := range strings.Split(string(out), "\n") {
		rest, ok := strings.CutPrefix(line, "PKG ")
		if !ok {
			continue
		}
		f := strings.SplitN(rest, "\t", 3)
		if len(f) < 3 {
			continue
		}
		r := BuildrootRPM{NEVRA: f[0], HeaderSHA256: f[1]}
		if i := strings.Index(f[2], "Key ID "); i >= 0 {
			r.Signer = strings.TrimSpace(f[2][i+len("Key ID "):])
		}
		res = append(res, r)
	}
	return res
}

// Roots gives the root directories of this user below the mock dir: each
// CONFIG-UNIQUEEXT and CONFIG-bootstrap-UNIQUEEXT whose uniqueext starts
// with repoctl-UID.
func (m *Mock) Roots() ([]string, error) {
	entries, err := os.ReadDir(m.Paths.MockDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	prefix := "-" + config.MockRootPrefix() + "-"
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.Contains(e.Name(), prefix) {
			out = append(out, filepath.Join(m.Paths.MockDir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// ClassOf gives the failure class of a failed mock call in a step from its
// exit code (build.html#failures). repoctl's own records (timeout, cancel)
// and root.log (host) come first; the caller checks them.
func ClassOf(step string, err error) string {
	switch run.ExitCode(err) {
	case ExitBuildDep:
		return "builddep"
	case ExitTimeout:
		return "timeout"
	case 1:
		if step == "srpm" {
			return "srpm"
		}
		return "build"
	}
	return "mock"
}

// HostProblem reports whether root.log shows a host problem: no space on a
// device, or a repo metadata error.
func HostProblem(rootLog []byte) bool {
	s := string(rootLog)
	return strings.Contains(s, "No space left on device") || strings.Contains(s, "Failed to download metadata") ||
		strings.Contains(s, "Cannot download repomd.xml")
}
