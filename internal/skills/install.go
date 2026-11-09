package skills

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// The scopes of an install (api.Skill.Installed[].Scope).
const (
	ScopeUser    = "user"
	ScopeProject = "project"
	ScopePlugin  = "plugin"
	ScopeSystem  = "system"
)

// ManifestFile is the manifest of skills install in the install root.
const ManifestFile = ".repoctl-skills.json"

// PChanged is the problem of a skill file that a human changed, or of a
// skill directory that skills install did not write. install and uninstall
// refuse it.
const PChanged = api.PSkillsChanged

// Place is an install place of Claude Code: the skills directory, the
// settings file and, for a project, the set and its exclude file.
type Place struct {
	Scope    string
	Root     string // the skills directory: ~/.claude/skills or SET/.claude/skills
	Settings string // ~/.claude/settings.json or SET/.claude/settings.json
	SetDir   string // project only
	Exclude  string // project only: the info/exclude file of the set repository
}

// UserPlace gives the place of skills install --user.
func UserPlace(home string) Place {
	return Place{Scope: ScopeUser, Root: filepath.Join(home, ".claude", "skills"), Settings: filepath.Join(home, ".claude", "settings.json")}
}

// ProjectPlace gives the place of skills install --project in the set
// directory.
func ProjectPlace(ctx context.Context, setDir string) (Place, error) {
	p := Place{
		Scope: ScopeProject, SetDir: setDir,
		Root:     filepath.Join(setDir, ".claude", "skills"),
		Settings: filepath.Join(setDir, ".claude", "settings.json"),
	}
	ex, err := git.New(setDir).Run(ctx, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return p, api.Errorf(api.PConfigInvalid, "set %s is not a git work tree: %v", setDir, err).WithPath(setDir)
	}
	if !filepath.IsAbs(ex) {
		ex = filepath.Join(setDir, ex)
	}
	p.Exclude = ex
	return p, nil
}

// Manifest is .repoctl-skills.json: what skills install wrote into one
// install root (agents.html#frontmatter).
type Manifest struct {
	// Version is the format of the file, 1.
	Version int `json:"version"`
	// Repoctl is the version of the binary that wrote the file last.
	Repoctl string          `json:"repoctl"`
	Skills  []ManifestSkill `json:"skills"`
	// Deny are the rules that install added to the settings file.
	Deny []string `json:"deny,omitempty"`
	// Hook says that install added the guard hook to the settings file.
	Hook bool `json:"hook,omitempty"`
	// Settings is the settings file, and SettingsCreated says that install
	// made it.
	Settings        string `json:"settings,omitempty"`
	SettingsCreated bool   `json:"settingsCreated,omitempty"`
	// Exclude are the lines that install added to .git/info/exclude.
	Exclude []string `json:"exclude,omitempty"`
}

// ManifestSkill is one skill of the manifest: the sha256 of each file that
// install wrote, by its path in the skill directory.
type ManifestSkill struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Files   map[string]string `json:"files"`
}

// Skill gives the entry of a skill, or nil.
func (m *Manifest) Skill(name string) *ManifestSkill {
	for i := range m.Skills {
		if m.Skills[i].Name == name {
			return &m.Skills[i]
		}
	}
	return nil
}

func (m *Manifest) put(s ManifestSkill) {
	if e := m.Skill(s.Name); e != nil {
		*e = s
		return
	}
	m.Skills = append(m.Skills, s)
	sort.Slice(m.Skills, func(i, j int) bool { return m.Skills[i].Name < m.Skills[j].Name })
}

func (m *Manifest) drop(name string) {
	for i := range m.Skills {
		if m.Skills[i].Name == name {
			m.Skills = append(m.Skills[:i], m.Skills[i+1:]...)
			return
		}
	}
}

// ReadManifest reads the manifest of an install root. A missing file is an
// empty manifest.
func ReadManifest(root string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(root, ManifestFile))
	if errors.Is(err, fs.ErrNotExist) {
		return &Manifest{Version: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "%s: %v", filepath.Join(root, ManifestFile), err).WithPath(filepath.Join(root, ManifestFile))
	}
	return &m, nil
}

func writeManifest(root string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, ManifestFile), append(b, '\n'), 0o644)
}

// Changed gives the files of an installed skill whose sha256 differs from
// the manifest: a change by a human. A missing file does not count.
func Changed(root string, e *ManifestSkill) []string {
	var out []string
	for rel, sum := range e.Files {
		b, err := os.ReadFile(filepath.Join(root, e.Name, rel))
		if err != nil {
			continue
		}
		if sha256Hex(b) != sum {
			out = append(out, filepath.Join(root, e.Name, rel))
		}
	}
	sort.Strings(out)
	return out
}

// InstallOptions are the inputs of Install.
type InstallOptions struct {
	Place   Place
	Skills  []Skill // the generic skills to install
	Version string  // the version of the binary
	// SetNames are the names of the set-local skills in the install root;
	// install never writes them.
	SetNames map[string]bool
}

// InstallResult tells what Install did.
type InstallResult struct {
	Installed []api.Skill
	// DenyAdded and HookAdded tell the change of the settings file.
	DenyAdded []string
	HookAdded bool
	Exclude   []string
}

// Install writes the skills as plain SKILL.md files, merges the deny list
// and the guard hook into the settings file and, for a project, adds the
// paths to the exclude file. It replaces the copies that it wrote before
// and refuses, before it writes anything, a file that a human changed, a
// directory that it did not write and the name of a set-local skill.
func Install(o InstallOptions) (*InstallResult, error) {
	p := o.Place
	m, err := ReadManifest(p.Root)
	if err != nil {
		return nil, err
	}
	for _, s := range o.Skills {
		dir := filepath.Join(p.Root, s.Name)
		if o.SetNames[s.Name] {
			return nil, api.Errorf(PChanged, "%s is a set-local skill; install never writes over it", dir).WithPath(dir).
				WithHint("rename the set-local skill; a set-local skill can only add", "")
		}
		e := m.Skill(s.Name)
		if e == nil {
			if _, err := os.Stat(dir); err == nil {
				return nil, api.Errorf(PChanged, "%s was not written by repoctl skills install", dir).WithPath(dir).
					WithHint("move the directory away, then install again", "")
			}
			continue
		}
		if ch := Changed(p.Root, e); len(ch) > 0 {
			return nil, api.Errorf(PChanged, "%s was changed by a human", ch[0]).WithPath(ch[0]).
				WithHint("keep the change elsewhere and remove the file, then install again", "")
		}
	}
	// The settings file first: an invalid file stops the install before a
	// skill is written.
	settingsBytes, err := os.ReadFile(p.Settings)
	created := errors.Is(err, fs.ErrNotExist)
	if err != nil && !created {
		return nil, err
	}
	st, err := ParseSettings(settingsBytes)
	if err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "%s: %v", p.Settings, err).WithPath(p.Settings).
			WithHint("correct the JSON of the settings file, then install again", "")
	}
	res := &InstallResult{}
	for _, s := range o.Skills {
		dir := filepath.Join(p.Root, s.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		text := s.Render()
		if err := os.WriteFile(filepath.Join(dir, FileName), text, 0o644); err != nil {
			return nil, err
		}
		m.put(ManifestSkill{Name: s.Name, Version: s.Version, Files: map[string]string{FileName: sha256Hex(text)}})
		item := s.APISkill()
		item.Installed = []api.SkillInstalledItem{{Scope: p.Scope, Path: dir, Version: s.Version, State: "same"}}
		res.Installed = append(res.Installed, item)
	}
	added, err := st.AddDeny(DenyList)
	if err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "%s: %v", p.Settings, err).WithPath(p.Settings)
	}
	hook, err := st.AddGuard()
	if err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "%s: %v", p.Settings, err).WithPath(p.Settings)
	}
	res.DenyAdded, res.HookAdded = added, hook
	m.Deny = appendNew(m.Deny, added...)
	m.Hook = m.Hook || hook
	if len(added) > 0 || hook || created {
		b, err := st.Bytes()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(p.Settings), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p.Settings, b, 0o644); err != nil {
			return nil, err
		}
	}
	m.Settings = p.Settings
	m.SettingsCreated = m.SettingsCreated || created
	if p.Scope == ScopeProject {
		lines := []string{"/.claude/skills/" + ManifestFile}
		for _, s := range m.Skills {
			lines = append(lines, "/.claude/skills/"+s.Name+"/")
		}
		if m.SettingsCreated {
			lines = append(lines, "/.claude/settings.json")
		}
		newLines, err := addExclude(p.Exclude, lines)
		if err != nil {
			return nil, err
		}
		m.Exclude = appendNew(m.Exclude, newLines...)
		res.Exclude = newLines
	}
	m.Version, m.Repoctl = 1, o.Version
	if err := writeManifest(p.Root, m); err != nil {
		return nil, err
	}
	return res, nil
}

// UninstallResult tells what Uninstall did.
type UninstallResult struct {
	Removed []api.Skill
	// Settings says that the deny rules and the guard hook of install were
	// removed: the last skill of the place is gone.
	Settings bool
}

// Uninstall removes the copies of the manifest, and with the last copy the
// deny rules and the guard hook that install added and the exclude lines.
// It refuses a file that a human changed and leaves set-local skills alone.
// names empty is each skill of the manifest.
func Uninstall(p Place, names []string) (*UninstallResult, error) {
	m, err := ReadManifest(p.Root)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		for _, s := range m.Skills {
			names = append(names, s.Name)
		}
	}
	var drop []ManifestSkill
	for _, n := range names {
		e := m.Skill(n)
		if e == nil {
			continue
		}
		if ch := Changed(p.Root, e); len(ch) > 0 {
			return nil, api.Errorf(PChanged, "%s was changed by a human", ch[0]).WithPath(ch[0]).
				WithHint("keep the change elsewhere and remove the file, then uninstall again", "")
		}
		drop = append(drop, *e)
	}
	res := &UninstallResult{}
	for _, e := range drop {
		dir := filepath.Join(p.Root, e.Name)
		for rel := range e.Files {
			_ = os.Remove(filepath.Join(dir, rel))
		}
		removeEmptyDirs(dir, p.Root)
		res.Removed = append(res.Removed, api.Skill{Name: e.Name, Source: SourceBuiltin, Version: e.Version,
			Installed: []api.SkillInstalledItem{{Scope: p.Scope, Path: dir, Version: e.Version}}})
		m.drop(e.Name)
	}
	if len(m.Skills) > 0 {
		if len(drop) > 0 {
			if err := writeManifest(p.Root, m); err != nil {
				return nil, err
			}
		}
		return res, nil
	}
	// The last skill is gone: remove what install added.
	if b, err := os.ReadFile(p.Settings); err == nil {
		st, err := ParseSettings(b)
		if err != nil {
			return nil, api.Errorf(api.PConfigInvalid, "%s: %v", p.Settings, err).WithPath(p.Settings)
		}
		if err := st.RemoveDeny(m.Deny); err != nil {
			return nil, err
		}
		if m.Hook {
			if err := st.RemoveGuard(); err != nil {
				return nil, err
			}
		}
		if st.Empty() && m.SettingsCreated {
			_ = os.Remove(p.Settings)
		} else {
			out, err := st.Bytes()
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(out, b) {
				if err := os.WriteFile(p.Settings, out, 0o644); err != nil {
					return nil, err
				}
			}
		}
		res.Settings = true
	}
	if p.Exclude != "" && len(m.Exclude) > 0 {
		if err := removeExclude(p.Exclude, m.Exclude); err != nil {
			return nil, err
		}
	}
	_ = os.Remove(filepath.Join(p.Root, ManifestFile))
	removeEmptyDirs(p.Root, filepath.Dir(filepath.Dir(p.Root)))
	return res, nil
}

// removeEmptyDirs removes dir and its empty parents up to stop.
func removeEmptyDirs(dir, stop string) {
	for dir != stop && strings.HasPrefix(dir, stop) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func appendNew(list []string, add ...string) []string {
	have := map[string]bool{}
	for _, x := range list {
		have[x] = true
	}
	for _, x := range add {
		if !have[x] {
			list = append(list, x)
			have[x] = true
		}
	}
	return list
}

// addExclude adds the lines that the exclude file does not hold and gives
// them.
func addExclude(file string, lines []string) ([]string, error) {
	b, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	have := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		have[strings.TrimSpace(sc.Text())] = true
	}
	var add []string
	for _, l := range lines {
		if !have[l] {
			add = append(add, l)
			have[l] = true
		}
	}
	if len(add) == 0 {
		return nil, nil
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		b = append(b, '\n')
	}
	b = append(b, []byte(strings.Join(add, "\n")+"\n")...)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, err
	}
	return add, os.WriteFile(file, b, 0o644)
}

// removeExclude removes these lines from the exclude file.
func removeExclude(file string, lines []string) error {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	drop := map[string]bool{}
	for _, l := range lines {
		drop[l] = true
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if !drop[strings.TrimSpace(l)] {
			out = append(out, l)
		}
	}
	text := strings.Join(out, "\n")
	if text != "" {
		text += "\n"
	}
	return os.WriteFile(file, []byte(text), 0o644)
}

// Copy is an installed copy of a generic skill.
type Copy struct {
	Name    string
	Scope   string
	Path    string // the skill directory
	Version string // metadata.repoctl-version of the copy
	// State is same, older, newer or changed.
	State string
}

// Copies finds the copies of the generic skills: user, project (when
// setDir is not empty) and system (systemDir).
func Copies(home, setDir, systemDir, version string) []Copy {
	type place struct{ scope, root string }
	places := []place{{ScopeUser, filepath.Join(home, ".claude", "skills")}}
	if setDir != "" {
		places = append(places, place{ScopeProject, filepath.Join(setDir, ".claude", "skills")})
	}
	if systemDir != "" {
		places = append(places, place{ScopeSystem, systemDir})
	}
	var out []Copy
	for _, pl := range places {
		m, _ := ReadManifest(pl.root)
		if m == nil {
			m = &Manifest{}
		}
		for _, name := range Names() {
			dir := filepath.Join(pl.root, name)
			text, err := os.ReadFile(filepath.Join(dir, FileName))
			if err != nil {
				continue
			}
			fm, _, perr := ParseFrontmatter(text)
			e := m.Skill(name)
			if e == nil && pl.scope == ScopeProject && (perr != nil || fm.Metadata["repoctl-version"] == "") {
				// A set-local skill with the name of a generic skill.
				continue
			}
			c := Copy{Name: name, Scope: pl.scope, Path: dir, Version: fm.Metadata["repoctl-version"]}
			switch {
			case e != nil && len(Changed(pl.root, e)) > 0:
				c.State = "changed"
			default:
				c.State = CompareVersion(c.Version, version)
			}
			out = append(out, c)
		}
	}
	return out
}

// CompareVersion gives same, older or newer for a copy of version v
// against the binary version.
func CompareVersion(v, binary string) string {
	switch {
	case strings.TrimPrefix(v, "v") == strings.TrimPrefix(binary, "v"):
		return "same"
	case v == "" || config.VersionLess(v, binary):
		return "older"
	default:
		return "newer"
	}
}

// Describe gives a short text of an install result for the terminal.
func (r *InstallResult) Describe(p Place) string {
	var parts []string
	if len(r.DenyAdded) > 0 {
		parts = append(parts, fmt.Sprintf("%d deny rules", len(r.DenyAdded)))
	}
	if r.HookAdded {
		parts = append(parts, "the guard hook")
	}
	if len(parts) == 0 {
		return p.Settings + " has the deny list and the guard hook"
	}
	return "added " + strings.Join(parts, " and ") + " to " + p.Settings
}
