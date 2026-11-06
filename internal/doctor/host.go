package doctor

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// GiB is 1024³ bytes.
const GiB = int64(1) << 30

// The free disk of the preflight of build (build.html#run, requirements).
var (
	MinFreeData  = 10 * GiB
	MinFreeCache = 10 * GiB
	MinFreeState = 1 * GiB
	MinFreeMock  = 20 * GiB
)

// The checks of this host, in the order of repoctl doctor. The set checks
// go between tools and disk (set.go).
func init() {
	Register(Spec{Group: GroupHost, Run: checkRepoctl})
	Register(Spec{Group: GroupHost, Needs: []string{NeedBuild, NeedCheck}, Run: checkGit})
	Register(Spec{Group: GroupHost, Needs: []string{NeedBuild, NeedMock}, Run: checkMock})
	Register(Spec{Group: GroupHost, Needs: []string{NeedBuild, NeedMock}, Run: checkMockConfigs})
	Register(Spec{Group: GroupHost, Needs: []string{NeedBuild, NeedPublish}, Run: checkTools})
	registerSet()
	Register(Spec{Group: GroupHost, Needs: []string{NeedBuild, NeedPublish}, Run: checkDisk})
	Register(Spec{Group: GroupHost, Needs: []string{NeedBuild, NeedPublish, NeedCheck}, Run: checkXDG})
	Register(Spec{Group: GroupHost, Run: checkLinger})
	Register(Spec{Group: GroupHost, Run: checkSharedState})
	registerSetLate()
	// The sign group belongs to another part of repoctl. Until it registers
	// its checks, one line says that they did not run.
	Register(Spec{Group: GroupSign, Placeholder: true, Run: func(context.Context, *Env) []Check {
		return []Check{Ok("sign", "sign: not checked by this release (signing.html#doctor)")}
	}})
	Register(Spec{Group: GroupRegistry, Run: checkRegistryAccess})
	Register(Spec{Group: GroupRegistry, Run: checkServedStale})
	Register(Spec{Group: GroupCheck, Run: checkCheck})
}

// version runs PROG ARGS and gives the first line of its output.
func version(ctx context.Context, prog string, args ...string) (string, bool) {
	if _, ok := run.LookPath(prog); !ok {
		return "", false
	}
	out, err := (&run.Cmd{Name: prog, Args: args}).Output(ctx)
	if err != nil {
		return "", true
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line, true
}

// lastField gives the version word of a --version line: the first field
// that starts with a digit.
func lastField(line string) string {
	for _, f := range strings.Fields(line) {
		if f != "" && f[0] >= '0' && f[0] <= '9' {
			return strings.TrimSuffix(f, ",")
		}
	}
	return line
}

func osName() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	defer f.Close()
	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			vals[k] = strings.Trim(v, `"`)
		}
	}
	if vals["NAME"] != "" && vals["VERSION_ID"] != "" {
		return strings.TrimSuffix(vals["NAME"], " Linux") + " " + vals["VERSION_ID"]
	}
	if vals["PRETTY_NAME"] != "" {
		return vals["PRETTY_NAME"]
	}
	return runtime.GOOS
}

func checkRepoctl(_ context.Context, env *Env) []Check {
	v := "repoctl " + env.Version
	if env.Commit != "" {
		v += " (" + env.Commit + ")"
	}
	return []Check{Ok("repoctl", v+", "+runtime.Version()+", "+osName())}
}

func checkGit(ctx context.Context, _ *Env) []Check {
	line, ok := version(ctx, "git", "--version")
	if !ok {
		c := Fail("git", "git is not installed", "install git. run: sudo dnf install git-core")
		return []Check{c}
	}
	return []Check{Ok("git", "git "+lastField(line))}
}

// rpmautospecPlugin reports the mock plugin rpmautospec of this host.
func rpmautospecPlugin() bool {
	for _, pat := range []string{
		"/usr/lib/python3*/site-packages/mockbuild/plugins/rpmautospec.py",
		"/usr/lib64/python3*/site-packages/mockbuild/plugins/rpmautospec.py",
	} {
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			return true
		}
	}
	return false
}

func inGroup(name string) (bool, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return false, err
	}
	u, err := user.Current()
	if err != nil {
		return false, err
	}
	ids, err := u.GroupIds()
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if id == g.Gid {
			return true, nil
		}
	}
	return false, nil
}

func checkMock(ctx context.Context, env *Env) []Check {
	m := env.mock()
	line, ok := version(ctx, m.Program, "--version")
	if !ok {
		c := Fail("mock", "mock is not installed", "install mock. run: sudo dnf install mock")
		c.Code = api.PMockUnavailable
		return []Check{c}
	}
	parts := []string{"mock " + lastField(line)}
	var problems, fixes []string
	test := env.Paths.TestHost
	who := env.User
	if who == "" {
		who = "this user"
	}
	switch in, err := inGroup("mock"); {
	case test:
		parts = append(parts, "group mock not checked (test host)")
	case err != nil || !in:
		problems = append(problems, who+" is not in group mock")
		fixes = append(fixes, "sudo usermod -a -G mock "+who)
	default:
		parts = append(parts, who+" is in group mock")
	}
	switch {
	case rpmautospecPlugin():
		parts = append(parts, "rpmautospec plugin")
	case test:
		parts = append(parts, "rpmautospec plugin not checked (test host)")
	default:
		problems = append(problems, "the mock plugin rpmautospec is missing")
		fixes = append(fixes, "sudo dnf install mock-rpmautospec")
	}
	if pl, ok := version(ctx, "podman", "--version"); ok {
		parts = append(parts, "podman "+lastField(pl))
	} else if !test {
		problems = append(problems, "podman is not installed")
		fixes = append(fixes, "sudo dnf install podman")
	}
	if len(problems) > 0 {
		c := Fail("mock", strings.Join(append(parts[:1:1], problems...), ", "), "make mock ready. run: "+strings.Join(fixes, " && "))
		c.Code = api.PMockUnavailable
		return []Check{c}
	}
	return []Check{Ok("mock", strings.Join(parts, ", "))}
}

func checkMockConfigs(_ context.Context, env *Env) []Check {
	if env.Set == nil {
		return nil
	}
	var have, missing []string
	for _, id := range env.TargetIDs() {
		t, ok := env.Set.Target(id)
		if !ok {
			continue
		}
		if _, err := os.Stat(env.Paths.MockConfig(t.Mock)); err != nil {
			missing = append(missing, id+" "+t.Mock)
			continue
		}
		have = append(have, id+" "+t.Mock)
	}
	if len(missing) > 0 {
		c := Fail("mock.configs", "missing on this host: "+strings.Join(missing, ", "),
			"install the mock config, or correct targets[].mock. run: sudo dnf install mock-core-configs")
		c.Code = api.PMockConfigMissing
		return []Check{c}
	}
	return []Check{Ok("mock.configs", "mock configs: "+strings.Join(have, ", "))}
}

func checkTools(ctx context.Context, env *Env) []Check {
	type tool struct {
		name, prog, pkg string
		args            []string
		needs           []string
	}
	tools := []tool{
		{"rpmspec", "rpmspec", "rpm-build", []string{"--version"}, []string{NeedBuild}},
		{"rpmsign", "rpmsign", "rpm-sign", []string{"--version"}, []string{NeedPublish}},
		{"createrepo_c", "createrepo_c", "createrepo_c", []string{"--version"}, []string{NeedBuild, NeedPublish}},
		{"gpg", "gpg", "gnupg2", []string{"--version"}, []string{NeedPublish}},
	}
	var have, missing, pkgs []string
	for _, t := range tools {
		if env.Need != "" && !contains(t.needs, env.Need) {
			continue
		}
		line, ok := version(ctx, t.prog, t.args...)
		if !ok {
			missing = append(missing, t.name)
			pkgs = append(pkgs, t.pkg)
			continue
		}
		have = append(have, t.name+" "+lastField(line))
	}
	if len(missing) > 0 {
		return []Check{Fail("tools", "missing: "+strings.Join(missing, ", "), "install them. run: sudo dnf install "+strings.Join(pkgs, " "))}
	}
	return []Check{Ok("tools", "tools: "+strings.Join(have, ", "))}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// free gives the free bytes of the file system of the nearest existing
// directory of p, and that directory.
func free(p string) (int64, string, error) {
	dir := p
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, dir, err
	}
	return int64(st.Bavail) * int64(st.Bsize), dir, nil //nolint:unconvert // the types differ per platform
}

func humanGiB(n int64) string {
	if n >= 10*GiB {
		return fmt.Sprintf("%d GiB", n/GiB)
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/float64(GiB))
}

func checkDisk(_ context.Context, env *Env) []Check {
	type need struct {
		path string
		min  int64
	}
	var needs []need
	switch env.Need {
	case NeedPublish:
		needs = []need{{env.Paths.DataHome, env.PublishBytes + GiB}}
	default:
		needs = []need{{env.Paths.DataHome, MinFreeData}, {env.Paths.CacheHome, MinFreeCache}, {env.Paths.StateHome, MinFreeState}, {env.Paths.MockDir, MinFreeMock}}
	}
	minFree, minPath := int64(-1), ""
	var short []string
	for _, n := range needs {
		f, _, err := free(n.path)
		if err != nil {
			continue
		}
		if minFree < 0 || f < minFree {
			minFree, minPath = f, n.path
		}
		if f < n.min {
			short = append(short, fmt.Sprintf("%s free in %s, needs %s", humanGiB(f), env.tilde(n.path), humanGiB(n.min)))
		}
	}
	if minFree < 0 {
		return []Check{Warn("disk", "cannot read the free space", "")}
	}
	text := "disk: " + humanGiB(minFree) + " free in " + env.tilde(minPath)
	if len(short) > 0 {
		if env.Paths.TestHost {
			return []Check{Ok("disk", text+" (test host: "+strings.Join(short, "; ")+")")}
		}
		return []Check{Fail("disk", strings.Join(short, "; "), "free space. run: repoctl clean --all --dry-run")}
	}
	return []Check{Ok("disk", text)}
}

func checkXDG(_ context.Context, env *Env) []Check {
	dirs := []string{env.Paths.ConfigHome, env.Paths.CacheHome, env.Paths.DataHome, env.Paths.StateHome}
	if env.Paths.RuntimeDir != "" {
		dirs = append(dirs, env.Paths.RuntimeDir)
	}
	me := os.Getuid()
	private := true
	var bad []string
	for _, d := range dirs {
		fi, err := os.Stat(d)
		if err != nil {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if ok && int(st.Uid) != me {
			bad = append(bad, env.tilde(d)+" is owned by another user")
			continue
		}
		if fi.Mode().Perm()&0o022 != 0 {
			bad = append(bad, fmt.Sprintf("%s has mode %04o", env.tilde(d), fi.Mode().Perm()))
			continue
		}
		if fi.Mode().Perm() != 0o700 {
			private = false
		}
	}
	who := env.User
	if who == "" {
		who = fmt.Sprint(me)
	}
	if len(bad) > 0 {
		return []Check{Fail("xdg", strings.Join(bad, "; "), "make them private to "+who+". run: chmod go-w "+env.tilde(env.Paths.StateHome))}
	}
	mode := "mode 0700"
	if !private {
		mode = "not writable by others"
	}
	return []Check{Ok("xdg", "XDG directories: owned by "+who+", "+mode)}
}

func checkLinger(_ context.Context, env *Env) []Check {
	if env.User == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join("/var/lib/systemd/linger", env.User)); err == nil {
		return []Check{Ok("build.linger", "build.linger: lingering is on")}
	}
	return []Check{Warn("build.linger", "lingering is off: a logout removes the set lock of a run",
		"keep the runs of this user after a logout. run: loginctl enable-linger "+env.User)}
}

// Magic numbers of network file systems (statfs f_type).
var networkFS = map[int64]string{
	0x6969:     "NFS",
	0xFF534D42: "CIFS",
	0xFE534D42: "SMB2",
	0x517B:     "SMB",
	0x00C36400: "Ceph",
	0x0BD00BD0: "Lustre",
	0x47504653: "GPFS",
	0x5346414F: "AFS",
	0x65735546: "FUSE",
}

func checkSharedState(_ context.Context, env *Env) []Check {
	var shared []string
	for _, d := range []string{env.Paths.StateHome, env.Paths.DataHome} {
		_, dir, err := free(d)
		if err != nil {
			continue
		}
		var st syscall.Statfs_t
		if syscall.Statfs(dir, &st) != nil {
			continue
		}
		if name, ok := networkFS[int64(st.Type)]; ok { //nolint:unconvert // the types differ per platform
			shared = append(shared, env.tilde(d)+" is on "+name)
		}
	}
	if len(shared) > 0 {
		return []Check{Warn("host.shared-state", strings.Join(shared, "; ")+": two hosts can write the same state",
			"put XDG_STATE_HOME and XDG_DATA_HOME on a local disk of this host")}
	}
	return []Check{Ok("host.shared-state", "host.shared-state: state and data on a local file system")}
}
