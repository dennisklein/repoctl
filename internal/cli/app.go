// Package cli is the command line of repoctl: the command tree, the flags,
// the help topics and the terminal output (docs/design/cli.html,
// docs/design/terminal.html).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/GSI-HPC/go-clikit/cobratree"
	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/plan"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// App is one invocation of repoctl: its streams, its environment, its
// global flags and what it loaded.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	Now    func() time.Time

	// Global flags.
	dir         string
	configPath  string
	targetArgs  []string
	jsonOut     bool
	progress    string
	progressLog string
	// progressEnvBad says that REPOCTL_PROGRESS has a value that repoctl
	// cannot use; the frame wrote its note.
	progressEnvBad bool
	quiet          bool
	verbose        int
	noColor        bool

	// Per-command flags that map to settings (jobs, lock-wait, listen).
	settingFlags map[string]string

	cmd   *cobra.Command
	paths config.Paths
	style Style

	setLoaded bool
	setDir    string
	set       *config.Set
	setErr    error
	eff       *config.Effective
	userFile  *config.UserFile

	// warnings collects the warnings of the command for the envelope and
	// the terminal.
	warnings []api.Problem
}

// NewApp gives an App on the process streams and environment.
func NewApp() *App {
	return &App{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, Now: time.Now}
}

// Main runs repoctl with the process arguments and gives the exit code.
func Main() int {
	a := NewApp()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Execute(ctx, os.Args[1:])
}

// Execute runs the command line and gives the exit code.
func (a *App) Execute(ctx context.Context, args []string) int {
	if a.Getenv == nil {
		a.Getenv = os.Getenv
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	a.paths = config.NewPaths(a.Getenv)
	root := a.newRoot()
	root.SetArgs(args)
	root.SetIn(a.Stdin)
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return api.ExitOK
	}
	if cobratree.IsUsage(err) || isUsage(err) {
		a.style = NewStyle(a.Stderr, a.Getenv, a.noColor)
		fmt.Fprintf(a.Stderr, "repoctl: error: %s\n", firstLine(err.Error()))
		var nft *plan.NotForTarget
		if errors.As(err, &nft) && len(nft.Targets) > 0 {
			fmt.Fprintf(a.Stderr, "%s\n", a.style.Dim("hint: build it on its own target. run: ")+a.style.Bold("repoctl "+a.cmdName()+" "+nft.Pkg+"@"+nft.Targets[0]))
		}
		return api.ExitUsage
	}
	// Errors that the command frame did not handle (PreRun).
	a.printError(err)
	return exitCode(err)
}

func (a *App) cmdName() string {
	if a.cmd == nil {
		return ""
	}
	return strings.TrimPrefix(a.cmd.CommandPath(), "repoctl ")
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func isUsage(err error) bool {
	var ue *plan.UsageError
	var nft *plan.NotForTarget
	var ae *api.Error
	if errors.As(err, &ae) && ae.ExitCode == api.ExitUsage {
		return true
	}
	return errors.As(err, &ue) || errors.As(err, &nft)
}

// usagef gives a usage error (exit 2).
func usagef(format string, args ...any) error {
	return cobratree.Usagef(format, args...)
}

func exitCode(err error) int {
	if err == nil {
		return api.ExitOK
	}
	if e, ok := api.AsError(err); ok {
		return e.ExitCode
	}
	if errors.Is(err, context.Canceled) {
		return api.ExitInterrupted
	}
	return cobratree.Status(err, api.ExitFailed)
}

// Paths gives the XDG paths.
func (a *App) Paths() config.Paths { return a.paths }

// Style gives the style of stderr.
func (a *App) Style() Style { return a.style }

// JSON reports --json.
func (a *App) JSON() bool { return a.jsonOut }

// Quiet reports -q.
func (a *App) Quiet() bool { return a.quiet }

// Verbose gives the count of -v.
func (a *App) Verbose() int { return a.verbose }

// TargetArgs gives the values of --target.
func (a *App) TargetArgs() []string { return a.targetArgs }

// Warn adds a warning to the result of the command.
func (a *App) Warn(p api.Problem) { a.warnings = append(a.warnings, p) }

// Set gives the set of the command: discovery from -C, REPOCTL_DIR or the
// current directory, then repoctl.yaml.
func (a *App) Set() (*config.Set, string, error) {
	if !a.setLoaded {
		a.setLoaded = true
		start := a.dir
		if start == "" {
			start = "."
		}
		dir, err := config.Discover(start)
		if config.IsNoSet(err) && a.cmd != nil {
			err = config.NoSet(start, a.cmdName())
		}
		if err != nil {
			a.setErr = err
		} else {
			a.setDir = dir
			a.set, a.setErr = config.LoadSet(dir)
		}
	}
	return a.set, a.setDir, a.setErr
}

// MaybeSet gives the set, or nil without an error when there is no set.
func (a *App) MaybeSet() (*config.Set, string, error) {
	s, d, err := a.Set()
	if err != nil && config.IsNoSet(err) {
		return nil, "", nil
	}
	return s, d, err
}

// Settings gives the effective user settings for the set of the command (or
// without a set).
func (a *App) Settings() (*config.Effective, error) {
	if a.eff != nil {
		return a.eff, nil
	}
	path, explicit := a.userConfigPath()
	uf, err := config.LoadUser(path, explicit)
	if err != nil {
		return nil, err
	}
	a.userFile = uf
	setName := ""
	if s, _, _ := a.MaybeSet(); s != nil {
		setName = s.Name
	}
	flags := map[string]string{}
	for k, v := range a.settingFlags {
		flags[k] = v
	}
	if a.progress != "" {
		flags["progress"] = a.progress
	}
	eff, err := config.Resolve(uf, setName, a.Getenv, flags)
	if err != nil {
		return nil, err
	}
	a.warnings = append(a.warnings, eff.Warnings...)
	a.eff = eff
	return eff, nil
}

// UserConfigPath gives the user config file and whether the human named it.
func (a *App) userConfigPath() (string, bool) {
	if a.cmd != nil && a.cmd.Flags().Changed("config") {
		return a.configPath, true
	}
	if v := a.Getenv("REPOCTL_CONFIG"); v != "" {
		return v, true
	}
	return a.paths.UserConfig(), false
}

// UserConfigPath gives the effective user config file ("" for none).
func (a *App) UserConfigPath() string {
	p, _ := a.userConfigPath()
	return p
}

// Targets gives the active targets of the set after --target.
func (a *App) Targets(set *config.Set, rejectRetired bool) ([]string, error) {
	return plan.FilterTargets(set, a.targetArgs, rejectRetired)
}

// Here gives the package whose worktree holds the current directory, or "".
func (a *App) Here(set *config.Set, setDir string) string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if a.dir != "" {
		wd, _ = filepath.Abs(a.dir)
	}
	rel, err := filepath.Rel(filepath.Join(setDir, "packages"), wd)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return ""
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	if _, ok := set.Package(first); ok {
		return first
	}
	return ""
}

// LockWait gives the effective --lock-wait.
func (a *App) LockWait() time.Duration {
	eff, err := a.Settings()
	if err != nil {
		return 0
	}
	return eff.Duration("lock_wait")
}

// SetLock takes the set lock of the set for the command.
func (a *App) SetLock(ctx context.Context, set *config.Set, shared bool) (*lock.SetLock, error) {
	// A lock wait that repoctl cannot use (REPOCTL_LOCK_WAIT=bogus) fails
	// the command and names the variable.
	eff, err := a.Settings()
	if err != nil {
		return nil, err
	}
	h := lock.Holder{Command: a.cmdName(), PID: os.Getpid(), Host: hostname(), User: username(a.Getenv), Started: a.Now()}
	return lock.Acquire(ctx, a.paths.SetLock(set.Name), h, eff.Duration("lock_wait"), shared, lock.What{
		Code: api.PSetBusy, Name: "set " + set.Name, Lock: "the set lock", Retry: a.retryCommand("--lock-wait 2h"),
	})
}

// retryCommand gives the command line of this command with its selectors
// and targets, and then extra: the run: part of a hint that runs the same
// command again.
func (a *App) retryCommand(extra string) string {
	parts := []string{"repoctl", a.cmdName()}
	if a.cmd != nil {
		parts = append(parts, a.cmd.Flags().Args()...)
		if a.cmd.Flags().Changed("target") {
			for _, t := range a.targetArgs {
				parts = append(parts, "--target", t)
			}
		}
	}
	if extra != "" {
		parts = append(parts, extra)
	}
	return strings.Join(parts, " ")
}

// Context gives the context of a command: with -vvv, each child command is
// written to stderr.
func (a *App) Context(ctx context.Context) context.Context {
	if a.verbose >= 3 {
		ctx = run.WithTrace(ctx, a.Stderr)
	}
	return ctx
}

// ProgressOptions gives the cliprogress options of the command from the
// flags, the variables and the streams.
func (a *App) ProgressOptions(program string) cliprogress.Options {
	mode := cliprogress.Setting{Flag: "--progress", Value: a.progress, Variable: "REPOCTL_PROGRESS", Env: a.Getenv("REPOCTL_PROGRESS")}
	mode.Given = a.cmd != nil && a.cmd.Flags().Changed("progress")
	logSet := cliprogress.Setting{Flag: "--progress-log", Value: a.progressLog, Variable: "REPOCTL_PROGRESS_LOG", Env: a.Getenv("REPOCTL_PROGRESS_LOG")}
	logSet.Given = a.cmd != nil && a.cmd.Flags().Changed("progress-log")
	onTerm := cliprogress.IsTerminal(a.Stderr)
	o := cliprogress.Options{
		Program:    program,
		Mode:       mode,
		Log:        logSet,
		Stderr:     a.Stderr,
		OnTerminal: onTerm,
		Dumb:       a.Getenv("TERM") == "dumb",
		IntoPipe:   cliprogress.IsPipe(a.Stdout),
		Size:       func() (int, int, error) { return cliprogress.TerminalSize(a.Stderr) },
		Foreground: func() bool { return cliprogress.InForeground(a.Stderr) },
		ASCII:      !cliprogress.UTF8Locale(a.Getenv),
		Now:        a.Now,
	}
	o.LogOptions.Program = "repoctl"
	o.LogOptions.Version = config.Version
	o.Theme = a.style.Theme()
	// --progress tree is the live tree that cliprogress calls tty.
	if o.Mode.Value == "tree" {
		o.Mode.Value = "tty"
	}
	if o.Mode.Env == "tree" {
		o.Mode.Env = "tty"
	}
	// auto draws the tree when standard error is a terminal, and plain
	// lines when it is not (terminal.html#streams). cliprogress shows
	// none in both cases where the tree does not fit, and none when
	// standard output is a pipe; repoctl asks only standard error.
	o.IntoPipe = false
	if a.progressEnvBad {
		o.Mode.Env = "none"
	}
	if !o.OnTerminal || o.Dumb {
		if o.Mode.Given && (o.Mode.Value == "" || o.Mode.Value == "auto") {
			o.Mode.Value = "plain"
		}
		if !o.Mode.Given && (o.Mode.Env == "" || o.Mode.Env == "auto") {
			o.Mode.Env = "plain"
		}
	}
	return o
}

func hostname() string {
	h, _ := os.Hostname()
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

func username(getenv func(string) string) string {
	if u := getenv("USER"); u != "" {
		return u
	}
	if u := getenv("LOGNAME"); u != "" {
		return u
	}
	return fmt.Sprint(os.Getuid())
}

// Hostname gives the short host name.
func Hostname() string { return hostname() }
