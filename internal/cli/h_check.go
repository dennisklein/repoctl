package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/advisory"
	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/doctor"
	"github.com/dennisklein/repoctl/internal/journal"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/plan"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/upstream"
)

// The handlers of checks: doctor, check, advisories, upstream.

// doctorEnv gives the environment of the doctor checks and the preflight.
func (a *App) doctorEnv(targets []string) *doctor.Env {
	env := &doctor.Env{Paths: a.paths, Getenv: a.Getenv, Now: a.Now, Targets: targets, User: username(a.Getenv)}
	env.Version, env.Commit = version()
	if eff, err := a.Settings(); err == nil {
		env.Settings = eff
	}
	set, dir, err := a.Set()
	switch {
	case err != nil && config.IsNoSet(err):
	case err != nil:
		env.SetErr = err
	default:
		env.Set, env.SetDir = set, dir
	}
	return env
}

// Preflight runs the host checks that a command needs (doctor.Preflight):
// need is build, publish, check or mock. It fails with exit 9
// (host.not-ready, mock.unavailable, mock.config-missing). targets are the
// selected targets; publishBytes is the size of the RPMs that a publish
// signs.
func (a *App) Preflight(ctx context.Context, need string, targets []string, publishBytes int64) error {
	env := a.doctorEnv(targets)
	env.PublishBytes = publishBytes
	return doctor.Preflight(ctx, env, need)
}

func hDoctor(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	strict, _ := cmd.Flags().GetBool("strict")
	only, _ := cmd.Flags().GetString("only")
	if only != "" {
		ok := false
		for _, g := range doctor.Groups {
			ok = ok || g == only
		}
		if !ok {
			return nil, usagef("--only %q: want one of %s", only, strings.Join(doctor.Groups, ", "))
		}
	}
	env := a.doctorEnv(nil)
	checks := doctor.Run(ctx, env, only)
	rep := doctor.Report(checks)
	res := &Result{Kind: api.KindDoctorReport, Items: []any{rep}, Human: func(o *Out) error {
		if err := doctor.Write(o.Stdout, o.StdoutStyle, checks); err != nil {
			return err
		}
		if !o.Quiet {
			o.Printf("%s\n", doctor.Totals(rep))
		}
		return nil
	}}
	switch {
	case rep.Counts.Failed > 0:
		return res, api.Errorf(api.PHostNotReady, "%s failed: this host is not ready", chkPlural(rep.Counts.Failed, "required check")).
			WithExit(api.ExitHostNotReady)
	case strict && rep.Counts.Warning > 0:
		// The totals line says it; exit 8 needs no error line.
		return res, &silent{err: api.Errorf(api.PDoctorWarning, "%s with --strict", chkPlural(rep.Counts.Warning, "warning")).WithExit(api.ExitDoctorWarning)}
	}
	return res, nil
}

func chkPlural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// checkBy gives who started a check: the timer when systemd runs it.
func checkBy(getenv func(string) string) string {
	if getenv("INVOCATION_ID") != "" {
		return "timer"
	}
	return "human"
}

// checkInputs gives the inputs of the advisory checks of the set.
func (a *App) checkInputs(set *config.Set, dir string) *advisory.Inputs {
	lf, _ := lock.Read(dir)
	eff, _ := a.Settings()
	opts := mock.Options{}
	if eff != nil {
		opts.NoSync, opts.TmpFS = eff.Bool("mock.nosync"), eff.String("mock.tmpfs")
	}
	return &advisory.Inputs{
		Paths: a.paths, Set: set, SetDir: dir, Lock: lf, Mock: mock.New(a.paths, opts),
		Upstream: upstream.NewClient(a.paths, a.Getenv), Now: a.Now,
	}
}

// loadDismissals reads the dismissal file. An invalid file is a warning,
// and nothing is dismissed.
func (a *App) loadDismissals(dir string) *advisory.Dismissals {
	d, err := advisory.LoadDismissals(dir)
	if err != nil {
		if e, ok := api.AsError(err); ok {
			a.Warn(e.Problem)
		}
		return nil
	}
	return d
}

func (a *App) advTimer(dir string) *advisory.Timer {
	return &advisory.Timer{UnitDir: advisory.UnitDir(a.paths), SetDir: dir, TestHost: a.paths.TestHost, User: username(a.Getenv), Now: a.Now}
}

func (a *App) runCheck(ctx context.Context, set *config.Set, dir string, codes []api.AdvisoryCode, targets []string, notes *[]string) (*advisory.Summary, error) {
	in := a.checkInputs(set, dir)
	host := hostname()
	j := &advisory.FileJournal{Paths: a.paths, Repoctl: config.Version, Now: a.Now,
		Origin: advisory.Origin{Via: "cli", User: username(a.Getenv), Host: host, PID: os.Getpid()}}
	if a.Getenv("CI") == "true" {
		j.Origin.Via = "ci"
	}
	o := advisory.Options{Codes: codes, Targets: targets, By: checkBy(a.Getenv), Journal: j, OnWait: func(since time.Time) {
		*notes = append(*notes, "check running since "+since.Local().Format("15:04")+"; this gives its result")
	}, Retain: func() { a.retainChecks(set.Name, host) }}
	h := lock.Holder{Command: a.cmdName(), PID: os.Getpid(), Host: host, User: username(a.Getenv), Started: a.Now()}
	return advisory.Check(ctx, in, o, h)
}

// retainChecks keeps the newest state.keep_checks check runs of the set
// on this host (contract.html#writing). They do not count toward
// state.keep_runs, and the check leaves the build and publish runs alone.
func (a *App) retainChecks(set, host string) {
	keep := 30
	if eff, err := a.Settings(); err == nil {
		keep = eff.Int("state.keep_checks")
	}
	_, _ = journal.Retain(journal.Readers(a.paths, set, host), set, journal.Retention{KeepChecks: keep, OnlyChecks: true})
}

func hCheck(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	fl := cmd.Flags()
	install, _ := fl.GetBool("install-timer")
	remove, _ := fl.GetBool("remove-timer")
	checkFlag, _ := fl.GetBool("check")
	minLevelText, _ := fl.GetString("min-level")
	codeArgs, _ := fl.GetStringArray("code")
	minLevel, ok := advisory.ParseLevel(minLevelText)
	if !ok {
		return nil, usagef("--min-level %q: want info, warn or required", minLevelText)
	}
	if install && remove {
		return nil, usagef("--install-timer and --remove-timer exclude each other")
	}
	set, dir, err := a.Set()
	if err != nil {
		return nil, err
	}
	if install || remove {
		return a.checkTimer(ctx, set, dir, install)
	}
	var codes []api.AdvisoryCode
	for _, c := range codeArgs {
		for _, part := range strings.Split(c, ",") {
			part = strings.TrimSpace(part)
			found := false
			var known []string
			for _, k := range advisory.Codes() {
				known = append(known, string(k.Code))
				found = found || string(k.Code) == part
			}
			if !found {
				return nil, usagef("--code %q: repoctl check runs %s", part, strings.Join(known, ", "))
			}
			codes = append(codes, api.AdvisoryCode(part))
		}
	}
	var targets []string
	if len(a.TargetArgs()) > 0 {
		if targets, err = a.Targets(set, true); err != nil {
			return nil, err
		}
	}
	if err := a.Preflight(ctx, doctor.NeedCheck, targets, 0); err != nil {
		return nil, err
	}
	var notes []string
	sum, err := a.runCheck(ctx, set, dir, codes, targets, &notes)
	if err != nil {
		return nil, err
	}
	if a.JSON() {
		// The human output gives these as notes.
		for _, s := range skippedNotes(sum.Skipped) {
			a.Warn(api.Problem{Code: api.PCheckIncomplete, Message: s})
		}
	}
	d := a.loadDismissals(dir)
	list := advisory.Current(sum.File, d, nil)
	item := api.AdvisoryList{Advisories: list, Check: sum.File.Summary()}
	item.Check.Timer = a.advTimer(dir).State(ctx)
	failed := sum.Failed()
	if sum.Waited {
		failed = sum.File.Failed()
	}
	open := 0
	for _, adv := range list {
		if adv.State == api.AdvisoryStateOpen && advisory.AtLeast(adv.Level, minLevel) {
			open++
		}
	}
	res := &Result{Kind: api.KindAdvisoryList, Items: []any{item}, Targets: activeTargetIDs(set), Human: func(o *Out) error {
		quiet := o.Quiet && len(failed) == 0 && (!checkFlag || open == 0)
		for _, n := range notes {
			o.Note("%s", n)
		}
		if quiet {
			return nil
		}
		writeAdvisoryTable(o, list, false)
		for _, s := range skippedNotes(sum.Skipped) {
			o.Note("%s", s)
		}
		for _, p := range failed {
			r, _ := sum.File.Find(resultCodeOf(sum, p), p.Target)
			from := ""
			if r.GoodAt != "" {
				from = "; from the check of " + api.MustTime(r.GoodAt).UTC().Format("2006-01-02 15:04")
			}
			fmt.Fprintln(o.Stderr, o.Style.Amber("warning:")+" check failed: "+p.Code+from+": "+p.Message)
		}
		o.Printf("%s\n", checkTotals(o, list, sum))
		if len(failed) == 0 {
			advisoryHint(o, list)
		}
		return nil
	}}
	switch {
	case len(failed) > 0:
		e := api.Errorf(failed[0].Code, "a check failed: %s", failed[0].Message).WithExit(api.ExitFailed)
		return res, e.WithHint("run it again later", "repoctl check")
	case checkFlag && open > 0:
		// Exit 11 is a finding, not a failure: no error line.
		return res, &silent{err: api.Errorf(api.PAdvisoryOpen, "%s at level %s or higher", chkPlural(open, "open advisory"), minLevel).
			WithExit(api.ExitDifferences)}
	}
	return res, nil
}

// resultCodeOf finds the code of a failed problem in the results of a run.
func resultCodeOf(sum *advisory.Summary, p api.Problem) api.AdvisoryCode {
	for _, r := range sum.File.Results {
		if r.Problem != nil && r.Problem.Message == p.Message && r.Target == p.Target {
			return r.Code
		}
	}
	return ""
}

// skippedNotes gives one note per reason: the codes and targets that did
// not run.
func skippedNotes(skips []advisory.Skip) []string {
	type group struct {
		codes []string
	}
	byReason := map[string]*group{}
	var order []string
	for _, s := range skips {
		g, ok := byReason[s.Reason]
		if !ok {
			g = &group{}
			byReason[s.Reason] = g
			order = append(order, s.Reason)
		}
		if !chkContains(g.codes, string(s.Code)) {
			g.codes = append(g.codes, string(s.Code))
		}
	}
	var out []string
	for _, r := range order {
		out = append(out, r+", so "+chkJoinAnd(byReason[r].codes)+" did not run (run: repoctl repo pull)")
	}
	return out
}

func chkContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func chkJoinAnd(list []string) string {
	switch len(list) {
	case 0:
		return ""
	case 1:
		return list[0]
	}
	return strings.Join(list[:len(list)-1], ", ") + " and " + list[len(list)-1]
}

// writeAdvisoryTable writes the open advisories, required first; -v (or
// all) adds the dismissed rows and the column STATE.
func writeAdvisoryTable(o *Out, list []api.Advisory, all bool) {
	st := PlainStyle()
	t := NewTable("ADVISORY", "LEVEL", "STATE", "FACTS")
	t.Wide[2] = !all
	rows := 0
	for _, adv := range list {
		if adv.State != api.AdvisoryStateOpen && adv.State != api.AdvisoryStatePending && !all && o.Verbose == 0 {
			continue
		}
		facts := advisory.Short(adv, st.Arrow())
		if adv.CarriedFrom != "" {
			facts += " (from the check of " + api.MustTime(adv.CarriedFrom).UTC().Format("2006-01-02 15:04") + ")"
		}
		if adv.Dismissal != nil && adv.Dismissal.Stale {
			facts += " (dismissal stale)"
		}
		t.Row(adv.ID, string(adv.Level), string(adv.State), facts)
		rows++
	}
	if rows > 0 {
		_ = t.Write(o.Stdout, all || o.Verbose > 0)
	}
}

// checkTotals gives the totals line of repoctl check, for stdout.
func checkTotals(o *Out, list []api.Advisory, sum *advisory.Summary) string {
	counts := map[api.AdvisoryLevel]int{}
	open, dismissed := 0, 0
	for _, adv := range list {
		switch adv.State {
		case api.AdvisoryStateDismissed:
			dismissed++
		default:
			open++
			counts[adv.Level]++
		}
	}
	line := fmt.Sprintf("%d open", open)
	var parts []string
	for _, l := range []api.AdvisoryLevel{api.AdvisoryLevelRequired, api.AdvisoryLevelWarn, api.AdvisoryLevelInfo} {
		if counts[l] == 0 {
			continue
		}
		s := fmt.Sprintf("%d %s", counts[l], l)
		if l == api.AdvisoryLevelRequired {
			s = o.StdoutStyle.Red(s)
		}
		parts = append(parts, s)
	}
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	}
	if dismissed > 0 {
		line += fmt.Sprintf("  ·  %d dismissed", dismissed)
	}
	var targets []string
	all := false
	for _, r := range sum.Ran {
		if r.Target == "" {
			all = true
			continue
		}
		if !chkContains(targets, r.Target) {
			targets = append(targets, r.Target)
		}
	}
	switch {
	case sum.Waited:
		line += "  ·  checked " + sum.File.Time().Local().Format("15:04") + " by another check"
	case len(targets) > 0:
		line += "  ·  " + chkJoinAnd(targets) + " checked in " + api.FormatDuration(sum.Duration)
	case all:
		line += "  ·  checked in " + api.FormatDuration(sum.Duration)
	}
	if dismissed > 0 && o.Verbose == 0 {
		line += "  (-v shows all)"
	}
	return line
}

// advisoryHint writes the hint of the first open advisory.
func advisoryHint(o *Out, list []api.Advisory) {
	for _, adv := range list {
		if adv.State != api.AdvisoryStateOpen {
			continue
		}
		if adv.Level == api.AdvisoryLevelRequired {
			o.Hint("read the facts and the closure", "repoctl advisories show "+adv.ID)
		} else {
			o.Hint("read the facts", "repoctl advisories show "+adv.ID)
		}
		return
	}
}

// checkTimer installs or removes the check timer of the set.
func (a *App) checkTimer(ctx context.Context, set *config.Set, dir string, install bool) (*Result, error) {
	t := a.advTimer(dir)
	cf, _ := advisory.ReadCheckFile(a.paths, set.Name)
	d := a.loadDismissals(dir)
	item := api.AdvisoryList{Advisories: advisory.Current(cf, d, nil), Check: cf.Summary()}
	home := a.Getenv("HOME")
	tilde := func(p string) string {
		if rel, err := filepath.Rel(home, p); err == nil && home != "" && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
		return p
	}
	if !install {
		had, err := t.Remove(ctx)
		if err != nil {
			return nil, err
		}
		item.Check.Timer = t.State(ctx)
		return &Result{Kind: api.KindAdvisoryList, Items: []any{item}, Human: func(o *Out) error {
			if had {
				o.Printf("stopped and removed %s\n", t.Instance())
			} else {
				o.Note("this set has no timer %s", t.Instance())
			}
			return nil
		}}, nil
	}
	r, err := t.Install(ctx)
	if err != nil {
		return nil, err
	}
	item.Check.Timer = t.State(ctx)
	return &Result{Kind: api.KindAdvisoryList, Items: []any{item}, Human: func(o *Out) error {
		for _, p := range r.Wrote {
			o.Printf("wrote %s\n", tilde(p))
		}
		for _, p := range r.Updated {
			o.Printf("updated %s\n", tilde(p))
		}
		o.Printf("started %s (next: %s)\n", r.Started, r.Next)
		if !r.Linger && !o.Quiet {
			u := username(a.Getenv)
			fmt.Fprintln(o.Stderr, o.Style.Dim("note: lingering is off. The timer runs only while you are logged in."))
			fmt.Fprintln(o.Stderr, o.Style.Dim("      Persistent=true runs a missed check at the next login."))
			fmt.Fprintln(o.Stderr, o.Style.Dim("      To run it always: loginctl enable-linger "+u))
		}
		return nil
	}}, nil
}

// advisoryList gives the current list of the set: the last check (after a
// check with refresh) with the dismissals.
func (a *App) advisoryList(ctx context.Context, refresh bool) (*config.Set, string, *advisory.CheckFile, []api.Advisory, *advisory.Dismissals, error) {
	set, dir, err := a.Set()
	if err != nil {
		return nil, "", nil, nil, nil, err
	}
	var cf *advisory.CheckFile
	if refresh {
		if err := a.Preflight(ctx, doctor.NeedCheck, nil, 0); err != nil {
			return nil, "", nil, nil, nil, err
		}
		var notes []string
		sum, err := a.runCheck(ctx, set, dir, nil, nil, &notes)
		if err != nil {
			return nil, "", nil, nil, nil, err
		}
		cf = sum.File
	} else if cf, err = advisory.ReadCheckFile(a.paths, set.Name); err != nil {
		return nil, "", nil, nil, nil, err
	}
	d := a.loadDismissals(dir)
	// HOOK: the advisories of the runs with staged nodes (pending
	// soname.removed and abi.changed) join here when the run records
	// carry them.
	return set, dir, cf, advisory.Current(cf, d, nil), d, nil
}

// checkHead gives the head line of advisories ls: the age and the source
// of the last check.
func (a *App) checkHead(cf *advisory.CheckFile) string {
	if cf == nil {
		return "never checked: run repoctl check"
	}
	at := cf.Time()
	s := "check " + at.UTC().Format("2006-01-02 15:04")
	if cf.By != "" {
		s += " (" + cf.By + ")"
	}
	s += ", " + advisory.Ago(a.Now().Sub(at)) + " ago"
	if cf.Ok() {
		return s + " · ok"
	}
	return s + " · failed"
}

func hAdvisories(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	levelText, _ := cmd.Flags().GetString("level")
	withDismissed, _ := cmd.Flags().GetBool("dismissed")
	refresh, _ := cmd.Flags().GetBool("refresh")
	var level api.AdvisoryLevel
	if levelText != "" {
		l, ok := advisory.ParseLevel(levelText)
		if !ok {
			return nil, usagef("--level %q: want info, warn or required", levelText)
		}
		level = l
	}
	set, dir, cf, list, d, err := a.advisoryList(ctx, refresh)
	if err != nil {
		return nil, err
	}
	var shown []api.Advisory
	for _, adv := range list {
		if level == "" || adv.Level == level {
			shown = append(shown, adv)
		}
	}
	if shown == nil {
		shown = []api.Advisory{}
	}
	item := api.AdvisoryList{Advisories: shown, Check: cf.Summary()}
	item.Check.Timer = a.advTimer(dir).State(ctx)
	byID := map[string]api.Advisory{}
	for _, adv := range list {
		byID[adv.ID] = adv
	}
	return &Result{Kind: api.KindAdvisoryList, Items: []any{item}, Targets: activeTargetIDs(set), Human: func(o *Out) error {
		o.Printf("%s\n", a.checkHead(cf))
		writeAdvisoryTable(o, shown, withDismissed)
		if withDismissed && d != nil {
			t := NewTable("DISMISSED", "BY", "AT", "NOTE")
			n := 0
			for _, id := range d.IDs() {
				e, _ := d.Get(id)
				if _, ok := byID[id]; ok && !advisory.Stale(e, byID) {
					continue
				}
				t.Row(id+" (stale)", e.By, e.At.UTC().Format("2006-01-02"), e.Note)
				n++
			}
			if n > 0 {
				_ = o.Table(t)
				o.Hint("remove the stale dismissals", "repoctl advisories undismiss --stale")
			}
		}
		open, dismissed := 0, 0
		for _, adv := range shown {
			if adv.State == api.AdvisoryStateDismissed {
				dismissed++
			} else {
				open++
			}
		}
		if !o.Quiet {
			line := fmt.Sprintf("%d open", open)
			if dismissed > 0 {
				line += fmt.Sprintf("  ·  %d dismissed", dismissed)
				if !withDismissed && o.Verbose == 0 {
					line += "  (--dismissed shows them)"
				}
			}
			o.Printf("%s\n", line)
		}
		if cf == nil {
			o.Hint("run the check daily", "repoctl check --install-timer")
		} else {
			advisoryHint(o, shown)
		}
		return nil
	}}, nil
}

func hAdvShow(ctx context.Context, a *App, _ *cobra.Command, args []string) (*Result, error) {
	_, _, cf, list, _, err := a.advisoryList(ctx, false)
	if err != nil {
		return nil, err
	}
	id := args[0]
	var adv *api.Advisory
	for i := range list {
		if list[i].ID == id {
			adv = &list[i]
		}
	}
	if adv == nil {
		return nil, api.Errorf(api.PAdvisoryUnknown, "no advisory has the id %s", id).WithHint("list the advisories", "repoctl advisories ls")
	}
	return &Result{Kind: api.KindAdvisory, Items: []any{*adv}, Human: func(o *Out) error {
		src := adv.Source
		if cf != nil && src == "check" {
			src = "check " + cf.Time().UTC().Format("2006-01-02 15:04")
			if cf.By != "" {
				src += " (" + cf.By + ")"
			}
		}
		o.Printf("%s  %s · %s · %s\n", adv.ID, adv.Level, adv.State, src)
		const w = 8
		kv := func(key string, lines []string) {
			for i, l := range lines {
				if i == 0 {
					o.KV(w, key, l)
				} else {
					o.KV(w, "", l)
				}
			}
		}
		kv("facts", advisory.FactsLines(*adv))
		if c := advisory.ClosureLine(*adv); c != "" {
			kv("closure", []string{c})
		}
		if adv.CarriedFrom != "" {
			kv("carried", []string{"the last check failed for this code; these facts come from the check of " + api.MustTime(adv.CarriedFrom).UTC().Format("2006-01-02 15:04")})
		}
		if s := adv.Suggestion; s != nil {
			var lines []string
			if s.Text != "" {
				lines = append(lines, s.Text)
			}
			if s.Command != "" {
				lines = append(lines, s.Command)
			}
			kv("suggests", lines)
		}
		if dm := adv.Dismissal; dm != nil {
			l := "by " + dm.By + " " + api.MustTime(dm.At).UTC().Format("2006-01-02") + ": " + dm.Note
			if dm.Stale {
				l += " (stale: the facts changed)"
			}
			kv("dismissed", []string{l})
		}
		kv("digest", []string{adv.FactsDigest})
		return nil
	}}, nil
}

// gitEmail gives git config user.email in the set directory.
func chkGitEmail(ctx context.Context, dir string, getenv func(string) string) string {
	if out, err := git.New(dir).Run(ctx, "config", "user.email"); err == nil && strings.TrimSpace(out) != "" {
		return strings.TrimSpace(out)
	}
	return username(getenv) + "@" + hostname()
}

func hAdvDismiss(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	note, _ := cmd.Flags().GetString("note")
	if len(strings.TrimSpace(note)) < advisory.MinNote {
		return nil, usagef("dismiss needs --note with at least %d characters: the note tells the team why", advisory.MinNote)
	}
	set, dir, err := a.Set()
	if err != nil {
		return nil, err
	}
	h := lock.Holder{Command: a.cmdName(), PID: os.Getpid(), Host: hostname(), User: username(a.Getenv), Started: a.Now()}
	l, err := advisory.LockDismissals(ctx, a.paths, set.Name, h)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	d, err := advisory.LoadDismissals(dir)
	if err != nil {
		return nil, err
	}
	cf, err := advisory.ReadCheckFile(a.paths, set.Name)
	if err != nil {
		return nil, err
	}
	current := advisory.Current(cf, d, nil)
	done, err := advisory.Dismiss(d, current, args, note, chkGitEmail(ctx, dir, a.Getenv), a.Now())
	if err != nil {
		return nil, err
	}
	if err := d.Save(); err != nil {
		return nil, err
	}
	after := advisory.Current(cf, d, nil)
	var changed []api.Advisory
	for _, adv := range after {
		if chkContains(args, adv.ID) {
			changed = append(changed, adv)
		}
	}
	msg := "chore: dismiss " + strings.Join(args, " ")
	next := []api.NextStep{{Action: "commit", Command: "git add " + advisory.DismissalPath + " && git commit -m \"" + msg + "\" && git push"}}
	item := api.AdvisoryList{Advisories: changed, Check: cf.Summary()}
	return &Result{Kind: api.KindAdvisoryList, Items: []any{item}, Next: next, Targets: activeTargetIDs(set), Human: func(o *Out) error {
		for _, r := range done {
			o.Printf("dismissed %s (facts %s)\n", r.ID, api.ShortDigest(r.Facts))
		}
		return nil
	}}, nil
}

func hAdvUndismiss(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	stale, _ := cmd.Flags().GetBool("stale")
	if stale == (len(args) > 0) {
		return nil, usagef("give the ids to undismiss, or --stale")
	}
	set, dir, err := a.Set()
	if err != nil {
		return nil, err
	}
	h := lock.Holder{Command: a.cmdName(), PID: os.Getpid(), Host: hostname(), User: username(a.Getenv), Started: a.Now()}
	l, err := advisory.LockDismissals(ctx, a.paths, set.Name, h)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	d, err := advisory.LoadDismissals(dir)
	if err != nil {
		return nil, err
	}
	cf, err := advisory.ReadCheckFile(a.paths, set.Name)
	if err != nil {
		return nil, err
	}
	removed := args
	if stale {
		removed, err = advisory.UndismissStale(d, advisory.Current(cf, d, nil), cf.Complete(set))
	} else {
		err = advisory.Undismiss(d, args)
	}
	if err != nil {
		return nil, err
	}
	if len(removed) > 0 {
		if err := d.Save(); err != nil {
			return nil, err
		}
	}
	after := advisory.Current(cf, d, nil)
	changed := []api.Advisory{}
	for _, adv := range after {
		if chkContains(removed, adv.ID) {
			changed = append(changed, adv)
		}
	}
	var next []api.NextStep
	if len(removed) > 0 {
		msg := "chore: undismiss " + strings.Join(removed, " ")
		if stale {
			msg = "chore: remove stale advisory dismissals"
		}
		next = []api.NextStep{{Action: "commit", Command: "git add " + advisory.DismissalPath + " && git commit -m \"" + msg + "\" && git push"}}
	}
	item := api.AdvisoryList{Advisories: changed, Check: cf.Summary()}
	return &Result{Kind: api.KindAdvisoryList, Items: []any{item}, Next: next, Targets: activeTargetIDs(set), Human: func(o *Out) error {
		for _, id := range removed {
			o.Printf("undismissed %s\n", id)
		}
		if len(removed) == 0 {
			o.Note("no stale dismissal")
		}
		return nil
	}}, nil
}

func hUpstream(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	fl := cmd.Flags()
	checkFlag, _ := fl.GetBool("check")
	releases, _ := fl.GetBool("releases")
	refresh, _ := fl.GetBool("refresh")
	jobs, _ := fl.GetInt("jobs")
	if jobs < 1 {
		return nil, usagef("-j %d: want 1 or more", jobs)
	}
	set, dir, err := a.Set()
	if err != nil {
		return nil, err
	}
	var pkgs []string
	if len(args) > 0 {
		if pkgs, err = plan.PackageSelectors(set, args, a.Here(set, dir)); err != nil {
			return nil, err
		}
	}
	lf, err := lock.Read(dir)
	if err != nil {
		return nil, err
	}
	c := upstream.NewClient(a.paths, a.Getenv)
	c.Refresh, c.Jobs, c.Now = refresh, jobs, a.Now
	rep, err := c.Query(ctx, set, dir, lf, pkgs)
	if err != nil {
		return nil, err
	}
	for _, w := range rep.Warnings {
		a.Warn(w)
	}
	// The dismissals of upstream.release.
	d := a.loadDismissals(dir)
	in := &advisory.Inputs{Set: set}
	advs, _ := advisory.UpstreamAdvisories(in, rep.Items)
	advisory.ApplyDismissals(advs, d)
	dismissed := map[string]string{}
	for _, adv := range advs {
		if adv.State == api.AdvisoryStateDismissed && adv.Dismissal != nil {
			dismissed[adv.Subject] = adv.Dismissal.Note
		}
	}
	items := make([]any, 0, len(rep.Items))
	counts := map[api.UpstreamStatus]int{}
	var open []string
	for _, u := range rep.Items {
		items = append(items, u)
		counts[u.Status]++
		if u.Status == api.UpstreamStatusUpdateAvailable {
			if _, ok := dismissed[u.Package]; !ok {
				open = append(open, u.Package)
			}
		}
	}
	res := &Result{Kind: api.KindUpstream, Items: items, Human: func(o *Out) error {
		t := NewTable("PACKAGE", "CURRENT", "LATEST", "RELEASED", "FEDORA", "STATUS", "NOTE")
		shownRows, folded := 0, 0
		var details []string
		for _, u := range rep.Items {
			normal := u.Status == api.UpstreamStatusUpToDate || u.Status == api.UpstreamStatusNone
			if normal && o.Verbose == 0 && len(args) == 0 {
				folded++
				continue
			}
			released, fedora := "-", "-"
			if u.Github != nil && u.Github.Date != "" {
				released = api.MustTime(u.Github.Date).UTC().Format("2006-01-02")
			}
			if u.Fedora != nil && u.Fedora.Version != "" {
				fedora = u.Fedora.Version
			}
			note := u.Notes
			// An unknown row gives a short reason; the notes keep the
			// error, which -v shows under the table.
			if r := rep.Reasons[u.Package]; r != "" {
				note = r
				details = append(details, u.Package+": "+u.Notes)
			}
			if n, ok := dismissed[u.Package]; ok {
				note = "dismissed: " + n
			}
			cur, latest := chkDash(u.Current.Version), chkDash(u.Latest)
			t.Row(u.Package, cur, latest, released, fedora, string(u.Status), note)
			shownRows++
		}
		if shownRows > 0 {
			if err := o.Table(t); err != nil {
				return err
			}
		}
		if releases {
			rt := NewTable("PACKAGE", "RELEASE", "URL")
			n := 0
			for _, u := range rep.Items {
				if u.Github == nil {
					continue
				}
				for _, tag := range u.Github.Newer {
					url := "https://github.com/" + u.Github.Repo + "/releases/tag/" + tag
					if tag == u.Github.Tag && u.Github.URL != "" {
						url = u.Github.URL
					}
					rt.Row(u.Package, tag, url)
					n++
				}
			}
			if n > 0 {
				o.Printf("\n")
				if err := o.Table(rt); err != nil {
					return err
				}
			}
		}
		if !o.Quiet {
			o.Printf("%s\n", upstreamTotals(len(rep.Items), counts, len(dismissed), folded > 0 && o.Verbose == 0))
		}
		// The errors of the unknown rows: -v.
		if o.Verbose > 0 {
			for _, d := range details {
				o.Note("%s", d)
			}
		}
		if len(open) > 0 && !releases {
			o.Hint("read the release notes", "repoctl upstream "+strings.Join(open, " ")+" --releases")
		}
		return nil
	}}
	switch {
	case rep.AllFailed():
		var first error
		for _, e := range rep.Failed {
			first = e
			break
		}
		return res, api.Errorf(api.PUpstreamUnreachable, "no upstream answered: %v", first).WithExit(api.ExitFailed).
			WithHint("try again later", "repoctl upstream --refresh")
	case checkFlag && len(open) > 0:
		// Exit 11 is a finding, not a failure: no error line.
		return res, &silent{err: api.Errorf(api.PUpstreamRelease, "%s", chkPlural(len(open), "update")+" available: "+strings.Join(open, ", ")).
			WithExit(api.ExitDifferences)}
	}
	return res, nil
}

// activeTargetIDs gives the active targets of the set.
func activeTargetIDs(set *config.Set) []api.TargetId {
	var out []api.TargetId
	for _, t := range set.ActiveTargets() {
		out = append(out, t.ID)
	}
	return out
}

func chkDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// upstreamTotals gives the totals line of repoctl upstream.
func upstreamTotals(n int, counts map[api.UpstreamStatus]int, dismissed int, folded bool) string {
	var parts []string
	if c := counts[api.UpstreamStatusUpdateAvailable]; c > 0 {
		s := chkPlural(c, "update")
		if dismissed > 0 {
			s += fmt.Sprintf(" (%d dismissed)", dismissed)
		}
		parts = append(parts, s)
	}
	for _, st := range []api.UpstreamStatus{api.UpstreamStatusFrozen, api.UpstreamStatusAhead, api.UpstreamStatusUnknown, api.UpstreamStatusUpToDate} {
		if c := counts[st]; c > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c, st))
		}
	}
	if c := counts[api.UpstreamStatusNone]; c > 0 {
		parts = append(parts, fmt.Sprintf("%d without upstream", c))
	}
	line := chkPlural(n, "package") + ": " + strings.Join(parts, ", ")
	if folded {
		line += "  (-v shows all)"
	}
	return line
}
