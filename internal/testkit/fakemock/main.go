// Command fakemock stands in for mock(1) in the tests of repoctl. It builds
// with rpmbuild on the host and emulates the parts of the mock CLI that
// repoctl uses: -r with a wrapper config, --uniqueext, --resultdir,
// --macro-file, --addrepo, --buildsrpm, --rebuild, --chroot, --shell,
// --debug-config, --clean, --scrub, --orphanskill, --update and the
// plugins rpmautospec and buildroot_lock.
//
// A mock config is a file NAME.cfg in the directory of the include line of
// the wrapper config (REPOCTL_TEST_MOCK_CONFIG_DIR). fakemock reads these
// keys of it:
//
//	config_opts['root'] = 'rocky+epel-9-x86_64'
//	config_opts['dist'] = 'el9'
//	config_opts['macros']['%rhel'] = '9'
//	config_opts['fakemock_provides'] = ['gcc', 'make']   # the base and EPEL repos
//
// The roots are directories below REPOCTL_TEST_MOCK_DIR. FAKEMOCK_LOG, if
// set, gets one JSON line per call.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type opts struct {
	root, uniqueext, resultdir, macroFile, spec, sources string
	addrepo                                              []string
	buildsrpm, rebuild, chroot, shell, debugConfig       bool
	clean, orphanskill, update, init, version            bool
	scrub                                                string
	timeout                                              int
	defines                                              []string
	rest                                                 []string
}

type config struct {
	path      string
	root      string
	dist      string
	rhel      string
	provides  []string
	autospec  bool
	text      string
	baseFile  string
	bootstrap string
}

func main() { os.Exit(run(os.Args[1:])) }

func die(code int, format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	return code
}

func parse(args []string) (*opts, error) {
	o := &opts{}
	val := func(i *int, name string) (string, error) {
		a := args[*i]
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, nil
		}
		if *i+1 >= len(args) {
			return "", fmt.Errorf("option %s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, _ := strings.Cut(a, "=")
		var err error
		switch name {
		case "-r", "--root":
			o.root, err = val(&i, name)
		case "--uniqueext":
			o.uniqueext, err = val(&i, name)
		case "--resultdir":
			o.resultdir, err = val(&i, name)
		case "--macro-file":
			o.macroFile, err = val(&i, name)
		case "--addrepo":
			var v string
			v, err = val(&i, name)
			o.addrepo = append(o.addrepo, v)
		case "--spec":
			o.spec, err = val(&i, name)
		case "--sources":
			o.sources, err = val(&i, name)
		case "--rpmbuild_timeout":
			var v string
			v, err = val(&i, name)
			o.timeout, _ = strconv.Atoi(v)
		case "--define", "-D":
			var v string
			v, err = val(&i, name)
			o.defines = append(o.defines, v)
		case "--config-opts", "--enable-plugin", "--disable-plugin", "--plugin-option", "--isolation", "--bootstrap-image":
			_, err = val(&i, name)
		case "--scrub":
			o.scrub, err = val(&i, name)
		case "--buildsrpm":
			o.buildsrpm = true
		case "--rebuild":
			o.rebuild = true
		case "--chroot":
			o.chroot = true
		case "--shell":
			o.shell = true
		case "--debug-config":
			o.debugConfig = true
		case "--clean":
			o.clean = true
		case "--orphanskill":
			o.orphanskill = true
		case "--update":
			o.update = true
		case "--init":
			o.init = true
		case "--version":
			o.version = true
		case "-q", "-v", "--quiet", "--verbose", "-n", "-N", "--no-clean", "--no-cleanup-after", "--nocheck", "--cleanup-after":
		case "--":
			o.rest = append(o.rest, args[i+1:]...)
			i = len(args)
		default:
			if strings.HasPrefix(a, "-") {
				return nil, fmt.Errorf("fakemock: unknown option %s", a)
			}
			o.rest = append(o.rest, a)
		}
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

var (
	reInclude  = regexp.MustCompile(`include\(['"]([^'"]+)['"]\)`)
	reRoot     = regexp.MustCompile(`config_opts\['root'\]\s*=\s*['"]([^'"]+)['"]`)
	reDist     = regexp.MustCompile(`config_opts\['dist'\]\s*=\s*['"]([^'"]+)['"]`)
	reRhel     = regexp.MustCompile(`config_opts\['macros'\]\['%rhel'\]\s*=\s*['"]([^'"]+)['"]`)
	reProvides = regexp.MustCompile(`config_opts\['fakemock_provides'\]\s*=\s*\[([^\]]*)\]`)
	reAutospec = regexp.MustCompile(`config_opts\['plugin_conf'\]\['rpmautospec_enable'\]\s*=\s*True`)
	reImage    = regexp.MustCompile(`config_opts\['bootstrap_image'\]\s*=\s*['"]([^'"]+)['"]`)
)

func confDir() string {
	if v := os.Getenv("REPOCTL_TEST_MOCK_CONFIG_DIR"); v != "" {
		return v
	}
	return "/etc/mock"
}

func mockDir() string {
	if v := os.Getenv("REPOCTL_TEST_MOCK_DIR"); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), "fakemock")
}

func loadConfig(name string) (*config, error) {
	c := &config{}
	path := name
	if !strings.HasSuffix(name, ".cfg") {
		path = filepath.Join(confDir(), name+".cfg")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not find required config file: %s", path)
	}
	c.path = path
	text := string(b)
	if m := reAutospec.FindString(text); m != "" {
		c.autospec = true
	}
	base := text
	c.baseFile = path
	if m := reInclude.FindStringSubmatch(text); m != nil {
		bb, err := os.ReadFile(m[1])
		if err != nil {
			return nil, fmt.Errorf("could not find included config file: %s", m[1])
		}
		base = string(bb)
		c.baseFile = m[1]
	}
	c.text = base
	if m := reRoot.FindStringSubmatch(base); m != nil {
		c.root = m[1]
	} else {
		c.root = strings.TrimSuffix(filepath.Base(c.baseFile), ".cfg")
	}
	c.dist, c.rhel = "el9", "9"
	if m := reDist.FindStringSubmatch(base); m != nil {
		c.dist = m[1]
	}
	if m := reRhel.FindStringSubmatch(base); m != nil {
		c.rhel = m[1]
	}
	if m := reProvides.FindStringSubmatch(base); m != nil {
		for _, p := range strings.Split(m[1], ",") {
			if p = strings.Trim(strings.TrimSpace(p), `'"`); p != "" {
				c.provides = append(c.provides, p)
			}
		}
	}
	c.bootstrap = "quay.io/rockylinux/rockylinux:" + c.rhel
	if m := reImage.FindStringSubmatch(base); m != nil {
		c.bootstrap = m[1]
	}
	return c, nil
}

func (c *config) rootDir(o *opts) string {
	n := c.root
	if o.uniqueext != "" {
		n += "-" + o.uniqueext
	}
	return filepath.Join(mockDir(), n)
}

func (c *config) bootstrapDir(o *opts) string {
	n := c.root + "-bootstrap"
	if o.uniqueext != "" {
		n += "-" + o.uniqueext
	}
	return filepath.Join(mockDir(), n)
}

func logCall(args []string) {
	p := os.Getenv("FAKEMOCK_LOG")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(map[string]any{"args": args, "pid": os.Getpid()})
	fmt.Fprintf(f, "%s\n", b)
}

func run(args []string) int {
	logCall(args)
	o, err := parse(args)
	if err != nil {
		return die(5, "%v", err)
	}
	if o.version {
		fmt.Println("6.1")
		return 0
	}
	if o.root == "" {
		o.root = "default"
	}
	c, err := loadConfig(o.root)
	if err != nil {
		return die(5, "%v", err)
	}
	switch {
	case o.debugConfig:
		return debugConfig(c)
	case o.orphanskill:
		return orphanskill(c, o)
	case o.clean || o.scrub != "":
		if o.clean || o.scrub == "all" || o.scrub == "chroot" {
			os.RemoveAll(c.rootDir(o))
		}
		if o.scrub == "bootstrap" || o.scrub == "all" {
			os.RemoveAll(c.bootstrapDir(o))
		}
		return 0
	case o.update || o.init:
		if err := initRoot(c, o); err != nil {
			return die(1, "%v", err)
		}
		return 0
	case o.buildsrpm:
		return withRoot(c, o, buildSRPM)
	case o.rebuild:
		return withRoot(c, o, rebuild)
	case o.chroot || o.shell:
		return chroot(c, o)
	}
	return die(5, "fakemock: no mode given")
}

func debugConfig(c *config) int {
	fmt.Printf("config_opts['root'] = '%s'\n", c.root)
	fmt.Printf("config_opts['dist'] = '%s'\n", c.dist)
	fmt.Printf("config_opts['macros']['%%rhel'] = '%s'\n", c.rhel)
	fmt.Printf("config_opts['mock_run_uuid'] = '%d-%d'\n", time.Now().UnixNano(), os.Getpid())
	fmt.Printf("config_opts['config_paths'] = ['%s']\n", c.path)
	fi, _ := os.Stderr.Stat()
	fmt.Printf("config_opts['print_main_output'] = %v\n", fi != nil && fi.Mode()&os.ModeCharDevice != 0)
	fmt.Printf("config_opts['verbose'] = 1\n")
	sum := sha256.Sum256([]byte(c.text))
	fmt.Printf("config_opts['fakemock_base_sha256'] = '%s'\n", hex.EncodeToString(sum[:]))
	return 0
}

func initRoot(c *config, o *opts) error {
	for _, d := range []string{c.rootDir(o), c.bootstrapDir(o)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func orphanskill(c *config, o *opts) int {
	b, err := os.ReadFile(filepath.Join(c.rootDir(o), "fakemock.pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGUSR1)
	}
	return 0
}

type stateLog struct {
	f     *os.File
	stack []string
}

func (s *stateLog) start(what string) {
	fmt.Fprintf(s.f, "%s - Start: %s\n", time.Now().UTC().Format("2006-01-02 15:04:05,000"), what)
	s.stack = append(s.stack, what)
}

func (s *stateLog) finish() {
	if len(s.stack) == 0 {
		return
	}
	what := s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]
	fmt.Fprintf(s.f, "%s - Finish: %s\n", time.Now().UTC().Format("2006-01-02 15:04:05,000"), what)
}

func (s *stateLog) finishAll() {
	for len(s.stack) > 0 {
		s.finish()
	}
}

type env struct {
	c       *config
	o       *opts
	root    string
	result  string
	state   *stateLog
	rootLog io.Writer
	ctx     context.Context
	killed  *bool
}

// withRoot makes the root, holds the pid file, opens the logs and runs fn.
// SIGUSR1 (from --orphanskill) stops the build: mock then ends with exit 1.
func withRoot(c *config, o *opts, fn func(e *env) int) int {
	if o.resultdir == "" {
		return die(5, "fakemock: --resultdir is required")
	}
	if err := initRoot(c, o); err != nil {
		return die(1, "%v", err)
	}
	if err := os.MkdirAll(o.resultdir, 0o755); err != nil {
		return die(1, "%v", err)
	}
	root := c.rootDir(o)
	pidFile := filepath.Join(root, "fakemock.pid")
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644)
	defer os.Remove(pidFile)
	sf, err := os.OpenFile(filepath.Join(o.resultdir, "state.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return die(1, "%v", err)
	}
	defer sf.Close()
	rl, err := os.OpenFile(filepath.Join(o.resultdir, "root.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return die(1, "%v", err)
	}
	defer rl.Close()
	_ = os.WriteFile(filepath.Join(o.resultdir, "hw_info.log"), []byte("CPU info:\nfakemock\n"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	killed := false
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGUSR1, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		killed = true
		cancel()
	}()
	e := &env{c: c, o: o, root: root, result: o.resultdir, state: &stateLog{f: sf}, rootLog: rl, ctx: ctx, killed: &killed}
	fmt.Printf("INFO: mock.py version 6.1 starting (fakemock)...\n")
	e.state.start("chroot init")
	fmt.Fprintf(rl, "DEBUG util.py: ensuring that dir exists: %s\n", root)
	e.state.finish()
	code := fn(e)
	e.state.finishAll()
	if killed && code == 0 {
		code = 1
	}
	return code
}

func (e *env) rpmbuild(logName string, args ...string) (string, int) {
	ctx := e.ctx
	var cancel context.CancelFunc
	if e.o.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(e.o.timeout)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "rpmbuild", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	lf, err := os.OpenFile(filepath.Join(e.result, logName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err.Error(), 1
	}
	defer lf.Close()
	var out strings.Builder
	w := io.MultiWriter(lf, &out)
	cmd.Stdout, cmd.Stderr = w, w
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	err = cmd.Run()
	if err == nil {
		return out.String(), 0
	}
	if e.o.timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) && e.ctx.Err() == nil {
		fmt.Fprintf(lf, "ERROR: Command timed out after %d seconds\n", e.o.timeout)
		return out.String(), 11
	}
	return out.String(), 1
}

func (e *env) macroArgs(top string) []string {
	args := []string{
		"--define", "_topdir " + top,
		"--define", "dist ." + e.c.dist,
		"--define", "rhel " + e.c.rhel,
		"--define", e.c.dist + " 1",
		"--define", "_build_name_fmt %%{NAME}-%%{VERSION}-%%{RELEASE}.%%{ARCH}.rpm",
		"--define", "debug_package %{nil}",
		"--define", "_binary_payload w2.xzdio",
		"--define", "_source_payload w2.xzdio",
	}
	if e.o.macroFile != "" {
		args = append(args, "--load", e.o.macroFile)
	}
	for _, d := range e.o.defines {
		args = append(args, "--define", d)
	}
	return args
}

// buildSRPM: mock --buildsrpm --spec SPEC --sources DIR.
func buildSRPM(e *env) int {
	if e.o.spec == "" || e.o.sources == "" {
		return die(5, "fakemock: --buildsrpm needs --spec and --sources")
	}
	spec, err := os.ReadFile(e.o.spec)
	if err != nil {
		return die(1, "%v", err)
	}
	text := string(spec)
	e.state.start("rpmautospec")
	if e.c.autospec {
		if _, err := os.Stat(filepath.Join(e.o.sources, ".git")); err == nil {
			text, err = autospec(e.o.sources, filepath.Base(e.o.spec), text)
			if err != nil {
				fmt.Fprintf(e.rootLog, "ERROR: rpmautospec: %v\n", err)
				return die(1, "rpmautospec: %v", err)
			}
			fmt.Fprintf(e.rootLog, "INFO: rpmautospec: processed %s\n", filepath.Base(e.o.spec))
		}
	}
	// The fallback macros of rpmautospec without a git history.
	text = strings.ReplaceAll(text, "%autorelease", "1%{?dist}")
	text = strings.ReplaceAll(text, "%{autorelease}", "1%{?dist}")
	text = strings.ReplaceAll(text, "%autochangelog", "* Thu Jan 01 1970 fakemock <fakemock@example.org> - 0-1\n- fallback changelog")
	e.state.finish()
	top, err := os.MkdirTemp(e.root, "srpm-")
	if err != nil {
		return die(1, "%v", err)
	}
	defer os.RemoveAll(top)
	specOut := filepath.Join(top, filepath.Base(e.o.spec))
	if err := os.WriteFile(specOut, []byte(text), 0o644); err != nil {
		return die(1, "%v", err)
	}
	e.state.start("buildsrpm")
	args := append(e.macroArgs(top), "--define", "_sourcedir "+e.o.sources, "--define", "_srcrpmdir "+e.result, "-bs", specOut)
	out, code := e.rpmbuild("build.log", args...)
	if code != 0 {
		fmt.Print(out)
		fmt.Fprintf(os.Stderr, "ERROR: Exception(%s) Config(%s) Command failed\n", e.o.spec, e.c.root)
		return code
	}
	e.state.finish()
	return 0
}

// autospec emulates the rpmautospec plugin: %autorelease is the number of
// commits since the last change of Version, %autochangelog the subjects.
func autospec(dir, specName, text string) (string, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.Output()
		return string(out), err
	}
	log, err := git("log", "--format=%H%x09%an%x09%ae%x09%ad%x09%s", "--date=format:%a %b %d %Y", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git log: %w", err)
	}
	version := func(rev string) string {
		out, err := git("show", rev+":"+specName)
		if err != nil {
			return ""
		}
		for _, l := range strings.Split(out, "\n") {
			if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Version") {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	cur := version("HEAD")
	n := 0
	var cl strings.Builder
	for _, l := range lines {
		f := strings.SplitN(l, "\t", 5)
		if len(f) < 5 {
			continue
		}
		v := version(f[0])
		if v != cur {
			break
		}
		n++
	}
	rel := n
	for i, l := range lines {
		f := strings.SplitN(l, "\t", 5)
		if len(f) < 5 {
			continue
		}
		v := version(f[0])
		if i > 0 {
			cl.WriteString("\n")
		}
		fmt.Fprintf(&cl, "* %s %s <%s> - %s-%d\n- %s\n", f[3], f[1], f[2], v, rel, strings.ReplaceAll(f[4], "%", "%%"))
		if v != cur {
			break
		}
		if i >= 20 {
			break
		}
	}
	text = strings.ReplaceAll(text, "%autorelease", strconv.Itoa(rel)+"%{?dist}")
	text = strings.ReplaceAll(text, "%{autorelease}", strconv.Itoa(rel)+"%{?dist}")
	text = strings.ReplaceAll(text, "%autochangelog", strings.TrimRight(cl.String(), "\n"))
	return text, nil
}

type rpmEntry struct {
	Name      string  `json:"name"`
	Epoch     *string `json:"epoch"`
	Version   string  `json:"version"`
	Release   string  `json:"release"`
	Arch      string  `json:"arch"`
	License   string  `json:"license"`
	Sigmd5    string  `json:"sigmd5"`
	Signature *string `json:"signature"`
	URL       string  `json:"url"`
	// Not in buildroot_lock.json: for --chroot rpm -qa.
	header string
	keyID  string
}

// baseRPMs are the RPMs of the fake build root, signed by the distribution.
func baseRPMs(c *config) []rpmEntry {
	var out []rpmEntry
	names := []string{"bash", "coreutils", "gcc", "glibc", "make", "redhat-rpm-config", "rpm-build"}
	for _, n := range names {
		sum := sha256.Sum256([]byte(n + c.dist))
		md5 := hex.EncodeToString(sum[:16])
		out = append(out, rpmEntry{
			Name: n, Version: "1.0", Release: "1." + c.dist, Arch: "x86_64", License: "GPL-2.0-or-later",
			Sigmd5: md5, URL: "https://dl.rockylinux.org/" + n, header: hex.EncodeToString(sum[:]), keyID: "702d426d350d275d",
		})
	}
	return out
}

func repoDirs(addrepo []string) []string {
	var out []string
	for _, r := range addrepo {
		r = strings.TrimPrefix(r, "file://")
		if filepath.IsAbs(r) {
			out = append(out, r)
		}
	}
	return out
}

type repoRPM struct {
	path     string
	provides []string
	nevra    [5]string
	license  string
}

func scanRepos(dirs []string) []repoRPM {
	var out []repoRPM
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(p string, de os.DirEntry, err error) error {
			if err != nil || de.IsDir() || !strings.HasSuffix(p, ".rpm") || strings.HasSuffix(p, ".src.rpm") {
				return nil
			}
			q, err := exec.Command("rpm", "-qp", "--nosignature", "--qf", "%{NAME}\t%{EPOCHNUM}\t%{VERSION}\t%{RELEASE}\t%{ARCH}\t%{LICENSE}\n", p).Output()
			if err != nil {
				return nil
			}
			f := strings.Split(strings.TrimSpace(string(q)), "\t")
			if len(f) < 6 {
				return nil
			}
			pr, _ := exec.Command("rpm", "-qp", "--nosignature", "--provides", p).Output()
			var provs []string
			for _, l := range strings.Split(string(pr), "\n") {
				if l = strings.TrimSpace(l); l != "" {
					name, _, _ := strings.Cut(l, " ")
					provs = append(provs, name)
				}
			}
			out = append(out, repoRPM{path: p, provides: provs, nevra: [5]string{f[0], f[1], f[2], f[3], f[4]}, license: f[5]})
			return nil
		})
	}
	return out
}

// rebuild: mock --rebuild SRPM.
func rebuild(e *env) int {
	if len(e.o.rest) != 1 {
		return die(5, "fakemock: --rebuild needs one SRPM")
	}
	srpm := e.o.rest[0]
	e.state.start("build phase for " + filepath.Base(srpm))
	e.state.start("dnf builddep")
	reqs, err := exec.Command("rpm", "-qp", "--nosignature", "--requires", srpm).Output()
	if err != nil {
		fmt.Fprintf(e.rootLog, "ERROR: cannot read %s\n", srpm)
		return die(1, "cannot read %s", srpm)
	}
	base := map[string]bool{}
	for _, p := range e.c.provides {
		base[p] = true
	}
	for _, r := range baseRPMs(e.c) {
		base[r.Name] = true
	}
	repos := scanRepos(repoDirs(e.o.addrepo))
	installed := map[string]repoRPM{}
	var missing []string
	for _, l := range strings.Split(string(reqs), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "rpmlib(") {
			continue
		}
		name, _, _ := strings.Cut(l, " ")
		if base[name] {
			continue
		}
		found := false
		for _, r := range repos {
			for _, p := range r.provides {
				if p == name {
					installed[r.path] = r
					found = true
				}
			}
		}
		if !found {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		for _, m := range missing {
			fmt.Fprintf(e.rootLog, "DEBUG util.py: No matching package to install: '%s'\n", m)
		}
		fmt.Fprintf(e.rootLog, "DEBUG util.py: Error: Some packages could not be found.\n")
		fmt.Fprintf(os.Stderr, "ERROR: Command failed: dnf builddep\nNo matching package to install: '%s'\n", missing[0])
		return 30
	}
	e.state.finish()
	if *e.killed {
		return 1
	}
	top, err := os.MkdirTemp(e.root, "rpm-")
	if err != nil {
		return die(1, "%v", err)
	}
	defer os.RemoveAll(top)
	e.state.start("rpmbuild " + filepath.Base(srpm))
	args := append(e.macroArgs(top), "--define", "_rpmdir "+e.result, "--define", "_srcrpmdir "+e.result, "--nodeps", "--rebuild", srpm)
	_, code := e.rpmbuild("build.log", args...)
	if code != 0 {
		if *e.killed {
			fmt.Fprintf(os.Stderr, "ERROR: Exception: build stopped (orphanskill)\n")
			return 1
		}
		fmt.Fprintf(os.Stderr, "ERROR: Exception(%s) Config(%s) Command failed: rpmbuild\n", srpm, e.c.root)
		return code
	}
	e.state.finish()
	// rpmbuild writes into RESULT/ARCH/; mock writes flat.
	_ = filepath.WalkDir(e.result, func(p string, de os.DirEntry, err error) error {
		if err == nil && !de.IsDir() && strings.HasSuffix(p, ".rpm") && filepath.Dir(p) != e.result {
			_ = os.Rename(p, filepath.Join(e.result, filepath.Base(p)))
		}
		return nil
	})
	for _, a := range []string{"x86_64", "noarch", "i686", "aarch64"} {
		_ = os.Remove(filepath.Join(e.result, a))
	}
	if _, err := os.Stat(filepath.Join(e.result, filepath.Base(srpm))); err != nil {
		if b, err := os.ReadFile(srpm); err == nil {
			_ = os.WriteFile(filepath.Join(e.result, filepath.Base(srpm)), b, 0o644)
		}
	}
	// buildroot_lock plugin.
	rpms := baseRPMs(e.c)
	var paths []string
	for p := range installed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		r := installed[p]
		b, _ := os.ReadFile(p)
		sum := sha256.Sum256(b)
		var epoch *string
		if r.nevra[1] != "0" {
			v := r.nevra[1]
			epoch = &v
		}
		rpms = append(rpms, rpmEntry{Name: r.nevra[0], Epoch: epoch, Version: r.nevra[2], Release: r.nevra[3], Arch: r.nevra[4],
			License: r.license, Sigmd5: hex.EncodeToString(sum[:16]), URL: "file://" + p, header: hex.EncodeToString(sum[:])})
	}
	sort.Slice(rpms, func(i, j int) bool { return rpms[i].Name < rpms[j].Name })
	lock := map[string]any{
		"version":   "1.1.0",
		"buildroot": map[string]any{"rpms": rpms},
		"bootstrap": map[string]any{"image_digest": "sha256:" + strings.Repeat("ab", 32), "architecture": "amd64"},
		"config":    map[string]any{"bootstrap_image": e.c.bootstrap, "target_arch": "x86_64", "dist": e.c.dist},
	}
	b, _ := json.MarshalIndent(lock, "", "  ")
	_ = os.WriteFile(filepath.Join(e.result, "buildroot_lock.json"), append(b, '\n'), 0o644)
	// The root keeps the list for --chroot rpm -qa.
	var qa strings.Builder
	for _, r := range rpms {
		ep := "0"
		if r.Epoch != nil {
			ep = *r.Epoch
		}
		sig := "(none)"
		if r.keyID != "" {
			sig = "RSA/SHA256, Mon 05 Jan 2026 10:00:00 AM UTC, Key ID " + r.keyID
		}
		fmt.Fprintf(&qa, "PKG %s-%s:%s-%s.%s\t%s\t%s\n", r.Name, ep, r.Version, r.Release, r.Arch, r.header, sig)
	}
	qa.WriteString("PKG gpg-pubkey-0:702d426d-5f1b8a6c.(none)\t" + strings.Repeat("0", 64) + "\t(none)\n")
	_ = os.WriteFile(filepath.Join(e.root, "fakemock-rpmqa"), []byte(qa.String()), 0o644)
	e.state.finish()
	return 0
}

// chroot runs a command "in the root": rpm -qa with --qf gives the build
// root; rpm -q --whatprovides X searches the base and the repos.
func chroot(c *config, o *opts) int {
	cmd := o.rest
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	if len(cmd) == 1 && strings.Contains(cmd[0], " ") {
		cmd = strings.Fields(cmd[0])
	}
	if len(cmd) == 0 {
		return 0
	}
	if cmd[0] == "rpm" && len(cmd) >= 2 && cmd[1] == "-qa" {
		b, err := os.ReadFile(filepath.Join(c.rootDir(o), "fakemock-rpmqa"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR: the root is not initialized")
			return 1
		}
		os.Stdout.Write(b)
		return 0
	}
	if cmd[0] == "rpm" && len(cmd) >= 4 && cmd[1] == "-q" && cmd[2] == "--whatprovides" {
		want := cmd[3]
		for _, p := range c.provides {
			if p == want {
				fmt.Printf("%s-1.0-1.%s.x86_64\n", want, c.dist)
				return 0
			}
		}
		for _, r := range scanRepos(repoDirs(o.addrepo)) {
			for _, p := range r.provides {
				if p == want {
					fmt.Printf("%s-%s-%s.%s\n", r.nevra[0], r.nevra[2], r.nevra[3], r.nevra[4])
					return 0
				}
			}
		}
		fmt.Printf("no package provides %s\n", want)
		return 1
	}
	if cmd[0] == "rpmspec" || cmd[0] == "rpm" || cmd[0] == "sh" || cmd[0] == "bash" || cmd[0] == "true" || cmd[0] == "rpmkeys" {
		// Run on the host with the macros of the target.
		x := exec.Command(cmd[0], cmd[1:]...)
		if cmd[0] == "rpmspec" || cmd[0] == "rpm" {
			x.Args = append([]string{cmd[0], "--define", "dist ." + c.dist, "--define", "rhel " + c.rhel, "--define", c.dist + " 1"}, cmd[1:]...)
		}
		x.Stdin, x.Stdout, x.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := x.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode()
			}
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "fakemock: command not emulated: %s\n", strings.Join(cmd, " "))
	return 0
}
