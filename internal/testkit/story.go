package testkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"
)

// SetName and SetBranch are the set of the story (build.html#story).
const (
	SetName   = "slurm-26.05"
	SetBranch = "set/slurm-26.05"
)

// Pkg is one package of the story fixture.
type Pkg struct {
	ID      string
	Name    string // RPM name; default ID
	Version string
	// Branch is the package branch; "" for an in-tree package.
	Branch string
	// External packages live in their own bare repository (git.url).
	External bool
	// Path is the in-tree path below the set root.
	Path    string
	Targets []string
	// BuildRequires of the spec.
	BuildRequires []string
	// Provides adds Provides to the main package, for example sonames.
	Provides []string
	// Requires adds Requires to the main package.
	Requires []string
	// Subpackages besides the main package: devel by default.
	Subpackages []string
	Upstream    string
	Notes       string
	Role        string
	// Build is the shell of %build; "" does nothing. "false" fails the build.
	Build string
}

// StoryPackages are the packages of the set slurm-26.05 in the order of
// repoctl.yaml. Edges on el9: 14, on el10: 13 (rocm-smi is el9 only).
func StoryPackages() []*Pkg {
	return []*Pkg{
		{ID: "munge", Version: "0.5.18", Branch: "munge/slurm-26.05", Upstream: "{github: dun/munge, fedora: munge}",
			Provides: []string{"libmunge.so.2()(64bit)"}},
		{ID: "ucx", Version: "1.22.0", Branch: "ucx/slurm-26.05", Upstream: "{github: openucx/ucx, fedora: ucx}",
			Provides: []string{"libucp.so.0()(64bit)", "libucs.so.0()(64bit)", "libuct.so.0()(64bit)"}, Subpackages: []string{"devel", "debuginfo"}},
		{ID: "rocm-smi", Version: "7.14.0", Branch: "slurm-26.05", External: true, Targets: []string{"el9"},
			Upstream: "{github: ROCm/rocm-systems, tags: '^therock-[0-9]'}", Notes: "Follow the TheRock stream. rocm-X.Y.Z is frozen at 7.2.4."},
		{ID: "pmix", Version: "6.1.0", Branch: "pmix/slurm-26.05", BuildRequires: []string{"munge-devel"},
			Upstream: "{github: openpmix/openpmix, tags: '^v6', fedora: pmix}", Provides: []string{"libpmix.so.2()(64bit)"}},
		{ID: "pmix_v5", Version: "5.0.9", Branch: "pmix/slurm-26.05-v5", BuildRequires: []string{"munge-devel"},
			Upstream: "{github: openpmix/openpmix, tags: '^v5'}"},
		{ID: "pmix_v4", Version: "4.2.9", Branch: "pmix/slurm-26.05-v4", BuildRequires: []string{"munge-devel"},
			Upstream: "{github: openpmix/openpmix, tags: '^v4', frozen: 4.2.9}", Notes: "Upstream 4.x is frozen at 4.2.9."},
		{ID: "prrte", Version: "4.0.0", Branch: "prrte/slurm-26.05", BuildRequires: []string{"pmix-devel"},
			Upstream: "{github: openpmix/prrte, fedora: prrte}"},
		{ID: "slurm", Version: "26.05.4", Branch: "slurm/slurm-26.05",
			BuildRequires: []string{"munge-devel", "ucx-devel", "rocm-smi-devel", "pmix-devel", "pmix_v5-devel", "pmix_v4-devel"},
			Requires:      []string{"libucp.so.0()(64bit)"},
			Upstream:      `{github: SchedMD/slurm, tags: '^slurm-', version: '^slurm-(\d+)-(\d+)-(\d+)', fedora: slurm}`},
		{ID: "openmpi", Version: "5.0.11", Branch: "openmpi/slurm-26.05", BuildRequires: []string{"ucx-devel", "pmix-devel", "prrte-devel"},
			Requires: []string{"libucp.so.0()(64bit)"}, Upstream: `{github: open-mpi/ompi, tags: '^v5\.', fedora: openmpi}`},
		{ID: "slurm-singularity-exec", Version: "3.2.0", Branch: "slurm-singularity-exec/slurm-26.05", BuildRequires: []string{"slurm-devel"},
			Upstream: "{github: GSI-HPC/slurm-singularity-exec}"},
		{ID: "slurm-release", Version: "26.05", Path: "packages/slurm-release", Role: "release"},
	}
}

// Story is a set repository with the packages of the story, a fake host
// (fake mock, mock configs, XDG directories) and the repoctl binary.
type Story struct {
	T    testing.TB
	Root string
	// Origin is the bare set repository: the remote origin of SetDir.
	Origin string
	// Externals maps an external package to its bare repository.
	Externals map[string]string
	// SetDir is a clone of Origin on the set branch, without worktrees.
	SetDir string
	Home   string
	Bin    string
	// MockDir and MockConfigDir are the fake /var/lib/mock and /etc/mock.
	MockDir, MockConfigDir string
	// MockLog gets one JSON line per call of the fake mock.
	MockLog string
	Pkgs    []*Pkg
	// Seed is a work clone of Origin that the fixture commits in.
	Seed string

	env   map[string]string
	clock time.Time
}

// Option changes the story before it is written.
type Option func(*Story)

// WithPackages replaces the packages of the story.
func WithPackages(p ...*Pkg) Option { return func(s *Story) { s.Pkgs = p } }

// Modify changes one package before it is written.
func Modify(id string, f func(*Pkg)) Option {
	return func(s *Story) {
		for _, p := range s.Pkgs {
			if p.ID == id {
				f(p)
			}
		}
	}
}

// NewStory writes the story fixture. It sets the environment of the test
// process (HOME, XDG_*, PATH, GIT_*), so a test with a story must not call
// t.Parallel.
func NewStory(t testing.TB, opts ...Option) *Story {
	t.Helper()
	root, err := os.MkdirTemp("", "rs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", filepath.Join(root, "home", ".gnupg"), "--kill", "all").Run()
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
		_ = os.RemoveAll(root)
	})
	s := &Story{
		T: t, Root: root, Externals: map[string]string{},
		Home: filepath.Join(root, "home"), Bin: filepath.Join(root, "bin"),
		MockDir: filepath.Join(root, "mock"), MockConfigDir: filepath.Join(root, "etc-mock"),
		MockLog: filepath.Join(root, "mock.log"),
		Pkgs:    StoryPackages(),
		clock:   time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
	}
	for _, o := range opts {
		o(s)
	}
	for _, d := range []string{s.Home, s.Bin, s.MockDir, s.MockConfigDir, filepath.Join(root, "run")} {
		must(t, os.MkdirAll(d, 0o700))
	}
	s.setEnv()
	s.writeBin()
	s.writeMockConfigs()
	s.writeRepos()
	return s
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (s *Story) setEnv() {
	gitcfg := filepath.Join(s.Home, ".gitconfig")
	must(s.T, os.WriteFile(gitcfg, []byte(`[user]
	name = Dennis Klein
	email = d.klein@example.org
[init]
	defaultBranch = main
[commit]
	gpgsign = false
[protocol "file"]
	allow = always
[advice]
	detachedHead = false
`), 0o644))
	s.env = map[string]string{
		"HOME":                         s.Home,
		"USER":                         "tester",
		"XDG_CONFIG_HOME":              filepath.Join(s.Home, ".config"),
		"XDG_CACHE_HOME":               filepath.Join(s.Home, ".cache"),
		"XDG_DATA_HOME":                filepath.Join(s.Home, ".local", "share"),
		"XDG_STATE_HOME":               filepath.Join(s.Home, ".local", "state"),
		"XDG_RUNTIME_DIR":              filepath.Join(s.Root, "run"),
		"GIT_CONFIG_GLOBAL":            gitcfg,
		"GIT_CONFIG_NOSYSTEM":          "1",
		"GNUPGHOME":                    filepath.Join(s.Home, ".gnupg"),
		"PATH":                         s.Bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"REPOCTL_TEST_MOCK_DIR":        s.MockDir,
		"REPOCTL_TEST_MOCK_CONFIG_DIR": s.MockConfigDir,
		"REPOCTL_TEST_HOST":            "1",
		"FAKEMOCK_LOG":                 s.MockLog,
		"LC_ALL":                       "C.UTF-8",
		"TZ":                           "UTC",
		"NO_COLOR":                     "",
		"REPOCTL_DIR":                  "",
		"REPOCTL_TARGET":               "",
		"REPOCTL_JSON":                 "",
		"REPOCTL_CONFIG":               "",
		// A CI runner under systemd passes these on; repoctl reads them as
		// a run by a timer.
		"INVOCATION_ID":  "",
		"JOURNAL_STREAM": "",
	}
	for k, v := range s.env {
		s.T.Setenv(k, v)
	}
}

// Env gives the environment of repoctl in the story, as KEY=VALUE.
func (s *Story) Env() []string {
	out := os.Environ()
	keys := make([]string, 0, len(s.env))
	for k := range s.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+s.env[k])
	}
	return out
}

// Setenv sets a variable of the story environment.
func (s *Story) Setenv(k, v string) {
	s.env[k] = v
	s.T.Setenv(k, v)
}

var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
)

// ModuleRoot gives the root of the repoctl module.
func ModuleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// Binaries builds repoctl and the fake mock once per test process and gives
// their directory.
func Binaries(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "repoctl-bin-")
		if buildErr != nil {
			return
		}
		for name, pkg := range map[string]string{"repoctl": "./cmd/repoctl", "mock": "./internal/testkit/fakemock"} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(buildDir, name), pkg)
			cmd.Dir = ModuleRoot()
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("go build %s: %w\n%s", pkg, err, out)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildDir
}

func (s *Story) writeBin() {
	dir := Binaries(s.T)
	for _, n := range []string{"repoctl", "mock"} {
		b, err := os.ReadFile(filepath.Join(dir, n))
		must(s.T, err)
		must(s.T, os.WriteFile(filepath.Join(s.Bin, n), b, 0o755))
	}
	must(s.T, os.WriteFile(filepath.Join(s.Bin, "podman"), []byte("#!/bin/sh\necho 'podman version 5.6.0'\n"), 0o755))
}

// MockConfig is the text of a fake host mock config.
func MockConfig(root, dist, rhel string, provides ...string) string {
	q := make([]string, len(provides))
	for i, p := range provides {
		q[i] = "'" + p + "'"
	}
	return fmt.Sprintf("config_opts['root'] = '%s'\nconfig_opts['dist'] = '%s'\nconfig_opts['macros']['%%rhel'] = '%s'\nconfig_opts['fakemock_provides'] = [%s]\n",
		root, dist, rhel, strings.Join(q, ", "))
}

func (s *Story) writeMockConfigs() {
	base := []string{"gcc", "gcc-c++", "make", "systemd-rpm-macros", "hwloc-devel", "numactl-devel", "rdma-core-devel", "openssl-devel", "libevent-devel"}
	must(s.T, os.WriteFile(filepath.Join(s.MockConfigDir, "rocky+epel-9-x86_64.cfg"), []byte(MockConfig("rocky+epel-9-x86_64", "el9", "9", base...)), 0o644))
	must(s.T, os.WriteFile(filepath.Join(s.MockConfigDir, "rocky+epel-10-x86_64.cfg"), []byte(MockConfig("rocky+epel-10-x86_64", "el10", "10", base...)), 0o644))
}

// Git runs git in dir and gives its trimmed stdout.
func (s *Story) Git(dir string, args ...string) string {
	s.T.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(s.Env(), "GIT_AUTHOR_DATE="+s.clock.Format(time.RFC3339), "GIT_COMMITTER_DATE="+s.clock.Format(time.RFC3339))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		s.T.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

// Tick moves the clock of the fixture commits by one minute.
func (s *Story) Tick() { s.clock = s.clock.Add(time.Minute) }

var specTmpl = template.Must(template.New("spec").Parse(`Name:           {{.Name}}
Version:        {{.Version}}
Release:        {{.Release}}
Summary:        {{.Name}} for the repoctl tests
License:        MIT
URL:            https://example.org/{{.Name}}
Source0:        README.{{.Name}}
{{range .BuildRequires}}BuildRequires:  {{.}}
{{end}}{{range .Provides}}Provides:       {{.}}
{{end}}{{range .Requires}}Requires:       {{.}}
{{end}}
%description
{{.Name}} for the repoctl tests.
{{range .Subs}}
%package {{.}}
Summary:        {{$.Name}} {{.}}
Requires:       %{name}%{?_isa} = %{version}-%{release}

%description {{.}}
{{$.Name}} {{.}} for the repoctl tests.
{{end}}
%prep

%build
{{.Build}}

%install
mkdir -p %{buildroot}%{_datadir}/{{.Name}}
install -m 0644 %{SOURCE0} %{buildroot}%{_datadir}/{{.Name}}/README
{{range .Subs}}mkdir -p %{buildroot}%{_datadir}/{{$.Name}}-{{.}}
echo {{.}} > %{buildroot}%{_datadir}/{{$.Name}}-{{.}}/{{.}}.txt
{{end}}
%files
%{_datadir}/{{.Name}}
{{range .Subs}}
%files {{.}}
%{_datadir}/{{$.Name}}-{{.}}
{{end}}
%changelog
{{.Changelog}}
`))

// Spec gives the spec file of a package.
func (p *Pkg) Spec() string {
	name := p.Name
	if name == "" {
		name = p.ID
	}
	subs := p.Subpackages
	if subs == nil && p.Role != "release" {
		subs = []string{"devel"}
	}
	data := map[string]any{
		"Name": name, "Version": p.Version, "BuildRequires": p.BuildRequires, "Provides": p.Provides,
		"Requires": p.Requires, "Subs": subs, "Build": p.Build,
		"Release": "%autorelease", "Changelog": "%autochangelog",
	}
	if p.Path != "" {
		data["Release"] = "1%{?dist}"
		data["Changelog"] = "* Tue Sep 01 2026 Dennis Klein <d.klein@example.org> - " + p.Version + "-1\n- First release"
	}
	var b bytes.Buffer
	if err := specTmpl.Execute(&b, data); err != nil {
		panic(err)
	}
	return b.String()
}

// SpecName gives the file name of the spec.
func (p *Pkg) SpecName() string {
	if p.Name != "" {
		return p.Name + ".spec"
	}
	return p.ID + ".spec"
}

func (p *Pkg) rpmName() string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

func (s *Story) writeFiles(dir string, files map[string]string) {
	for name, text := range files {
		p := filepath.Join(dir, name)
		must(s.T, os.MkdirAll(filepath.Dir(p), 0o755))
		must(s.T, os.WriteFile(p, []byte(text), 0o644))
	}
}

func (p *Pkg) files() map[string]string {
	return map[string]string{
		p.SpecName():            p.Spec(),
		"README." + p.rpmName(): p.ID + " " + p.Version + "\n",
		".gitignore":            "/*.tar.*\n",
	}
}

func (s *Story) writeRepos() {
	s.Origin = filepath.Join(s.Root, "origin.git")
	s.Git(s.Root, "init", "-q", "--bare", s.Origin)
	s.Seed = filepath.Join(s.Root, "seed")
	s.Git(s.Root, "init", "-q", s.Seed)
	s.Git(s.Seed, "remote", "add", "origin", s.Origin)
	commits := map[string]string{}
	for _, p := range s.Pkgs {
		if p.Path != "" {
			continue
		}
		dir := s.Seed
		if p.External {
			bare := filepath.Join(s.Root, "ext", p.ID+".git")
			s.Git(s.Root, "init", "-q", "--bare", bare)
			s.Externals[p.ID] = bare
			dir = filepath.Join(s.Root, "ext", p.ID)
			s.Git(s.Root, "init", "-q", dir)
			s.Git(dir, "remote", "add", "origin", bare)
		}
		s.Git(dir, "checkout", "-q", "--orphan", p.Branch)
		s.Git(dir, "rm", "-rfq", "--ignore-unmatch", ".")
		s.writeFiles(dir, p.files())
		s.Git(dir, "add", "-A")
		s.Tick()
		s.Git(dir, "commit", "-qm", fmt.Sprintf("Import %s %s", p.ID, p.Version))
		commits[p.ID] = s.Git(dir, "rev-parse", "HEAD")
		s.Git(dir, "push", "-q", "origin", p.Branch)
	}
	// The set branch.
	s.Git(s.Seed, "checkout", "-q", "--orphan", SetBranch)
	s.Git(s.Seed, "rm", "-rfq", "--ignore-unmatch", ".")
	files := map[string]string{
		"repoctl.yaml": s.SetYAML(),
		".gitignore":   "/packages/*\n!/packages/slurm-release/\n",
		"rpm/macros":   "%_with_pmix 1\n",
	}
	for _, p := range s.Pkgs {
		if p.Path != "" {
			for n, t := range p.files() {
				files[filepath.Join(p.Path, n)] = t
			}
		}
	}
	s.writeFiles(s.Seed, files)
	s.Git(s.Seed, "add", "-A")
	s.Tick()
	s.Git(s.Seed, "commit", "-qm", "Start the set "+SetName)
	s.writeFiles(s.Seed, map[string]string{"repoctl.lock": s.LockText(commits)})
	s.Git(s.Seed, "add", "repoctl.lock")
	s.Tick()
	s.Git(s.Seed, "commit", "-qm", "Pin the packages")
	s.Git(s.Seed, "push", "-q", "origin", SetBranch)
	s.SetDir = filepath.Join(s.Root, "slurm-repo")
	s.Git(s.Root, "clone", "-q", "--branch", SetBranch, s.Origin, s.SetDir)
}

// SetYAML gives repoctl.yaml of the story.
func (s *Story) SetYAML() string {
	var b strings.Builder
	b.WriteString(`apiVersion: repoctl/v1alpha1
name: slurm-26.05

repo:
  name: slurm
  url: https://repo.example.org/slurm-26.05

targets:
  - id: el9
    mock: rocky+epel-9-x86_64
    plugins: [rpmautospec]
    macros: [rpm/macros]
    path: el9
  - id: el10
    mock: rocky+epel-10-x86_64
    plugins: [rpmautospec]
    macros: [rpm/macros]
    path: el10

defaults:
  git:
    remote: origin

packages:
`)
	for _, p := range s.Pkgs {
		fmt.Fprintf(&b, "  - id: %s\n", p.ID)
		if p.External {
			fmt.Fprintf(&b, "    git: {url: %s}\n", "file://"+filepath.Join(s.Root, "ext", p.ID+".git"))
		}
		if p.Branch != "" {
			fmt.Fprintf(&b, "    branch: %s\n", p.Branch)
		}
		if p.Path != "" {
			fmt.Fprintf(&b, "    path: %s\n", p.Path)
		}
		if len(p.Targets) > 0 {
			fmt.Fprintf(&b, "    targets: [%s]\n", strings.Join(p.Targets, ", "))
		}
		if p.Upstream != "" {
			fmt.Fprintf(&b, "    upstream: %s\n", p.Upstream)
		}
		if p.Notes != "" {
			fmt.Fprintf(&b, "    notes: %s\n", p.Notes)
		}
		if p.Role != "" {
			fmt.Fprintf(&b, "    role: %s\n", p.Role)
		}
	}
	return b.String()
}

// LockText gives repoctl.lock for these commits.
func (s *Story) LockText(commits map[string]string) string {
	var b strings.Builder
	b.WriteString("# Written by repoctl pin. Do not edit.\napiVersion: repoctl/v1alpha1\npackages:\n")
	for _, p := range s.Pkgs {
		c, ok := commits[p.ID]
		if !ok {
			continue
		}
		if p.External {
			fmt.Fprintf(&b, "  %s:\n    url: %s\n    branch: %s\n    commit: %s\n", p.ID, "file://"+s.Externals[p.ID], p.Branch, c)
			continue
		}
		fmt.Fprintf(&b, "  %s: {branch: %s, commit: %s}\n", p.ID, p.Branch, c)
	}
	return b.String()
}

// Pkg gives a package of the story.
func (s *Story) Pkg(id string) *Pkg {
	for _, p := range s.Pkgs {
		if p.ID == id {
			return p
		}
	}
	s.T.Fatalf("no package %s in the story", id)
	return nil
}

// Commit adds a commit to the branch of a package on its remote and gives
// the commit. change edits the package before the files are written again.
func (s *Story) Commit(id, msg string, change func(p *Pkg)) string {
	s.T.Helper()
	p := s.Pkg(id)
	dir := s.Seed
	if p.External {
		dir = filepath.Join(s.Root, "ext", p.ID)
	}
	if p.Path != "" {
		s.T.Fatalf("Commit: %s is in-tree; change the set directory", id)
	}
	s.Git(dir, "checkout", "-q", p.Branch)
	if change != nil {
		change(p)
	}
	s.writeFiles(dir, p.files())
	s.Git(dir, "add", "-A")
	s.Tick()
	s.Git(dir, "commit", "-q", "--allow-empty", "-m", msg)
	c := s.Git(dir, "rev-parse", "HEAD")
	s.Git(dir, "push", "-q", "origin", p.Branch)
	return c
}

// Bump sets a new version of a package in a new commit on its remote.
func (s *Story) Bump(id, version string) string {
	return s.Commit(id, fmt.Sprintf("Update to %s", version), func(p *Pkg) { p.Version = version })
}

// BranchCommit gives the commit of a package branch on its remote.
func (s *Story) BranchCommit(id string) string {
	p := s.Pkg(id)
	repo := s.Origin
	if p.External {
		repo = s.Externals[id]
	}
	return s.Git(repo, "rev-parse", p.Branch)
}

// Result is the result of one repoctl call.
type Result struct {
	Stdout, Stderr string
	Code           int
}

// Run runs repoctl in the set directory.
func (s *Story) Run(args ...string) Result { return s.RunIn(s.SetDir, args...) }

// RunIn runs repoctl in dir.
func (s *Story) RunIn(dir string, args ...string) Result {
	s.T.Helper()
	cmd := exec.Command(filepath.Join(s.Bin, "repoctl"), args...)
	cmd.Dir = dir
	cmd.Env = s.Env()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := Result{Stdout: out.String(), Stderr: errb.String()}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			s.T.Fatalf("repoctl %s: %v", strings.Join(args, " "), err)
		}
		r.Code = ee.ExitCode()
	}
	return r
}

// MustRun runs repoctl in the set directory and fails the test on a
// non-zero exit.
func (s *Story) MustRun(args ...string) Result {
	s.T.Helper()
	r := s.Run(args...)
	if r.Code != 0 {
		s.T.Fatalf("repoctl %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), r.Code, r.Stdout, r.Stderr)
	}
	return r
}

// RunJSON runs repoctl with --json, validates the envelope against the
// output schema and gives it.
func (s *Story) RunJSON(args ...string) (map[string]any, Result) {
	s.T.Helper()
	r := s.Run(append(args, "--json")...)
	if err := Validate("Envelope", []byte(r.Stdout)); err != nil {
		s.T.Fatalf("repoctl %s --json: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, r.Stdout, r.Stderr)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &env); err != nil {
		s.T.Fatal(err)
	}
	return env, r
}
