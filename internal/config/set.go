// Package config reads the configuration of repoctl: the set config
// repoctl.yaml, the user config, the REPOCTL_* variables, the set discovery
// and the XDG directories (docs/design/config.html).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/api"
)

// SetFile is the name of the set config.
const SetFile = "repoctl.yaml"

// Set is repoctl.yaml: what to build.
type Set struct {
	APIVersion string     `yaml:"apiVersion"`
	Name       string     `yaml:"name"`
	MinRepoctl string     `yaml:"minRepoctl,omitempty"`
	Repo       Repo       `yaml:"repo"`
	Store      Store      `yaml:"store,omitempty"`
	Sign       SignConfig `yaml:"sign,omitempty"`
	Retention  Retention  `yaml:"retention,omitempty"`
	Targets    []Target   `yaml:"targets"`
	Defaults   Defaults   `yaml:"defaults,omitempty"`
	Packages   []Package  `yaml:"packages"`
	Skills     []string   `yaml:"skills,omitempty"`
}

// Repo is repo: of repoctl.yaml.
type Repo struct {
	Name      string `yaml:"name"`
	Debuginfo *bool  `yaml:"debuginfo,omitempty"`
	Source    *bool  `yaml:"source,omitempty"`
	URL       string `yaml:"url,omitempty"`
}

// HasDebuginfo gives repo.debuginfo, default true.
func (r Repo) HasDebuginfo() bool { return r.Debuginfo == nil || *r.Debuginfo }

// HasSource gives repo.source, default true.
func (r Repo) HasSource() bool { return r.Source == nil || *r.Source }

// Store is store: of repoctl.yaml.
type Store struct {
	OCI string `yaml:"oci,omitempty"`
}

// SignConfig is sign: of repoctl.yaml.
type SignConfig struct {
	Keys    []Key      `yaml:"keys,omitempty"`
	Release Release    `yaml:"release,omitempty"`
	Checks  SignChecks `yaml:"checks,omitempty"`
}

// Key is one entry of sign.keys.
type Key struct {
	ID          string `yaml:"id"`
	File        string `yaml:"file"`
	Fingerprint string `yaml:"fingerprint"`
	State       string `yaml:"state"`
}

// The key states.
const (
	KeyActive   = "active"
	KeyRetiring = "retiring"
	KeyRemoved  = "removed"
	KeyRevoked  = "revoked"
)

// Release is sign.release.
type Release struct {
	Keyfile string `yaml:"keyfile,omitempty"`
}

// SignChecks is sign.checks.
type SignChecks struct {
	KeyExpiryWarn  Duration `yaml:"key_expiry_warn,omitempty"`
	KeyMinValidity Duration `yaml:"key_min_validity,omitempty"`
}

// Retention is retention: of repoctl.yaml.
type Retention struct {
	Snapshots SnapshotRetention `yaml:"snapshots,omitempty"`
}

// SnapshotRetention is retention.snapshots.
type SnapshotRetention struct {
	Keep        int      `yaml:"keep,omitempty"`
	KeepDays    Duration `yaml:"keep_days,omitempty"`
	KeepRemoved Duration `yaml:"keep_removed,omitempty"`
}

// Target is one entry of targets.
type Target struct {
	ID           string        `yaml:"id"`
	Mock         string        `yaml:"mock"`
	Plugins      []string      `yaml:"plugins"`
	Macros       []string      `yaml:"macros,omitempty"`
	Path         string        `yaml:"path,omitempty"`
	Repos        []string      `yaml:"repos,omitempty"`
	State        string        `yaml:"state,omitempty"`
	ClosureRepos []ClosureRepo `yaml:"closure_repos,omitempty"`
}

// Retired reports targets[].state: retired.
func (t Target) Retired() bool { return t.State == "retired" }

// ClientPath gives targets[].path, default the target id.
func (t Target) ClientPath() string {
	if t.Path != "" {
		return t.Path
	}
	return t.ID
}

// Arch gives the architecture of the target: the last part of its mock
// config name.
func (t Target) Arch() string {
	i := strings.LastIndex(t.Mock, "-")
	if i < 0 {
		return ""
	}
	return t.Mock[i+1:]
}

// ClosureRepo is one entry of targets[].closure_repos.
type ClosureRepo struct {
	ID  string `yaml:"id"`
	URL string `yaml:"url"`
}

// Defaults is defaults: of repoctl.yaml.
type Defaults struct {
	Git DefaultsGit `yaml:"git,omitempty"`
}

// DefaultsGit is defaults.git.
type DefaultsGit struct {
	Remote string `yaml:"remote,omitempty"`
}

// Package is one entry of packages.
type Package struct {
	ID       string      `yaml:"id"`
	Branch   string      `yaml:"branch,omitempty"`
	Path     string      `yaml:"path,omitempty"`
	Git      *PackageGit `yaml:"git,omitempty"`
	Targets  []string    `yaml:"targets,omitempty"`
	Role     string      `yaml:"role,omitempty"`
	Upstream *Upstream   `yaml:"upstream,omitempty"`
	Notes    string      `yaml:"notes,omitempty"`
}

// InTree reports an in-tree package (path, no branch).
func (p Package) InTree() bool { return p.Path != "" }

// External reports a package of an external repository.
func (p Package) External() bool { return p.Git != nil && p.Git.URL != "" }

// URL gives git.url of an external package, else "".
func (p Package) URL() string {
	if p.Git == nil {
		return ""
	}
	return p.Git.URL
}

// PackageGit is packages[].git.
type PackageGit struct {
	URL string `yaml:"url"`
}

// Upstream is packages[].upstream.
type Upstream struct {
	GitHub  string `yaml:"github,omitempty" json:"github,omitempty"`
	Tags    string `yaml:"tags,omitempty" json:"tags,omitempty"`
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	Frozen  string `yaml:"frozen,omitempty" json:"frozen,omitempty"`
	Fedora  string `yaml:"fedora,omitempty" json:"fedora,omitempty"`
}

// Remote gives defaults.git.remote, default origin.
func (s *Set) Remote() string {
	if s.Defaults.Git.Remote != "" {
		return s.Defaults.Git.Remote
	}
	return "origin"
}

// Package gives the package with the id.
func (s *Set) Package(id string) (*Package, bool) {
	for i := range s.Packages {
		if s.Packages[i].ID == id {
			return &s.Packages[i], true
		}
	}
	return nil, false
}

// Target gives the target with the id.
func (s *Set) Target(id string) (*Target, bool) {
	for i := range s.Targets {
		if s.Targets[i].ID == id {
			return &s.Targets[i], true
		}
	}
	return nil, false
}

// ActiveTargets gives the targets that are not retired, in file order.
func (s *Set) ActiveTargets() []Target {
	var out []Target
	for _, t := range s.Targets {
		if !t.Retired() {
			out = append(out, t)
		}
	}
	return out
}

// PackageTargets gives the target ids of a package: packages[].targets, or
// all active targets.
func (s *Set) PackageTargets(p Package) []string {
	var out []string
	for _, t := range s.ActiveTargets() {
		if len(p.Targets) == 0 || contains(p.Targets, t.ID) {
			out = append(out, t.ID)
		}
	}
	return out
}

// Nodes gives every node PKG@TARGET of the set, in package order, then
// target order.
func (s *Set) Nodes() []api.NodeId {
	var out []api.NodeId
	for _, p := range s.Packages {
		for _, t := range s.PackageTargets(p) {
			out = append(out, api.Node(p.ID, t))
		}
	}
	return out
}

// ReleasePackage gives the package with role release, if any.
func (s *Set) ReleasePackage() (*Package, bool) {
	for i := range s.Packages {
		if s.Packages[i].Role == "release" {
			return &s.Packages[i], true
		}
	}
	return nil, false
}

// OCIPackage gives the OCI package of a target: STORE/SET-TARGET.
func (s *Set) OCIPackage(target string) string {
	if s.Store.OCI == "" {
		return ""
	}
	return strings.TrimSuffix(s.Store.OCI, "/") + "/" + s.Name + "-" + target
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

// ParseSet reads repoctl.yaml. path names the file in messages.
func ParseSet(data []byte, path string) (*Set, error) {
	// apiVersion and minRepoctl first: a newer file stops with
	// contract.newer-file, not config.unknown-key.
	var head struct {
		APIVersion string `yaml:"apiVersion"`
		MinRepoctl string `yaml:"minRepoctl"`
	}
	if err := yaml.Unmarshal(data, &head); err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "%s: %v", path, yamlMessage(err)).WithPath(path).
			WithHint("correct the file", "repoctl schema config")
	}
	if head.APIVersion == "" {
		return nil, api.Errorf(api.PConfigInvalid, "%s: apiVersion is missing", path).WithPath(path).
			WithHint("add the line apiVersion: "+api.APIVersion, "")
	}
	if !contains(api.ReadAPIVersions, head.APIVersion) {
		return nil, api.Errorf(api.PContractNewerFile, "%s: apiVersion %s is newer than this repoctl (reads %s)", path, head.APIVersion, strings.Join(api.ReadAPIVersions, ", ")).
			WithPath(path).WithHint("upgrade repoctl on this host", "repoctl help upgrade")
	}
	if head.MinRepoctl != "" && VersionLess(Version, head.MinRepoctl) {
		return nil, api.Errorf(api.PContractNewerFile, "%s: minRepoctl %s is newer than this repoctl %s", path, head.MinRepoctl, Version).
			WithPath(path).WithHint("upgrade repoctl on this host", "repoctl help upgrade")
	}
	var s Set
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		msg := yamlMessage(err)
		if strings.Contains(msg, "not found in type") {
			return nil, api.Errorf(api.PConfigUnknownKey, "%s: %s", path, msg).WithPath(path).
				WithHint("remove the key, or upgrade repoctl when a newer release knows it", "repoctl schema config")
		}
		return nil, api.Errorf(api.PConfigInvalid, "%s: %s", path, msg).WithPath(path).
			WithHint("correct the file", "repoctl schema config")
	}
	if err := s.Validate(path); err != nil {
		return nil, err
	}
	return &s, nil
}

// yamlMessage gives the text of a yaml.v3 error without its prefix.
func yamlMessage(err error) string {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		return strings.Join(te.Errors, "; ")
	}
	return strings.TrimPrefix(err.Error(), "yaml: ")
}

// Validate checks the rules of repoctl.yaml (config.html#rules).
func (s *Set) Validate(file string) error {
	bad := func(code api.ProblemCode, format string, args ...any) error {
		return api.Errorf(code, "%s: %s", file, fmt.Sprintf(format, args...)).WithPath(file).
			WithHint("correct the file", "repoctl schema config")
	}
	if !nameRE.MatchString(s.Name) {
		return bad(api.PConfigInvalid, "name %q: want lowercase letters, digits, . and -", s.Name)
	}
	if s.Repo.Name == "" {
		return bad(api.PConfigInvalid, "repo.name is missing")
	}
	if len(s.Targets) == 0 {
		return bad(api.PConfigInvalid, "targets is empty")
	}
	targets := map[string]bool{}
	for i, t := range s.Targets {
		where := fmt.Sprintf("targets[%d]", i)
		if !nameRE.MatchString(t.ID) {
			return bad(api.PConfigInvalid, "%s.id %q: want lowercase letters, digits, . and -", where, t.ID)
		}
		if targets[t.ID] {
			return bad(api.PConfigInvalid, "%s.id %q is two times in targets", where, t.ID)
		}
		targets[t.ID] = true
		if t.Mock == "" {
			return bad(api.PConfigInvalid, "%s.mock is missing", where)
		}
		if t.Arch() != "x86_64" {
			return bad(api.PConfigUnsupported, "%s.mock %q: this release supports only x86_64", where, t.Mock)
		}
		if !contains(t.Plugins, "rpmautospec") {
			return bad(api.PConfigInvalid, "%s.plugins must contain rpmautospec: without it, a rebuild commit gives no new NVR", where)
		}
		switch t.State {
		case "", "active", "retired":
		default:
			return bad(api.PConfigInvalid, "%s.state %q: want active or retired", where, t.State)
		}
	}
	ids := map[string]bool{}
	release := 0
	for i, p := range s.Packages {
		where := fmt.Sprintf("packages[%d]", i)
		if p.ID == "" {
			return bad(api.PConfigInvalid, "%s.id is missing", where)
		}
		if ids[p.ID] {
			return bad(api.PConfigInvalid, "%s.id %q is two times in packages", where, p.ID)
		}
		ids[p.ID] = true
		if (p.Branch == "") == (p.Path == "") {
			return bad(api.PConfigInvalid, "%s (%s): give exactly one of branch and path", where, p.ID)
		}
		if p.Path != "" && (path.IsAbs(p.Path) || strings.HasPrefix(path.Clean(p.Path), "..")) {
			return bad(api.PConfigInvalid, "%s (%s): path %q must be a relative path in the set", where, p.ID, p.Path)
		}
		if p.Path != "" && p.Git != nil {
			return bad(api.PConfigInvalid, "%s (%s): an in-tree package has no git.url", where, p.ID)
		}
		for _, t := range p.Targets {
			if !targets[t] {
				return bad(api.PConfigInvalid, "%s.targets: %q is not a target of the set", where, t)
			}
		}
		if p.Role != "" {
			if p.Role != "release" {
				return bad(api.PConfigInvalid, "%s.role %q: want release", where, p.Role)
			}
			release++
			if p.Path == "" {
				return bad(api.PConfigInvalid, "%s (%s): the release package is in-tree and needs path", where, p.ID)
			}
		}
	}
	if release > 1 {
		return bad(api.PConfigInvalid, "more than one package has role: release")
	}
	for i, k := range s.Sign.Keys {
		switch k.State {
		case KeyActive, KeyRetiring, KeyRemoved, KeyRevoked:
		default:
			return bad(api.PConfigInvalid, "sign.keys[%d].state %q: want active, retiring, removed or revoked", i, k.State)
		}
	}
	return nil
}

// LoadSet reads repoctl.yaml in dir.
func LoadSet(dir string) (*Set, error) {
	p := filepath.Join(dir, SetFile)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "%s: %v", p, err).WithPath(p)
	}
	return ParseSet(data, p)
}
