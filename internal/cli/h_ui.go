package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/ui"
)

// The handlers of web UI: ui.

func hUI(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	stop, _ := cmd.Flags().GetBool("stop")
	remove, _ := cmd.Flags().GetString("remove")
	noBrowser, _ := cmd.Flags().GetBool("no-browser")
	if stop && remove != "" {
		return nil, usagef("--stop and --remove do not go together")
	}
	eff, err := a.Settings()
	if err != nil {
		return nil, err
	}
	o := ui.LaunchOptions{
		Paths: a.Paths(), Version: config.Version, Listen: eff.String("ui.listen"),
		Open:   eff.Bool("ui.open") && !noBrowser,
		Stdout: a.Stdout, Stderr: a.Stderr, Getenv: a.Getenv,
	}
	if cliprogress.IsTerminal(a.Stdin) && cliprogress.IsTerminal(a.Stderr) {
		o.Ask = askYes(a.Stdin, a.Stderr)
	}
	switch {
	case stop:
		return nil, ui.Stop(o)
	case remove != "":
		return nil, ui.Remove(o, remove)
	}
	set, dir, err := a.MaybeSet()
	if err != nil {
		return nil, err
	}
	if set != nil {
		o.SetName, o.SetDir = set.Name, dir
	}
	agent := ui.AgentConfig{
		Command: eff.String("agent.command"), Args: eff.List("agent.args"),
		Timeout: eff.Duration("agent.timeout"), Keep: eff.Int("state.keep_agent"),
	}
	home, _ := os.UserHomeDir()
	if h := a.Getenv("HOME"); h != "" {
		home = h
	}
	o.Server = func() (*ui.Server, error) {
		return ui.New(ui.Options{
			Paths: a.Paths(), Source: ui.InProcess(a.inProcess), Version: config.Version, Home: home,
			User: username(a.Getenv), Host: Hostname(), Agent: agent, Now: a.Now, Log: a.Stderr,
		})
	}
	return nil, ui.Launch(ctx, o)
}

// inProcess runs a repoctl command line in this process for the pages of
// the UI: a new App, without stdin, and without the variables that change
// what a --json command prints.
func (a *App) inProcess(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	child := &App{Stdin: strings.NewReader(""), Stdout: stdout, Stderr: stderr, Getenv: ui.QuietEnv(a.Getenv), Now: a.Now}
	return child.Execute(ctx, args)
}

// askYes asks a question on the terminal; only y or yes is yes.
func askYes(in io.Reader, out io.Writer) func(string) bool {
	return func(q string) bool {
		fmt.Fprint(out, q)
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil {
			return false
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true
		}
		return false
	}
}

// UIDoctorChecks gives the checks of the web UI for repoctl doctor
// (host.stale-server and agent.command), for the doctor handler.
func UIDoctorChecks(a *App) []api.DoctorReportCheck {
	eff, err := a.Settings()
	if err != nil {
		eff = config.DefaultSettings()
	}
	agent := ui.AgentConfig{Command: eff.String("agent.command")}
	return ui.DoctorChecks(a.Paths(), config.Version, agent)
}
