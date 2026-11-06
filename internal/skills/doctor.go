package skills

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
)

// The doctor checks of this package (cli.html#cmd-doctor). They belong to
// the group host.
const (
	CheckAgentCommand    = "agent.command"
	CheckGuard           = "skills.guard"
	CheckVersionMismatch = "skills.version-mismatch"
)

// DoctorEnv is what the doctor checks of the agents read.
type DoctorEnv struct {
	// Home is the home directory of the human.
	Home string
	// SetDir is the set directory, or "" outside a set.
	SetDir string
	// SystemDir is the directory of the skills of the RPM; "" skips it.
	SystemDir string
	// Version is the version of the binary.
	Version string
	// AgentCommand is agent.command of the user config.
	AgentCommand string
	// LookPath finds a program; nil is exec.LookPath.
	LookPath func(string) (string, error)
	// RunVersion runs PROGRAM --version and gives its output; nil runs it
	// with a time limit of 10 s.
	RunVersion func(ctx context.Context, program string) (string, error)
}

// DoctorChecks gives the checks agent.command, skills.guard and
// skills.version-mismatch, for the registry of repoctl doctor.
func DoctorChecks(ctx context.Context, env DoctorEnv) []api.DoctorReportCheck {
	return []api.DoctorReportCheck{
		AgentCommandCheck(ctx, env),
		GuardCheck(env),
		VersionMismatchCheck(env),
	}
}

func check(name string, status api.CheckStatus, detail, remediation string) api.DoctorReportCheck {
	return api.DoctorReportCheck{Name: name, Group: "host", Status: status, Detail: detail, Remediation: remediation}
}

// AgentCommandCheck checks agent.command of the user config: the program is
// on the PATH, and PROGRAM --version works (agents.html#no-agent). Without
// an agent, the agent buttons of the web UI are off, which is no warning.
func AgentCommandCheck(ctx context.Context, env DoctorEnv) api.DoctorReportCheck {
	cmd := strings.TrimSpace(env.AgentCommand)
	if cmd == "" {
		return check(CheckAgentCommand, api.CheckStatusOk, "no agent in the user config: the agent buttons of the web UI are off", "")
	}
	look := env.LookPath
	if look == nil {
		look = exec.LookPath
	}
	path, err := look(cmd)
	if err != nil {
		return check(CheckAgentCommand, api.CheckStatusWarning,
			fmt.Sprintf("agent buttons are off: %s not found", cmd),
			"install the agent, or correct agent.command in the user config. run: repoctl config show")
	}
	runVersion := env.RunVersion
	if runVersion == nil {
		runVersion = func(ctx context.Context, program string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, program, "--version").Output()
			return string(out), err
		}
	}
	out, err := runVersion(ctx, path)
	if err != nil {
		return check(CheckAgentCommand, api.CheckStatusWarning,
			fmt.Sprintf("agent buttons are off: %s --version fails: %v", cmd, err),
			"repair the agent, or correct agent.command in the user config")
	}
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return check(CheckAgentCommand, api.CheckStatusOk, strings.TrimSpace(cmd+" "+first), "")
}

// GuardCheck checks that a guard hook covers the set directory when a skill
// of repoctl is installed, and that each settings file with the guard has
// the deny list of this binary (agents.html#guard).
func GuardCheck(env DoctorEnv) api.DoctorReportCheck {
	user := UserPlace(env.Home)
	places := []Place{user}
	if env.SetDir != "" {
		places = append(places, Place{Scope: ScopeProject, Root: filepath.Join(env.SetDir, ".claude", "skills"),
			Settings: filepath.Join(env.SetDir, ".claude", "settings.json"), SetDir: env.SetDir})
	}
	var installed []string // the scopes with an install
	guarded := false
	var differ []string
	for _, p := range places {
		if m, err := ReadManifest(p.Root); err == nil && len(m.Skills) > 0 {
			installed = append(installed, p.Scope)
		}
		for _, file := range settingsFiles(p) {
			b, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			st, err := ParseSettings(b)
			if err != nil {
				continue
			}
			if !st.HasGuard() {
				continue
			}
			guarded = true
			if miss := st.MissingDeny(); len(miss) > 0 {
				differ = append(differ, fmt.Sprintf("%s lacks %d deny rules (%s)", file, len(miss), miss[0]))
			}
		}
	}
	scope := "--user"
	if len(installed) > 0 && installed[len(installed)-1] == ScopeProject {
		scope = "--project"
	}
	switch {
	case len(installed) == 0:
		return check(CheckGuard, api.CheckStatusOk, "no skill of repoctl installed", "")
	case !guarded:
		where := "this host"
		if env.SetDir != "" {
			where = env.SetDir
		}
		return check(CheckGuard, api.CheckStatusWarning,
			fmt.Sprintf("skills of repoctl are installed (%s), but no guard hook covers %s", strings.Join(installed, ", "), where),
			"install the deny list and the guard hook. run: repoctl skills install "+scope)
	case len(differ) > 0:
		return check(CheckGuard, api.CheckStatusWarning, "the deny list differs from this binary: "+strings.Join(differ, "; "),
			"merge the deny list of this binary. run: repoctl skills install "+scope)
	}
	return check(CheckGuard, api.CheckStatusOk, "the guard hook and the deny list cover the skills of repoctl", "")
}

// settingsFiles gives the settings files of Claude Code that apply to a
// place: settings.json and settings.local.json.
func settingsFiles(p Place) []string {
	dir := filepath.Dir(p.Settings)
	return []string{p.Settings, filepath.Join(dir, "settings.local.json")}
}

// VersionMismatchCheck warns about each installed copy of a generic skill
// with another version than the binary, older or newer
// (agents.html#frontmatter).
func VersionMismatchCheck(env DoctorEnv) api.DoctorReportCheck {
	copies := Copies(env.Home, env.SetDir, env.SystemDir, env.Version)
	var bad []string
	scopes := map[string]bool{}
	for _, c := range copies {
		if c.State == "older" || c.State == "newer" {
			v := c.Version
			if v == "" {
				v = "no version"
			}
			bad = append(bad, fmt.Sprintf("%s %s (%s)", c.Path, v, c.State))
			scopes[c.Scope] = true
		}
	}
	if len(bad) == 0 {
		if len(copies) == 0 {
			return check(CheckVersionMismatch, api.CheckStatusOk, "no installed copy of a skill of repoctl", "")
		}
		return check(CheckVersionMismatch, api.CheckStatusOk, fmt.Sprintf("%d installed copies have version %s", len(copies), env.Version), "")
	}
	fix := "update the plugin or the RPM"
	switch {
	case scopes[ScopeUser]:
		fix = "install the skills of this binary. run: repoctl skills install --user"
	case scopes[ScopeProject]:
		fix = "install the skills of this binary. run: repoctl skills install --project"
	}
	return check(CheckVersionMismatch, api.CheckStatusWarning,
		fmt.Sprintf("%d copies differ from repoctl %s: %s", len(bad), env.Version, strings.Join(bad, ", ")), fix)
}
