package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dennisklein/repoctl/internal/advisory"
	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/doctor"
	"github.com/dennisklein/repoctl/internal/localcopy"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/publish"
	"github.com/dennisklein/repoctl/internal/publish/tree"
	"github.com/dennisklein/repoctl/internal/publish/wire"
	"github.com/dennisklein/repoctl/internal/sign"
	"github.com/dennisklein/repoctl/internal/snapshot"
	"github.com/dennisklein/repoctl/internal/tools/run"
	"github.com/dennisklein/repoctl/internal/ui"
)

// The handlers of repository: publish and repo.

// EnvUnattended allows publish --yes without a terminal.
const EnvUnattended = "REPOCTL_ALLOW_UNATTENDED_PUBLISH"

// PublishEnv wires the publish runs; tests and the integrator can replace
// it.
var PublishEnv = func(ctx context.Context, a *App, set *config.Set, dir, command string) (*publish.Env, func(), error) {
	eff, err := a.Settings()
	if err != nil {
		return nil, func() {}, err
	}
	opts := mock.Options{NoSync: eff.Bool("mock.nosync"), TmpFS: eff.String("mock.tmpfs")}
	via := api.RunOrigin("cli")
	if a.Getenv("CI") == "true" {
		via = "ci"
	}
	return wire.Env(ctx, wire.Options{Set: set, SetDir: dir, Paths: a.paths, Eff: eff, Getenv: a.Getenv, Now: a.Now,
		User: username(a.Getenv), Host: hostname(), Via: via, Mock: opts, Inputs: a.checkInputs(set, dir), Command: command})
}

func flagStr(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

func flagBool(cmd *cobra.Command, name string) bool {
	v, _ := cmd.Flags().GetBool(name)
	return v
}

func flagStrs(cmd *cobra.Command, name string) []string {
	v, _ := cmd.Flags().GetStringArray(name)
	return v
}

// publishRequest is the common part of a publish run of any kind.
type publishRequest struct {
	req    publish.Request
	dryRun bool
	yes    bool
	verb   string // publish, restore, prune, re-sign, import
	args   []string
	// confirmFD is the file descriptor of the UI confirmation
	// (--confirm-fd), or 0.
	confirmFD int
}

// commandLine gives a command as typed: its path, its arguments and the
// flags that are set, without the flags of drop.
func commandLine(cmd *cobra.Command, args []string, drop ...string) string {
	parts := append([]string{cmd.CommandPath()}, args...)
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if slices.Contains(drop, f.Name) || f.Hidden {
			return
		}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			for _, v := range sv.GetSlice() {
				parts = append(parts, "--"+f.Name, shellWord(v))
			}
			return
		}
		if f.Value.Type() == "bool" {
			if f.Value.String() == "true" {
				parts = append(parts, "--"+f.Name)
			}
			return
		}
		parts = append(parts, "--"+f.Name, shellWord(f.Value.String()))
	})
	return strings.Join(parts, " ")
}

// shellWord quotes a word for sh when it needs quotes.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.,/:@=+") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hPublish runs publish.
func hPublish(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	pr := publishRequest{verb: "publish", dryRun: flagBool(cmd, "dry-run"), yes: flagBool(cmd, "yes")}
	if fd, err := cmd.Flags().GetInt("confirm-fd"); err == nil && cmd.Flags().Changed("confirm-fd") {
		if fd < 3 {
			return nil, usagef("--confirm-fd needs a file descriptor above 2")
		}
		if pr.dryRun || pr.yes {
			return nil, usagef("--confirm-fd excludes --dry-run and --yes")
		}
		pr.confirmFD = fd
	}
	pr.req = publish.Request{Kind: publish.KindPublish, Run: flagStr(cmd, "run"), Hold: flagStrs(cmd, "hold"),
		Reason: flagStr(cmd, "reason"), Accept: flagStrs(cmd, "accept-advisory")}
	if len(pr.req.Hold) > 0 && len(strings.TrimSpace(pr.req.Reason)) < 8 {
		return nil, usagef("--hold needs --reason with 8 characters or more")
	}
	return runPublish(ctx, a, cmd, pr)
}

// runPublish runs a publish run of any kind: step 1, the summary, the
// confirmation and steps 2 to 8.
func runPublish(ctx context.Context, a *App, cmd *cobra.Command, pr publishRequest) (*Result, error) {
	set, dir, err := a.Set()
	if err != nil {
		return nil, err
	}
	targets, err := a.Targets(set, true)
	if err != nil {
		return nil, err
	}
	terminal := a.isTerminal()
	// A UI confirmation stands for the human at the terminal
	// (cli.html#ui-confirmation): read it once, check its origin and its
	// match with the command line.
	var conf *ui.Confirmation
	if pr.confirmFD > 0 {
		if conf, err = a.readConfirmation(pr.confirmFD); err != nil {
			return nil, err
		}
		if err := matchConfirmation(conf, set.Name, a.TargetArgs(), pr.req); err != nil {
			return nil, err
		}
	}
	if err := advisory.ValidateAccept(pr.req.Accept, pr.req.Reason, terminal || conf != nil); err != nil {
		return nil, err
	}
	pr.req.Targets, pr.req.Explicit = targets, len(a.TargetArgs()) > 0 || pr.req.Kind == publish.KindImport
	pr.req.TraceParent = a.Getenv("TRACEPARENT")
	if !pr.dryRun && conf == nil && !terminal && pr.yes {
		if a.Getenv(EnvUnattended) != "1" {
			return nil, api.Errorf(api.PPublishUnattendedRefused, "%s: stdin is not a terminal, and %s is not set", pr.verb, EnvUnattended).
				WithHint("run the command in a terminal and type the set name when asked", "").
				WithHint("for a dedicated publish job only, set "+EnvUnattended+"=1 in its environment", "")
		}
		pr.req.Unattended = true
	}
	lk, err := a.SetLock(ctx, set, false)
	if err != nil {
		return nil, err
	}
	defer lk.Release()
	command := commandLine(cmd, pr.args, "dry-run", "json", "yes")
	env, cleanup, err := PublishEnv(ctx, a, set, dir, command)
	defer cleanup()
	if err != nil {
		return nil, err
	}
	if conf != nil {
		env.Via = "ui"
	}
	var steps []string
	env.Progress = func(target, step, state string, d time.Duration) {
		steps = append(steps, fmt.Sprintf("%-6s %-9s %-7s %s", target, step, state, api.FormatDuration(d)))
		if a.verbose > 0 && !a.jsonOut {
			fmt.Fprintf(a.Stderr, "  %s %s %s %s\n", target, step, state, api.FormatDuration(d))
		}
	}
	plan, err := publish.Prepare(ctx, env, pr.req)
	if err != nil {
		var ue *publish.UsageError
		if errors.As(err, &ue) {
			return nil, usagef("%s", ue.Msg)
		}
		return nil, err
	}
	var size int64
	for _, t := range plan.Targets {
		for _, n := range t.Take {
			for _, f := range n.Files {
				size += f.Size
			}
		}
	}
	if err := a.Preflight(ctx, doctor.NeedPublish, plan.TargetIDs(), size); err != nil {
		return nil, err
	}
	for _, w := range plan.Warnings {
		a.Warn(w)
	}
	for _, t := range plan.Targets {
		for _, w := range t.Warnings {
			a.Warn(w)
		}
	}
	summary := func(o *Out) { writeSummary(o, set, pr.verb, plan) }
	if pr.dryRun {
		next := command
		if pr.req.Kind == publish.KindPrune {
			next += " --apply"
		}
		run := planRun(env, plan)
		return &Result{Kind: api.KindRun, Items: []any{run}, Run: run.ID, Targets: plan.TargetIDs(),
			Next: []api.NextStep{{Action: "publish", Reason: "the checks passed", Command: next, Human: true}},
			Human: func(o *Out) error {
				summary(o)
				o.Printf("dry run: nothing was signed or pushed\n")
				return nil
			}}, nil
	}
	switch {
	case conf != nil:
		if err := checkConfirmedPlan(conf, plan); err != nil {
			return nil, err
		}
	case !terminal && !pr.yes:
		// No human: the summary, then the refusal.
		run := planRun(env, plan)
		return &Result{Kind: api.KindRun, Items: []any{run}, Run: run.ID, Targets: plan.TargetIDs(), Human: func(o *Out) error { summary(o); return nil }},
			api.Errorf(api.PPublishNeedsConfirmation, "%s needs a confirmation, and there is no terminal", "repoctl "+commandName(cmd)).
				WithHint("check the summary, then confirm with --yes", commandLine(cmd, pr.args, "json", "yes")+" --dry-run")
	case terminal && !pr.yes:
		if !a.jsonOut {
			summary(&Out{Stdout: a.Stderr, Stderr: a.Stderr, Style: a.style, StdoutStyle: a.style})
		}
		q := fmt.Sprintf("Type the set name to %s %s of %s: ", confirmWhat(pr.verb, plan), strings.Join(plan.TargetIDs(), " "), set.Name)
		if err := a.askExact(pr.verb, q, set.Name); err != nil {
			return nil, err
		}
	}
	for _, t := range plan.Targets {
		if t.Gate != nil && t.Gate.Trust != nil {
			for _, id := range t.Gate.Trust.Added {
				if pr.req.Unattended {
					return nil, api.Errorf(api.PSignTrustChange, "the publish of %s changes the key file; a human types the key id %s", t.Target, id)
				}
				// The UI confirmation holds the typed key ids.
				if conf == nil && (!pr.yes || terminal) {
					if err := a.confirmKeyID(fmt.Sprintf("Type the key id to trust %s on %s:", id, t.Target), id); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	res, err := publish.Execute(ctx, env, plan)
	if res == nil {
		return nil, err
	}
	run := res.Run
	for _, w := range res.Warnings {
		a.Warn(w)
	}
	out := &Result{Kind: api.KindRun, Items: []any{run}, Run: run.ID, Targets: plan.TargetIDs(),
		Human: func(o *Out) error { writeRunResult(o, a, set, pr.verb, plan, run, steps); return nil }}
	if err == nil {
		out.Next = []api.NextStep{{Action: "other", Reason: "the mirror job serves the new snapshot within minutes", Command: "repoctl status"}}
		if len(plan.Targets) > 0 && len(plan.Targets[0].Rebuild) > 0 {
			out.Next = append([]api.NextStep{{Action: "rebuild", Human: true, Reason: "the builds that left need a new release",
				Command: "repoctl rebuild " + strings.Join(plan.Targets[0].Rebuild, " ") + ` --reason "after restore of ` + plan.Targets[0].Target + `"`}}, out.Next...)
		}
	}
	return out, err
}

func confirmWhat(verb string, p *publish.Plan) string {
	if n := p.SignCount(); n > 0 {
		return fmt.Sprintf("sign %d RPMs and %s", n, verb)
	}
	return verb
}

// askExact asks for the set name on the terminal; another answer gives
// exit 6.
func (a *App) askExact(verb, question, want string) error {
	fmt.Fprint(a.Stderr, a.style.Bold(question))
	line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
	if got := strings.TrimSpace(line); got != want {
		return api.Errorf(api.PPublishNeedsConfirmation, "%s: %q is not the set name; nothing is signed or pushed", verb, got).
			WithHint("run the command again and type: "+want, "")
	}
	return nil
}

// parentPID gives the pid of the parent process; tests replace it.
var parentPID = os.Getppid

// readConfirmation reads the UI confirmation from a file descriptor, once,
// and closes it (cli.html#ui-confirmation). Only a child of the repoctl ui
// server that ui.json names may pass one.
func (a *App) readConfirmation(fd int) (*ui.Confirmation, error) {
	refuse := func(format string, args ...any) error {
		return api.Errorf(api.PPublishNeedsConfirmation, "publish --confirm-fd: "+format, args...).
			WithHint("publish in a terminal, or confirm the publish in the UI", "repoctl ui")
	}
	f := os.NewFile(uintptr(fd), "confirm-fd")
	if f == nil {
		return nil, refuse("file descriptor %d is not open", fd)
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<16))
	_ = f.Close()
	if err != nil {
		return nil, refuse("file descriptor %d cannot be read: %v", fd, err)
	}
	rec, ok := ui.ReadRecord(a.paths.UIJSON())
	if !ok {
		return nil, refuse("no repoctl ui server runs (%s)", a.paths.UIJSON())
	}
	if ppid := parentPID(); rec.PID != ppid {
		return nil, refuse("the parent process %d is not the repoctl ui server %d", ppid, rec.PID)
	}
	var c ui.Confirmation
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, refuse("the UI confirmation is not JSON: %v", err)
	}
	if c.V != 1 || c.Command != "publish" {
		return nil, refuse("the UI confirmation is for %q (v %d), not for publish", c.Command, c.V)
	}
	return &c, nil
}

// sameSet reports whether two lists hold the same strings.
func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// matchConfirmation compares a UI confirmation with the command line: the
// set name, the targets, the run, the accepted advisories and their reason.
func matchConfirmation(c *ui.Confirmation, set string, targets []string, req publish.Request) error {
	if c.Set != set {
		return api.Errorf(api.PPublishNeedsConfirmation, "publish: %q is not the set name; nothing is signed or pushed", c.Set).
			WithHint("type the set name in the confirm dialog: "+set, "")
	}
	refuse := func(what string, got, want any) error {
		return api.Errorf(api.PPublishNeedsConfirmation, "publish: the UI confirmation has %s %v, the command line %v; nothing is signed or pushed", what, got, want).
			WithHint("review the publish again in the UI", "")
	}
	switch {
	case len(c.Targets) > 0 && !sameSet(c.Targets, targets):
		return refuse("the targets", c.Targets, targets)
	case c.Run != req.Run:
		return refuse("the run", c.Run, req.Run)
	case !sameSet(c.Accept, req.Accept):
		return refuse("the accepted advisories", c.Accept, req.Accept)
	case len(c.Accept) > 0 && c.Reason != req.Reason:
		return refuse("the reason", c.Reason, req.Reason)
	case c.PlanDigest == "":
		return refuse("the plan digest", "none", "of the review")
	}
	return nil
}

// checkConfirmedPlan checks a UI confirmation against the plan under the
// set lock: the plan digest of the review, then the typed key ids against
// the added keys of the trust block.
func checkConfirmedPlan(c *ui.Confirmation, p *publish.Plan) error {
	if c.PlanDigest != p.Digest {
		return api.Errorf(api.PPublishPlanChanged, "publish: the plan changed since the review (plan digest %s, reviewed %s); nothing is signed or pushed",
			shortDigest(p.Digest), shortDigest(c.PlanDigest)).WithHint("review the publish again in the UI", "")
	}
	var added []string
	for _, t := range p.Targets {
		if t.Gate != nil && t.Gate.Trust != nil {
			added = append(added, t.Gate.Trust.Added...)
		}
	}
	if !sameSet(c.Keys, added) {
		return api.Errorf(api.PPublishNeedsConfirmation, "publish: the typed key ids %v are not the added keys %v of the trust block; nothing is signed or pushed", c.Keys, added).
			WithHint("review the publish again in the UI", "")
	}
	return nil
}

// shortDigest gives the first 12 characters of a digest without its
// algorithm.
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// planRun gives the Run of a dry run: the checks and the summary, no step
// after check.
func planRun(env *publish.Env, p *publish.Plan) *api.Run {
	run := &api.Run{ID: p.RunID, Kind: "publish", Set: env.Set.Name, State: api.RunStateOk,
		Origin:    api.RunOriginItem{Via: env.Via, User: env.User, Host: env.Host},
		SetCommit: p.SetCommit, StartedAt: api.FormatTime(p.Started), Nodes: []api.NodeResult{},
		Files:  api.RunFiles{Journal: env.Paths.Journal(env.Set.Name, p.RunID)},
		Signer: &p.Signer, PlanDigest: p.Digest, PublishKind: p.Kind, Publish: map[string]api.RunPublishValue{}, FromRuns: p.FromRuns}
	if run.Origin.Via == "" {
		run.Origin.Via = "cli"
	}
	for _, t := range p.Targets {
		v := api.RunPublishValue{State: publish.StepSkipped, Store: t.Store.Ref, Held: t.Held,
			Sign:  &api.RunPublishValueSign{New: t.Sign.New, Keep: t.Sign.Keep, Resign: t.Sign.Resign},
			Steps: []api.RunPublishValueStep{{Step: "check", State: publish.StepOK}}, Rebuild: t.Rebuild}
		if t.Base != nil {
			v.Base = t.Base.Digest()
		}
		if t.Advisory != nil {
			v.Gate = t.Advisory.Gate
		}
		run.Publish[t.Target] = v
		for _, n := range t.Take {
			run.Nodes = append(run.Nodes, api.NodeResult{Node: n.Node, Package: n.Package, Target: n.Target, State: api.NodeStateStaged, Commit: n.Commit, SRPM: n.Draft.SRPM})
			run.Counts.Staged++
		}
	}
	for _, s := range p.Skipped {
		run.PublishBlockers = append(run.PublishBlockers, api.RunPublishBlocker{Target: s.Target, Code: s.Code, Message: s.Message})
	}
	return run
}

// writeSummary writes the publish summary (publish.html#summary).
func writeSummary(o *Out, set *config.Set, verb string, p *publish.Plan) {
	for _, t := range p.Targets {
		head := fmt.Sprintf("%s of %s", t.Target, set.Name)
		switch {
		case len(p.FromRuns) > 0:
			head += "  from run " + strings.Join(p.FromRuns, ", ")
		case t.RefSnap != nil:
			head += "  files of " + t.RefSnap.Meta.Publish
		}
		o.KV(7, verb, p.ID+"  "+head)
		switch {
		case t.Base != nil:
			o.KV(7, "base", fmt.Sprintf("latest %s (%s), local copy same", t.Base.Meta.Publish, api.ShortDigest(t.Base.Digest())))
		case t.First:
			o.KV(7, "base", "none: the first snapshot of "+t.Target)
		}
		o.KV(7, "store", t.Store.Ref)
		o.KV(7, "signer", strings.TrimSpace(p.Signer.KeyID+"  "+sign.GroupFingerprint(p.Signer.Fingerprint)))
		if t.Gate != nil && t.Gate.Deployed != "" {
			o.KV(7, "", t.Gate.Deployed)
		}
		if len(t.Rows) > 0 {
			tb := NewTable("PACKAGE", "PUBLISHED", "NEW", "RPMS")
			if p.Kind == publish.KindRestore {
				tb = NewTable("PACKAGE", "LATEST", "AFTER", "RPMS LEAVE")
			}
			tb.Right[3] = true
			for _, r := range t.Rows {
				nw := r.New
				if nw == "" {
					nw = "-"
				}
				pub := r.Published
				if pub == "" {
					pub = "-"
				}
				tb.Row(r.Package, pub, nw, fmt.Sprint(r.RPMs))
			}
			_ = o.Table(tb)
		}
		switch p.Kind {
		case publish.KindPublish:
			o.Printf("%d packages change: %d new RPMs, %d kept, %d after  ·  %d packages do not change\n",
				len(t.Rows), t.Sign.New, t.Sign.Keep, t.Sign.New+t.Sign.Keep, t.Current)
		case publish.KindRestore:
			n := 0
			for _, r := range t.Removed {
				n += len(r.SHA256)
			}
			o.Printf("%d builds leave: %d RPMs go into removed[]. Clients that have them keep them: dnf does not downgrade.\n", len(t.Removed), n)
		case publish.KindPrune:
			o.Printf("%d builds leave: %d files go into removed[]\n", len(t.Removed), len(t.Remove))
		}
		keep := []string{}
		for k, n := range t.Keep {
			if k == "" {
				k = "unsigned"
			}
			keep = append(keep, fmt.Sprintf("%s %d", k, n))
		}
		sort.Strings(keep)
		keepText := fmt.Sprint(t.Sign.Keep)
		if len(keep) > 0 {
			keepText += " (" + strings.Join(keep, ", ") + ")"
		}
		o.KV(7, "sign", fmt.Sprintf("%d new RPMs with %s  ·  keep %s  ·  re-sign %d", t.Sign.New, p.Signer.KeyID, keepText, t.Sign.Resign))
		trust := "trust no change"
		if t.Gate != nil && t.Gate.Trust != nil {
			trust = "trust CHANGE"
		}
		commits := "set commit " + api.ShortCommit(p.SetCommit)
		if n := len(t.Take); n > 0 {
			commits += " and " + countOf(n, "package commit", "package commits")
		}
		checks := fmt.Sprintf("ok: %s pushed · %s · gates 1–3 · signer probe", commits, trust)
		o.KV(7, "checks", checks)
		if t.Advisory != nil && t.Advisory.Line != "" {
			o.KV(7, "", t.Advisory.Line)
		}
		if t.Gate != nil {
			for _, l := range t.Gate.TrustLines {
				o.KV(7, "trust", l)
			}
		}
		for _, h := range t.Held {
			nvr := h.NVR
			if nvr == "" {
				nvr = "no build in the base"
			}
			o.KV(7, "held", fmt.Sprintf("%s keeps %s", h.Node, nvr))
			o.KV(7, "", "reason: "+h.Reason)
		}
		for _, k := range t.KeptRelease {
			o.KV(7, "kept", k+": deployment record")
		}
		if len(t.Rebuild) > 0 {
			o.KV(7, "after", fmt.Sprintf("%d packages need a rebuild before the next publish: %s", len(t.Rebuild), strings.Join(t.Rebuild, " ")))
		}
	}
	for _, s := range p.Skipped {
		o.Printf("not publishable: %s\n", s.Message)
	}
}

// writeRunResult writes the result of a publish run.
func writeRunResult(o *Out, a *App, set *config.Set, verb string, p *publish.Plan, run *api.Run, steps []string) {
	mark := o.StdoutStyle.Mark("ok")
	if run.State != api.RunStateOk {
		mark = o.StdoutStyle.Mark("failed")
	}
	okSteps := 0
	for _, v := range run.Publish {
		for _, s := range v.Steps {
			if s.State == publish.StepOK || s.State == publish.StepSkipped {
				okSteps++
			}
		}
	}
	o.Printf("%s %s  %s  %d steps %s  (-v shows each step)\n", mark, verb, api.FormatDuration(time.Duration(run.DurationMs)*time.Millisecond), okSteps, run.State)
	if a.verbose > 0 {
		for _, s := range steps {
			o.Printf("  %s\n", s)
		}
	}
	for _, t := range p.Targets {
		v := run.Publish[t.Target]
		was := "none"
		if t.Base != nil {
			was = t.Base.Meta.Publish
		}
		switch {
		case v.State == publish.StepOK && v.Snapshot != nil:
			o.Printf("published  %s  %s (%s)  latest was %s\n", t.Target, p.ID, api.ShortDigest(v.Snapshot.Digest), was)
		case v.Pushable:
			o.Printf("pushable   %s  %s: the tree is signed and verified, latest is still %s\n", t.Target, p.ID, was)
		default:
			o.Printf("failed     %s  %s\n", t.Target, p.ID)
		}
	}
	if run.State == api.RunStateOk && set.Repo.URL != "" {
		o.Printf("served     behind until the mirror job runs (every 5 min)\n")
	}
}

// hRepoPush runs repo push: complete a publish, an import or a re-seed.
func hRepoPush(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	from, man, reseed := flagStr(cmd, "from"), flagStr(cmd, "manifest"), flagBool(cmd, "reseed")
	if from != "" || reseed {
		if len(a.TargetArgs()) != 1 {
			return nil, usagef("repo push --from and --reseed need one --target ID")
		}
		pr := publishRequest{verb: "import", dryRun: flagBool(cmd, "dry-run"), yes: flagBool(cmd, "yes")}
		pr.req = publish.Request{Kind: publish.KindImport, ImportDir: from, ImportManifest: man}
		if reseed {
			pr.verb, pr.req.Kind = "re-seed", publish.KindReseed
		}
		if from != "" && man == "" {
			return nil, usagef("repo push --from DIR needs --manifest FILE")
		}
		return runPublish(ctx, a, cmd, pr)
	}
	set, dir, err := a.Set()
	if err != nil {
		return nil, err
	}
	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	target := ""
	if t := a.TargetArgs(); len(t) == 1 {
		target = t[0]
	}
	lk, err := a.SetLock(ctx, set, false)
	if err != nil {
		return nil, err
	}
	defer lk.Release()
	env, cleanup, err := PublishEnv(ctx, a, set, dir, "repoctl repo push")
	defer cleanup()
	if err != nil {
		return nil, err
	}
	res, err := publish.Complete(ctx, env, id, target)
	if res == nil {
		return nil, err
	}
	for _, w := range res.Warnings {
		a.Warn(w)
	}
	run := res.Run
	return &Result{Kind: api.KindRun, Items: []any{run}, Run: run.ID, Next: []api.NextStep{{Action: "other", Reason: "check the served tree", Command: "repoctl status"}},
		Human: func(o *Out) error {
			for t, v := range run.Publish {
				if v.Snapshot != nil {
					o.Printf("%-9s %s  %s (%s)\n", v.State, t, v.Snapshot.Tag, api.ShortDigest(v.Snapshot.Digest))
				}
			}
			return nil
		}}, err
}

// hRepoRestore runs repo restore REF.
func hRepoRestore(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	if len(args) != 1 {
		return nil, usagef("repo restore needs REF: a publish id or a digest")
	}
	pr := publishRequest{verb: "restore", args: args, dryRun: flagBool(cmd, "dry-run"), yes: flagBool(cmd, "yes")}
	pr.req = publish.Request{Kind: publish.KindRestore, Ref: args[0], Accept: flagStrs(cmd, "accept-advisory"), Reason: flagStr(cmd, "reason")}
	return runPublish(ctx, a, cmd, pr)
}

// hRepoPrune runs repo prune: a dry run, or with --apply a publish run of
// kind prune.
func hRepoPrune(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	pr := publishRequest{verb: "prune", dryRun: !flagBool(cmd, "apply"), yes: flagBool(cmd, "yes")}
	keepLast, _ := cmd.Flags().GetInt("keep-last")
	spec := publish.PruneSpec{Package: flagStr(cmd, "package"), All: flagBool(cmd, "all"), KeepLast: keepLast, NVRs: flagStrs(cmd, "nvr")}
	if s := flagStr(cmd, "older-than"); s != "" {
		d, err := config.ParseDuration(s)
		if err != nil {
			return nil, usagef("--older-than: %v", err)
		}
		spec.OlderThan = d
	}
	if spec.All && spec.Package == "" {
		return nil, usagef("--all needs --package NAME")
	}
	if spec.Package == "" && spec.KeepLast == 0 && spec.OlderThan == 0 && len(spec.NVRs) == 0 {
		return nil, usagef("repo prune needs a selector: --nvr, --package, --keep-last or --older-than")
	}
	pr.req = publish.Request{Kind: publish.KindPrune, Prune: spec}
	if r := flagStr(cmd, "repo"); r != "" {
		k, ok := tree.ParseKind(r)
		if !ok {
			return nil, usagef("--repo: binary, source or debuginfo")
		}
		pr.req.Repo = k
	}
	return runPublish(ctx, a, cmd, pr)
}

// hRepoResign runs repo resign --signer ID.
func hRepoResign(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	id := flagStr(cmd, "signer")
	if id == "" {
		return nil, usagef("repo resign needs --signer ID")
	}
	pr := publishRequest{verb: "re-sign", dryRun: flagBool(cmd, "dry-run"), yes: flagBool(cmd, "yes")}
	pr.req = publish.Request{Kind: publish.KindResign, SignerID: id}
	if r := flagStr(cmd, "repo"); r != "" {
		k, ok := tree.ParseKind(r)
		if !ok {
			return nil, usagef("--repo: binary, source or debuginfo")
		}
		pr.req.Repo = k
	}
	return runPublish(ctx, a, cmd, pr)
}

// openStore opens the OCI package of a target.
func openStore(a *App, set *config.Set, target string) (*snapshot.Store, error) {
	if set.Store.OCI == "" {
		return nil, api.Errorf(api.PSnapshotNotFound, "repoctl.yaml has no store.oci").WithHint("set the store", "repoctl config")
	}
	return snapshot.Open(set.OCIPackage(target), snapshot.Options{Getenv: a.Getenv, UserAgent: "repoctl/" + config.Version})
}

// hRepoPull runs repo pull [REF].
func hRepoPull(ctx context.Context, a *App, _ *cobra.Command, args []string) (*Result, error) {
	set, dir, err := a.Set()
	if err != nil {
		return nil, err
	}
	targets, err := a.Targets(set, true)
	if err != nil {
		return nil, err
	}
	ref := ""
	if len(args) > 0 {
		ref = args[0]
		if len(targets) != 1 && len(a.TargetArgs()) != 1 {
			return nil, usagef("repo pull REF needs one --target ID")
		}
	}
	lk, err := a.SetLock(ctx, set, false)
	if err != nil {
		return nil, err
	}
	defer lk.Release()
	var items []any
	var lines []string
	rel := ""
	if p, ok := set.ReleasePackage(); ok {
		rel = p.ID
	}
	for _, t := range targets {
		store, err := openStore(a, set, t)
		if err != nil {
			return nil, err
		}
		if ref == "" {
			if _, ok, err := store.Latest(ctx); err != nil {
				return nil, err
			} else if !ok {
				lines = append(lines, fmt.Sprintf("%-6s no snapshot", t))
				continue
			}
		}
		res, err := localcopy.Pull(ctx, localcopy.PullOptions{Store: store, Ref: ref, Set: set.Name, Target: t,
			Dir: localcopy.Dir(a.paths, set.Name, t), Layout: tree.LayoutOf(set), Keys: localcopy.Keys(set, dir),
			Keyfile: tree.KeyfileName(set), Release: rel, User: username(a.Getenv)})
		if res != nil {
			for _, w := range res.Warnings {
				a.Warn(w)
			}
		}
		if err != nil {
			return nil, err
		}
		s, _ := localcopy.ReadSnapshot(localcopy.Dir(a.paths, set.Name, t))
		if s == nil {
			v := res.Snapshot.API(store.Ref, set.Repo.Name, nil)
			s = &v
		}
		s.Local = "same"
		s.Blobs = &api.SnapshotBlobs{New: res.Stats.New, Known: res.Stats.Known, Bytes: int(res.Stats.Bytes)}
		items = append(items, *s)
		what := fmt.Sprintf("%d new blobs (%s), %d known", res.Stats.New, sizeText(res.Stats.Bytes), res.Stats.Known)
		if res.Same {
			what = "already the local copy"
		}
		lines = append(lines, fmt.Sprintf("%-6s %s (%s)  %s", t, s.Publish, api.ShortDigest(s.Digest), what))
	}
	return &Result{Kind: api.KindSnapshot, Items: items, Targets: targets,
		Next: []api.NextStep{{Action: "other", Reason: "the local copy equals latest", Command: "repoctl plan"}},
		Human: func(o *Out) error {
			for _, l := range lines {
				o.Printf("pulled  %s\n", l)
			}
			return nil
		}}, nil
}

func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// hRepoSnapshots runs repo snapshots: the chain of latest, newest first.
func hRepoSnapshots(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	set, _, err := a.Set()
	if err != nil {
		return nil, err
	}
	targets, err := a.Targets(set, false)
	if err != nil {
		return nil, err
	}
	limit, _ := cmd.Flags().GetInt("limit")
	all, history := flagBool(cmd, "all"), flagBool(cmd, "history")
	var items []any
	var lists []api.SnapshotList
	for _, t := range targets {
		store, err := openStore(a, set, t)
		if err != nil {
			return nil, err
		}
		list := api.SnapshotList{Target: t, Snapshots: []api.Snapshot{}}
		local, _ := localcopy.ReadSnapshot(localcopy.Dir(a.paths, set.Name, t))
		if history {
			if list.Snapshots, err = snapshotHistory(ctx, a, set, t, store, local); err != nil {
				return nil, err
			}
		} else {
			d, ok, err := store.Latest(ctx)
			if err != nil {
				return nil, err
			}
			if ok {
				chain, err := store.Chain(ctx, d, limit)
				if err != nil {
					return nil, err
				}
				on := map[string]bool{}
				for i, s := range chain {
					v := s.API(store.Ref, set.Repo.Name, keyIDOf(set))
					v.Tags = []string{s.Meta.Publish}
					if i == 0 {
						v.Tags = []string{snapshot.TagLatest, s.Meta.Publish}
						if local != nil {
							v.Local = "behind"
							if local.Digest == v.Digest {
								v.Local = "same"
							}
						}
					}
					if i+1 < len(chain) {
						v.Added = s.Added(chain[i+1], set.Repo.Name)
					} else {
						v.Added = v.RPMs
					}
					on[s.Meta.Publish] = true
					list.Snapshots = append(list.Snapshots, v)
				}
				if all {
					tags, err := store.Tags(ctx)
					if err != nil {
						return nil, err
					}
					for _, tag := range tags {
						if tag == snapshot.TagLatest || on[tag] {
							continue
						}
						s, err := store.Get(ctx, tag)
						if err != nil {
							continue
						}
						v := s.API(store.Ref, set.Repo.Name, keyIDOf(set))
						v.Tags = []string{tag}
						list.Snapshots = append(list.Snapshots, v)
					}
				}
			}
		}
		lists = append(lists, list)
		items = append(items, list)
	}
	return &Result{Kind: api.KindSnapshotList, Items: items, Targets: targets, Human: func(o *Out) error {
		for i, l := range lists {
			if i > 0 {
				o.Printf("\n")
			}
			writeSnapshots(o, l, set.OCIPackage(l.Target), history)
		}
		return nil
	}}, nil
}

// snapshotHistory gives the publish history of latest from history[] of the
// RepoManifest of the local copy, newest first. A snapshot that gc deleted
// has deleted: true and no RPM counts.
func snapshotHistory(ctx context.Context, a *App, set *config.Set, t string, store *snapshot.Store, local *api.Snapshot) ([]api.Snapshot, error) {
	m, err := manifest.ReadLocal(a.paths, set, t)
	if err != nil || m == nil {
		return []api.Snapshot{}, err
	}
	keyID := keyIDOf(set)
	var out []api.Snapshot
	for i := len(m.History) - 1; i >= 0; i-- {
		h := m.History[i]
		v := api.Snapshot{Target: t, Publish: h.Publish, Kind: h.Kind, Parent: h.Parent, SetCommit: h.SetCommit,
			User: h.User, Host: h.Host, Created: h.Created}
		if h.Signer != "" {
			v.Signer = &api.SnapshotSigner{Fingerprint: h.Signer, KeyID: keyID(h.Signer)}
		}
		for _, acc := range h.Accepted {
			v.Acceptances = append(v.Acceptances, api.SnapshotAcceptance{ID: acc.ID, FactsDigest: acc.FactsDigest, Reason: acc.Reason})
		}
		for _, hd := range h.Held {
			v.Held = append(v.Held, api.SnapshotHeldItem{Node: hd.Node, Reason: hd.Reason})
		}
		if h.Trust != nil && (len(h.Trust.Added) > 0 || len(h.Trust.Removed) > 0) {
			v.Trust = &api.SnapshotTrust{Added: h.Trust.Added, Removed: h.Trust.Removed}
		}
		// The history has no digest of its own snapshot; the next entry
		// names it as its parent.
		if len(out) > 0 {
			v.Digest = out[len(out)-1].Parent
		} else if local != nil {
			v.Digest = local.Digest
		}
		if v.Digest == "" {
			continue
		}
		out = append(out, v)
	}
	// The counts come from the snapshots that gc did not delete.
	snaps := make([]*snapshot.Snapshot, len(out))
	for i := range out {
		s, err := store.Get(ctx, out[i].Digest)
		if err != nil {
			if e, ok := api.AsError(err); ok && e.Code == api.PSnapshotNotFound {
				out[i].Deleted = true
				continue
			}
			return nil, err
		}
		snaps[i] = s
		out[i].RPMs = len(s.RPMs(set.Repo.Name))
		out[i].Tags = []string{s.Meta.Publish}
		if i == 0 {
			out[i].Tags = []string{snapshot.TagLatest, s.Meta.Publish}
		}
	}
	for i := range out {
		switch {
		case snaps[i] == nil:
		case out[i].Parent == "":
			out[i].Added = out[i].RPMs
		case i+1 < len(out) && snaps[i+1] != nil:
			out[i].Added = snaps[i].Added(snaps[i+1], set.Repo.Name)
		}
	}
	if out == nil {
		out = []api.Snapshot{}
	}
	return out, nil
}

// writeSnapshots writes the snapshots of one target in the form of
// cli.html#cmd-repo-snapshots: the OCI package, then one row per
// snapshot, latest marked with *.
func writeSnapshots(o *Out, l api.SnapshotList, pkg string, history bool) {
	if len(l.Snapshots) == 0 {
		o.Printf("%s  %s: no snapshot\n", l.Target, pkg)
		return
	}
	o.Printf("%s  %s\n", o.StdoutStyle.Bold(l.Target), pkg)
	tb := NewTable("PUBLISH", "KIND", "DIGEST", "CREATED", "SIGNER", "RPMS", "NEW")
	tb.Right[5], tb.Right[6] = true, true
	marks := []string{}
	latest := false
	var notes [][]string
	for _, s := range l.Snapshots {
		mark := " "
		if len(s.Tags) > 0 && s.Tags[0] == snapshot.TagLatest {
			mark, latest = "*", true
		}
		marks = append(marks, mark)
		signer := ""
		if s.Signer != nil {
			signer = s.Signer.KeyID
			if signer == "" {
				signer = api.ShortFingerprint(s.Signer.Fingerprint)
			}
		}
		created := s.Created
		if tm, err := api.ParseTime(s.Created); err == nil {
			created = tm.Local().Format("2006-01-02 15:04")
		}
		rpms, added := fmt.Sprint(s.RPMs), fmt.Sprint(s.Added)
		digest := api.ShortDigest(s.Digest)
		if s.Deleted {
			rpms, added, digest = "-", "-", digest+" (deleted)"
		}
		tb.Row(s.Publish, s.Kind, digest, created, signer, rpms, added)
		var n []string
		if history {
			for _, acc := range s.Acceptances {
				n = append(n, "accepted "+acc.ID+": "+acc.Reason)
			}
			for _, h := range s.Held {
				n = append(n, "held "+h.Node+": "+h.Reason)
			}
			if t := s.Trust; t != nil {
				for _, f := range t.Added {
					n = append(n, "trust added "+api.ShortFingerprint(f))
				}
				for _, f := range t.Removed {
					n = append(n, "trust removed "+api.ShortFingerprint(f))
				}
			}
		}
		notes = append(notes, n)
	}
	var b strings.Builder
	_ = tb.Write(&b, o.Verbose > 0)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	for i, line := range lines {
		if i == 0 {
			o.Printf("  %s\n", line)
			continue
		}
		o.Printf("%s %s\n", marks[i-1], line)
		for _, n := range notes[i-1] {
			o.Printf("      %s\n", n)
		}
	}
	if latest {
		o.Printf("* = latest\n")
	}
}

func keyIDOf(set *config.Set) func(string) string {
	return func(fpr string) string {
		for _, k := range set.Sign.Keys {
			if strings.EqualFold(strings.ReplaceAll(k.Fingerprint, " ", ""), fpr) {
				return k.ID
			}
		}
		return ""
	}
}

// repoDir gives the directory of a repository of a target: of the local
// copy, of the staging of a run, or --path.
func repoDir(a *App, set *config.Set, target, run, pathFlag string, kind api.RepoKind) (dir, source string) {
	l := tree.LayoutOf(set)
	switch {
	case pathFlag != "":
		return pathFlag, "local"
	case run != "":
		return filepath.Join(a.paths.StagingTarget(set.Name, run, target), l.Dir(kind)), "staging"
	}
	return filepath.Join(localcopy.Dir(a.paths, set.Name, target), l.Dir(kind)), "local"
}

// hRepoLs runs repo ls: the packages of a repository.
func hRepoLs(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	set, _, err := a.Set()
	if err != nil {
		return nil, err
	}
	targets, err := a.Targets(set, false)
	if err != nil {
		return nil, err
	}
	kind, ok := tree.ParseKind(flagStr(cmd, "repo"))
	if !ok {
		return nil, usagef("--repo: binary, source or debuginfo")
	}
	glob, run, pathFlag, removed := flagStr(cmd, "name"), flagStr(cmd, "run"), flagStr(cmd, "path"), flagBool(cmd, "removed")
	if pathFlag != "" {
		targets = targets[:1]
	}
	var items []any
	var all []api.RepoContents
	summaries := map[string]string{}
	for _, t := range targets {
		dir, source := repoDir(a, set, t, run, pathFlag, kind)
		rc := api.RepoContents{Target: t, Repo: filepath.Base(dir), Kind: kind, Source: source, Run: run, Path: dir, Packages: []api.RepoContentsPackage{}}
		md, pkgs, err := tree.ReadPrimary(ctx, dir)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if md != nil {
			rc.Revision = md.Revision
			_, serr := os.Stat(filepath.Join(dir, "repodata", "repomd.xml.asc"))
			rc.Signed = serr == nil
		}
		if source == "local" && pathFlag == "" {
			if s, _ := localcopy.ReadSnapshot(localcopy.Dir(a.paths, set.Name, t)); s != nil {
				rc.Snapshot = s.Publish
			}
		}
		for _, p := range pkgs {
			if glob != "" {
				if ok, _ := path.Match(glob, p.Name); !ok {
					continue
				}
			}
			rp := api.RepoContentsPackage{Name: p.Name, Epoch: p.Epoch(), Version: p.Version.Ver, Release: p.Version.Rel, Arch: p.Arch,
				NVRA: p.NVRA(), Sourcerpm: p.Format.Sourcerpm, SizeBytes: int(p.Size.Package)}
			summaries[p.NVRA()] = p.Summary
			if p.Time.Build > 0 {
				rp.BuildTime = api.FormatTime(time.Unix(p.Time.Build, 0))
			}
			rc.Packages = append(rc.Packages, rp)
		}
		sort.Slice(rc.Packages, func(i, j int) bool { return rc.Packages[i].NVRA < rc.Packages[j].NVRA })
		if removed {
			m, _ := manifest.ReadRepo(filepath.Join(filepath.Dir(dir), set.Repo.Name))
			if m != nil {
				for _, r := range m.Removed {
					rc.Removed = append(rc.Removed, api.RepoContentsRemovedItem{NVR: r.NVR, Publish: r.Publish, Reason: r.Reason, RPMs: len(r.SHA256)})
				}
			}
		}
		all = append(all, rc)
		items = append(items, rc)
	}
	return &Result{Kind: api.KindRepoContents, Items: items, Targets: targets, Human: func(o *Out) error {
		for i, rc := range all {
			if i > 0 {
				o.Printf("\n")
			}
			// The header names the target, the repository and where the
			// tree is (cli.html#cmd-repo-ls).
			where := rc.Path
			switch {
			case rc.Source == "staging":
				where = "staging " + rc.Run
			case pathFlag == "" && rc.Snapshot != "":
				where = "latest " + rc.Snapshot + " (local copy)"
			case pathFlag == "":
				where = "no local copy"
			}
			o.Printf("%s  %s\n", o.StdoutStyle.Bold(rc.Target+" / "+rc.Repo), where)
			if removed {
				tb := NewTable("NVR", "PUBLISH", "REASON", "RPMS")
				tb.Right[3] = true
				for _, r := range rc.Removed {
					tb.Row(r.NVR, r.Publish, r.Reason, fmt.Sprint(r.RPMs))
				}
				_ = o.Table(tb)
				o.Printf("%s\n", countOf(len(rc.Removed), "removed NVR", "removed NVRs"))
				continue
			}
			tb := NewTable("NAME", "VERSION", "SIZE", "SUMMARY")
			for _, p := range rc.Packages {
				tb.Row(p.Name, fmt.Sprintf("%d:%s-%s.%s", p.Epoch, p.Version, p.Release, p.Arch), sizeText(int64(p.SizeBytes)), summaries[p.NVRA])
			}
			_ = o.Table(tb)
			o.Printf("%s\n", countOf(len(rc.Packages), "RPM", "RPMs"))
		}
		return nil
	}}, nil
}

// servedClient is the HTTP client of repo verify --served.
var servedClient = &http.Client{Timeout: 10 * time.Minute}

// verifyKeys gives the keys of repo verify --key FPR: the key of the
// client trust set with this fingerprint (or key id), else the public key
// that gpg exports from the GnuPG home of the user into dir.
func verifyKeys(ctx context.Context, trust []tree.Key, fprs []string, dir string) ([]tree.Key, error) {
	var out []tree.Key
	for _, arg := range fprs {
		fpr := strings.ToUpper(strings.TrimPrefix(strings.ReplaceAll(arg, " ", ""), "0x"))
		found := false
		for _, k := range trust {
			if (k.Fingerprint != "" && k.Fingerprint == fpr) || (k.ID != "" && k.ID == arg) {
				out, found = append(out, k), true
				break
			}
		}
		if found {
			continue
		}
		if !fingerprintRE.MatchString(fpr) {
			return nil, usagef("--key %s is not a fingerprint and not a key id of sign.keys", arg)
		}
		var b bytes.Buffer
		if err := (&run.Cmd{Name: "gpg", Args: []string{"--batch", "--armor", "--export", fpr}, Stdout: &b}).Run(ctx); err != nil || b.Len() == 0 {
			return nil, usagef("--key %s: the key is not in sign.keys, and gpg has no public key with this fingerprint (gpg --import it first)", arg)
		}
		f := filepath.Join(dir, fpr+".asc")
		if err := os.WriteFile(f, b.Bytes(), 0o600); err != nil {
			return nil, err
		}
		out = append(out, tree.Key{Fingerprint: fpr, File: f})
	}
	return out, nil
}

// fingerprintRE is a v4 or v5 fingerprint, or a long key id.
var fingerprintRE = regexp.MustCompile(`^([0-9A-F]{16}|[0-9A-F]{40}|[0-9A-F]{64})$`)

// servedURL gives <repo.url>/<target.path>: the URL of the served tree of
// a target.
func servedURL(set *config.Set, target string) string {
	t, _ := set.Target(target)
	return strings.TrimSuffix(set.Repo.URL, "/") + "/" + strings.Trim(t.ClientPath(), "/")
}

// hRepoVerify runs repo verify: the checks of verify on the local copy, a
// directory, a snapshot or the served tree.
func hRepoVerify(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	set, dir, err := a.Set()
	if err != nil {
		return nil, err
	}
	targets, err := a.Targets(set, false)
	if err != nil {
		return nil, err
	}
	pathFlag, snapRef, run, served := flagStr(cmd, "path"), flagStr(cmd, "snapshot"), flagStr(cmd, "run"), flagBool(cmd, "served")
	n := 0
	for _, set := range []bool{pathFlag != "", snapRef != "", run != "", served} {
		if set {
			n++
		}
	}
	if n > 1 {
		return nil, usagef("--snapshot, --run, --path and --served exclude each other")
	}
	if served && set.Repo.URL == "" {
		return nil, api.Errorf(api.PConfigInvalid, "repo.url is not set in repoctl.yaml: --served needs the URL of the served tree")
	}
	// verify reads the local copy and the staging under the shared set
	// lock, so no publish or pull changes them meanwhile.
	lk, err := a.SetLock(ctx, set, true)
	if err != nil {
		return nil, err
	}
	defer lk.Release()
	keys := localcopy.Keys(set, dir)
	if fprs := flagStrs(cmd, "key"); len(fprs) > 0 {
		kdir, err := os.MkdirTemp("", "rvk-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(kdir)
		if keys, err = verifyKeys(ctx, keys, fprs, kdir); err != nil {
			return nil, err
		}
	}
	kr, err := tree.NewKeyring(ctx, keys)
	if err != nil {
		return nil, err
	}
	defer kr.Close()
	rel := ""
	if p, ok := set.ReleasePackage(); ok {
		rel = p.ID
	}
	rep := api.DoctorReport{Worst: api.CheckStatusOk, Checks: []api.DoctorReportCheck{}}
	var failed error
	add := func(name string, st api.CheckStatus, detail, fix string) {
		rep.Checks = append(rep.Checks, api.DoctorReportCheck{Name: name, Group: "target", Status: st, Detail: detail, Remediation: fix})
		switch st {
		case api.CheckStatusOk:
			rep.Counts.Ok++
		case api.CheckStatusWarning:
			rep.Counts.Warning++
			if rep.Worst == api.CheckStatusOk {
				rep.Worst = st
			}
		case api.CheckStatusFailed:
			rep.Counts.Failed++
			rep.Worst = st
		}
	}
	for _, t := range targets {
		tdir := localcopy.Dir(a.paths, set.Name, t)
		// name is the check of the tree: the target, or "TARGET served".
		name, note, fix := t, "", "repoctl repo pull --target "+t
		switch {
		case pathFlag != "":
			tdir = pathFlag
		case run != "":
			tdir = a.paths.Staging(set.Name, run) + "/" + t
		case snapRef != "":
			store, err := openStore(a, set, t)
			if err != nil {
				return nil, err
			}
			s, err := store.Get(ctx, snapRef)
			if err != nil {
				return nil, err
			}
			tmp, err := os.MkdirTemp("", "rv-")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(tmp)
			have, _ := localcopy.Have(tdir)
			if _, err := store.Download(ctx, s, tmp, have); err != nil {
				return nil, err
			}
			tdir = tmp
		case served:
			name, fix = t+" served", "read the journal of the mirror job on the HTTP server (manual: runbook Fix a failed mirror job)"
			tmp, err := os.MkdirTemp("", "rv-")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(tmp)
			fo := tree.FetchOptions{Layout: tree.LayoutOf(set), Client: servedClient}
			if rel != "" {
				fo.Keyfile, fo.Release = tree.KeyfileName(set), rel
			}
			sv, err := tree.FetchServed(ctx, servedURL(set, t), tmp, fo)
			if err != nil {
				add(name, api.CheckStatusFailed, err.Error(), fix)
				if failed == nil {
					e, ok := api.AsError(err)
					if !ok {
						e = api.Errorf(api.PVerifyFailed, "%v", err)
					}
					failed = e.WithTarget(t)
				}
				continue
			}
			note = "at " + sv.URL + ": "
			if s := sv.Snapshot; s != nil {
				note = fmt.Sprintf("%s (%s) at %s: ", s.Publish, api.ShortDigest(s.Digest), sv.URL)
				if s.Set != set.Name || s.Target != t {
					add(name, api.CheckStatusFailed, fmt.Sprintf("%s names %s %s, not %s %s", tree.ServedSnapshotFile, s.Set, s.Target, set.Name, t), fix)
					if failed == nil {
						failed = api.Errorf(api.PVerifyFailed, "the served tree of %s names %s %s", t, s.Set, s.Target).WithTarget(t)
					}
					continue
				}
			}
			tdir = tmp
		}
		if _, err := os.Stat(tdir); err != nil {
			add(name, api.CheckStatusFailed, "no tree at "+tdir, fix)
			failed = api.Errorf(api.PVerifyFailed, "%s has no tree to verify", t).WithTarget(t)
			continue
		}
		o := tree.VerifyOptions{Layout: tree.LayoutOf(set), Keyring: kr, Records: true}
		if rel != "" && run == "" {
			o.Keyfile, o.Release = tree.KeyfileName(set), rel
		}
		if run != "" {
			o.Keyring = nil
		}
		res, err := tree.Verify(ctx, tdir, o)
		if err != nil {
			return nil, err
		}
		if err := res.Err(); err != nil {
			msgs := []string{}
			for _, p := range res.Problems {
				msgs = append(msgs, p.Message)
			}
			add(name, api.CheckStatusFailed, note+strings.Join(msgs, "; "), fix)
			if failed == nil {
				e, _ := api.AsError(err)
				failed = e.WithTarget(t)
			}
			continue
		}
		// The counts are the RPM files of all repositories, as the RPMS
		// of repo snapshots and sign ls.
		signers := []string{}
		for fpr, n := range res.Signers {
			id := keyIDOf(set)(fpr)
			if id == "" {
				id = api.ShortFingerprint(fpr)
			}
			signers = append(signers, fmt.Sprintf("%s (%d)", id, n))
		}
		sort.Strings(signers)
		detail := fmt.Sprintf("%s%d RPMs in %d repositories, ", note, res.RPMs, res.Repos)
		if run != "" {
			// A staging has no signatures (cli.html#cmd-repo-verify).
			detail += "not signed (staging), checksums ok"
		} else {
			detail += fmt.Sprintf("signers %s, checksums ok, %d repomd.xml.asc ok", strings.Join(signers, " "), res.Repomd)
		}
		add(name, api.CheckStatusOk, detail, "")
		if res.Records != nil {
			add(name+" records", api.CheckStatusOk, "provenance and SBOM equal to the RepoManifest, subjects equal to the files", "")
			for _, w := range res.Records.Warnings {
				add(name+" records", api.CheckStatusWarning, w.Message, "")
			}
		}
		if flagBool(cmd, "el") {
			env := &publish.MockEL{Mock: mock.New(a.paths, mock.Options{}), Set: set, SetDir: dir, Work: a.paths.MockWork(set.Name, "verify")}
			kf := []string{}
			if p := filepath.Join(tdir, tree.KeyfileName(set)); statOK(p) {
				kf = append(kf, p)
			}
			if err := env.VerifyEL(ctx, t, tdir, kf); err != nil {
				add(name+" verify root", api.CheckStatusFailed, err.Error(), "")
				if failed == nil {
					failed = err
				}
			} else {
				add(name+" verify root", api.CheckStatusOk, "key file "+tree.KeyfileName(set)+", checksig ok", "")
			}
		}
	}
	return &Result{Kind: api.KindDoctorReport, Items: []any{rep}, Targets: targets, Human: func(o *Out) error {
		for _, c := range rep.Checks {
			o.Printf("%s %s: %s\n", o.StdoutStyle.MarkOfCheck(c.Status), c.Name, c.Detail)
		}
		return nil
	}}, failed
}

// hRepoGC runs repo gc: the dry run, or with --apply the deletes, oldest
// first.
func hRepoGC(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	set, _, err := a.Set()
	if err != nil {
		return nil, err
	}
	targets, err := a.Targets(set, false)
	if err != nil {
		return nil, err
	}
	r := snapshot.DefaultRetention
	if n := set.Retention.Snapshots.Keep; n > 0 {
		r.Keep = n
	}
	if d := time.Duration(set.Retention.Snapshots.KeepDays); d > 0 {
		r.KeepDays = d
	}
	if d := time.Duration(set.Retention.Snapshots.KeepRemoved); d > 0 {
		r.KeepRemoved = d
	}
	if cmd.Flags().Changed("keep") {
		r.Keep, _ = cmd.Flags().GetInt("keep")
	}
	for flag, dst := range map[string]*time.Duration{"keep-days": &r.KeepDays, "keep-removed": &r.KeepRemoved} {
		if cmd.Flags().Changed(flag) {
			d, err := config.ParseDuration(flagStr(cmd, flag))
			if err != nil {
				return nil, usagef("--%s: %v", flag, err)
			}
			*dst = d
		}
	}
	apply := flagBool(cmd, "apply")
	if apply && !a.isTerminal() && !flagBool(cmd, "yes") {
		return nil, api.Errorf(api.PPublishNeedsConfirmation, "repo gc --apply needs a confirmation, and there is no terminal").WithHint("", "repoctl repo gc --apply --yes")
	}
	if apply && !a.isTerminal() && a.Getenv("INVOCATION_ID") != "" {
		return nil, api.Errorf(api.PPublishUnattendedRefused, "gc never runs unattended: it deletes history")
	}
	if apply {
		lk, err := a.SetLock(ctx, set, false)
		if err != nil {
			return nil, err
		}
		defer lk.Release()
	}
	var items []any
	var lists []api.SnapshotList
	deleteCount := 0
	var j *publish.FileJournal
	var runID string
	if apply {
		runID = snapshot.NewID(a.Now())
		runID = "r" + runID[1:]
		j, err = publish.OpenJournal(a.paths.Journal(set.Name, runID), runID, a.Now)
		if err != nil {
			return nil, err
		}
		defer j.Close()
		_ = j.Append(api.EventTypeRunStarted, map[string]any{"set": set.Name, "kind": "gc", "trace": tree.SHA256Bytes([]byte(runID))[:32],
			"origin": map[string]any{"via": "cli", "user": username(a.Getenv), "host": hostname()}})
	}
	var gcErr error
	why := map[string]string{}
	for _, t := range targets {
		store, err := openStore(a, set, t)
		if err != nil {
			return nil, err
		}
		list := api.SnapshotList{Target: t, Snapshots: []api.Snapshot{}}
		d, ok, err := store.Latest(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			lists, items = append(lists, list), append(items, list)
			continue
		}
		chain, err := store.Chain(ctx, d, 0)
		if err != nil {
			return nil, err
		}
		var removed []snapshot.RemovedEntry
		if m, _ := manifest.ReadLocal(a.paths, set, t); m != nil {
			for _, rm := range m.Removed {
				e := snapshot.RemovedEntry{NVR: rm.NVR, Removed: snapshot.IDTime(rm.Publish)}
				for _, s := range rm.SHA256 {
					e.SHA256 = append(e.SHA256, s)
				}
				removed = append(removed, e)
			}
		}
		decisions := snapshot.GCPlan(chain, nil, removed, r, a.Now())
		del := snapshot.DeleterFor(store, a.Getenv)
		for _, dc := range decisions {
			v := dc.Snapshot.API(store.Ref, set.Repo.Name, keyIDOf(set))
			v.GC = "keep"
			why[t+"\x00"+v.Publish] = dc.Why
			if dc.Delete {
				v.GC = "delete"
				deleteCount++
				if apply && gcErr == nil {
					if err := del.Delete(ctx, store, dc.Snapshot); err != nil {
						gcErr = err
						if e, ok := api.AsError(err); !ok || e.Code != api.PRegistryDenied {
							gcErr = api.Errorf(api.PGCFailed, "the delete of %s failed: %v", dc.Snapshot.Meta.Publish, err).WithCause(err)
						}
					} else {
						v.Deleted = true
						_ = j.Append(api.EventTypeGCDeleted, api.GcDeleted{Target: t, Publish: dc.Snapshot.Meta.Publish, Digest: dc.Snapshot.Digest()})
					}
				}
			}
			list.Snapshots = append(list.Snapshots, v)
		}
		lists, items = append(lists, list), append(items, list)
	}
	if j != nil {
		state, code := api.RunStateOk, 0
		if gcErr != nil {
			state, code = api.RunStateFailed, exitCode(gcErr)
		}
		_ = j.Append(api.EventTypeRunFinished, api.RunFinished{State: state, ExitCode: code})
	}
	res := &Result{Kind: api.KindSnapshotList, Items: items, Targets: targets, Run: runID, Human: func(o *Out) error {
		for _, l := range lists {
			for _, s := range l.Snapshots {
				o.Printf("%-6s %-24s %-6s %s\n", l.Target, s.Publish, s.GC, why[l.Target+"\x00"+s.Publish])
			}
		}
		if !apply && deleteCount > 0 {
			o.Printf("dry run: %d snapshots to delete\n", deleteCount)
		}
		return nil
	}}
	if !apply && deleteCount > 0 {
		res.Next = []api.NextStep{{Action: "other", Human: true, Reason: "delete the old snapshots", Command: "repoctl repo gc --apply"}}
	}
	if gcErr != nil {
		return res, gcErr
	}
	if deleteCount == 0 {
		return res, api.Errorf(api.PPublishNothingToPublish, "nothing to delete").WithExit(api.ExitNothingToPublish)
	}
	return res, nil
}

func statOK(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
