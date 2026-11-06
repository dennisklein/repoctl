package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/GSI-HPC/go-clikit/cobratree"
	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/GSI-HPC/go-clikit/progress/display"
	"github.com/GSI-HPC/go-clikit/progress/eventlog"
	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/advisory"
	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/build"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/doctor"
	"github.com/dennisklein/repoctl/internal/journal"
	"github.com/dennisklein/repoctl/internal/localcopy"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/plan"
	"github.com/dennisklein/repoctl/internal/publish"
	"github.com/dennisklein/repoctl/internal/sign"
	"github.com/dennisklein/repoctl/internal/snapshot"
	"github.com/dennisklein/repoctl/internal/staging"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/tools/run"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// The handlers of build runs: build, runs, logs, cancel, clean, status and
// mock (cli.html#group-build, #cmd-clean, #cmd-mock, #cmd-status).

// buildEngine gives the plan and step srpm of one run of repoctl build:
// plan.Compute and the SRPM host (build.Engine).
var buildEngine = func() (build.Planner, build.SRPMBuilder) {
	e := &build.Engine{}
	return e, e
}

// StagedOf of the plan commands reads the staged index of the set
// (build.html#staging).
func init() {
	StagedOf = func(a *App, set *config.Set) plan.Staged {
		idx, err := staging.Load(a.Paths(), set.Name, hostname())
		if err != nil {
			return plan.NoStaged{}
		}
		return idx
	}
}

func hBuild(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	// Usage errors first: a --progress that cannot draw stops before the
	// set lock and the lock check.
	if _, _, err := cliprogress.Choose(rbProgressOptions(a)); err != nil {
		return nil, usagef("%s", err.Error())
	}
	fl := cmd.Flags()
	failFast, _ := fl.GetBool("fail-fast")
	force, _ := fl.GetBool("force")
	fromFlag, _ := fl.GetString("from")
	withDeps, _ := fl.GetBool("with-deps")
	rdeps, _ := fl.GetBool("rdeps")
	from := workspace.From(fromFlag)
	if from != workspace.FromLock && from != workspace.FromWorktree {
		return nil, usagef("--from %s: use lock or worktree", fromFlag)
	}
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	targets, err := a.Targets(set, true)
	if err != nil {
		return nil, err
	}
	sels, err := plan.ParseSelectors(args)
	if err != nil {
		return nil, err
	}
	sel, err := plan.Select(set, sels, targets, a.Here(set, setDir))
	if err != nil {
		return nil, err
	}
	eff, err := a.Settings()
	if err != nil {
		return nil, err
	}
	p := a.Paths()
	host, user := hostname(), username(a.Getenv)
	runID, origin, err := rbOrigin(a, cmd, p.Runs(set.Name), user, host)
	if err != nil {
		return nil, err
	}

	// Before run.started: the set lock, the lock check and the preflight.
	sl, err := a.SetLock(ctx, set, false)
	if err != nil {
		return nil, err
	}
	defer sl.Release()
	lf, err := lock.Read(setDir)
	if err != nil {
		return nil, err
	}
	if from == workspace.FromLock {
		if err := workspace.CheckClean(ctx, p, set, setDir, lf, sel.Packages()); err != nil {
			return nil, err
		}
	}
	if err := a.Preflight(ctx, doctor.NeedBuild, sel.Targets, 0); err != nil {
		return nil, err
	}
	hml, err := rbHostMockLock(ctx, a, true)
	if err != nil {
		if _, ok := api.AsError(err); ok {
			return nil, err
		}
	} else {
		defer hml.Release()
	}
	staged := rbRetain(a, set, eff)

	if runID == "" {
		runID = journal.NewID(journal.KindBuild, a.Now(), p.Runs(set.Name))
	}
	if err := os.MkdirAll(p.Runs(set.Name), 0o700); err != nil {
		return nil, err
	}
	po := rbProgressOptions(a)
	po.DefaultLog = p.ProgressLog(set.Name, runID)
	po.LogOptions.RunID = runID
	po.Trace = func() progress.TraceContext {
		tc, _ := progress.ParseTraceContext(a.Getenv("TRACEPARENT"), a.Getenv("TRACESTATE"))
		return tc
	}
	sess, err := cliprogress.Start(ctx, po)
	if err != nil {
		return nil, usagef("%s", err.Error())
	}
	cctx, cspan := progress.Start(sess.Context(), progress.KindCommand, "build")
	errOut := sess.Writer(a.Stderr)

	cancels := make(chan journal.CancelRequest, 4)
	stopSignals := rbSignals(a, set, runID, origin, cancels)
	defer stopSignals()

	setCommit, _ := git.New(setDir).RevParse(ctx, "HEAD")
	o := a.out()
	o.Stderr = errOut
	opts := build.Options{
		Set: set, SetDir: setDir, Paths: p, Lock: lf, SetLock: sl,
		LockWhat: lock.What{Code: api.PSetBusy, Name: "set " + set.Name, Lock: "the set lock"},
		RunID:    runID, Origin: origin, Selection: sel, Selectors: args, Parsed: sels, Here: a.Here(set, setDir), From: from,
		Force: force, WithDeps: withDeps, Rdeps: rdeps, FailFast: failFast, Jobs: eff.Int("jobs"),
		Timeouts: build.Timeouts{SRPM: eff.Duration("build.timeout.srpm"), RPM: eff.Duration("build.timeout.rpm"),
			Stage: eff.Duration("build.timeout.stage"), Idle: eff.Duration("build.timeout.idle"), Run: eff.Duration("build.timeout.run")},
		SetCommit: setCommit, Version: config.Version, Staged: staged,
		Cancel: cancels, Warn: a.Warn,
	}
	opts.Planner, opts.SRPM = buildEngine()
	opts.Fresh = func(ctx context.Context, targets []string) ([]api.Problem, error) {
		_, warns, err := rbFreshPlan(ctx, a, set, setDir, targets)
		return warns, err
	}
	opts.MockOptions.NoSync = eff.Bool("mock.nosync")
	opts.MockOptions.TmpFS = eff.String("mock.tmpfs")
	opts.Planned = func(r *api.Run, pl *build.Plan) {
		var parts []string
		parts = append(parts, fmt.Sprintf("%d to build", pl.Summary.Build))
		if pl.Summary.Current > 0 {
			parts = append(parts, fmt.Sprintf("%d current", pl.Summary.Current))
		}
		if pl.Summary.Staged > 0 {
			parts = append(parts, fmt.Sprintf("%d staged", pl.Summary.Staged))
		}
		o.Note("run %s: %s, %d at a time", runID, strings.Join(parts, ", "), r.Selection.Jobs)
	}
	start := a.Now()
	res, err := build.Run(cctx, opts)
	var spanEnd error
	if res != nil && res.Run != nil {
		switch res.Run.State {
		case api.RunStateCanceled:
			spanEnd = context.Canceled
		case api.RunStateFailed:
			spanEnd = errors.New("failed")
		}
	}
	if res != nil && res.Forced {
		spanEnd = context.Canceled
	}
	cspan.End(spanEnd)
	shown := sess.Mode() != cliprogress.ModeNone
	sess.Finish(false)
	if err != nil && (res == nil || res.Run == nil) {
		return nil, err
	}
	return rbBuildResult(a, res, err, shown, a.Now().Sub(start))
}

// rbFreshGate gives the freshness check of plan, build and status: latest
// in the registry of store.oci against the local copies, keys/deployed.yaml
// and the pushable trees of this user (publish.html#freshness).
func rbFreshGate(a *App, set *config.Set, setDir string) localcopy.Gate {
	p := a.Paths()
	g := localcopy.Gate{Set: set, Paths: p, Getenv: a.Getenv, User: username(a.Getenv),
		Pushable: func(t string) []localcopy.Pushable { return publish.PushableFor(p, set.Name, t) }}
	if deps, err := sign.ReadDeployments(setDir); err == nil && deps != nil {
		g.Deployed = func(t string) bool {
			for _, d := range deps.Deployments {
				if d.Target == t {
					return true
				}
			}
			return false
		}
	}
	return g
}

// rbFreshPlan runs the freshness check of plan and build for the targets:
// it gives the results by target, the warnings, and the error that stops
// the command (state.behind, exit 13). Without store.oci it does nothing.
func rbFreshPlan(ctx context.Context, a *App, set *config.Set, setDir string, targets []string) (map[string]localcopy.Freshness, []api.Problem, error) {
	fs := rbFreshGate(a, set, setDir).Check(ctx, targets)
	by := map[string]localcopy.Freshness{}
	for _, f := range fs {
		by[f.Target] = f
	}
	warns, err := localcopy.PlanOutcome(fs)
	return by, warns, err
}

var reSession = regexp.MustCompile(`^[0-9a-f]{16}$`)

// rbOrigin reads the hidden flags of a build that the web UI or the MCP
// server starts: --run-id, and the origin of run.started from --origin,
// --origin-session and --origin-client, else from REPOCTL_ORIGIN and
// REPOCTL_ORIGIN_SESSION. Without either, a run is from cli, or from ci
// when CI=true.
func rbOrigin(a *App, cmd *cobra.Command, runs, user, host string) (string, api.RunStartedOrigin, error) {
	fl := cmd.Flags()
	o := api.RunStartedOrigin{Via: api.RunOriginCli, User: user, Host: host, PID: os.Getpid()}
	if a.Getenv("CI") == "true" {
		o.Via = api.RunOriginCi
	}
	via, _ := fl.GetString("origin")
	from := "--origin"
	if via == "" {
		via, from = a.Getenv("REPOCTL_ORIGIN"), "REPOCTL_ORIGIN"
	}
	if via != "" {
		if !slices.Contains(api.RunOriginValues, api.RunOrigin(via)) {
			return "", o, usagef("%s %s: want cli, ui, mcp or ci", from, via)
		}
		o.Via = api.RunOrigin(via)
	}
	// The session is 16 hex digits (events.schema.json); a variable with
	// another value is left out, a flag is a usage error.
	o.Session, _ = fl.GetString("origin-session")
	if o.Session != "" && !reSession.MatchString(o.Session) {
		return "", o, usagef("--origin-session %s: want 16 hex digits", o.Session)
	}
	if s := a.Getenv("REPOCTL_ORIGIN_SESSION"); o.Session == "" && reSession.MatchString(s) {
		o.Session = s
	}
	o.Client, _ = fl.GetString("origin-client")
	id, _ := fl.GetString("run-id")
	if id == "" {
		return "", o, nil
	}
	if !journal.IsRunID(id) || !strings.HasPrefix(id, journal.PrefixOf(journal.KindBuild)+"-") {
		return "", o, usagef("--run-id %s: want a build run id such as r-20261102-1750-a3f", id)
	}
	if _, err := os.Stat(filepath.Join(runs, id+".jsonl")); err == nil {
		return "", o, usagef("--run-id %s: the run exists", id)
	}
	return id, o, nil
}

// rbSignals turns SIGINT and SIGTERM into cancel requests: the first is a
// cancel, the second a forced cancel (build.html#cancel). repoctl cancel
// writes its request next to the journal before it sends SIGINT.
func rbSignals(a *App, set *config.Set, runID string, origin api.RunStartedOrigin, out chan<- journal.CancelRequest) func() {
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		n := 0
		for {
			select {
			case <-done:
				return
			case <-sig:
				n++
				req := journal.CancelRequest{By: origin.Via, User: origin.User, Reason: "signal"}
				if c, ok := journal.ReadCancel(a.Paths(), set.Name, runID); ok {
					req = c
				}
				req.Force = req.Force || n > 1
				select {
				case out <- req:
				default:
				}
			}
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
	}
}

// rbRetain runs the retention of the journals at the start of a build and
// gives the staged index of the set.
func rbRetain(a *App, set *config.Set, eff *config.Effective) *staging.Index {
	p := a.Paths()
	idx, err := staging.Load(p, set.Name, hostname())
	if err != nil {
		return nil
	}
	keep := idx.Runs()
	o := journal.Readers(p, set.Name, hostname())
	_, _ = journal.Retain(o, set.Name, journal.Retention{KeepRuns: eff.Int("state.keep_runs"), KeepPublishes: eff.Int("state.keep_publishes"),
		Keep: func(r *api.Run) bool { _, ok := keep[r.ID]; return ok }})
	return idx
}

// multiError is the error of a failed build: one problem per failed or
// blocked node in errors[]; the first is the error line.
type multiError struct {
	first *api.Error
	all   []api.Problem
}

func (m *multiError) Error() string           { return m.first.Error() }
func (m *multiError) Unwrap() error           { return m.first }
func (m *multiError) Problems() []api.Problem { return m.all }

// rbBuildResult gives the result of repoctl build: the Run, the report of
// the failed nodes and the staging on stdout, the summary line and the
// error with its hints (terminal.html#build-failed).
func rbBuildResult(a *App, res *build.Result, runErr error, shown bool, took time.Duration) (*Result, error) {
	r := res.Run
	r.Next = rbNext(r)
	out := &Result{Kind: api.KindRun, Items: []any{r}, Run: r.ID, Targets: r.Selection.Targets}
	var err error
	switch {
	case res.Forced:
		err = api.Errorf(api.PRunCanceled, "run %s stopped at once; the mock roots may still hold processes", r.ID).
			WithExit(api.ExitInterrupted).WithRun(r.ID).WithHint("clean the mock roots", "repoctl mock clean")
	case runErr != nil:
		err = runErr
	case r.State == api.RunStateCanceled:
		e := api.Errorf(api.PRunCanceled, "run %s was canceled", r.ID).WithExit(api.ExitInterrupted).WithRun(r.ID)
		if r.CancelRequested != nil && r.CancelRequested.Reason == "deadline" {
			e.Message = fmt.Sprintf("run %s passed build.timeout.run and was canceled", r.ID)
		}
		err = e
	case r.State == api.RunStateFailed:
		err = rbFailure(r, res.Plan)
	}
	out.ShownWarnings = true
	out.Human = func(o *Out) error {
		rbReport(o, a, r)
		if !res.Forced && (err != nil || shown || took >= time.Second) {
			rbSummary(o, r, took)
		}
		for _, w := range a.warnings {
			if !rbCovered(w, r, res.Plan) {
				o.Warning(w)
			}
		}
		return nil
	}
	if err == nil {
		out.Next = r.Next
		out.NextShown = false
	}
	return out, err
}

// rbCovered reports a warning dep.not-for-target of a node that failed
// with the class builddep: the %if hint of the error says it.
func rbCovered(w api.Problem, r *api.Run, pl *build.Plan) bool {
	if w.Code != api.PDepNotForTarget || w.Node == "" {
		return false
	}
	for _, n := range r.Nodes {
		if n.Node == w.Node && n.State == api.NodeStateFailed {
			_, ok := rbIfHint(n, pl)
			return ok
		}
	}
	return false
}

// rbNext gives the Next steps of a build run.
func rbNext(r *api.Run) []api.NextStep {
	switch {
	case r.State == api.RunStateOk && r.Unpinned:
		return []api.NextStep{{Action: "pin", Reason: "a run from worktrees is never published: pin the commits, then build from the lock", Command: "repoctl pin"}}
	case r.State == api.RunStateOk && len(r.Publishable) > 0:
		return []api.NextStep{{Action: "publish", Human: true, Reason: "the staging of run " + r.ID + " is publishable", Command: "repoctl publish"}}
	case r.State == api.RunStateFailed:
		for _, n := range r.Nodes {
			if n.State == api.NodeStateFailed {
				return []api.NextStep{{Action: "triage", Reason: n.Node + " failed", Command: "repoctl logs " + r.ID + " " + n.Node}}
			}
		}
	}
	return nil
}

var reRhel = regexp.MustCompile(`[0-9]+`)

// rbFailure gives the error of a failed run: node.failed for each failed
// node and node.blocked for each blocked node, with up to three hints.
func rbFailure(r *api.Run, pl *build.Plan) error {
	var first *api.Error
	var all []api.Problem
	for _, n := range r.Nodes {
		switch n.State {
		case api.NodeStateFailed:
			msg := "failed"
			step := ""
			if n.Error != nil {
				msg, step = n.Error.Message, string(n.Error.Step)
			}
			e := api.Errorf(api.PNodeFailed, "%s: %s", n.Node, msg).WithNode(n.Node).WithRun(r.ID)
			p := e.Problem
			p.Message = fmt.Sprintf("%s failed in step %s: %s", n.Node, step, msg)
			p.Hint = api.HintText("read the log", "repoctl logs "+n.Node)
			all = append(all, p)
			if first == nil {
				first = e
				first = first.WithHint("read the log", "repoctl logs "+n.Node)
				if h, ok := rbIfHint(n, pl); ok {
					first = first.WithHint(h, "")
				}
			}
		case api.NodeStateBlocked:
			e := api.Errorf(api.PNodeBlocked, "%s is blocked: it needs %s", n.Node, strings.Join(n.BlockedBy, ", ")).WithNode(n.Node).WithRun(r.ID)
			all = append(all, e.Problem)
		case api.NodeStateCanceled:
			e := api.Errorf(api.PNodeCanceled, "%s was canceled", n.Node).WithNode(n.Node).WithRun(r.ID).WithExit(api.ExitBuildFailed)
			all = append(all, e.Problem)
		}
	}
	if first == nil {
		if len(all) == 0 {
			return api.Errorf(api.PNodeFailed, "run %s failed", r.ID).WithRun(r.ID)
		}
		first = &api.Error{Problem: all[0]}
	}
	if !r.Unpinned {
		for _, t := range r.Publishable {
			first = first.WithHint(t+" is publishable now", "repoctl publish --target "+t)
			break
		}
	}
	if len(first.Hints) > 3 {
		first.Hints = first.Hints[:3]
	}
	for i := range all {
		all[i].ExitCode = api.ExitBuildFailed
	}
	return &multiError{first: first, all: all}
}

// rbIfHint gives the %if hint of a builddep failure: a missing
// BuildRequires that a package of the set provides only on other targets.
func rbIfHint(n api.NodeResult, pl *build.Plan) (string, bool) {
	if pl == nil || n.Error == nil || n.Error.Class != api.FailureClassBuilddep {
		return "", false
	}
	for _, x := range pl.NotForTarget {
		if x.Node != n.Node || !slices.Contains(n.Error.Missing, x.Requires) || len(x.Targets) == 0 {
			continue
		}
		rhel := reRhel.FindString(n.Target)
		if rhel == "" {
			return "", false
		}
		return fmt.Sprintf("%s builds for %s only: guard its BuildRequires in %s.spec with %%if 0%%{?rhel} < %s, or build it for %s",
			x.Package, strings.Join(x.Targets, " "), n.Package, rhel, n.Target), true
	}
	return "", false
}

// rbErrorLines gives up to four error lines of the excerpt of a failed
// node: for a builddep failure the lines that name a missing package,
// else the lines that read as errors.
func rbErrorLines(e *api.NodeError) []string {
	if e == nil || e.Excerpt == nil {
		return nil
	}
	var errs, named []string
	for _, l := range e.Excerpt.Lines {
		text := strings.TrimSpace(rbStripLogPrefix(l))
		for _, m := range e.Missing {
			if strings.Contains(text, m) {
				named = append(named, text)
				break
			}
		}
		if build.ErrorLine.MatchString(l) {
			errs = append(errs, text)
		}
	}
	if len(named) > 0 {
		errs = named
	}
	return errs[:min(len(errs), 4)]
}

// rbReport writes the report of a run on stdout: each failed node with
// the error lines of its mock logs, and the staging.
func rbReport(o *Out, a *App, r *api.Run) {
	st := o.StdoutStyle
	for _, n := range r.Nodes {
		if n.State != api.NodeStateFailed || n.Error == nil {
			continue
		}
		line := fmt.Sprintf("%s %s failed in step %s", st.Mark("failed"), n.Node, n.Error.Step)
		if n.DurationMs > 0 {
			line += " after " + build.Dur(time.Duration(n.DurationMs)*time.Millisecond)
		}
		if n.Commit != "" {
			line += " (commit " + rbShort(n.Commit) + ")"
		}
		fmt.Fprintln(o.Stdout, line)
		lines := rbErrorLines(n.Error)
		for _, l := range lines {
			fmt.Fprintf(o.Stdout, "  %s  %s\n", st.Dim(filepath.Base(n.Error.Excerpt.File)), termtext.Escape(l))
		}
		if len(lines) == 0 {
			fmt.Fprintf(o.Stdout, "  %s\n", termtext.Escape(n.Error.Message))
		}
	}
	if r.Staging != nil && r.Staging.Path != "" {
		line := fmt.Sprintf("%s  %s", st.Dim("staging"), rbTilde(a, r.Staging.Path))
		if len(r.Publishable) > 0 && !r.Unpinned {
			line += " (" + strings.Join(r.Publishable, " ") + " complete)"
		}
		fmt.Fprintln(o.Stdout, line)
	}
}

var reLogPrefix = regexp.MustCompile(`^(DEBUG|INFO|WARNING) [a-z_]+\.py: `)

func rbStripLogPrefix(s string) string { return reLogPrefix.ReplaceAllString(s, "") }

// rbSummary writes the summary line of a run to stderr.
func rbSummary(o *Out, r *api.Run, took time.Duration) {
	if o.Quiet {
		return
	}
	word, mark := "done", o.Style.Mark("ok")
	switch r.State {
	case api.RunStateFailed:
		word, mark = "failed", o.Style.Mark("failed")
	case api.RunStateCanceled:
		word, mark = "canceled", o.Style.Mark("canceled")
	}
	d := took
	if r.DurationMs > 0 {
		d = time.Duration(r.DurationMs) * time.Millisecond
	}
	fmt.Fprintf(o.Stderr, "repoctl: %s build: %s in %s: %s\n", mark, word, build.Dur(d), build.CountWords(r.Counts, true))
}

func rbShort(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

// rbTilde writes a path below the home directory with ~.
func rbTilde(a *App, p string) string {
	home := a.Getenv("HOME")
	if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// ---------------------------------------------------------------- runs

func hRuns(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	set, _, err := a.Set()
	if err != nil {
		return nil, err
	}
	fl := cmd.Flags()
	limit, _ := fl.GetInt("limit")
	stateF, _ := fl.GetString("state")
	kindF, _ := fl.GetString("kind")
	follow, _ := fl.GetBool("follow")
	p := a.Paths()
	o := journal.Readers(p, set.Name, hostname())
	if len(args) == 1 {
		e, err := journal.Find(o, set.Name, args[0], kindF)
		if err != nil {
			return nil, err
		}
		if follow {
			return rbFollow(ctx, a, e)
		}
		r := e.Run()
		r.Next = rbNext(r)
		return &Result{Kind: api.KindRun, Items: []any{r}, Run: r.ID, Human: func(o *Out) error { rbDetail(o, a, r); return nil }}, nil
	}
	if follow {
		e, err := journal.Find(o, set.Name, "latest", kindF)
		if err != nil {
			return nil, err
		}
		return rbFollow(ctx, a, e)
	}
	entries, err := journal.List(o, set.Name, false)
	if err != nil {
		return nil, err
	}
	var runs []*api.Run
	publishedIn := map[api.RunId]string{}
	for _, e := range entries {
		r := e.Run()
		if r.Kind == journal.KindPublish && r.State == api.RunStateOk {
			for _, from := range r.FromRuns {
				if _, ok := publishedIn[from]; !ok {
					publishedIn[from] = r.ID
				}
			}
		}
	}
	for _, e := range entries {
		r := e.Run()
		if (kindF != "" && r.Kind != kindF) || (stateF != "" && string(r.State) != stateF) {
			continue
		}
		runs = append(runs, r)
		if limit > 0 && len(runs) == limit {
			break
		}
	}
	items := make([]any, len(runs))
	for i, r := range runs {
		items[i] = r
	}
	return &Result{Kind: api.KindRun, Items: items, Human: func(o *Out) error {
		if len(runs) == 0 {
			o.Note("no runs of %s on this host", set.Name)
			return nil
		}
		t := NewTable("RUN", "STATE", "STARTED", "TIME", "BUILDS", "PUBLISHED IN")
		for _, r := range runs {
			pub := publishedIn[r.ID]
			if pub == "" {
				pub = "-"
			}
			t.RowCells(Cell{Text: r.ID}, C(string(r.State), o.StdoutStyle.State), Cell{Text: rbTime(r.StartedAt)},
				Cell{Text: rbRunTime(a, r)}, Cell{Text: rbBuilds(r)}, Cell{Text: pub})
		}
		return o.Table(t)
	}}, nil
}

func rbTime(t api.Time) string {
	tt, err := time.Parse(time.RFC3339Nano, t)
	if err != nil {
		return t
	}
	return tt.Local().Format("2006-01-02 15:04")
}

func rbRunTime(a *App, r *api.Run) string {
	if r.DurationMs > 0 {
		return build.Dur(time.Duration(r.DurationMs) * time.Millisecond)
	}
	if r.State == api.RunStateRunning || r.State == api.RunStatePlanning {
		if t, err := time.Parse(time.RFC3339Nano, r.StartedAt); err == nil {
			return build.Dur(a.Now().Sub(t))
		}
	}
	return "-"
}

// rbBuilds gives the BUILDS column: ok of build nodes, and the failures.
func rbBuilds(r *api.Run) string {
	if r.Kind != journal.KindBuild {
		return "-"
	}
	c := r.Counts
	total := c.Waiting + c.Queued + c.Running + c.Ok + c.Failed + c.Blocked + c.Canceled
	s := strconv.Itoa(c.Ok)
	if r.State == api.RunStateRunning || r.State == api.RunStatePlanning {
		s = fmt.Sprintf("%d/%d", c.Ok, total)
	}
	if c.Failed > 0 {
		s += fmt.Sprintf(" · %d failed", c.Failed)
	}
	return s
}

// rbDetail writes the nodes of one run.
func rbDetail(o *Out, a *App, r *api.Run) {
	st := o.StdoutStyle
	head := fmt.Sprintf("%s  %s  %s  %s", r.ID, r.Kind, st.State(string(r.State)), rbTime(r.StartedAt))
	if t := rbRunTime(a, r); t != "-" {
		head += "  " + t
	}
	if r.CancelRequested != nil && r.State == api.RunStateRunning {
		head += "  interrupting"
	}
	fmt.Fprintln(o.Stdout, head)
	fmt.Fprintf(o.Stdout, "%s  %s %s@%s\n", st.Dim("origin"), r.Origin.Via, r.Origin.User, r.Origin.Host)
	t := NewTable("NODE", "STATE", "TIME", "DETAIL")
	folded := map[api.NodeState]int{}
	for _, n := range r.Nodes {
		if (n.State == api.NodeStateCurrent || n.State == api.NodeStateStaged) && o.Verbose == 0 {
			folded[n.State]++
			continue
		}
		detail := ""
		switch {
		case n.Error != nil:
			detail = n.Error.Message
		case len(n.BlockedBy) > 0:
			detail = "it needs " + strings.Join(n.BlockedBy, ", ")
		case len(n.WaitingFor) > 0:
			detail = "waiting for " + strings.Join(n.WaitingFor, ", ")
		}
		tm := ""
		if n.DurationMs > 0 {
			tm = build.Dur(time.Duration(n.DurationMs) * time.Millisecond)
		}
		t.RowCells(Cell{Text: st.Mark(string(n.State)) + " " + n.Node}, C(string(n.State), st.State), Cell{Text: tm}, Cell{Text: detail})
	}
	if len(t.Rows) > 0 {
		_ = o.Table(t)
	}
	var parts []string
	for _, s := range []api.NodeState{api.NodeStateCurrent, api.NodeStateStaged} {
		if folded[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", folded[s], s))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(o.Stdout, "%s  (-v shows all)\n", strings.Join(parts, ", "))
	}
	if r.Staging != nil && r.Kind == journal.KindBuild {
		fmt.Fprintf(o.Stdout, "%s  %s\n", st.Dim("staging"), rbTilde(a, r.Staging.Path))
	}
}

// rbProgressOptions gives the progress options of build and runs
// --follow: a step counts nodes.
func rbProgressOptions(a *App) cliprogress.Options {
	po := a.ProgressOptions("repoctl")
	po.Noun = func(n int) string {
		if n == 1 {
			return "1 node"
		}
		return strconv.Itoa(n) + " nodes"
	}
	return po
}

// rbView draws the progress log of a run as build draws it: the live
// tree, the counter or the plain lines that --progress chooses.
type rbView struct {
	tail  *eventlog.Tail
	shown interface {
		progress.Sink
		Start()
		Close()
	}
}

// rbFollowView gives the view of the progress log at path, or nil when
// --progress chooses none.
func rbFollowView(a *App, path string) (*rbView, error) {
	po := rbProgressOptions(a)
	mode, note, err := cliprogress.Choose(po)
	if err != nil {
		return nil, usagef("%s", err.Error())
	}
	if note != "" {
		fmt.Fprintln(a.Stderr, termtext.Escape(note))
	}
	term := display.NewTerminal(a.Stderr, display.TerminalOptions{Size: po.Size, Foreground: po.Foreground, Program: "repoctl"})
	v := &rbView{}
	switch mode {
	case cliprogress.ModeTTY:
		v.shown = display.NewTree(term, display.TreeOptions{Now: po.Now, ASCII: po.ASCII, Theme: po.Theme})
	case cliprogress.ModeCounter:
		v.shown = display.NewCounter(term, display.CounterOptions{Now: po.Now, ASCII: po.ASCII, Theme: po.Theme})
	case cliprogress.ModePlain:
		v.shown = display.NewPlain(term, display.PlainOptions{Now: po.Now, ASCII: po.ASCII, Theme: po.Theme, Noun: po.Noun})
	default:
		return nil, nil
	}
	v.tail = eventlog.NewTail(path, eventlog.NewForwarder(v.shown, progress.TraceID{}))
	v.shown.Start()
	return v, nil
}

func (v *rbView) poll() {
	if v != nil {
		_, _ = v.tail.Poll()
	}
}

func (v *rbView) close() {
	if v != nil {
		_, _ = v.tail.Poll()
		v.shown.Close()
	}
}

// rbFollow tails the journal of a run until run.finished, or until the
// process of the run is gone (run.abandoned). It draws the progress log of
// the run as build draws it; without a display, it writes a line to
// stderr for each node that starts or ends.
func rbFollow(ctx context.Context, a *App, e journal.Entry) (*Result, error) {
	o := a.out()
	var off int64
	var part []byte
	f := e.Fold
	set := f.Run().Set
	f.Reset()
	alive := journal.LockAlive(a.Paths(), set)
	if alive == nil {
		alive = func(*api.Run) bool { return false }
	}
	view, err := rbFollowView(a, a.Paths().ProgressLog(set, e.ID))
	if err != nil {
		return nil, err
	}
	defer func() { view.close() }()
	for {
		view.poll()
		b, err := os.ReadFile(e.Path)
		if err == nil && int64(len(b)) > off {
			part = append(part, b[off:]...)
			off = int64(len(b))
			for {
				i := strings.IndexByte(string(part), '\n')
				if i < 0 {
					break
				}
				l, err := journal.Decode(part[:i])
				part = part[i+1:]
				if err != nil || l.Event == nil {
					continue
				}
				f.Add(l)
				if view == nil {
					rbFollowLine(o, l)
				}
			}
		}
		r := f.Run()
		if f.Finished() {
			view.close()
			view = nil
			res := &Result{Kind: api.KindRun, Items: []any{r}, Run: r.ID, Human: func(o *Out) error { rbDetail(o, a, r); return nil }}
			if r.ExitCode != 0 {
				return res, api.Errorf("run."+string(r.State), "run %s ended %s", r.ID, r.State).WithExit(r.ExitCode).WithRun(r.ID)
			}
			return res, nil
		}
		if r.Origin.Host == hostname() && !alive(r) && !rbEndedSince(e.Path, off) {
			view.close()
			view = nil
			r.State = api.RunStateAbandoned
			res := &Result{Kind: api.KindRun, Items: []any{r}, Run: r.ID, Human: func(o *Out) error { rbDetail(o, a, r); return nil }}
			return res, api.Errorf(api.PRunAbandoned, "run %s ended without a result: its process is gone", r.ID).WithRun(r.ID).
				WithHint("clean the mock roots", "repoctl mock clean")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// rbEndedSince reports a journal that grew past off: the process of the
// run wrote its last lines and let go of the lock after the last read, so
// the next read gives run.finished.
func rbEndedSince(path string, off int64) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Size() > off
}

func rbFollowLine(o *Out, l journal.Line) {
	if o.Quiet {
		return
	}
	ns, ok := l.Event.(*api.NodeStateEvent)
	if !ok {
		return
	}
	st := o.Style
	switch ns.State {
	case api.NodeStateRunning:
		fmt.Fprintf(o.Stderr, "%s %s\n", st.Mark("running"), l.Node)
	case api.NodeStateOk:
		fmt.Fprintf(o.Stderr, "%s %s  %s\n", st.Mark("ok"), l.Node, build.Dur(time.Duration(ns.DurationMs)*time.Millisecond))
	case api.NodeStateFailed:
		msg := ""
		if ns.Error != nil {
			msg = ns.Error.Message
		}
		fmt.Fprintf(o.Stderr, "%s %s  %s\n", st.Mark("failed"), l.Node, termtext.Escape(msg))
	case api.NodeStateBlocked:
		fmt.Fprintf(o.Stderr, "%s %s  blocked: it needs %s\n", st.Mark("blocked"), l.Node, strings.Join(ns.BlockedBy, ", "))
	case api.NodeStateCanceled:
		fmt.Fprintf(o.Stderr, "%s %s  canceled\n", st.Mark("canceled"), l.Node)
	}
}

// ---------------------------------------------------------------- logs

var reRunID = regexp.MustCompile(`^(r|p)-[0-9]{8}-[0-9]{4}-[0-9a-f]{3}$`)

func hLogs(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	set, _, err := a.Set()
	if err != nil {
		return nil, err
	}
	fl := cmd.Flags()
	file, _ := fl.GetString("file")
	step, _ := fl.GetString("step")
	follow, _ := fl.GetBool("follow")
	switch file {
	case "repoctl", "root", "build", "state":
	default:
		return nil, usagef("--file %s: use repoctl, root, build or state", file)
	}
	switch step {
	case "", "srpm", "rpm", "stage":
	default:
		return nil, usagef("--step %s: use srpm, rpm or stage", step)
	}
	runArg, nodeArg := "", ""
	for _, x := range args {
		if reRunID.MatchString(x) || x == "latest" {
			runArg = x
		} else {
			nodeArg = x
		}
	}
	p := a.Paths()
	o := journal.Readers(p, set.Name, hostname())
	var entry journal.Entry
	switch {
	case runArg != "":
		if entry, err = journal.Find(o, set.Name, runArg, ""); err != nil {
			return nil, err
		}
	default:
		es, err := journal.List(o, set.Name, false)
		if err != nil {
			return nil, err
		}
		found := false
		for _, e := range es {
			r := e.Run()
			if r.Kind != journal.KindBuild {
				continue
			}
			if nodeArg == "" || slices.ContainsFunc(r.Nodes, func(n api.NodeResult) bool {
				return n.Node == nodeArg && n.State != api.NodeStateCurrent && n.State != api.NodeStateStaged
			}) {
				entry, found = e, true
				break
			}
		}
		if !found {
			if nodeArg != "" {
				return nil, api.Errorf(api.PRunNotFound, "no build run of this host built %s", nodeArg).WithNode(nodeArg)
			}
			return nil, api.Errorf(api.PRunNotFound, "no build run of %s on this host", set.Name)
		}
	}
	r := entry.Run()
	var node *api.NodeResult
	for i := range r.Nodes {
		n := &r.Nodes[i]
		if (nodeArg != "" && n.Node == nodeArg) || (nodeArg == "" && n.State == api.NodeStateFailed) {
			node = n
			break
		}
	}
	if node == nil {
		if nodeArg == "" {
			return nil, api.Errorf(api.PRunNotFound, "run %s has no failed node; name the node", r.ID).WithRun(r.ID)
		}
		return nil, api.Errorf(api.PRunNotFound, "run %s has no node %s", r.ID, nodeArg).WithRun(r.ID)
	}
	path := p.NodeLog(set.Name, r.ID, node.Node)
	if file != "repoctl" {
		s := step
		if s == "" || s == "stage" {
			s = "rpm"
			if node.Error != nil && node.Error.Step == api.StepSRPM {
				s = "srpm"
			}
			if node.Logs != nil && node.Logs.Mock != nil {
				if mp, ok := node.Logs.Mock[file+".log"]; ok {
					path = mp
				}
			}
		}
		if path == p.NodeLog(set.Name, r.ID, node.Node) {
			path = filepath.Join(p.MockLogs(set.Name, r.ID, node.Node, s), file+".log")
		}
	}
	streamed := false
	if follow {
		if err := rbFollowLog(ctx, a, entry, node.Node, path); err != nil {
			return nil, err
		}
		streamed = true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, api.Errorf(api.PRunNotFound, "%s has no %s log in run %s", node.Node, file, r.ID).WithPath(path).WithRun(r.ID)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(data) == 0 {
		lines = nil
	}
	first := 1
	if step != "" && file == "repoctl" {
		lines, first = rbStepLines(lines, step)
	}
	whole := a.verbose > 0 || !cliprogress.IsTerminal(a.Stdout) || node.State != api.NodeStateFailed
	from := 0
	if !whole {
		from = -1
		for i, l := range lines {
			if build.ErrorLine.MatchString(l) {
				from = max(i-5, 0)
				break
			}
		}
		if from < 0 {
			from = max(len(lines)-20, 0)
		}
	}
	nl := api.NodeLog{Run: r.ID, Node: node.Node, State: node.State, Path: path, SizeBytes: len(data), FirstLine: first + from,
		Lines: lines[from:], Truncated: from > 0, Error: node.Error}
	return &Result{Kind: api.KindNodeLog, Items: []any{nl}, Run: r.ID, Human: func(o *Out) error {
		if !streamed {
			rbLogHuman(o, a, r, node, nl, len(lines), whole, file)
		}
		return nil
	}}, nil
}

// rbStepLines gives the lines of one step of a node log, between its
// marker and the marker of the next step.
func rbStepLines(lines []string, step string) ([]string, int) {
	start, end := -1, len(lines)
	for i, l := range lines {
		if !strings.HasPrefix(l, build.Marker) {
			continue
		}
		rest := strings.TrimPrefix(l, build.Marker)
		if strings.HasSuffix(rest, " started") {
			if strings.TrimSuffix(rest, " started") == step {
				start = i
			} else if start >= 0 && end == len(lines) {
				end = i
			}
		}
	}
	if start < 0 {
		return nil, 1
	}
	return lines[start:end], start + 1
}

// rbLogHuman writes the log: the header and the hint to stderr, the lines
// to stdout. On a terminal, a failed node shows an excerpt from 5 lines
// above the first error, with numbered lines, error marks and folded INFO
// lines (terminal.html#logs). A pipe gets the whole log as it is, so that
// logs … > FILE holds only the log.
func rbLogHuman(o *Out, a *App, r *api.Run, n *api.NodeResult, nl api.NodeLog, total int, whole bool, file string) {
	st := o.Style
	head := fmt.Sprintf("%s %s", n.Node, st.State(string(n.State)))
	if n.Error != nil {
		head += " in " + string(n.Error.Step)
	}
	if n.DurationMs > 0 {
		head += " after " + build.Dur(time.Duration(n.DurationMs)*time.Millisecond)
	}
	head += "  run " + r.ID
	if n.Commit != "" {
		head += "  commit " + rbShort(n.Commit)
	}
	fmt.Fprintln(o.Stderr, head)
	fmt.Fprintf(o.Stderr, "%s  %s\n", st.Dim("log"), rbTilde(a, nl.Path))
	last := nl.FirstLine + len(nl.Lines) - 1
	if !cliprogress.IsTerminal(a.Stdout) {
		var errs []string
		for i, l := range nl.Lines {
			fmt.Fprintln(o.Stdout, termtext.Escape(l))
			if build.ErrorLine.MatchString(l) {
				errs = append(errs, strconv.Itoa(nl.FirstLine+i))
			}
		}
		rbLogEnd(o, n, errs, file)
		return
	}
	if !whole && nl.Truncated {
		fmt.Fprintf(o.Stderr, "lines %d–%d of %d, from 5 lines above the first error  (-v shows all)\n", nl.FirstLine, last, total)
	}
	so := o.StdoutStyle
	width := len(strconv.Itoa(max(last, 1)))
	var errs []string
	info := 0
	flushInfo := func() {
		if info > 0 {
			word := "line"
			if info > 1 {
				word = "lines"
			}
			fmt.Fprintf(o.Stdout, "%s %s %s %d INFO %s\n", strings.Repeat(" ", width), so.Dim("│"), so.Ellipsis(), info, word)
			info = 0
		}
	}
	for i, l := range nl.Lines {
		num := nl.FirstLine + i
		if !whole && strings.HasPrefix(l, "INFO:") && !build.ErrorLine.MatchString(l) {
			info++
			continue
		}
		flushInfo()
		bar := so.Dim("│")
		text := termtext.Escape(l)
		if build.ErrorLine.MatchString(l) {
			bar = so.Red("▌")
			errs = append(errs, strconv.Itoa(num))
		}
		fmt.Fprintf(o.Stdout, "%*d %s %s\n", width, num, bar, text)
	}
	flushInfo()
	rbLogEnd(o, n, errs, file)
}

// rbLogEnd writes the error lines and the hint of a node log to stderr.
func rbLogEnd(o *Out, n *api.NodeResult, errs []string, file string) {
	if len(errs) > 0 {
		fmt.Fprintf(o.Stderr, "errors: %s\n", strings.Join(errs, ", "))
	}
	if n.Error != nil && file == "repoctl" {
		switch n.Error.Class {
		case api.FailureClassBuilddep, api.FailureClassHost:
			o.Hint("read the dnf output of mock", "repoctl logs "+n.Node+" --file root")
		case api.FailureClassBuild, api.FailureClassSRPM:
			o.Hint("read the rpmbuild output", "repoctl logs "+n.Node+" --file build")
		}
	}
}

// rbFollowLog waits for new lines of a node log until the node ends,
// and writes them to stdout as they come.
func rbFollowLog(ctx context.Context, a *App, e journal.Entry, node api.NodeId, path string) error {
	var off int64
	alive := journal.LockAlive(a.Paths(), e.Run().Set)
	for {
		if b, err := os.ReadFile(path); err == nil && int64(len(b)) > off {
			_, _ = io.WriteString(a.Stdout, termtext.Escape(string(b[off:])))
			off = int64(len(b))
		}
		f, err := journal.FoldFile(journal.FoldOptions{Journal: e.Path, Host: hostname(), Alive: alive})
		if err != nil {
			return err
		}
		r := f.Run()
		done := f.Finished() || r.State == api.RunStateAbandoned
		for _, n := range r.Nodes {
			if n.Node == node && journal.IsTerminal(n.State) {
				done = true
			}
		}
		if done {
			if b, err := os.ReadFile(path); err == nil && int64(len(b)) > off {
				_, _ = io.WriteString(a.Stdout, termtext.Escape(string(b[off:])))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------- cancel

func hCancel(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	set, _, err := a.Set()
	if err != nil {
		return nil, err
	}
	fl := cmd.Flags()
	wait, _ := fl.GetBool("wait")
	force, _ := fl.GetBool("force")
	p := a.Paths()
	holder, _ := lock.ReadHolder(p.SetLock(set.Name))
	if holder == nil || holder.Run == "" || !strings.HasPrefix(holder.Run, "r-") {
		return nil, api.Errorf(api.PRunNotFound, "no build of %s runs on this host", set.Name)
	}
	if holder.Host != hostname() || holder.User != username(a.Getenv) {
		return nil, api.Errorf(api.PCancelNotHolder, "run %s belongs to %s on %s; only that user on that host can cancel it",
			holder.Run, holder.User, holder.Host).WithRun(holder.Run)
	}
	if len(args) == 1 && args[0] != "latest" && args[0] != holder.Run {
		return nil, api.Errorf(api.PRunNotFound, "run %s is not running; the running run is %s", args[0], holder.Run).WithRun(args[0])
	}
	ro := journal.Readers(p, set.Name, hostname())
	e, err := journal.Find(ro, set.Name, holder.Run, journal.KindBuild)
	if err != nil {
		return nil, err
	}
	r := e.Run()
	o := a.out()
	if r.CancelRequested != nil && !force {
		o.Note("run %s is interrupting: it stops its mock roots (at most 60 s)", r.ID)
	} else {
		req := journal.CancelRequest{By: api.RunOriginCli, User: username(a.Getenv), Reason: "signal", Force: force}
		if err := journal.WriteCancel(p, set.Name, r.ID, req); err != nil {
			return nil, err
		}
		if err := syscall.Kill(holder.PID, syscall.SIGINT); err != nil {
			return nil, api.Errorf(api.PRunNotFound, "the process %d of run %s is gone", holder.PID, r.ID).WithRun(r.ID)
		}
		if force {
			o.Note("run %s: stopping its mock roots at once (pid %d)", r.ID, holder.PID)
		} else {
			o.Note("run %s: canceling (pid %d); the mock roots stop within 60 s", r.ID, holder.PID)
		}
	}
	if wait {
		alive := journal.LockAlive(p, set.Name)
		for {
			f, err := journal.FoldFile(journal.FoldOptions{Journal: e.Path, Paths: &p, Host: hostname(), Alive: alive})
			if err == nil {
				r = f.Run()
				if f.Finished() || r.State == api.RunStateAbandoned {
					break
				}
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	} else if f, err := journal.FoldFile(journal.FoldOptions{Journal: e.Path, Paths: &p, Host: hostname(), Alive: journal.LockAlive(p, set.Name)}); err == nil {
		r = f.Run()
	}
	res := &Result{Kind: api.KindRun, Items: []any{r}, Run: r.ID, Human: func(o *Out) error {
		if wait {
			fmt.Fprintf(o.Stdout, "%s  %s\n", r.ID, o.StdoutStyle.State(string(r.State)))
		}
		return nil
	}}
	if wait && r.State == api.RunStateAbandoned {
		res.Human = func(o *Out) error {
			fmt.Fprintf(o.Stdout, "%s  abandoned\n", r.ID)
			o.Hint("clean the mock roots", "repoctl mock clean")
			return nil
		}
	}
	return res, nil
}

// ---------------------------------------------------------------- clean

func hClean(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	fl := cmd.Flags()
	get := func(n string) bool { v, _ := fl.GetBool(n); return v }
	setName, _ := fl.GetString("set")
	olderStr, _ := fl.GetString("older-than")
	pubID, _ := fl.GetString("publish")
	dry, yes, all := get("dry-run"), get("yes"), get("all")
	older := time.Duration(0)
	if olderStr != "" {
		d, err := rbParseAge(olderStr)
		if err != nil {
			return nil, usagef("--older-than %s: %v", olderStr, err)
		}
		older = d
	}
	p := a.Paths()
	var items []any
	var freed int64
	add := func(c api.Clean) {
		items = append(items, c)
		if c.Action != "kept" {
			freed += int64(c.Bytes)
		}
	}
	confirm := func(what string) error {
		if yes || dry {
			return nil
		}
		if !cliprogress.IsTerminal(a.Stdin) {
			return api.Errorf(api.PCleanConfirm, "%s needs --yes without a terminal", what).WithExit(api.ExitNeedsConfirmation)
		}
		ans, err := cliprogress.Ask(ctx, a.Stdin, a.Stderr, what+": delete? [y/N] ")
		if err != nil || !cliprogress.Yes(ans) {
			return api.Errorf(api.PCleanConfirm, "nothing deleted").WithExit(api.ExitNeedsConfirmation)
		}
		return nil
	}
	action := func() string {
		if dry {
			return "would-delete"
		}
		return "deleted"
	}
	remove := func(path string) {
		if !dry {
			_ = os.RemoveAll(path)
		}
	}
	if setName != "" {
		if err := confirm("clean --set " + setName); err != nil {
			return nil, err
		}
		if pubs, _ := os.ReadDir(filepath.Join(p.SetData(setName), "publishes")); len(pubs) > 0 {
			return nil, api.Errorf(api.PStateUnpushed, "set %s has a signed tree in publishes/; push it or clean it with --publish", setName).WithExit(api.ExitStateConflict)
		}
		for _, d := range []struct{ path, kind string }{{p.SetData(setName), "staging"}, {p.SetState(setName), "state"}} {
			if _, err := os.Stat(d.path); err == nil {
				add(api.Clean{Path: d.path, Kind: d.kind, Bytes: int(dirSize(d.path)), Action: action()})
				remove(d.path)
			}
		}
		return rbCleanResult(items, freed, dry), nil
	}
	set, _, err := a.Set()
	if err != nil {
		return nil, err
	}
	if !dry {
		sl, err := a.SetLock(ctx, set, false)
		if err != nil {
			return nil, err
		}
		defer sl.Release()
	}
	doStaging, doLogs, doCache := get("staging") || all, get("logs") || all, get("cache") || all
	if !doStaging && !doLogs && !doCache && !get("git") && pubID == "" {
		doStaging = true
	}
	if all {
		older = 0
		if err := confirm("clean --all"); err != nil {
			return nil, err
		}
	}
	if doStaging {
		for _, c := range rbCleanStaging(a, set, older, dry) {
			add(c)
		}
	}
	if doLogs {
		eff, _ := a.Settings()
		idx, _ := staging.Load(p, set.Name, hostname())
		keep := map[api.RunId][]api.NodeId{}
		if idx != nil {
			keep = idx.Runs()
		}
		ro := journal.Readers(p, set.Name, hostname())
		gone, err := journal.Retain(ro, set.Name, journal.Retention{KeepRuns: eff.Int("state.keep_runs"), KeepPublishes: eff.Int("state.keep_publishes"),
			KeepChecks: eff.Int("state.keep_checks"), Checks: true, DryRun: dry, Keep: func(r *api.Run) bool { _, ok := keep[r.ID]; return ok }})
		if err != nil {
			return nil, err
		}
		for _, e := range gone {
			size := rbFileSize(e.Path) + dirSize(p.Logs(set.Name, e.ID))
			add(api.Clean{Path: e.Path, Kind: "state", Bytes: int(size), Action: action(), Run: e.ID})
		}
	}
	if doCache {
		age := older
		if olderStr == "" && !all {
			age = 30 * 24 * time.Hour
		}
		for _, c := range []struct{ dir, kind string }{{p.SRPMCache(), "srpm-cache"}, {p.SourceCache(), "source-cache"},
			{p.UpstreamCache(), "upstream-cache"}, {filepath.Join(p.CacheHome, "mock"), "mock"}} {
			entries, _ := os.ReadDir(c.dir)
			for _, en := range entries {
				path := filepath.Join(c.dir, en.Name())
				fi, err := en.Info()
				if err != nil || a.Now().Sub(fi.ModTime()) < age {
					continue
				}
				add(api.Clean{Path: path, Kind: c.kind, Bytes: int(dirSize(path)), Action: action()})
				remove(path)
			}
		}
	}
	if get("git") {
		cs, err := rbCleanGit(ctx, a, dry)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			add(c)
		}
	}
	if pubID != "" {
		path := p.Publish(set.Name, pubID)
		if _, err := os.Stat(path); err != nil {
			return nil, api.Errorf(api.PRunNotFound, "no stopped publish %s", pubID).WithPath(path)
		}
		add(api.Clean{Path: path, Kind: "publish", Bytes: int(dirSize(path)), Action: action(), Run: pubID})
		remove(path)
	}
	return rbCleanResult(items, freed, dry), nil
}

// rbCleanGit runs clean --git (lifecycle.html#clones): a bare clone of
// $XDG_DATA_HOME/repoctl/git goes when it has no worktree and no commit
// that is not on its remote. Each clone is pruned (git worktree prune),
// checked and deleted under its clone lock. A clone with such commits stays
// with the warning git.mirror-unpushed. A dry run takes no lock and prunes
// nothing: it leaves out the worktrees that a prune would remove.
func rbCleanGit(ctx context.Context, a *App, dry bool) ([]api.Clean, error) {
	p := a.Paths()
	entries, _ := os.ReadDir(p.GitClones())
	var out []api.Clean
	for _, en := range entries {
		if !en.IsDir() || !strings.HasSuffix(en.Name(), ".git") {
			continue
		}
		path := filepath.Join(p.GitClones(), en.Name())
		c, warn, err := rbCleanClone(ctx, a, path, strings.TrimSuffix(en.Name(), ".git"), dry)
		if err != nil {
			return out, err
		}
		if warn != nil {
			a.Warn(*warn)
		}
		out = append(out, c)
	}
	return out, nil
}

// rbCleanClone decides about one bare clone, and deletes it.
func rbCleanClone(ctx context.Context, a *App, path, hash string, dry bool) (api.Clean, *api.Problem, error) {
	c := api.Clean{Path: path, Kind: "git", Bytes: int(dirSize(path))}
	g := git.New(path)
	url, _ := g.Run(ctx, "config", "--get", "remote.origin.url")
	url = strings.TrimSpace(url)
	if !dry {
		p := a.Paths()
		l, err := lock.Acquire(ctx, p.CloneLock(hash), lock.Holder{Command: a.cmdName(), PID: os.Getpid(), Host: hostname(), User: username(a.Getenv), Started: a.Now()},
			max(a.LockWait(), workspace.CloneLockMinWait), false,
			lock.What{Code: api.PGitBusy, Name: "the clone " + path + " (" + url + ")", Lock: "the clone lock", Retry: "repoctl clean --git --lock-wait 10m"})
		if err != nil {
			return c, nil, err
		}
		defer l.Release()
		if _, err := g.Run(ctx, "worktree", "prune"); err != nil {
			c.Action, c.Reason = "kept", "git worktree prune failed: "+firstLine(err.Error())
			return c, nil, nil
		}
	}
	trees, err := rbCloneWorktrees(ctx, g)
	if err != nil {
		c.Action, c.Reason = "kept", "git worktree list failed: "+firstLine(err.Error())
		return c, nil, nil
	}
	if len(trees) > 0 {
		c.Action, c.Reason = "kept", "worktree "+rbTilde(a, trees[0])
		if len(trees) > 1 {
			c.Reason += fmt.Sprintf(" and %d more", len(trees)-1)
		}
		return c, nil, nil
	}
	unpushed, n, err := rbCloneUnpushed(ctx, g)
	if err != nil {
		c.Action, c.Reason = "kept", "the check of unpushed commits failed: "+firstLine(err.Error())
		return c, nil, nil
	}
	if n > 0 {
		c.Action, c.Reason = "kept", fmt.Sprintf("%s not on the remote (%s)", plural(n, "commit"), strings.Join(unpushed, ", "))
		w := api.Problem{Code: api.PGitMirrorUnpushed, Path: path,
			Message: fmt.Sprintf("the clone %s of %s keeps %s that the remote does not have, on %s", rbTilde(a, path), url, plural(n, "commit"), strings.Join(unpushed, ", ")),
			Hint:    api.HintText("push the commits", "git -C "+path+" push origin "+strings.Join(unpushed, " "))}
		return c, &w, nil
	}
	c.Reason = "no worktree, each commit on the remote"
	if url != "" {
		c.Reason += " (" + url + ")"
	}
	if dry {
		c.Action = "would-delete"
		return c, nil, nil
	}
	if err := os.RemoveAll(path); err != nil {
		return c, nil, err
	}
	c.Action = "deleted"
	return c, nil, nil
}

// rbCloneWorktrees gives the worktrees of a bare clone that a prune would
// keep: not the bare entry, not a prunable one.
func rbCloneWorktrees(ctx context.Context, g *git.Git) ([]string, error) {
	out, err := g.Output(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var trees []string
	for _, block := range strings.Split(string(out), "\n\n") {
		path, bare, prunable := "", false, false
		for _, l := range strings.Split(block, "\n") {
			k, v, _ := strings.Cut(l, " ")
			switch k {
			case "worktree":
				path = v
			case "bare":
				bare = true
			case "prunable":
				prunable = true
			}
		}
		if path != "" && !bare && !prunable {
			trees = append(trees, path)
		}
	}
	return trees, nil
}

// rbCloneUnpushed gives the local branches of a clone with commits that no
// remote branch holds, and the number of those commits.
func rbCloneUnpushed(ctx context.Context, g *git.Git) ([]string, int, error) {
	branches, err := g.Lines(ctx, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return nil, 0, err
	}
	var out []string
	total := 0
	for _, b := range branches {
		if b == "" {
			continue
		}
		n, err := g.Run(ctx, "rev-list", "--count", "refs/heads/"+b, "--not", "--remotes")
		if err != nil {
			return nil, 0, err
		}
		if k, _ := strconv.Atoi(strings.TrimSpace(n)); k > 0 {
			out = append(out, b)
			total += k
		}
	}
	return out, total, nil
}

func rbFileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func rbParseAge(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		d, err := strconv.Atoi(n)
		if err != nil || d < 0 {
			return 0, errors.New("use a duration such as 30d or 12h")
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// rbCleanStaging gives what clean --staging does with each staging of the
// set: it keeps a staging with staged nodes, the staging of the newest and
// of a running run, and the staging of a run of another host.
func rbCleanStaging(a *App, set *config.Set, older time.Duration, dry bool) []api.Clean {
	p := a.Paths()
	dir := filepath.Join(p.SetData(set.Name), "staging")
	entries, _ := os.ReadDir(dir)
	if len(entries) == 0 {
		return nil
	}
	idx, _ := staging.Load(p, set.Name, hostname())
	stagedRuns := map[api.RunId][]api.NodeId{}
	if idx != nil {
		for _, e := range idx.Entries() {
			pkg, t := api.SplitNode(e.Node)
			if rbInLatest(p, set, t, pkg, e.Commit) {
				continue
			}
			stagedRuns[e.Run] = append(stagedRuns[e.Run], e.Node)
		}
	}
	ro := journal.Readers(p, set.Name, hostname())
	runs, _ := journal.List(ro, set.Name, true)
	byID := map[api.RunId]*api.Run{}
	newest := ""
	for _, e := range runs {
		r := e.Run()
		byID[r.ID] = r
		if r.Kind == journal.KindBuild && newest == "" && r.Origin.Host == hostname() {
			newest = r.ID
		}
	}
	var out []api.Clean
	for _, en := range entries {
		id := en.Name()
		path := filepath.Join(dir, id)
		c := api.Clean{Path: path, Kind: "staging", Bytes: int(dirSize(path)), Run: id}
		r := byID[id]
		fi, _ := en.Info()
		switch {
		case len(stagedRuns[id]) > 0:
			c.Action, c.Reason = "kept", "staged nodes: "+strings.Join(stagedRuns[id], ", ")
		case r != nil && (r.State == api.RunStateRunning || r.State == api.RunStatePlanning):
			c.Action, c.Reason = "kept", "running"
		case r != nil && r.Origin.Host != "" && r.Origin.Host != hostname():
			c.Action, c.Reason = "kept", "a run of "+r.Origin.Host
		case id == newest:
			c.Action, c.Reason = "kept", "newest run"
		case fi != nil && older > 0 && a.Now().Sub(fi.ModTime()) < older:
			c.Action, c.Reason = "kept", "newer than --older-than"
		default:
			c.Action = "deleted"
			if dry {
				c.Action = "would-delete"
			}
			switch {
			case r == nil:
				c.Reason = "no journal"
			case r.State == api.RunStateOk:
				c.Reason = "published or built again"
			default:
				c.Reason = string(r.State) + ", no staged node"
			}
			if !dry {
				_ = os.RemoveAll(path)
			}
		}
		out = append(out, c)
	}
	return out
}

func rbInLatest(p config.Paths, set *config.Set, target, pkg, commit string) bool {
	m, err := manifest.ReadLocal(p, set, target)
	if err != nil || m == nil {
		return false
	}
	b, ok := m.Find(commit, "")
	return ok && b.Package == pkg
}

func rbCleanResult(items []any, freed int64, dry bool) *Result {
	return &Result{Kind: api.KindClean, Items: items, Human: func(o *Out) error {
		for _, it := range items {
			c := it.(api.Clean)
			verb := "delete"
			if c.Action == "kept" {
				verb = "keep"
			}
			name := c.Run
			if name == "" {
				name = filepath.Base(c.Path)
			}
			line := fmt.Sprintf("%-6s  %s %s", verb, c.Kind, name)
			if c.Reason != "" {
				line += "   " + c.Reason
			}
			fmt.Fprintln(o.Stdout, line)
		}
		if dry {
			fmt.Fprintf(o.Stdout, "would free %s\n", HumanBytes(freed))
		} else if freed > 0 {
			fmt.Fprintf(o.Stdout, "freed %s\n", HumanBytes(freed))
		} else if len(items) == 0 {
			o.Note("nothing to clean")
		}
		return nil
	}}
}

// ---------------------------------------------------------------- status

func hStatus(ctx context.Context, a *App, _ *cobra.Command, _ []string) (*Result, error) {
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	p := a.Paths()
	host := hostname()
	st := api.Status{Set: api.StatusSet{Name: set.Name, Config: filepath.Join(setDir, "repoctl.yaml")}}
	if rp, ok := set.ReleasePackage(); ok {
		st.Set.Package = rp.ID
	}
	g := git.New(setDir)
	sg := &api.StatusSetGit{}
	sg.Branch, _ = g.CurrentBranch(ctx)
	sg.Commit, _ = g.RevParse(ctx, "HEAD")
	sg.Dirty, _ = g.Dirty(ctx)
	if sg.Branch != "" && sg.Commit != "" {
		if remote, err := g.RevParse(ctx, "refs/remotes/"+set.Remote()+"/"+sg.Branch); err == nil {
			sg.Pushed, _ = g.IsAncestor(ctx, sg.Commit, remote)
		}
	}
	st.Set.Git = sg
	for _, t := range set.Targets {
		st.Targets = append(st.Targets, api.Target{ID: t.ID, Mock: t.Mock, Plugins: t.Plugins, Repos: t.Repos, Path: t.Path, State: t.State})
	}
	lf, err := lock.Read(setDir)
	if err != nil {
		return nil, err
	}
	var describe map[string]string
	var states []workspace.PackageState
	st.Packages, describe, states = rbLockStates(ctx, p, set, setDir, lf)
	snaps := rbStatusSnapshots(ctx, a, set, setDir)
	for _, sv := range snaps {
		st.Snapshots = append(st.Snapshots, sv.item)
		a.warnAll(sv.warns)
	}
	st.Signing = rbStatusSigning(a, set, setDir)
	if h, _ := lock.ReadHolder(p.SetLock(set.Name)); h != nil {
		st.LockHolder = &api.StatusLockHolder{Command: h.Command, PID: h.PID, Host: h.Host, User: h.User, Started: api.FormatTime(h.Started), Run: h.Run}
	}
	ro := journal.Readers(p, set.Name, host)
	var last *api.Run
	if es, err := journal.List(ro, set.Name, false); err == nil {
		for _, e := range es {
			r := e.Run()
			if r.Kind != journal.KindBuild {
				continue
			}
			sum := &api.RunSummary{ID: r.ID, Kind: r.Kind, State: r.State, Origin: r.Origin.Via, By: r.Origin.User, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
			c := r.Counts
			sum.Counts = &c
			if (r.State == api.RunStateRunning || r.State == api.RunStatePlanning) && st.ActiveRun == nil {
				st.ActiveRun = sum
				continue
			}
			if st.LastRun == nil {
				st.LastRun, last = sum, r
			}
		}
	}
	idx, _ := staging.Load(p, set.Name, host)
	if srcs, err := workspace.Sources(ctx, p, set, setDir, lf, workspace.FromLock); err == nil {
		inf := &api.StatusInFlight{}
		for _, n := range set.Nodes() {
			pkg, t := api.SplitNode(n)
			src, ok := srcs[pkg]
			if !ok || rbInLatest(p, set, t, pkg, src.Commit) {
				continue
			}
			inf.Nodes++
			if idx != nil {
				if sb, ok := idx.Find(t, pkg, src.Commit); ok && !sb.Missing {
					inf.Staged++
					continue
				}
			}
			inf.Build++
		}
		if inf.Nodes > 0 {
			st.InFlight = inf
		}
	}
	if idx != nil {
		for run := range idx.Runs() {
			path := p.Staging(set.Name, run)
			if _, err := os.Stat(path); err == nil {
				st.Staging = append(st.Staging, api.StatusStagingItem{Run: run, Path: path, SizeBytes: int(dirSize(path))})
			}
		}
		slices.SortFunc(st.Staging, func(x, y api.StatusStagingItem) int { return strings.Compare(x.Run, y.Run) })
	}
	advs := rbStatusAdvisories(a, &st, set.Name, setDir)
	timer := a.advTimer(setDir).State(ctx)
	if st.Check != nil {
		st.Check.Timer = timer
	}
	st.Open = rbStatusSetup(ctx, a, set, setDir, states, sg, snaps, timer)
	st.Next = rbStatusNext(st, last, snaps)
	return &Result{Kind: api.KindStatus, Items: []any{st}, Next: st.Next, ShownWarnings: true, Human: func(o *Out) error {
		rbStatusHuman(o, a, setDir, st, last, describe, advs, snaps, timer)
		return nil
	}}, nil
}

// rbSnap is the snapshot row of one target in status: the item of the
// contract and what the terminal shows beyond it.
type rbSnap struct {
	item api.StatusSnapshot
	// fresh is the freshness check; nil without store.oci.
	fresh *localcopy.Freshness
	// local is snapshot.json of the local copy, or nil.
	local  *api.Snapshot
	served *localcopy.Served
	warns  []api.Problem
	// stale is the warning served.stale, or nil.
	stale *api.Problem
}

// rbStatusSnapshots gives the snapshot rows of status (publish.html#freshness,
// the status column): latest of each active target with one read of the
// registry, the local copy, the served tree of repo.url and the heartbeat
// of its mirror line (stale served tree). The reads of the registry and of
// the HTTP server run at the same time.
func rbStatusSnapshots(ctx context.Context, a *App, set *config.Set, setDir string) []rbSnap {
	p := a.Paths()
	var targets []string
	for _, t := range set.ActiveTargets() {
		targets = append(targets, t.ID)
	}
	gate := rbFreshGate(a, set, setDir)
	locals := map[string]*api.Snapshot{}
	var readTargets []string
	for _, t := range targets {
		locals[t], _ = localcopy.ReadSnapshot(localcopy.Dir(p, set.Name, t))
		if gate.Enabled() || locals[t] != nil {
			readTargets = append(readTargets, t)
		}
	}
	var fresh []localcopy.Freshness
	var served []localcopy.Served
	done := make(chan struct{})
	go func() {
		defer close(done)
		served = localcopy.ReadServed(ctx, nil, set, readTargets)
	}()
	fresh = gate.Check(ctx, targets)
	<-done
	byFresh := map[string]*localcopy.Freshness{}
	for i := range fresh {
		byFresh[fresh[i].Target] = &fresh[i]
	}
	byServed := map[string]*localcopy.Served{}
	for i := range served {
		byServed[served[i].Target] = &served[i]
	}
	now := a.Now()
	var out []rbSnap
	for _, t := range targets {
		sv := rbSnap{fresh: byFresh[t], local: locals[t], served: byServed[t]}
		sv.item = api.StatusSnapshot{Target: t, Local: "none", Served: "unknown"}
		known := true // latest of the registry is known
		switch f := sv.fresh; {
		case f == nil:
			// No store.oci: the local copy is all that status knows.
			if sv.local != nil {
				sv.item.Latest, sv.item.Local = rbLatestOfLocal(sv.local), "same"
			} else if m, err := manifest.ReadLocal(p, set, t); err == nil && m != nil {
				sv.item.Local = "same"
				sv.item.Latest = &api.StatusSnapshotLatest{Publish: m.Publish, Digest: "sha256:" + strings.Repeat("0", 64)}
				if d, err := rbLocalDigest(p, set, t); err == nil {
					sv.item.Latest.Digest = d
				}
			}
			known = false
		case f.Result == localcopy.FreshUnreachable:
			// registry: unknown. latest is what this host last saw.
			if sv.local != nil {
				sv.item.Latest, sv.item.Local = rbLatestOfLocal(sv.local), "same"
			}
			known = false
			sv.warns = append(sv.warns, *f.Warning())
		case f.Result == localcopy.FreshMissing:
			sv.item.Local = f.StatusLocal()
			known = false
			w := f.Warning()
			w.Hint = api.HintText("re-seed the OCI package from the newest local copy", "repoctl repo push --reseed --target "+t)
			sv.warns = append(sv.warns, *w)
		case f.Result == localcopy.FreshFirst:
		default:
			sv.item.Local = f.StatusLocal()
			if f.Latest != nil {
				sv.item.Latest = &api.StatusSnapshotLatest{Publish: f.Latest.Meta.Publish, Digest: f.Latest.Digest(), Kind: f.Latest.Meta.Kind,
					User: f.Latest.Meta.User, Host: f.Latest.Meta.Host, At: api.FormatTime(f.Latest.Meta.Created)}
			} else if sv.local != nil {
				sv.item.Latest = rbLatestOfLocal(sv.local)
			}
			if w := f.OrphanedWarning(); w != nil {
				sv.warns = append(sv.warns, *w)
			}
		}
		if s := sv.served; s != nil {
			switch {
			case known && sv.item.Latest != nil:
				sv.item.Served = localcopy.ServedState(s, sv.item.Latest.Digest)
			case sv.local != nil && localcopy.ServedState(s, sv.local.Digest) == "same":
				sv.item.Served = "same"
			}
			if s.Mirror != nil {
				sv.item.Mirror = s.Mirror.API(now)
				sv.stale = s.Mirror.StaleWarning(t, now, rbClock(now))
				if sv.stale != nil {
					sv.warns = append(sv.warns, *sv.stale)
				}
			}
		}
		out = append(out, sv)
	}
	return out
}

// rbLatestOfLocal gives latest from snapshot.json of the local copy, the
// cache of the annotations.
func rbLatestOfLocal(s *api.Snapshot) *api.StatusSnapshotLatest {
	l := &api.StatusSnapshotLatest{Publish: s.Publish, Digest: s.Digest, User: s.User, Host: s.Host, At: s.Created}
	for _, k := range snapshot.Kinds {
		if s.Kind == k {
			l.Kind = s.Kind
		}
	}
	return l
}

// rbClock formats a time of today as 15:04, else as 2006-01-02 15:04, in
// the time zone of the host.
func rbClock(now time.Time) func(time.Time) string {
	return func(t time.Time) string {
		t, n := t.Local(), now.Local()
		if t.Year() == n.Year() && t.YearDay() == n.YearDay() {
			return t.Format("15:04")
		}
		return t.Format("2006-01-02 15:04")
	}
}

// rbStatusSigning gives the keys that need attention (Status.signing): a
// retiring or removed key that RPMs in latest carry, and a key of the key
// file that expires within sign.checks.key_expiry_warn (signing.html#expiry).
func rbStatusSigning(a *App, set *config.Set, setDir string) []api.StatusSigningItem {
	var targets []string
	for _, t := range set.ActiveTargets() {
		targets = append(targets, t.ID)
	}
	v, err := sign.NewView(set, setDir, a.Paths(), targets)
	if err != nil {
		return nil
	}
	v.Now = a.Now()
	var out []api.StatusSigningItem
	for _, k := range v.Trust.InState(config.KeyRetiring, config.KeyRemoved) {
		for _, t := range targets {
			if n := v.RPMs(k, t); n > 0 {
				out = append(out, api.StatusSigningItem{KeyID: k.ID, State: k.State, Target: t, RPMs: n})
			}
		}
	}
	warn, _ := sign.Checks(set)
	for _, k := range v.Trust.KeyfileKeys() {
		if k.Cert != nil && !k.Cert.Expires.IsZero() && k.Cert.ExpiresIn(v.Now, warn) {
			out = append(out, api.StatusSigningItem{KeyID: k.ID, State: k.State, Expires: api.FormatTime(k.Cert.Expires)})
		}
	}
	return out
}

// rbStatusAdvisories fills the check and the upstream updates of status
// from check.json and the dismissal file, and gives the open advisories.
func rbStatusAdvisories(a *App, st *api.Status, set, setDir string) []api.Advisory {
	cf, err := advisory.ReadCheckFile(a.Paths(), set)
	if err != nil || cf == nil {
		return nil
	}
	sum := cf.Summary()
	chk := &api.StatusCheck{At: cf.At, Failed: sum.Failed}
	var open []api.Advisory
	for _, adv := range advisory.Current(cf, a.loadDismissals(setDir), nil) {
		if adv.State != api.AdvisoryStateOpen {
			continue
		}
		open = append(open, adv)
		switch adv.Level {
		case api.AdvisoryLevelRequired:
			chk.Required++
		case api.AdvisoryLevelWarn:
			chk.Warn++
		default:
			chk.Info++
		}
		if adv.Code == api.AdvisoryCodeUpstreamRelease {
			var f api.FactsUpstreamRelease
			_ = advisory.DecodeFacts(adv, &f)
			if st.Upstream == nil {
				st.Upstream = &api.StatusUpstream{}
			}
			st.Upstream.Updates = append(st.Upstream.Updates, api.StatusUpstreamUpdate{Package: adv.Subject, Version: f.Upstream})
		}
	}
	st.Check = chk
	return open
}

// rbAdvisoryLine gives the advisories line of status: "1 required
// (closure.broken/pmix@el9), 3 info".
func rbAdvisoryLine(c *api.StatusCheck, open []api.Advisory) string {
	var parts []string
	for _, l := range []struct {
		n     int
		level api.AdvisoryLevel
	}{{c.Required, api.AdvisoryLevelRequired}, {c.Warn, api.AdvisoryLevelWarn}, {c.Info, api.AdvisoryLevelInfo}} {
		if l.n == 0 {
			continue
		}
		part := fmt.Sprintf("%d %s", l.n, l.level)
		if l.level != api.AdvisoryLevelInfo {
			var ids []string
			for _, adv := range open {
				if adv.Level == l.level && len(ids) < 3 {
					ids = append(ids, adv.ID)
				}
			}
			part += " (" + strings.Join(ids, ", ") + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// rbLocalDigest gives the digest of the local copy of a target: the sha256
// of its repomd.xml.
func rbLocalDigest(p config.Paths, set *config.Set, target string) (string, error) {
	sum, _, err := staging.SHA256File(filepath.Join(manifest.LocalRepo(p, set, target), "repodata", "repomd.xml"))
	if err != nil {
		return "", err
	}
	return "sha256:" + sum, nil
}

// rbLockStates gives the lock state of each package (workspace.States),
// and of each orphaned lock line, with the words of each state.
func rbLockStates(ctx context.Context, p config.Paths, set *config.Set, setDir string, lf *lock.File) ([]api.StatusPackage, map[string]string, []workspace.PackageState) {
	states, err := workspace.States(ctx, p, set, setDir, lf)
	if err != nil {
		return nil, nil, nil
	}
	out := make([]api.StatusPackage, 0, len(states))
	describe := map[string]string{}
	for _, s := range states {
		out = append(out, api.StatusPackage{Package: s.Package, Lock: s.State, LockCommit: s.LockCommit, HeadCommit: s.Head})
		describe[s.Package] = s.Describe()
	}
	return out, describe, states
}

// rbStatusNext gives the Next step of status.
func rbStatusNext(st api.Status, last *api.Run, snaps []rbSnap) []api.NextStep {
	switch {
	case st.ActiveRun != nil:
		return []api.NextStep{{Action: "other", Reason: "run " + st.ActiveRun.ID + " is running", Command: "repoctl runs " + st.ActiveRun.ID + " --follow"}}
	case last != nil && last.State == api.RunStateFailed:
		for _, n := range last.Nodes {
			if n.State == api.NodeStateFailed {
				return []api.NextStep{{Action: "triage", Reason: n.Node + " failed in run " + last.ID, Command: "repoctl logs " + last.ID + " " + n.Node}}
			}
		}
	}
	// The local copy first: plan and build stop while it is behind.
	var behind []string
	for _, sv := range snaps {
		f := sv.fresh
		if f == nil {
			continue
		}
		switch f.Result {
		case localcopy.FreshNotCached, localcopy.FreshUnpushed:
			return []api.NextStep{{Action: "other", Human: true, Reason: "publish " + f.Publish + " of " + f.Target + " is not complete",
				Command: "repoctl repo push " + f.Publish + " --target " + f.Target}}
		case localcopy.FreshBehind:
			behind = append(behind, f.Target)
		}
	}
	if len(behind) > 0 {
		cmd := "repoctl repo pull"
		if len(behind) < len(snaps) {
			cmd += " --target " + strings.Join(behind, ",")
		}
		return []api.NextStep{{Action: "other", Reason: "latest of " + strings.Join(behind, ", ") + " is newer than the local copy", Command: cmd}}
	}
	var ahead []string
	for _, p := range st.Packages {
		// An unpinned package without a worktree needs a checkout first
		// (the setup step All packages pinned).
		if p.Lock == api.LockStateAhead || (p.Lock == api.LockStateUnpinned && p.HeadCommit != "") {
			ahead = append(ahead, p.Package)
		}
	}
	if len(ahead) > 0 {
		return []api.NextStep{{Action: "pin", Reason: "a worktree has a commit that is not pinned", Command: "repoctl pin " + strings.Join(ahead, " ")}}
	}
	if st.InFlight != nil && st.InFlight.Build > 0 {
		return []api.NextStep{{Action: "build", Reason: fmt.Sprintf("%d pinned nodes have no build", st.InFlight.Build), Command: "repoctl build"}}
	}
	if st.InFlight != nil && st.InFlight.Staged > 0 {
		return []api.NextStep{{Action: "publish", Human: true, Reason: fmt.Sprintf("%d staged nodes are not published", st.InFlight.Staged), Command: "repoctl publish"}}
	}
	// Then the first open setup step with a command (lifecycle.html#setup).
	for _, o := range st.Open {
		if o.Command == "" {
			continue
		}
		human := strings.HasPrefix(o.Command, "git ") || strings.HasPrefix(o.Command, "repoctl publish") || strings.HasPrefix(o.Command, "repoctl sign ")
		action := "other"
		if strings.HasPrefix(o.Command, "repoctl publish") {
			action = "publish"
		}
		return []api.NextStep{{Action: action, Human: human, Reason: "setup: " + o.Step, Command: o.Command}}
	}
	return nil
}

func rbStatusHuman(o *Out, a *App, setDir string, st api.Status, last *api.Run, describe map[string]string, advs []api.Advisory, snaps []rbSnap, timer string) {
	so := o.StdoutStyle
	gitText := ""
	if g := st.Set.Git; g != nil && g.Commit != "" {
		gitText = fmt.Sprintf("  (%s at %s", g.Branch, rbShort(g.Commit))
		switch {
		case g.Dirty:
			gitText += ", uncommitted changes"
		case g.Pushed:
			gitText += ", pushed"
		default:
			gitText += ", not pushed"
		}
		gitText += ")"
	}
	o.KV(10, "set", st.Set.Name+"  "+rbTilde(a, setDir)+gitText)
	if o.Verbose > 0 {
		if a.set != nil && a.set.Store.OCI != "" {
			o.KV(10, "store", a.set.Store.OCI)
		}
		rbMockLines(o, st.Targets)
	}
	for i, line := range rbSnapLines(snaps, o.Verbose > 0, a.Now()) {
		key := ""
		if i == 0 {
			key = "published"
		}
		o.KV(10, key, line)
	}
	for i, op := range st.Open {
		key := ""
		if i == 0 {
			key = "setup"
		}
		text := op.Step
		if text != "" {
			text = strings.ToLower(text[:1]) + text[1:]
		}
		o.KV(10, key, so.Amber(text))
	}
	if st.ActiveRun != nil {
		o.KV(10, "active run", fmt.Sprintf("%s  %s  %s", st.ActiveRun.ID, rbTime(st.ActiveRun.StartedAt), so.State(string(st.ActiveRun.State))))
	}
	if last != nil {
		text := fmt.Sprintf("%s  %s  %s", last.ID, rbTime(last.StartedAt), so.State(string(last.State)))
		var failed []string
		for _, n := range last.Nodes {
			if n.State == api.NodeStateFailed {
				failed = append(failed, n.Node)
			}
		}
		if len(failed) > 0 {
			text += " (" + strings.Join(failed, ", ") + ")"
		}
		for _, b := range last.PublishBlockers {
			text += ", " + b.Target + " is not publishable"
			break
		}
		o.KV(10, "last run", text)
	}
	if st.Check != nil {
		if line := rbAdvisoryLine(st.Check, advs); line != "" {
			o.KV(10, "advisories", line)
		}
		switch {
		case len(st.Check.Failed) > 0:
			o.KV(10, "check", fmt.Sprintf("%s  %s", rbTime(st.Check.At), so.Red(fmt.Sprintf("%d failed", len(st.Check.Failed)))))
		case o.Verbose > 0:
			o.KV(10, "check", rbTime(st.Check.At)+"  ok"+rbTimerText(st.Check.Timer))
		}
	} else if o.Verbose > 0 {
		o.KV(10, "check", "never ran on this host"+rbTimerText(timer))
	}
	if st.Upstream != nil && len(st.Upstream.Updates) > 0 {
		var ups []string
		for _, u := range st.Upstream.Updates {
			ups = append(ups, u.Package+" "+u.Version)
		}
		o.KV(10, "upstream", fmt.Sprintf("%d %s (%s)", len(ups), map[bool]string{true: "update", false: "updates"}[len(ups) == 1], strings.Join(ups, ", ")))
	}
	for i, sg := range st.Signing {
		key := ""
		if i == 0 {
			key = "signing"
		}
		o.KV(10, key, rbSigningText(sg))
	}
	if st.InFlight != nil {
		o.KV(10, "in flight", fmt.Sprintf("%d nodes: %d staged here, %d to build", st.InFlight.Nodes, st.InFlight.Staged, st.InFlight.Build))
	}
	if o.Verbose > 0 {
		o.KV(10, "staging", rbStagingText(st.Staging))
	}
	t := NewTable("PKG", "LOCK", "WORKTREE", "STATE")
	counts := map[api.LockState]int{}
	var order []api.LockState
	for _, p := range st.Packages {
		if counts[p.Lock] == 0 {
			order = append(order, p.Lock)
		}
		counts[p.Lock]++
		if (p.Lock == api.LockStateInSync || p.Lock == api.LockStateInTree || p.Lock == api.LockStateMissing) && o.Verbose == 0 {
			continue
		}
		words, lock := describe[p.Package], p.Lock
		if words == "" {
			words = string(p.Lock)
		}
		t.RowCells(Cell{Text: p.Package}, Cell{Text: rbShort(p.LockCommit)}, Cell{Text: rbShort(p.HeadCommit)}, C(words, func(x string) string {
			switch lock {
			case api.LockStateAhead, api.LockStateBehind, api.LockStateDirty, api.LockStateDiverged:
				return so.Amber(x)
			}
			return x
		}))
	}
	if len(t.Rows) > 0 {
		_ = o.Table(t)
	}
	var parts []string
	for _, s := range order {
		w := string(s)
		if s == api.LockStateInTree {
			w = "in tree"
		}
		parts = append(parts, fmt.Sprintf("%d %s", counts[s], w))
	}
	more := "  (-v shows all)"
	if o.Verbose > 0 {
		more = ""
	}
	fmt.Fprintf(o.Stdout, "%d packages: %s%s\n", len(st.Packages), strings.Join(parts, ", "), more)
	for _, w := range a.warnings {
		o.Warning(w)
	}
	for _, adv := range advs {
		if adv.Level == api.AdvisoryLevelRequired {
			o.Hint("read the facts and the closure", "repoctl advisories show "+adv.ID)
			break
		}
	}
	for _, sg := range st.Signing {
		if sg.RPMs > 0 {
			o.Hint("re-sign the RPMs of the "+sg.State+" key", "repoctl repo resign --signer "+sg.KeyID+" --target "+sg.Target)
			break
		}
	}
	rbSnapHints(o, snaps)
}

// rbMockLines writes the mock config of each active target (status -v).
func rbMockLines(o *Out, targets []api.Target) {
	w := 0
	for _, t := range targets {
		w = max(w, len(t.ID))
	}
	i := 0
	for _, t := range targets {
		if t.State == "retired" {
			continue
		}
		key := ""
		if i == 0 {
			key = "mock"
		}
		i++
		text := fmt.Sprintf("%-*s  %s", w, t.ID, t.Mock)
		if len(t.Plugins) > 0 {
			text += ", plugins " + strings.Join(t.Plugins, " ")
		}
		o.KV(10, key, text)
	}
}

// rbTimerText gives the state of the check timer for the check line.
func rbTimerText(timer string) string {
	switch timer {
	case "active":
		return ", timer active"
	case "off":
		return ", timer off"
	}
	return ""
}

// rbStagingText gives the staging line of status -v: the staging repos on
// disk with their size.
func rbStagingText(items []api.StatusStagingItem) string {
	if len(items) == 0 {
		return "none"
	}
	var total int64
	var runs []string
	for _, it := range items {
		total += int64(it.SizeBytes)
		runs = append(runs, it.Run)
	}
	return fmt.Sprintf("%d %s, %s (%s)", len(items), rbPlural(len(items), "run", "runs"), HumanBytes(total), strings.Join(runs, ", "))
}

// rbSnapRow is one published line of status: the targets with the same
// words.
type rbSnapRow struct {
	targets []string
	text    string
}

// rbAddRow adds a target to the row with the same words, or a new row.
func rbAddRow(rows []*rbSnapRow, target, text string) []*rbSnapRow {
	for _, r := range rows {
		if r.text == text {
			r.targets = append(r.targets, target)
			return rows
		}
	}
	return append(rows, &rbSnapRow{targets: []string{target}, text: text})
}

// rbSnapLines gives the published lines of status (terminal.html#status):
// one line per target, or one for the targets with the same words.
// verbose adds who made latest (publish.html#freshness).
func rbSnapLines(snaps []rbSnap, verbose bool, now time.Time) []string {
	var rows []*rbSnapRow
	clock := rbClock(now)
	for _, sv := range snaps {
		it := sv.item
		var parts []string
		unsure := sv.fresh != nil && (sv.fresh.Result == localcopy.FreshUnreachable || sv.fresh.Result == localcopy.FreshMissing)
		switch {
		case sv.fresh != nil && sv.fresh.Result == localcopy.FreshUnreachable:
			parts = append(parts, "registry unknown")
		case sv.fresh != nil && sv.fresh.Result == localcopy.FreshMissing:
			parts = append(parts, "latest missing")
		case it.Latest == nil:
			rows = rbAddRow(rows, it.Target, "not published")
			continue
		default:
			parts = append(parts, it.Latest.Publish)
			if t := rbParseTime(it.Latest.At); !t.IsZero() {
				parts = append(parts, t.Local().Format("2006-01-02 15:04"))
			}
			if verbose && it.Latest.User != "" {
				by := it.Latest.User
				if it.Latest.Host != "" {
					by += "@" + it.Latest.Host
				}
				parts = append(parts, by)
			}
		}
		local := "local " + it.Local
		if unsure {
			local = "no local copy"
			if sv.local != nil {
				local = "local copy " + sv.local.Publish
			}
		}
		served := "served " + it.Served
		if it.Served == "behind" {
			switch {
			case sv.stale != nil && it.Mirror != nil && it.Mirror.BehindSince != nil:
				served += " since " + clock(rbParseTime(*it.Mirror.BehindSince))
			case sv.served != nil && sv.served.Snapshot != nil && sv.served.Snapshot.Publish != "":
				served += " (" + sv.served.Snapshot.Publish + ")"
			}
		}
		rows = rbAddRow(rows, it.Target, strings.Join(parts, "  ")+"  "+local+", "+served)
	}
	w := 4
	for _, r := range rows {
		w = max(w, len(strings.Join(r.targets, " ")))
	}
	var out []string
	for _, r := range rows {
		out = append(out, pad(strings.Join(r.targets, " "), w)+"  "+r.text)
	}
	return out
}

// rbParseTime reads a time of the contract, or gives the zero time.
func rbParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// rbSigningText gives the words of one key of the signing line of status.
func rbSigningText(sg api.StatusSigningItem) string {
	if sg.RPMs > 0 {
		return fmt.Sprintf("%s is %s: %d RPMs in latest of %s carry it", sg.KeyID, sg.State, sg.RPMs, sg.Target)
	}
	if t := rbParseTime(sg.Expires); !t.IsZero() {
		return fmt.Sprintf("%s expires on %s", sg.KeyID, t.UTC().Format("2006-01-02"))
	}
	return sg.KeyID + " is " + sg.State
}

// rbSnapHints gives the hints of the snapshot rows: a pushable tree that
// is not complete, and the next run of the mirror job while a served tree
// is behind.
func rbSnapHints(o *Out, snaps []rbSnap) {
	behind := false
	for _, sv := range snaps {
		if f := sv.fresh; f != nil {
			switch f.Result {
			case localcopy.FreshNotCached:
				o.Hint("publish "+f.Publish+" is in latest, the cache step did not run", "repoctl repo push "+f.Publish+" --target "+f.Target)
			case localcopy.FreshUnpushed:
				o.Hint("complete the publish "+f.Publish+", or discard it with repoctl clean --publish "+f.Publish, "repoctl repo push "+f.Publish+" --target "+f.Target)
			}
		}
		if sv.item.Served == "behind" && sv.stale == nil {
			behind = true
		}
	}
	if behind {
		o.Hint("the mirror job on the HTTP server runs every 5 min", "")
	}
}

// ---------------------------------------------------------------- mock

// rbHostMockLock takes the host mock lock: exclusive for mock update and
// clean, shared for a build and mock shell.
func rbHostMockLock(ctx context.Context, a *App, shared bool) (*lock.SetLock, error) {
	p := a.Paths()
	return lock.Acquire(ctx, p.HostMockLock(), lock.Holder{Command: a.cmdName(), PID: os.Getpid(), Host: hostname(), User: username(a.Getenv), Started: a.Now()},
		a.LockWait(), shared, lock.What{Code: api.PMockBusy, Name: "the host mock lock", Lock: "the host mock lock", Retry: "repoctl " + a.cmdName() + " --lock-wait 2h"})
}

// rbMockLock takes the set lock and the host mock lock, exclusive.
func rbMockLock(ctx context.Context, a *App, set *config.Set) (func(), error) {
	sl, err := a.SetLock(ctx, set, false)
	if err != nil {
		return nil, err
	}
	hl, err := rbHostMockLock(ctx, a, false)
	if err != nil {
		sl.Release()
		return nil, err
	}
	return func() { hl.Release(); sl.Release() }, nil
}

func rbMock(a *App) *mock.Mock {
	eff, err := a.Settings()
	o := mock.Options{}
	if err == nil {
		o.NoSync, o.TmpFS = eff.Bool("mock.nosync"), eff.String("mock.tmpfs")
	}
	return mock.New(a.Paths(), o)
}

func hMockUpdate(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	if a.JSON() {
		return nil, usagef("repoctl %s has no --json", commandName(cmd))
	}
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	targets, err := a.Targets(set, true)
	if err != nil {
		return nil, err
	}
	p := a.Paths()
	if err := a.Preflight(ctx, doctor.NeedMock, targets, 0); err != nil {
		return nil, err
	}
	unlock, err := rbMockLock(ctx, a, set)
	if err != nil {
		return nil, err
	}
	defer unlock()
	m := rbMock(a)
	work := p.MockWork(set.Name, "mock")
	var lines []string
	var failed error
	for _, id := range targets {
		t, _ := set.Target(id)
		files, err := m.WriteFiles(work, mock.FromConfig(*t, setDir))
		if err != nil {
			return nil, err
		}
		ext := mock.VerifyExt(set.Name, id)
		if err := m.Update(ctx, mock.Call{Files: files, Uniqueext: ext, Log: io.Discard}); err != nil {
			lines = append(lines, fmt.Sprintf("%-5s failed: %s", id, err.Error()))
			if failed == nil {
				failed = api.Errorf(api.PHostNotReady, "mock --update of %s failed: %v", id, err).WithTarget(id)
			}
			continue
		}
		lines = append(lines, fmt.Sprintf("%-5s updated  %s (verify root %s)", id, t.Mock, ext))
	}
	return &Result{Human: func(o *Out) error {
		for _, l := range lines {
			fmt.Fprintln(o.Stdout, termtext.Escape(l))
		}
		return nil
	}}, failed
}

func hMockClean(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	if a.JSON() {
		return nil, usagef("repoctl %s has no --json", commandName(cmd))
	}
	set, _, err := a.Set()
	if err != nil {
		return nil, err
	}
	unlock, err := rbMockLock(ctx, a, set)
	if err != nil {
		return nil, err
	}
	defer unlock()
	m := rbMock(a)
	roots, err := m.Roots()
	if err != nil {
		return nil, err
	}
	mark := "-" + config.MockRootPrefix() + "-"
	var done []string
	for _, root := range roots {
		base := filepath.Base(root)
		i := strings.Index(base, mark)
		if i < 0 || strings.Contains(base[:i], "-bootstrap") {
			continue
		}
		call := mock.Call{Files: mock.Files{Config: base[:i]}, Uniqueext: base[i+1:]}
		_ = m.Clean(ctx, call)
		_ = m.Scrub(ctx, call, "bootstrap")
		done = append(done, base)
	}
	return &Result{Human: func(o *Out) error {
		if len(done) == 0 {
			o.Note("no mock roots of repoctl for this user")
		}
		for _, d := range done {
			fmt.Fprintf(o.Stdout, "cleaned  %s\n", d)
		}
		return nil
	}}, nil
}

func hMockShell(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	if a.JSON() {
		return nil, usagef("repoctl %s has no --json", commandName(cmd))
	}
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	t, ok := set.Target(args[0])
	if !ok {
		return nil, usagef("mock shell %s: no such target", args[0])
	}
	if err := a.Preflight(ctx, doctor.NeedMock, []string{t.ID}, 0); err != nil {
		return nil, err
	}
	hl, err := rbHostMockLock(ctx, a, true)
	if err != nil {
		if _, isAPI := api.AsError(err); isAPI {
			return nil, err
		}
	} else {
		defer hl.Release()
	}
	m := rbMock(a)
	work, err := os.MkdirTemp("", "repoctl-shell-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	files, err := m.WriteFiles(work, mock.FromConfig(*t, setDir))
	if err != nil {
		return nil, err
	}
	ext := mock.ShellExt()
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	shellArgs := []string{"-r", files.Config, "--uniqueext", ext, "--macro-file", files.Macros, "--shell"}
	if len(rest) > 0 {
		shellArgs = append(append(shellArgs, "--"), rest...)
	}
	err = (&run.Cmd{Name: m.Program, Args: shellArgs, Stdin: a.Stdin, Stdout: a.Stdout, Stderr: a.Stderr}).Run(ctx)
	_ = m.Scrub(context.WithoutCancel(ctx), mock.Call{Files: files, Uniqueext: ext}, "all")
	if code := run.ExitCode(err); err != nil && code > 0 {
		return &Result{}, &silent{err: cobratree.Coded(code, err)}
	}
	return &Result{}, err
}
