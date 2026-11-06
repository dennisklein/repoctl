package cli

import (
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
)

const buildLong = `Build packages into the staging repository of a new run.

repoctl reads repoctl.lock, makes the graph of each target and builds
each node whose commit is not in latest or in a staging. A node starts
when its dependencies are done. The published repository does not
change. Run "repoctl publish" to sign and publish the result.`

const buildExamples = `  # Build what changed, on all targets
  repoctl build

  # Try the commits of the worktrees before you pin them
  repoctl build ucx --from worktree`

// buildCommands: graph, plan, build, runs, logs, cancel (cli.html#group-build).
func (a *App) buildCommands() []*cobra.Command {
	graph := command("build", "graph [SELECTOR...]", "Show the dependency graph of each target", api.KindGraph, "read")
	graph.Flags().Bool("with-deps", false, "also select the dependencies")
	graph.Flags().Bool("rdeps", false, "also select the dependents")
	graph.Flags().String("run", "", "show the node states of the run in place of the plan actions")
	graph.Flags().String("format", "text", "the form: text, matrix, dot, json")
	graph.RunE = a.run(hGraph)

	plan := command("build", "plan [SELECTOR...]", "Show what a build does, and why", api.KindPlan, "read")
	plan.Flags().Bool("with-deps", false, "also select the dependencies (only with --force)")
	plan.Flags().Bool("rdeps", false, "also select the dependents (only with --force)")
	plan.Flags().Bool("force", false, "plan the selected nodes, also when they are current")
	plan.Flags().String("from", "lock", "take the package commits from: lock, worktree")
	plan.Flags().Bool("check", false, "exit 11 when a node has the action build")
	plan.RunE = a.run(hPlan)

	build := command("build", "build [SELECTOR...]", "Build packages into the staging repository of a new run", api.KindRun, "staging")
	build.Long = buildLong
	build.Example = buildExamples
	build.Flags().Bool("fail-fast", false, "start no new node after the first failure")
	build.Flags().Bool("force", false, "build the selected nodes, also when they are current\n(for tests; publish refuses a reused NVR)")
	build.Flags().String("from", "lock", "take the package commits from: lock, worktree")
	build.Flags().IntP("jobs", "j", 0, "build at most this many nodes at the same time\n(default: REPOCTL_JOBS, else jobs in the user config,\nelse 2)")
	build.Flags().Duration("lock-wait", 0, "wait this long for the set lock\n(default: REPOCTL_LOCK_WAIT, else 0: do not wait)")
	build.Flags().Bool("rdeps", false, "also select the dependents (only with --force)")
	build.Flags().Bool("with-deps", false, "also select the dependencies (only with --force)")
	// The origin of a run that the web UI or the MCP server starts
	// (contract.html#journal, run.started origin).
	build.Flags().String("run-id", "", "use this run id instead of a new one")
	build.Flags().String("origin", "", "the origin of the run: cli, ui, mcp, ci\n(default: REPOCTL_ORIGIN, else ci when CI=true, else cli)")
	build.Flags().String("origin-session", "", "the session id of the UI or MCP server\n(default: REPOCTL_ORIGIN_SESSION)")
	build.Flags().String("origin-client", "", "the MCP client of the run")
	for _, f := range []string{"run-id", "origin", "origin-session", "origin-client"} {
		_ = build.Flags().MarkHidden(f)
	}
	build.RunE = a.run(hBuild)

	runs := command("build", "runs [RUN]", "List the runs, or show one run", api.KindRun, "read")
	runs.Args = cobra.MaximumNArgs(1)
	runs.Flags().Int("limit", 20, "at most N runs")
	runs.Flags().String("state", "", "only runs in this state")
	runs.Flags().String("kind", "", "only runs of this kind: build, publish, check, gc")
	runs.Flags().Bool("follow", false, "tail the journal of a running run")
	runs.RunE = a.run(hRuns)

	logs := command("build", "logs [RUN] [NODE]", "Show the build log of a node", api.KindNodeLog, "read")
	logs.Args = cobra.MaximumNArgs(2)
	logs.Flags().String("file", "repoctl", "the file: repoctl, root, build, state")
	logs.Flags().String("step", "", "only the lines of this step: srpm, rpm, stage")
	logs.Flags().BoolP("follow", "f", false, "wait for new lines until the node ends")
	logs.RunE = a.run(hLogs)

	cancel := command("build", "cancel [RUN]", "Stop a running build", api.KindRun, "staging")
	cancel.Args = cobra.MaximumNArgs(1)
	cancel.Flags().Bool("wait", false, "wait for the end of the run")
	cancel.Flags().Bool("force", false, "stop the mock roots at once; the run ends abandoned")
	cancel.RunE = a.run(hCancel)

	return []*cobra.Command{graph, plan, build, runs, logs, cancel}
}
