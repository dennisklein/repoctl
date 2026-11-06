package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/GSI-HPC/go-clikit/cobratree"
	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// Annotations of the commands.
const (
	annoKind     = "kind"     // the --json kind
	annoClass    = "class"    // the safety class
	annoNoTarget = "noTarget" // --target is a usage error
	annoNoJSON   = "noJSON"   // --json is a usage error
)

const rootLong = `repoctl builds signed RPM repositories from a package set.

A package set is a git directory with repoctl.yaml and repoctl.lock.
repoctl gets the build order from the spec files and builds each new
commit in mock into a staging repository. Only a human publishes. A
publish signs the packages, pushes a snapshot to the registry and moves
the tag "latest". The snapshot in the registry is the truth.`

// groups of the help, in the order of a work day.
var groups = []*cobra.Group{
	{ID: "workspace", Title: "Workspace commands:"},
	{ID: "build", Title: "Build commands:"},
	{ID: "repository", Title: "Repository commands:"},
	{ID: "sign", Title: "Sign commands:"},
	{ID: "check", Title: "Check commands:"},
	{ID: "upstream", Title: "Upstream commands:"},
	{ID: "agent", Title: "Agent and UI commands:"},
	{ID: "admin", Title: "Admin commands:"},
}

func (a *App) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "repoctl",
		Short:         "Build signed RPM repositories from a package set",
		Long:          rootLong,
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       versionString(),
	}
	root.SetVersionTemplate("{{.Version}}\n")
	cobra.EnableCommandSorting = false
	root.SetUsageTemplate(rootUsage)
	root.AddGroup(groups...)
	pf := root.PersistentFlags()
	pf.SortFlags = false
	root.Flags().SortFlags = false
	pf.StringVarP(&a.dir, "dir", "C", "", "start in this directory (default: REPOCTL_DIR, else .)")
	pf.StringVar(&a.configPath, "config", "", "user config file (default: REPOCTL_CONFIG, else\n~/.config/repoctl/config.yaml)")
	pf.StringSliceVar(&a.targetArgs, "target", nil, "use only these target ids; repeat or separate with\ncommas (default: REPOCTL_TARGET, else all)")
	pf.BoolVar(&a.jsonOut, "json", false, "write the result as JSON (default: REPOCTL_JSON)")
	pf.StringVar(&a.progress, "progress", "", "progress display: auto, tree, counter, plain, none\n(default: REPOCTL_PROGRESS, else auto)")
	pf.StringVar(&a.progressLog, "progress-log", "", "append the progress events as JSONL to this file\n(default: REPOCTL_PROGRESS_LOG)")
	pf.BoolVarP(&a.quiet, "quiet", "q", false, "write no notes and no summary, only errors")
	pf.CountVarP(&a.verbose, "verbose", "v", "show all rows and log more: -v info, -vv debug,\n-vvv every command that repoctl runs\n(default: REPOCTL_VERBOSE, else 0)")
	pf.BoolVar(&a.noColor, "no-color", false, "draw without colour (default: NO_COLOR)")
	root.PersistentPreRunE = a.preRun

	for _, c := range a.commands() {
		root.AddCommand(c)
	}
	cobratree.Strict(root, cobratree.Topics(topicText, topicList))
	if h, _, err := root.Find([]string{"help"}); err == nil && h != nil {
		h.Short = "Help about a command or a topic"
		h.GroupID = ""
		// The topics graph and advisories share their names with commands;
		// "repoctl help NAME" prints the topic, "repoctl NAME --help" the
		// command (cli.html#topics).
		run, runE := h.Run, h.RunE
		h.Run = nil
		h.RunE = func(c *cobra.Command, args []string) error {
			if len(args) == 1 {
				if text, ok := topicText(args[0]); ok {
					_, err := fmt.Fprint(c.OutOrStdout(), text)
					return err
				}
			}
			if runE != nil {
				return runE(c, args)
			}
			run(c, args)
			return nil
		}
	}
	return root
}

// rootUsage is the usage template of cobra with the help topics after the
// command groups (cli.html#help). Subcommands use the template of cobra.
var rootUsage = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if not .HasParent}}

` + topicsHelp() + `{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}

Use "{{.CommandPath}} [command] --help" for more information about a command.
`

func (a *App) commands() []*cobra.Command {
	var out []*cobra.Command
	out = append(out, a.workspaceCommands()...)
	out = append(out, a.buildCommands()...)
	out = append(out, a.repoCommands()...)
	out = append(out, a.signCommands()...)
	out = append(out, a.checkCommands()...)
	out = append(out, a.agentCommands()...)
	out = append(out, a.adminCommands()...)
	return out
}

// preRun reads the variables that stand in for global flags and checks the
// flags that a command cannot use.
func (a *App) preRun(cmd *cobra.Command, _ []string) error {
	a.cmd = cmd
	fl := cmd.Flags()
	if !fl.Changed("dir") {
		if v := a.Getenv("REPOCTL_DIR"); v != "" {
			a.dir = v
		}
	}
	if !fl.Changed("target") {
		if v := a.Getenv("REPOCTL_TARGET"); v != "" {
			a.targetArgs = strings.Split(v, ",")
		}
	} else if inherits(cmd, annoNoTarget) {
		return usagef("repoctl %s does not take --target", strings.TrimPrefix(cmd.CommandPath(), "repoctl "))
	}
	if !fl.Changed("json") {
		if v := strings.ToLower(a.Getenv("REPOCTL_JSON")); v == "1" || v == "true" {
			a.jsonOut = cmd.Annotations[annoNoJSON] != "true"
		}
	} else if cmd.Annotations[annoNoJSON] == "true" && a.jsonOut {
		return usagef("repoctl %s has no --json", strings.TrimPrefix(cmd.CommandPath(), "repoctl "))
	}
	if !fl.Changed("quiet") && a.Getenv("REPOCTL_QUIET") == "1" {
		a.quiet = true
	}
	// REPOCTL_PROGRESS, REPOCTL_PROGRESS_LOG and REPOCTL_VERBOSE never fail
	// a command: one note, and repoctl continues without them
	// (config.html#environment-variables).
	var notes []string
	if !fl.Changed("verbose") {
		if v := a.Getenv("REPOCTL_VERBOSE"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				a.verbose = n
			} else {
				notes = append(notes, fmt.Sprintf("REPOCTL_VERBOSE is %q; it takes a number such as 2; it is ignored", v))
			}
		}
	}
	if fl.Changed("progress") {
		switch a.progress {
		case "auto", "plain", "none":
		case "tree", "counter":
			if !cliprogress.IsTerminal(a.Stderr) || a.Getenv("TERM") == "dumb" {
				what := "a live tree"
				if a.progress == "counter" {
					what = "a counter"
				}
				return usagef("--progress %s asks for %s, but standard error is not a terminal; use plain or none, or auto to draw one only where it can", a.progress, what)
			}
		default:
			return cobratree.InvalidValue("progress", a.progress, "auto, tree, counter, plain or none")
		}
	} else if v := a.Getenv("REPOCTL_PROGRESS"); v != "" && !slices.Contains([]string{"auto", "tree", "counter", "plain", "none"}, v) {
		notes = append(notes, fmt.Sprintf("REPOCTL_PROGRESS is %q; it takes auto, tree, counter, plain or none; no progress is shown", v))
		a.progressEnvBad = true
	}
	// Flags of the command that stand in for settings.
	a.settingFlags = map[string]string{}
	for key, flag := range map[string]string{"jobs": "jobs", "lock_wait": "lock-wait", "ui.listen": "listen"} {
		if f := fl.Lookup(flag); f != nil && f.Changed {
			a.settingFlags[key] = f.Value.String()
		}
	}
	a.style = NewStyle(a.Stderr, a.Getenv, a.noColor)
	for _, n := range notes {
		a.out().Note("%s", n)
	}
	return nil
}

func inherits(cmd *cobra.Command, anno string) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Annotations[anno] == "true" {
			return true
		}
	}
	return false
}

func versionString() string {
	v, c := version()
	if c != "" {
		return fmt.Sprintf("repoctl %s (%s)", v, c)
	}
	return "repoctl " + v
}

func version() (string, string) {
	bi := cobratree.ReadBuildInfo().Resolve(config.Version, config.Commit)
	commit := bi.Short(7)
	if bi.Version != "" {
		return strings.TrimPrefix(bi.Version, "v"), commit
	}
	return config.Version, commit
}

// command is a helper to make a command of a group.
func command(group, use, short string, kind api.Kind, class string) *cobra.Command {
	c := &cobra.Command{Use: use, Short: short, GroupID: group, Annotations: map[string]string{annoClass: class}}
	if kind != "" {
		c.Annotations[annoKind] = string(kind)
	} else {
		c.Annotations[annoNoJSON] = "true"
	}
	return c
}

// sub is a helper to make a subcommand (no group).
func sub(use, short string, kind api.Kind, class string) *cobra.Command {
	c := command("", use, short, kind, class)
	return c
}

// commandName gives the command path without "repoctl ".
func commandName(cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), "repoctl ")
}
