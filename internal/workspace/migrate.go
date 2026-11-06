package workspace

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// The files of a set branch of the bash era (cli.html#migration).
const (
	BashLock      = "packages.lock"
	BashCommon    = "scripts/common.sh"
	BashMacros    = "scripts/macros"
	BashBuild     = "scripts/build"
	BashInfo      = ".claude/skills/package-info/scripts/get-package-info"
	BashInfoSkill = ".claude/skills/package-info/SKILL.md"
	// MacrosFile is the macro file that migrate writes.
	MacrosFile = "rpm/macros"
)

// The warning codes of migrate. They are findings, not failures.
const (
	PMigrateNotes = api.PMigrateNotes
	PMigrateOrder = api.PMigrateOrder
	PMigrateKey   = api.PMigrateKey
)

// MigrateOptions are the flags of migrate.
type MigrateOptions struct {
	Dir    string
	Name   string
	DryRun bool
	Force  bool
}

// MigrateReport is the result of migrate.
type MigrateReport struct {
	Set   *config.Set
	Files []api.File
	// Texts are the bytes of each file, by path.
	Texts map[string][]byte
	// Summary is the line of each file.
	Summary  map[string]string
	Warnings []api.Problem
	// Pinned is the number of lines of packages.lock.
	Pinned int
}

// bashLine is one line of packages.lock: dir, branch, commit.
type bashLine struct{ dir, branch, commit string }

// Migrate converts packages.lock, scripts/common.sh, scripts/macros, the
// build lines of scripts/build and the tables REPO_MAP and DEFAULT_FILTERS
// of the package-info skill to repoctl.yaml, repoctl.lock and rpm/macros
// (cli.html#cmd-migrate). It deletes no old file.
func Migrate(ctx context.Context, o MigrateOptions) (*MigrateReport, error) {
	if err := CheckWorkTree(ctx, o.Dir); err != nil {
		return nil, err
	}
	read := func(rel string) []byte {
		b, _ := os.ReadFile(filepath.Join(o.Dir, filepath.FromSlash(rel)))
		return b
	}
	lockData, err := os.ReadFile(filepath.Join(o.Dir, BashLock))
	if err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "no %s in %s: this is not a set branch of the bash scripts", BashLock, o.Dir).
			WithPath(filepath.Join(o.Dir, BashLock)).WithHint("start a new set instead", "repoctl init")
	}
	if !o.Force && !o.DryRun {
		for _, f := range []string{config.SetFile, lock.FileName, MacrosFile} {
			if _, err := os.Stat(filepath.Join(o.Dir, filepath.FromSlash(f))); err == nil {
				return nil, api.Errorf(api.PConfigInvalid, "%s exists in %s", f, o.Dir).WithPath(f).
					WithHint("write over it", "repoctl migrate --force")
			}
		}
	}
	lines := parseBashLock(lockData)
	vars := shellVars(read(BashCommon))
	order, commented := buildLines(read(BashBuild))
	repoMap, filters, noted := skillTables(read(BashInfo))
	for _, p := range frozenNotes(read(BashInfoSkill)) {
		noted[p] = true
	}
	rep := &MigrateReport{Texts: map[string][]byte{}, Summary: map[string]string{}, Pinned: len(lines)}

	name := o.Name
	if name == "" {
		name = DefaultName(ctx, o.Dir)
	}
	repoName := firstNonEmpty(vars["reponame"], vars["repobasename"], DefaultRepo(name))
	target := migrateTarget(vars)
	macros := read(BashMacros)
	if macros != nil {
		target.Macros = []string{MacrosFile}
	}
	d := Draft{Name: name, Repo: repoName, Targets: []config.Target{target}}

	// The packages: the hand order of scripts/build, then the other lines
	// of packages.lock. A build line without a lock line and with a
	// directory in the set tree is in-tree.
	byDir := map[string]bashLine{}
	for _, l := range lines {
		byDir[l.dir] = l
	}
	var ids []string
	seen := map[string]bool{}
	for _, id := range append(append([]string{}, order...), commented...) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, l := range lines {
		if !seen[l.dir] {
			seen[l.dir] = true
			ids = append(ids, l.dir)
		}
	}
	fedora := fedoraRemotes(ctx, o.Dir)
	lf := lock.Empty()
	for _, id := range ids {
		pk := config.Package{ID: id}
		if l, ok := byDir[id]; ok {
			pk.Branch = l.branch
			lf.Set(id, lock.Entry{Branch: l.branch, Commit: l.commit})
		} else if tracked(ctx, o.Dir, "packages/"+id) {
			pk.Path = "packages/" + id
			if id == repoName+"-release" {
				pk.Role = "release"
			}
		} else {
			w := api.Warning(api.PLockMissingPackage, "%s: scripts/build builds it, but packages.lock has no line and the set tree has no packages/%s; it is left out", id, id)
			w.Package = id
			rep.Warnings = append(rep.Warnings, w)
			continue
		}
		u := &config.Upstream{GitHub: repoMap[id], Tags: filters[id], Fedora: fedora[id]}
		if *u != (config.Upstream{}) {
			pk.Upstream = u
		}
		d.Packages = append(d.Packages, pk)
	}
	// The key of the release package.
	if rp := releasePackage(d.Packages); rp != nil {
		key, keyfile, warn := releaseKey(ctx, o.Dir, rp.Path)
		if warn != nil {
			rep.Warnings = append(rep.Warnings, *warn)
		}
		if key != nil {
			d.Keys = []config.Key{*key}
			d.ReleaseKeyfile = keyfile
		}
	}
	data := d.Render()
	set, err := config.ParseSet(data, filepath.Join(o.Dir, config.SetFile))
	if err != nil {
		return nil, err
	}
	rep.Set = set
	if len(noted) > 0 {
		var list []string
		for _, id := range ids {
			if noted[id] {
				list = append(list, id)
			}
		}
		if len(list) > 0 {
			w := api.Warning(PMigrateNotes, "upstream notes are not migrated: %s", strings.Join(list, ", "))
			w.Hint = api.HintText("add notes: to these entries by hand", "$EDITOR repoctl.yaml")
			rep.Warnings = append(rep.Warnings, w)
		}
	}
	rep.Warnings = append(rep.Warnings, orderFindings(ctx, o.Dir, order, byDir, set, macros)...)

	keys := ""
	if len(d.Keys) > 0 {
		keys = fmt.Sprintf(", 1 key (%s)", d.Keys[0].ID)
	}
	rep.Summary[config.SetFile] = fmt.Sprintf("set %s, repo %s, %d target (%s), %d packages%s", name, repoName, len(d.Targets), target.ID, len(d.Packages), keys)
	rep.Summary[lock.FileName] = fmt.Sprintf("%d pinned packages from %s", len(lines), BashLock)
	rep.Texts[config.SetFile] = data
	lockText := lf.Format(packageOrder(set))
	rep.Texts[lock.FileName] = lockText
	if macros != nil {
		rep.Texts[MacrosFile] = macros
		rep.Summary[MacrosFile] = "from " + BashMacros
	}
	for _, rel := range []string{config.SetFile, lock.FileName, MacrosFile} {
		text, ok := rep.Texts[rel]
		if !ok {
			continue
		}
		kind := map[string]string{config.SetFile: "config", lock.FileName: "lock", MacrosFile: "macros"}[rel]
		if o.DryRun {
			action := "would-create"
			if _, err := os.Stat(filepath.Join(o.Dir, filepath.FromSlash(rel))); err == nil {
				action = "would-update"
			}
			rep.Files = append(rep.Files, api.File{Path: rel, Action: action, Kind: kind, Bytes: len(text)})
			continue
		}
		var f api.File
		var err error
		if rel == lock.FileName {
			f, err = writeLock(ctx, o.Dir, lf, packageOrder(set))
		} else {
			f, err = writeFile(o.Dir, rel, text, kind)
		}
		if err != nil {
			return nil, err
		}
		rep.Files = append(rep.Files, f)
	}
	return rep, nil
}

func firstNonEmpty(list ...string) string {
	for _, s := range list {
		if s != "" {
			return s
		}
	}
	return ""
}

// releasePackage gives the package with role release, or nil.
func releasePackage(pkgs []config.Package) *config.Package {
	for i := range pkgs {
		if pkgs[i].Role == "release" {
			return &pkgs[i]
		}
	}
	return nil
}

// parseBashLock reads packages.lock: "DIR BRANCH SHA" per line, # comments.
func parseBashLock(data []byte) []bashLine {
	var out []bashLine
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Fields(l)
		if len(f) >= 3 {
			out = append(out, bashLine{dir: strings.TrimSuffix(strings.TrimPrefix(f[0], "packages/"), "/"), branch: f[1], commit: f[2]})
		}
	}
	return out
}

var (
	assignRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	varRE    = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)
	funcRE   = regexp.MustCompile(`^(function\s+\S+|[A-Za-z_][A-Za-z0-9_]*\s*\(\))\s*\{?\s*$`)
)

// shellVars reads the assignments of scripts/common.sh outside of
// functions, and expands $VAR and ${VAR}. A command substitution gives ""
// (and x86_64 for the arch).
func shellVars(data []byte) map[string]string {
	vars := map[string]string{}
	depth := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if funcRE.MatchString(l) {
			depth++
			continue
		}
		if depth > 0 {
			depth += strings.Count(l, "{") - strings.Count(l, "}")
			continue
		}
		m := assignRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		v := m[2]
		if strings.Contains(v, "$(") {
			if strings.Contains(v, "_arch") {
				vars[m[1]] = "x86_64"
			} else {
				vars[m[1]] = ""
			}
			continue
		}
		if i := strings.Index(v, " #"); i >= 0 {
			v = v[:i]
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		vars[m[1]] = varRE.ReplaceAllStringFunc(v, func(s string) string {
			sm := varRE.FindStringSubmatch(s)
			return vars[firstNonEmpty(sm[1], sm[2])]
		})
	}
	return vars
}

// migrateTarget gives the target of scripts/common.sh: the mock root and
// the release directory el9.
func migrateTarget(vars map[string]string) config.Target {
	mock := vars["mockroot"]
	if mock == "" && vars["os"] != "" && vars["releasever"] != "" {
		mock = vars["os"] + "-" + vars["releasever"] + "-x86_64"
	}
	if !mockRE.MatchString(mock) {
		mock = DefaultTarget.Mock
	}
	id := vars["releasedir"]
	if !targetRE.MatchString(id) && vars["releasever"] != "" {
		id = "el" + vars["releasever"]
	}
	if !targetRE.MatchString(id) {
		id = DefaultTarget.ID
	}
	return config.Target{ID: id, Mock: mock, Plugins: []string{"rpmautospec"}, Path: id}
}

// A target id and a mock config name of scripts/common.sh that repoctl.yaml
// takes; any other value gives the default target.
var (
	targetRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
	mockRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]*$`)
)

var buildRE = regexp.MustCompile(`^(#\s*)?build\s+([A-Za-z0-9][A-Za-z0-9_.+-]*)\s*$`)

// buildLines gives the packages of the build lines of scripts/build at the
// top level, in the hand order, and those of the commented build lines.
func buildLines(data []byte) (order, commented []string) {
	depth := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if funcRE.MatchString(l) {
			depth++
			continue
		}
		if depth > 0 {
			depth += strings.Count(l, "{") - strings.Count(l, "}")
			continue
		}
		m := buildRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if m[1] != "" {
			commented = append(commented, m[2])
		} else {
			order = append(order, m[2])
		}
	}
	return order, commented
}

var entryRE = regexp.MustCompile(`^\[([^\]]+)\]="([^"]*)"`)

// skillTables reads REPO_MAP and DEFAULT_FILTERS of the package-info
// script. noted marks each entry with a comment above it: the comment is a
// note that migrate does not convert.
func skillTables(data []byte) (repoMap, filters map[string]string, noted map[string]bool) {
	repoMap, filters, noted = map[string]string{}, map[string]string{}, map[string]bool{}
	var table map[string]string
	comment := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(l, "declare -A REPO_MAP"):
			table = repoMap
		case strings.HasPrefix(l, "declare -A DEFAULT_FILTERS"):
			table = filters
		case table == nil:
		case l == ")":
			table = nil
		case strings.HasPrefix(l, "#"):
			comment = true
		default:
			if m := entryRE.FindStringSubmatch(l); m != nil {
				table[m[1]] = m[2]
				if comment {
					noted[m[1]] = true
				}
			}
			comment = false
		}
	}
	return repoMap, filters, noted
}

var skillLineRE = regexp.MustCompile("^- `([^`]+)`:")

// frozenNotes gives the packages whose entry in the filter list of the
// SKILL.md says that a stream is frozen.
func frozenNotes(data []byte) []string {
	var out []string
	cur := ""
	for _, l := range strings.Split(string(data), "\n") {
		if m := skillLineRE.FindStringSubmatch(l); m != nil {
			cur = m[1]
		} else if !strings.HasPrefix(l, "  ") {
			cur = ""
		}
		if cur != "" && strings.Contains(strings.ToLower(l), "frozen") {
			if len(out) == 0 || out[len(out)-1] != cur {
				out = append(out, cur)
			}
		}
	}
	return out
}

// fedoraRemotes maps a package to its Fedora name: a remote of the set
// repository at src.fedoraproject.org/rpms/NAME.
func fedoraRemotes(ctx context.Context, dir string) map[string]string {
	out := map[string]string{}
	lines, _ := git.New(dir).Lines(ctx, "remote", "-v")
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		_, after, ok := strings.Cut(f[1], "src.fedoraproject.org/rpms/")
		if !ok {
			continue
		}
		name := strings.TrimSuffix(strings.TrimSuffix(after, "/"), ".git")
		out[name] = name
	}
	return out
}

// tracked reports files of the set tree below path.
func tracked(ctx context.Context, dir, path string) bool {
	out, err := git.New(dir).Run(ctx, "ls-files", "--", path)
	return err == nil && out != ""
}

// releaseKey reads the key file of the release package with gpg: the key
// becomes sign.keys[0] (active), with the local part of its e-mail as id.
func releaseKey(ctx context.Context, dir, relPath string) (*config.Key, string, *api.Problem) {
	files, _ := filepath.Glob(filepath.Join(dir, relPath, "RPM-GPG-KEY-*"))
	if len(files) == 0 {
		return nil, "", nil
	}
	keyfile := filepath.Base(files[0])
	warn := func(format string, args ...any) *api.Problem {
		p := api.Warning(PMigrateKey, format, args...)
		p.Path = filepath.Join(relPath, keyfile)
		p.Hint = api.HintText("add the key to sign.keys by hand", "gpg --show-keys --with-fingerprint "+p.Path)
		return &p
	}
	if _, ok := run.LookPath("gpg"); !ok {
		return nil, keyfile, warn("gpg is not installed: the key of %s is not migrated", keyfile)
	}
	home, err := os.MkdirTemp("", "repoctl-gpg-")
	if err != nil {
		return nil, keyfile, warn("%v", err)
	}
	defer os.RemoveAll(home)
	out, err := (&run.Cmd{Name: "gpg", Args: []string{"--batch", "--homedir", home, "--show-keys", "--with-colons", files[0]}}).Output(ctx)
	if err != nil {
		return nil, keyfile, warn("gpg cannot read %s: %v", keyfile, err)
	}
	var fpr, uid string
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Split(l, ":")
		switch {
		case len(f) > 9 && f[0] == "fpr" && fpr == "":
			fpr = f[9]
		case len(f) > 9 && f[0] == "uid" && uid == "":
			uid = f[9]
		}
	}
	if fpr == "" {
		return nil, keyfile, warn("%s holds no key", keyfile)
	}
	id := "repo-key"
	if i, j := strings.Index(uid, "<"), strings.Index(uid, "@"); i >= 0 && j > i+1 {
		id = strings.ToLower(uid[i+1 : j])
	}
	return &config.Key{ID: id, Fingerprint: strings.ToUpper(fpr), State: config.KeyActive}, keyfile, nil
}

// orderFindings compares the hand order of scripts/build with the
// BuildRequires of the specs at the pinned commits, as rpmspec evaluates
// them on this host. Each package that the hand order builds before a
// package that it needs is a finding.
func orderFindings(ctx context.Context, dir string, order []string, byDir map[string]bashLine, set *config.Set, macros []byte) []api.Problem {
	if _, ok := run.LookPath("rpmspec"); !ok || len(order) < 2 {
		return nil
	}
	tmp, err := os.MkdirTemp("", "repoctl-migrate-")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(tmp)
	var load []string
	if macros != nil {
		m := filepath.Join(tmp, "macros")
		if os.WriteFile(m, macros, 0o644) == nil {
			load = []string{"--load", m}
		}
	}
	provides := map[string]map[string]bool{}
	needs := map[string][]string{}
	g := git.New(dir)
	for _, id := range order {
		var spec []byte
		if l, ok := byDir[id]; ok {
			spec, err = g.Show(ctx, l.commit, id+".spec")
		} else if pk, ok := set.Package(id); ok && pk.Path != "" {
			spec, err = os.ReadFile(filepath.Join(dir, pk.Path, id+".spec"))
		} else {
			continue
		}
		if err != nil {
			continue
		}
		f := filepath.Join(tmp, id+".spec")
		if os.WriteFile(f, spec, 0o644) != nil {
			continue
		}
		q := func(args ...string) []string {
			a := append(append([]string{"-q", "--define", "autorelease 1", "--define", "autochangelog %{nil}"}, load...), args...)
			out, err := (&run.Cmd{Name: "rpmspec", Args: append(a, f)}).Output(ctx)
			if err != nil {
				return nil
			}
			var names []string
			for _, l := range strings.Split(string(out), "\n") {
				if fs := strings.Fields(l); len(fs) > 0 && !strings.HasPrefix(fs[0], "(") {
					names = append(names, fs[0])
				}
			}
			return names
		}
		provides[id] = map[string]bool{}
		for _, n := range append(q("--qf", "%{NAME}\n"), q("--provides")...) {
			provides[id][n] = true
		}
		needs[id] = q("--buildrequires")
	}
	var out []api.Problem
	for i, a := range order {
		for _, b := range order[i+1:] {
			for _, br := range needs[a] {
				if provides[b][br] {
					w := api.Warning(PMigrateOrder, "the hand order of scripts/build builds %s before %s, but %s needs %s; repoctl builds in the order of the graph", a, b, a, br)
					w.Package = a
					out = append(out, w)
					break
				}
			}
		}
	}
	return out
}
