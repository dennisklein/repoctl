package workspace

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// Draft is a repoctl.yaml that init and migrate write, with the comments
// of the starter file (config.html#set-config).
type Draft struct {
	Name, Repo string
	RepoURL    string
	Targets    []config.Target
	Keys       []config.Key
	// ReleaseKeyfile is sign.release.keyfile.
	ReleaseKeyfile string
	Remote         string
	Packages       []config.Package
}

var plainRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./+@-]*$`)

// yaml11Bools are the plain words that a YAML 1.1 reader takes as booleans.
var yaml11Bools = map[string]bool{"y": true, "n": true, "yes": true, "no": true, "on": true, "off": true}

// scalar gives a YAML scalar of s: plain when it reads back as the same
// string, also for a YAML 1.1 reader, else single-quoted.
func scalar(s string) string {
	if plainRE.MatchString(s) && !yaml11Bools[strings.ToLower(s)] {
		var v any
		if err := yaml.Unmarshal([]byte(s), &v); err == nil {
			if str, ok := v.(string); ok && str == s {
				return s
			}
		}
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func flowList(list []string) string {
	q := make([]string, len(list))
	for i, s := range list {
		q[i] = scalar(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// comment pads a line to column 35 and adds a comment.
func comment(line, text string) string {
	if len(line) < 34 {
		line += strings.Repeat(" ", 34-len(line))
	}
	return line + " # " + text
}

// Render gives the text of repoctl.yaml.
func (d Draft) Render() []byte {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("apiVersion: %s", api.APIVersion)
	w("%s", comment("name: "+scalar(d.Name), "set id: in all local paths and OCI package names"))
	w("")
	w("repo:")
	w("%s", comment("  name: "+scalar(d.Repo), "gives "+d.Repo+", "+d.Repo+"-source and "+d.Repo+"-debuginfo"))
	if d.RepoURL != "" {
		w("%s", comment("  url: "+scalar(d.RepoURL), "what the clients use; needed to publish"))
	} else {
		w("%s", comment("  # url: https://repo.example.org/"+d.Name, "what the clients use; needed to publish"))
	}
	w("")
	w("%s", comment("# store:", "needed to publish"))
	w("%s", comment("#   oci: ghcr.io/ORG/REPO", "one OCI package SET-TARGET per target"))
	if len(d.Keys) > 0 || d.ReleaseKeyfile != "" {
		w("")
		w("%s", comment("sign:", "trusted keys; \"repoctl sign add\" writes them"))
		if len(d.Keys) > 0 {
			w("  keys:")
			for _, k := range d.Keys {
				w("    - id: %s", scalar(k.ID))
				if k.File != "" {
					w("      file: %s", scalar(k.File))
				}
				w("      fingerprint: %s", scalar(k.Fingerprint))
				w("      state: %s", scalar(k.State))
			}
		}
		if d.ReleaseKeyfile != "" {
			w("  release:")
			w("%s", comment("    keyfile: "+scalar(d.ReleaseKeyfile), "key file in the release package"))
		}
	}
	w("")
	w("%s", comment("targets:", "one mock chroot each"))
	for _, t := range d.Targets {
		w("  - id: %s", scalar(t.ID))
		w("    mock: %s", scalar(t.Mock))
		plugins := t.Plugins
		if len(plugins) == 0 {
			plugins = []string{"rpmautospec"}
		}
		w("%s", comment("    plugins: "+flowList(plugins), "required: a rebuild commit needs it"))
		if len(t.Macros) > 0 {
			w("    macros: %s", flowList(t.Macros))
		}
		if t.Path != "" {
			w("%s", comment("    path: "+scalar(t.Path), "client path below repo.url"))
		}
	}
	w("")
	remote := d.Remote
	if remote == "" {
		remote = "origin"
	}
	w("defaults:")
	w("  git:")
	w("%s", comment("    remote: "+scalar(remote), "the package branches are in this repository"))
	w("")
	if len(d.Packages) == 0 {
		w("packages: []")
		w("# packages:")
		w("#   - id: munge")
		w("#     branch: munge/%s", d.Name)
		w("#     upstream: {github: dun/munge, fedora: munge}")
		return []byte(b.String())
	}
	w("packages:")
	for _, p := range d.Packages {
		w("  - id: %s", scalar(p.ID))
		if p.Git != nil && p.Git.URL != "" {
			w("    git: {url: %s}", scalar(p.Git.URL))
		}
		if p.Branch != "" {
			w("    branch: %s", scalar(p.Branch))
		}
		if p.Path != "" {
			w("    path: %s", scalar(p.Path))
		}
		if len(p.Targets) > 0 {
			w("    targets: %s", flowList(p.Targets))
		}
		if u := p.Upstream; u != nil {
			var parts []string
			for _, kv := range [][2]string{{"github", u.GitHub}, {"tags", u.Tags}, {"version", u.Version}, {"frozen", u.Frozen}, {"fedora", u.Fedora}} {
				if kv[1] != "" {
					parts = append(parts, kv[0]+": "+scalar(kv[1]))
				}
			}
			if len(parts) > 0 {
				w("    upstream: {%s}", strings.Join(parts, ", "))
			}
		}
		if p.Notes != "" {
			w("    notes: %s", scalar(p.Notes))
		}
		if p.Role != "" {
			w("    role: %s", scalar(p.Role))
		}
	}
	return []byte(b.String())
}
