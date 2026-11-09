package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/mcp"
	"github.com/dennisklein/repoctl/internal/skills"
)

// The handlers of agents: mcp and skills (agents.html).

// MCPRunner gives the runner of the MCP server: each tool call runs the
// command tree of repoctl in this process, on a fresh App whose streams are
// buffers and whose environment follows the environment rule of the
// server (mcp.ChildGetenv). The server serializes the calls.
func MCPRunner(getenv func(string) string, now func() time.Time) mcp.Runner {
	return mcp.RunnerFunc(func(ctx context.Context, args []string) ([]byte, []byte, int) {
		var out, errb bytes.Buffer
		child := &App{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Getenv: mcp.ChildGetenv(getenv), Now: now}
		exit := child.Execute(ctx, args)
		return out.Bytes(), errb.Bytes(), exit
	})
}

// CommandTree gives the command tree of repoctl, for the checks of the MCP
// server: the safety class of each command and its flags.
func CommandTree() *cobra.Command {
	a := &App{Getenv: func(string) string { return "" }}
	return a.newRoot()
}

// toolMode reads --tools.
func toolMode(cmd *cobra.Command, cat *mcp.Catalog) (mcp.Mode, error) {
	spec, _ := cmd.Flags().GetString("tools")
	m, err := mcp.ParseMode(spec, cat)
	if err != nil {
		return m, usagef("--tools: %v", err)
	}
	return m, nil
}

func hMCPStart(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	cat, err := mcp.Load()
	if err != nil {
		return nil, err
	}
	mode, err := toolMode(cmd, cat)
	if err != nil {
		return nil, err
	}
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	var configArg *string
	if p, explicit := a.userConfigPath(); explicit {
		configArg = &p
	}
	eff, err := a.Settings()
	if err != nil {
		return nil, err
	}
	exe, _ := os.Executable()
	v, commit := version()
	tree := CommandTree()
	srv, err := mcp.New(mcp.Options{
		Catalog: cat, Mode: mode, SetDir: setDir, SetName: set.Name,
		Run:        MCPRunner(a.Getenv, a.Now),
		Tree:       func() *cobra.Command { return tree },
		Executable: exe, Environ: os.Environ(), ConfigArg: configArg,
		Jobs:    func() int { return eff.Int("jobs") },
		Paths:   a.Paths(),
		Skill:   func(name string) ([]byte, bool) { return skillText(name, v) },
		Version: v, Commit: commit, Now: a.Now,
	})
	if err != nil {
		return nil, err
	}
	t := &sdk.IOTransport{Reader: mcpReader{a.Stdin}, Writer: mcpWriter{a.Stdout}}
	if err := srv.Run(ctx, t); err != nil {
		return nil, api.Errorf(api.PInternal, "MCP server: %v", err)
	}
	return &Result{Raw: []byte{}}, nil
}

type mcpReader struct{ io.Reader }

func (r mcpReader) Close() error {
	if c, ok := r.Reader.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

type mcpWriter struct{ io.Writer }

func (mcpWriter) Close() error { return nil }

func hMCPTools(_ context.Context, _ *App, cmd *cobra.Command, _ []string) (*Result, error) {
	cat, err := mcp.Load()
	if err != nil {
		return nil, err
	}
	mode, err := toolMode(cmd, cat)
	if err != nil {
		return nil, err
	}
	return &Result{Raw: cat.JSON(mode)}, nil
}

// skillText gives the SKILL.md of a generic skill as install writes it.
func skillText(name, version string) ([]byte, bool) {
	s, ok := skills.Lookup(name, version)
	if !ok {
		return nil, false
	}
	return s.Render(), true
}

// skillsHome gives the home directory of the human.
func (a *App) skillsHome() string {
	if h := a.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// allSkills gives the generic skills and the set-local skills of the set,
// with their installed copies.
func (a *App) allSkills() ([]api.Skill, []skills.Skill, error) {
	v, _ := version()
	set, setDir, err := a.MaybeSet()
	if err != nil {
		return nil, nil, err
	}
	list := skills.Builtin(v)
	if set != nil && len(set.Skills) > 0 {
		local, errs := skills.SetSkills(setDir, set.Skills)
		for _, e := range errs {
			a.Warn(api.Warning(api.PConfigInvalid, "set-local skill: %v", e))
		}
		for _, s := range local {
			if s.Conflict {
				a.Warn(api.Problem{
					Code: api.PConfigInvalid, Path: s.Dir,
					Message: fmt.Sprintf("set-local skill %s has the name of a generic skill; a set-local skill can only add", s.Name),
					Hint:    "Rename the set-local skill.",
				})
			}
		}
		list = append(list, local...)
	}
	copies := skills.Copies(a.skillsHome(), setDir, skills.SystemDir, v)
	var items []api.Skill
	for _, s := range list {
		item := s.APISkill()
		if s.Source == skills.SourceBuiltin {
			for _, c := range copies {
				if c.Name == s.Name {
					item.Installed = append(item.Installed, api.SkillInstalledItem{Scope: c.Scope, Path: c.Path, Version: c.Version, State: c.State})
				}
			}
		} else {
			item.Installed = []api.SkillInstalledItem{{Scope: skills.ScopeProject, Path: s.Dir, Version: s.Version, State: "same"}}
		}
		items = append(items, item)
	}
	return items, list, nil
}

func hSkillsLs(_ context.Context, a *App, _ *cobra.Command, _ []string) (*Result, error) {
	items, _, err := a.allSkills()
	if err != nil {
		return nil, err
	}
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it
	}
	return &Result{Kind: api.KindSkill, Items: out, Human: func(o *Out) error {
		t := NewTable("NAME", "SOURCE", "VERSION", "INSTALLED", "DESCRIPTION")
		t.Wide[4] = true
		for _, it := range items {
			var inst []string
			for _, c := range it.Installed {
				s := c.Scope
				if c.State != "" && c.State != "same" {
					s += " (" + c.State + ")"
				}
				inst = append(inst, s)
			}
			installed := strings.Join(inst, ", ")
			if installed == "" {
				installed = "-"
			}
			name := it.Name
			if it.Conflict {
				name += " (conflict)"
			}
			t.Row(name, it.Source, it.Version, installed, it.Description)
		}
		if err := o.Table(t); err != nil {
			return err
		}
		for _, it := range items {
			for _, c := range it.Installed {
				if c.State == "older" || c.State == "newer" || c.State == "changed" {
					o.Note("%s copy of %s in %s is %s", c.Scope, it.Name, c.Path, c.State)
				}
			}
		}
		return nil
	}}, nil
}

func hSkillsShow(_ context.Context, a *App, _ *cobra.Command, args []string) (*Result, error) {
	items, list, err := a.allSkills()
	if err != nil {
		return nil, err
	}
	for i, s := range list {
		if s.Name != args[0] || (s.Source == skills.SourceSet && s.Conflict) {
			continue
		}
		text := s.Text
		if s.Source == skills.SourceBuiltin {
			text = s.Render()
		}
		return &Result{Kind: api.KindSkill, Items: []any{items[i]}, Human: func(o *Out) error {
			_, err := o.Stdout.Write(text)
			return err
		}}, nil
	}
	return nil, usagef("unknown skill %q (run: repoctl skills ls)", args[0])
}

func hSkillsPath(_ context.Context, _ *App, _ *cobra.Command, _ []string) (*Result, error) {
	return &Result{Raw: []byte(skills.SystemDir + "\n")}, nil
}

func hSkillsGuard(_ context.Context, a *App, _ *cobra.Command, _ []string) (*Result, error) {
	in, err := io.ReadAll(a.Stdin)
	if err != nil {
		in = nil
	}
	out := skills.GuardHook(in, skills.GuardEnv{Home: a.skillsHome(), UserConfigDir: a.Paths().ConfigHome})
	if out == nil {
		out = []byte{}
	}
	return &Result{Raw: out}, nil
}

// skillsPlace gives the install place of --user or --project.
func skillsPlace(ctx context.Context, a *App, cmd *cobra.Command) (skills.Place, string, error) {
	user, _ := cmd.Flags().GetBool("user")
	project, _ := cmd.Flags().GetBool("project")
	if user && project {
		return skills.Place{}, "", usagef("--user and --project exclude each other")
	}
	if !project {
		return skills.UserPlace(a.skillsHome()), "", nil
	}
	_, setDir, err := a.Set()
	if err != nil {
		return skills.Place{}, "", err
	}
	p, err := skills.ProjectPlace(ctx, setDir)
	return p, setDir, err
}

// skillNames checks the names of the command line against the generic
// skills; none is each generic skill.
func skillNames(args []string) ([]string, error) {
	if len(args) == 0 {
		return skills.Names(), nil
	}
	known := map[string]bool{}
	for _, n := range skills.Names() {
		known[n] = true
	}
	var out []string
	for _, n := range args {
		if !known[n] {
			return nil, usagef("unknown skill %q; the generic skills are %s", n, strings.Join(skills.Names(), ", "))
		}
		if !hasSkillName(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

func hasSkillName(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func hSkillsInstall(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	names, err := skillNames(args)
	if err != nil {
		return nil, err
	}
	place, setDir, err := skillsPlace(ctx, a, cmd)
	if err != nil {
		return nil, err
	}
	v, _ := version()
	var list []skills.Skill
	for _, n := range names {
		s, _ := skills.Lookup(n, v)
		list = append(list, s)
	}
	setNames := map[string]bool{}
	if place.Scope == skills.ScopeProject {
		if set, _, _ := a.Set(); set != nil {
			local, _ := skills.SetSkills(setDir, set.Skills)
			for _, s := range local {
				if filepath.Dir(s.Dir) == place.Root {
					setNames[s.Name] = true
				}
			}
		}
	}
	res, err := skills.Install(skills.InstallOptions{Place: place, Skills: list, Version: v, SetNames: setNames})
	if err != nil {
		return nil, err
	}
	items := make([]any, len(res.Installed))
	for i, it := range res.Installed {
		items[i] = it
	}
	return &Result{Kind: api.KindSkill, Items: items, Human: func(o *Out) error {
		for _, it := range res.Installed {
			o.Printf("installed %s %s into %s\n", it.Name, it.Version, it.Installed[0].Path)
		}
		o.Note("%s", res.Describe(place))
		if len(res.Exclude) > 0 {
			o.Note("added %d paths to %s", len(res.Exclude), place.Exclude)
		}
		return nil
	}}, nil
}

func hSkillsUninstall(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	var names []string
	if len(args) > 0 {
		var err error
		if names, err = skillNames(args); err != nil {
			return nil, err
		}
	}
	place, _, err := skillsPlace(ctx, a, cmd)
	if err != nil {
		return nil, err
	}
	res, err := skills.Uninstall(place, names)
	if err != nil {
		return nil, err
	}
	sort.Slice(res.Removed, func(i, j int) bool { return res.Removed[i].Name < res.Removed[j].Name })
	items := make([]any, len(res.Removed))
	for i, it := range res.Removed {
		items[i] = it
	}
	return &Result{Kind: api.KindSkill, Items: items, Human: func(o *Out) error {
		if len(res.Removed) == 0 {
			o.Note("no skill of repoctl is installed in %s", place.Root)
			return nil
		}
		for _, it := range res.Removed {
			path := place.Root
			if len(it.Installed) > 0 {
				path = it.Installed[0].Path
			}
			o.Printf("removed %s from %s\n", it.Name, path)
		}
		if res.Settings {
			o.Note("removed the deny list and the guard hook of repoctl from %s", place.Settings)
		}
		return nil
	}}, nil
}
