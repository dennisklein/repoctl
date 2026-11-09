package cli

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/GSI-HPC/go-clikit/cobratree"
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// adminCommands: doctor, clean, mock, config, schema, version, completion
// (cli.html#group-admin).
func (a *App) adminCommands() []*cobra.Command {
	doctor := command("admin", "doctor", "Check this host and the set for build, sign and registry", api.KindDoctorReport, "read")
	doctor.Args = cobra.NoArgs
	doctor.Flags().Bool("strict", false, "exit 8 when a check gives a warning")
	doctor.Flags().String("only", "", "run only the checks of this group: host, set, sign, registry, check")
	doctor.RunE = a.run(hDoctor)

	clean := command("admin", "clean", "Delete files that repoctl can make again", api.KindClean, "staging")
	clean.Args = cobra.NoArgs
	clean.Flags().Bool("staging", false, "the staging of runs without staged nodes (default)")
	clean.Flags().Bool("logs", false, "journals and logs of finished runs beyond the retention")
	clean.Flags().Bool("cache", false, "SRPM, source, upstream and mock caches unused for 30d")
	clean.Flags().Bool("git", false, "bare clones without a worktree and without unpushed commits")
	clean.Flags().String("publish", "", "the signed tree and state file of a publish that stopped")
	clean.Flags().Bool("all", false, "all of the above with --older-than 0")
	clean.Flags().String("older-than", "", "only files older than D")
	clean.Flags().String("set", "", "all files of set NAME on this host")
	clean.Flags().Bool("dry-run", false, "print what clean deletes; delete nothing")
	clean.Flags().BoolP("yes", "y", false, "do not ask on a terminal")
	clean.Flags().Duration("lock-wait", 0, "wait this long for the set lock")
	clean.RunE = a.run(hClean)

	mock := command("admin", "mock", "Update, clean or open the mock roots of the targets", "", "staging")
	mupdate := sub("update", "Run mock --update and update the verify roots", "", "staging")
	mupdate.Args = cobra.NoArgs
	mupdate.Flags().Duration("lock-wait", 0, "wait this long for the set lock")
	mupdate.RunE = a.run(hMockUpdate)
	mclean := sub("clean", "Run mock --clean and --scrub=bootstrap on the roots of repoctl", "", "staging")
	mclean.Args = cobra.NoArgs
	mclean.Flags().Duration("lock-wait", 0, "wait this long for the set lock")
	mclean.RunE = a.run(hMockClean)
	mshell := sub("shell TARGET [-- COMMAND...]", "Open mock --shell in a root of one target", "", "staging")
	mshell.Args = cobra.MinimumNArgs(1)
	mshell.RunE = a.run(hMockShell)
	mock.AddCommand(mupdate, mclean, mshell)

	cfg := command("admin", "config", "Show the effective settings and the paths", "", "read")
	cshow := sub("show", "Print each setting, its value and the layer that gave it", api.KindConfig, "read")
	cshow.Args = cobra.NoArgs
	cshow.RunE = a.run(configShow)
	cpaths := sub("paths", "Print the files and directories of this set and user", api.KindPath, "read")
	cpaths.Args = cobra.NoArgs
	cpaths.RunE = a.run(configPaths)
	cfg.Annotations[annoNoTarget] = "true"
	cfg.AddCommand(cshow, cpaths)

	schema := command("admin", "schema [NAME]", "Print a JSON Schema of a file, the output or a kind", "", "read")
	schema.Args = cobra.MaximumNArgs(1)
	schema.Annotations[annoNoTarget] = "true"
	schema.Flags().Bool("list", false, "list the names")
	schema.ValidArgsFunction = func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return schemaNames(), cobra.ShellCompDirectiveNoFileComp
	}
	schema.RunE = a.run(schemaCmd)

	ver := command("admin", "version", "Print the version and the commit of the build", api.KindVersion, "read")
	ver.Args = cobra.NoArgs
	ver.Annotations[annoNoTarget] = "true"
	ver.RunE = a.run(versionCmd)

	completion := command("admin", "completion bash|zsh|fish|powershell", "Write the completion script to stdout", "", "read")
	completion.Annotations[annoNoTarget] = "true"
	completion.Args = cobra.ExactArgs(1)
	completion.ValidArgs = []string{"bash", "zsh", "fish", "powershell"}
	completion.RunE = func(cmd *cobra.Command, args []string) error {
		root := cmd.Root()
		switch args[0] {
		case "bash":
			return root.GenBashCompletionV2(a.Stdout, true)
		case "zsh":
			return root.GenZshCompletion(a.Stdout)
		case "fish":
			return root.GenFishCompletion(a.Stdout, true)
		case "powershell":
			return root.GenPowerShellCompletionWithDesc(a.Stdout)
		}
		return cobratree.InvalidValue("shell", args[0], "bash, zsh, fish or powershell")
	}

	return []*cobra.Command{doctor, clean, mock, cfg, schema, ver, completion}
}

// FileSchemas are the schemas of the files of a set and of the user, by
// name. The packages that own the files register them (config, lock,
// user-config, manifest).
var FileSchemas = map[string]func() []byte{}

func schemaNames() []string {
	names := []string{"output", "events", "deployments"}
	for n := range FileSchemas {
		names = append(names, n)
	}
	sort.Strings(names)
	// Then the kinds of the output (contract.html#kinds).
	return append(names, api.KindNames()...)
}

func schemaCmd(_ context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	if list, _ := cmd.Flags().GetBool("list"); list {
		return &Result{Raw: []byte(strings.Join(schemaNames(), "\n") + "\n")}, nil
	}
	name := "config"
	if len(args) == 1 {
		name = args[0]
	}
	switch name {
	case "output":
		return &Result{Raw: api.SchemaFile("output.schema.json")}, nil
	case "events":
		return &Result{Raw: api.SchemaFile("events.schema.json")}, nil
	case "deployments":
		b, _ := api.KindSchema("KeyDeployments")
		return &Result{Raw: b}, nil
	}
	if f, ok := FileSchemas[name]; ok {
		return &Result{Raw: f()}, nil
	}
	if b, ok := api.KindSchema(name); ok {
		return &Result{Raw: b}, nil
	}
	return nil, usagef("unknown schema %q (run: repoctl schema --list)", name)
}

func versionCmd(_ context.Context, a *App, _ *cobra.Command, _ []string) (*Result, error) {
	v, c := version()
	item := api.Version{
		Version:         v,
		Commit:          c,
		Go:              runtime.Version(),
		Clikit:          clikitVersion(),
		APIVersions:     []string{api.APIVersion},
		ReadAPIVersions: api.ReadAPIVersions,
		Journal:         api.JournalVersion,
		EventLog:        1,
	}
	return &Result{Kind: api.KindVersion, Items: []any{item}, Human: func(o *Out) error {
		o.Printf("%s\n", versionString())
		return nil
	}}, nil
}

func clikitVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, d := range bi.Deps {
		if d.Path == "github.com/GSI-HPC/go-clikit" {
			return strings.TrimPrefix(d.Version, "v")
		}
	}
	return ""
}

func configShow(_ context.Context, a *App, _ *cobra.Command, _ []string) (*Result, error) {
	eff, err := a.Settings()
	if err != nil {
		return nil, err
	}
	if eff.Path != "" {
		if s, _, _ := a.MaybeSet(); s != nil {
			known := map[string]bool{s.Name: true}
			for _, n := range eff.SetNames {
				if !known[n] && !a.knownSet(n) {
					a.Warn(api.Problem{Code: api.PConfigUnknownSet, Message: fmt.Sprintf("the user config has sets.%s, but this host knows no set %s", n, n), Path: eff.Path})
				}
			}
		}
	}
	var items []any
	t := NewTable("KEY", "VALUE", "SOURCE")
	for _, v := range eff.All() {
		item := api.Config{Key: v.Name, Source: layerName(v.Layer)}
		s, _ := config.SettingByName(v.Name)
		switch {
		case s.Type == "list":
			item.Value = append([]string{}, v.List...)
		case v.Value == "" && s.Type == "string":
			item.Value = nil
		default:
			item.Value = typedValue(s.Type, v.Value)
		}
		switch v.Layer {
		case 2:
			item.Env = strings.TrimPrefix(v.Source, "env ")
		case 3:
			item.File = eff.Path
		}
		items = append(items, item)
		val := v.Value
		if s.Type == "list" {
			val = strings.Join(v.List, " ")
		}
		if val == "" {
			val = "-"
		}
		t.Row(v.Name, val, v.Source)
	}
	return &Result{Kind: api.KindConfig, Items: items, Human: func(o *Out) error { return o.Table(t) }}, nil
}

// knownSet reports whether this host has state of the set.
func (a *App) knownSet(name string) bool {
	_, err := os.Stat(a.paths.SetState(name))
	return err == nil
}

func layerName(l int) string {
	switch l {
	case 1:
		return "flag"
	case 2:
		return "env"
	case 3:
		return "user"
	case 4:
		return "set"
	}
	return "default"
}

func typedValue(typ, v string) any {
	switch typ {
	case "int":
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	case "bool":
		return v == "true"
	}
	return v
}

func configPaths(_ context.Context, a *App, _ *cobra.Command, _ []string) (*Result, error) {
	p := a.paths
	type entry struct{ name, path string }
	list := []entry{
		{"user config", a.UserConfigPath()},
		{"cache", p.CacheHome},
		{"git clones", p.GitClones()},
		{"runtime", p.Runtime()},
	}
	set, _, err := a.MaybeSet()
	if err != nil {
		return nil, err
	}
	if set != nil {
		list = append(list,
			entry{"data", p.SetData(set.Name)},
			entry{"state", p.SetState(set.Name)},
			entry{"runs", p.Runs(set.Name)},
			entry{"set lock", p.SetLock(set.Name)},
		)
		targets, terr := a.Targets(set, false)
		if terr != nil {
			return nil, terr
		}
		for _, t := range targets {
			list = append(list, entry{"local copy " + t, p.LocalCopy(set.Name, t)})
		}
	}
	var items []any
	t := NewTable("NAME", "PATH", "SIZE")
	for _, e := range list {
		it := api.Path{Name: e.name, Path: e.path}
		size := "-"
		if fi, err := os.Stat(e.path); err == nil {
			it.Exists = true
			if fi.IsDir() {
				it.Bytes = int(dirSize(e.path))
			} else {
				it.Bytes = int(fi.Size())
			}
			size = HumanBytes(int64(it.Bytes))
		}
		items = append(items, it)
		t.Row(e.name, e.path, size)
	}
	return &Result{Kind: api.KindPath, Items: items, Human: func(o *Out) error { return o.Table(t) }}, nil
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// HumanBytes writes a size in KiB, MiB or GiB with one decimal.
func HumanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KiB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MiB", float64(n)/(k*k))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(k*k*k))
}
