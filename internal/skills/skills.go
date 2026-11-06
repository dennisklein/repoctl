// Package skills holds the agent skills of repoctl: the generic skills that
// ship in the binary, the set-local skills of repoctl.yaml, their install
// into Claude Code with the deny list and the guard hook, the guard hook
// itself and the doctor checks of the skills (docs/design/agents.html#skills).
//
// A skill is one SKILL.md with a YAML frontmatter: name, description,
// allowed-tools and metadata. The embedded files carry no version: the
// binary writes its own version into metadata.repoctl-version and the
// output apiVersion that the skill reads into metadata.api-version when it
// shows or installs a skill (Render). Thus each copy tells its version.
package skills

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/api"
)

// The source of a skill (api.Skill.Source).
const (
	SourceBuiltin = "builtin"
	SourceSet     = "set"
)

// FileName is the file of a skill in its directory.
const FileName = "SKILL.md"

// SystemDir is the directory of the skills of the RPM (repoctl skills path).
const SystemDir = "/usr/share/repoctl/skills"

//go:embed builtin
var builtinFS embed.FS

// Skill is one skill: a generic skill of the binary or a set-local skill.
type Skill struct {
	Name        string
	Description string
	// Source is SourceBuiltin or SourceSet.
	Source string
	// Version is the version of the skill: the binary version for a
	// generic skill, metadata.version or metadata.repoctl-version for a
	// set-local skill.
	Version string
	// APIVersion is the output apiVersion that the skill reads.
	APIVersion   string
	AllowedTools []string
	// Dir is the directory of a set-local skill.
	Dir string
	// Text is SKILL.md as the binary embeds it, or as the set holds it.
	Text []byte
	// Conflict marks a set-local skill with the name of a generic skill.
	Conflict bool
}

// Frontmatter is the YAML head of a SKILL.md.
type Frontmatter struct {
	Name         string            `yaml:"name"`
	Description  string            `yaml:"description"`
	AllowedTools toolList          `yaml:"allowed-tools"`
	Metadata     map[string]string `yaml:"metadata"`
}

// toolList is allowed-tools: a YAML list or a comma-separated string.
type toolList []string

// UnmarshalYAML takes a sequence or a comma-separated scalar.
func (t *toolList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var out []string
		for _, p := range strings.Split(n.Value, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		*t = out
		return nil
	case yaml.SequenceNode:
		var out []string
		if err := n.Decode(&out); err != nil {
			return err
		}
		*t = out
		return nil
	}
	return fmt.Errorf("allowed-tools must be a list or a string")
}

// ParseFrontmatter splits a SKILL.md into its frontmatter and its body.
func ParseFrontmatter(text []byte) (Frontmatter, []byte, error) {
	var fm Frontmatter
	head, body, ok := splitFrontmatter(text)
	if !ok {
		return fm, text, errors.New("SKILL.md has no frontmatter between --- lines")
	}
	if err := yaml.Unmarshal(head, &fm); err != nil {
		return fm, body, fmt.Errorf("frontmatter: %w", err)
	}
	if fm.Name == "" {
		return fm, body, errors.New("frontmatter: name is missing")
	}
	if fm.Description == "" {
		return fm, body, errors.New("frontmatter: description is missing")
	}
	return fm, body, nil
}

// splitFrontmatter gives the YAML between the first two --- lines and the
// rest.
func splitFrontmatter(text []byte) (head, body []byte, ok bool) {
	t := bytes.TrimPrefix(text, []byte("\xef\xbb\xbf"))
	if !bytes.HasPrefix(t, []byte("---\n")) && !bytes.HasPrefix(t, []byte("---\r\n")) {
		return nil, text, false
	}
	_, rest, _ := bytes.Cut(t, []byte("\n"))
	off := 0
	for {
		line, next, found := bytes.Cut(rest[off:], []byte("\n"))
		if string(bytes.TrimRight(line, "\r")) == "---" {
			return rest[:off], next, true
		}
		if !found {
			return nil, text, false
		}
		off += len(line) + 1
	}
}

// Builtin gives the generic skills of the binary, sorted by name, with the
// version of the binary.
func Builtin(version string) []Skill {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		panic(err)
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		text, err := builtinFS.ReadFile("builtin/" + e.Name() + "/" + FileName)
		if err != nil {
			panic(err)
		}
		fm, _, err := ParseFrontmatter(text)
		if err != nil {
			panic(fmt.Sprintf("builtin skill %s: %v", e.Name(), err))
		}
		out = append(out, Skill{
			Name: fm.Name, Description: oneLine(fm.Description), Source: SourceBuiltin,
			Version: version, APIVersion: api.APIVersion, AllowedTools: fm.AllowedTools, Text: text,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names gives the names of the generic skills.
func Names() []string {
	var out []string
	for _, s := range Builtin("") {
		out = append(out, s.Name)
	}
	return out
}

// Lookup gives the generic skill with this name.
func Lookup(name, version string) (Skill, bool) {
	for _, s := range Builtin(version) {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// Render gives SKILL.md of a generic skill with metadata.repoctl-version
// and metadata.api-version of this binary. A set-local skill is given as it
// is.
func (s Skill) Render() []byte {
	if s.Source != SourceBuiltin {
		return s.Text
	}
	return setMetadata(s.Text, map[string]string{"repoctl-version": s.Version, "api-version": api.APIVersion})
}

// setMetadata sets keys of the metadata block of the frontmatter, in place
// of the lines that the file has, else at the end of the block.
func setMetadata(text []byte, values map[string]string) []byte {
	head, body, ok := splitFrontmatter(text)
	if !ok {
		return text
	}
	lines := strings.Split(strings.TrimSuffix(string(head), "\n"), "\n")
	done := map[string]bool{}
	inMeta, metaEnd := false, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "metadata:") {
			inMeta, metaEnd = true, i+1
			continue
		}
		if !inMeta {
			continue
		}
		if l != "" && !strings.HasPrefix(l, " ") {
			inMeta = false
			continue
		}
		metaEnd = i + 1
		key, _, found := strings.Cut(strings.TrimSpace(l), ":")
		if v, ok := values[key]; ok && found {
			lines[i] = "  " + key + ": " + v
			done[key] = true
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		if !done[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var add []string
	for _, k := range keys {
		add = append(add, "  "+k+": "+values[k])
	}
	if len(add) > 0 {
		if metaEnd < 0 {
			lines = append(lines, "metadata:")
			lines = append(lines, add...)
		} else {
			lines = append(lines[:metaEnd], append(add, lines[metaEnd:]...)...)
		}
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n---\n")
	b.Write(body)
	return b.Bytes()
}

// SetSkills reads the set-local skills of the directories of skills: in
// repoctl.yaml. Each subdirectory with a SKILL.md is a skill. A skill with
// the name of a generic skill gets Conflict: a set-local skill can only add.
func SetSkills(setDir string, dirs []string) ([]Skill, []error) {
	builtin := map[string]bool{}
	for _, n := range Names() {
		builtin[n] = true
	}
	var out []Skill
	var errs []error
	for _, d := range dirs {
		dir := d
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(setDir, d)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			file := filepath.Join(dir, e.Name(), FileName)
			text, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			fm, _, err := ParseFrontmatter(text)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", file, err))
				continue
			}
			if isRepoctlCopy(dir, e.Name(), fm) {
				// A copy of a generic skill that skills install wrote,
				// or that a human copied: not a set-local skill.
				continue
			}
			v := fm.Metadata["version"]
			if v == "" {
				v = fm.Metadata["repoctl-version"]
			}
			if v == "" {
				v = "-"
			}
			out = append(out, Skill{
				Name: fm.Name, Description: oneLine(fm.Description), Source: SourceSet, Version: v,
				APIVersion: fm.Metadata["api-version"], AllowedTools: fm.AllowedTools,
				Dir: filepath.Join(dir, e.Name()), Text: text, Conflict: builtin[fm.Name],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// isRepoctlCopy reports whether the skill directory dir/name holds a copy
// of a generic skill: it is in the manifest of dir, or it carries
// metadata.repoctl-version and the name of a generic skill.
func isRepoctlCopy(dir, name string, fm Frontmatter) bool {
	if m, err := ReadManifest(dir); err == nil && m.Skill(name) != nil {
		return true
	}
	if _, ok := Lookup(fm.Name, ""); ok && fm.Metadata["repoctl-version"] != "" && fm.Metadata["version"] == "" {
		return true
	}
	return false
}

// APISkill gives the item of kind Skill.
func (s Skill) APISkill() api.Skill {
	return api.Skill{Name: s.Name, Source: s.Source, Version: s.Version, Description: s.Description, APIVersion: s.APIVersion, Conflict: s.Conflict}
}

// sha256Hex gives the sha256 of b in hex.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
