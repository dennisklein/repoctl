package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/config/schema"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/plan"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// The handlers of workspace: init, migrate, checkout, pin, rebuild
// (cli.html#group-workspace).

func init() {
	for _, n := range schema.Names() {
		FileSchemas[n] = func() []byte { return schema.File(n) }
	}
}

// startDir gives the start directory of a command without a set: -C, else
// the current directory.
func (a *App) startDir() (string, error) {
	d := a.dir
	if d == "" {
		d = "."
	}
	abs, err := filepath.Abs(d)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", api.Errorf(api.PConfigInvalid, "%s is not a directory", d).WithPath(abs)
	}
	return abs, nil
}

// workspaceEnv gives the environment of the workspace commands.
func (a *App) workspaceEnv(cmd *cobra.Command, set *config.Set, setDir string) workspace.Env {
	e := workspace.Env{
		Paths: a.paths, Set: set, SetDir: setDir, LockWait: a.LockWait(),
		Holder: lock.Holder{Command: commandName(cmd), PID: os.Getpid(), Host: hostname(), User: username(a.Getenv), Started: a.Now()},
		Jobs:   2,
	}
	if eff, err := a.Settings(); err == nil {
		e.Jobs = eff.Int("jobs")
	}
	return e
}

// step is one numbered line of a Next list.
type step struct {
	text    string
	command string
	action  string
	human   bool
}

// nextOf gives the NextStep items of numbered steps.
func nextOf(steps []step) []api.NextStep {
	var out []api.NextStep
	for _, s := range steps {
		action := s.action
		if action == "" {
			action = "other"
		}
		out = append(out, api.NextStep{Action: action, Human: s.human, Reason: s.text, Command: s.command})
	}
	return out
}

// wsWriteSteps writes "Next: HEAD" and the numbered steps to stderr.
func wsWriteSteps(o *Out, head string, first int, steps []step) {
	if o.Quiet {
		return
	}
	fmt.Fprintln(o.Stderr, "Next: "+head)
	for i, s := range steps {
		for j, l := range strings.Split(s.text, "\n") {
			if j == 0 {
				fmt.Fprintf(o.Stderr, "  %2d. %s\n", first+i, l)
			} else {
				fmt.Fprintf(o.Stderr, "      %s\n", l)
			}
		}
	}
}

// fileLines gives one line per file: "wrote repoctl.yaml   SUMMARY", with
// the summaries in one column.
func fileLines(files []api.File, summary func(api.File) string) []string {
	heads := make([]string, len(files))
	width := 0
	for i, f := range files {
		verb := "wrote"
		switch f.Action {
		case "unchanged":
			verb = "kept"
		case "skipped":
			verb = "skipped"
		case "would-create", "would-update":
			verb = "would write"
		}
		heads[i] = verb + " " + f.Path
		width = max(width, len(heads[i]))
	}
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = heads[i]
		if s := summary(f); s != "" {
			out[i] += strings.Repeat(" ", width-len(heads[i])) + "   " + s
		}
	}
	return out
}

// packageSteps are the steps of "repoctl help packages" for the first
// package.
var packageSteps = []step{
	{text: "add id, branch, upstream to repoctl.yaml", action: "other", human: true},
	{text: "repoctl checkout --create PKG --from fedora      (or --orphan)", command: "repoctl checkout --create PKG --from fedora", action: "checkout"},
	{text: "write the spec; git -C packages/PKG commit          package branch", command: "git -C packages/PKG commit", action: "commit", human: true},
	{text: "git -C packages/PKG push origin BRANCH", command: "git -C packages/PKG push origin BRANCH", action: "push", human: true},
	{text: "repoctl pin PKG", command: "repoctl pin PKG", action: "pin"},
	{text: "git commit repoctl.yaml repoctl.lock; git push    set branch", command: "git commit repoctl.yaml repoctl.lock && git push", action: "commit", human: true},
	{text: "repoctl plan           expect: PKG \"new-package\"", command: "repoctl plan", action: "other"},
	{text: "repoctl build; repoctl publish", command: "repoctl build", action: "build"},
}

// newSetSteps gives the steps 3 to 15 of "repoctl help new-set" for the
// new set.
func newSetSteps(from, oldName, name string) []step {
	return []step{
		{text: "set repo.url in repoctl.yaml; edit the release package: Version " + setVersion(name) + "; repoctl release write", command: "repoctl release write", action: "other", human: true},
		{text: fmt.Sprintf("git commit -am \"feat: start set %s\"            set branch", name), command: fmt.Sprintf("git commit -am \"feat: start set %s\"", name), action: "commit", human: true},
		{text: "repoctl checkout --create --from-set " + from, command: "repoctl checkout --create --from-set " + from, action: "checkout"},
		{text: "git push origin BRANCH...                         package branches\n(checkout prints the full command, also for external packages)", command: "git push origin BRANCH...", action: "push", human: true},
		{text: "repoctl pin\ngit commit repoctl.lock && git push origin set/" + name, command: "repoctl pin", action: "pin"},
		{text: "repoctl doctor", command: "repoctl doctor", action: "other"},
		{text: "repoctl plan           expect: every node \"new-package\"", command: "repoctl plan", action: "other"},
		{text: "repoctl build", command: "repoctl build", action: "build"},
		{text: "repoctl publish        first snapshot of each target", command: "repoctl publish", action: "publish", human: true},
		{text: "an organization owner sets each new OCI package public and turns on\n\"Inherit access from source repository\" (doctor: registry.public)", action: "other", human: true},
		{text: "add one mirror line per target to the mirror job on the HTTP server,\nand put the key file of the release at its keyring", action: "other", human: true},
		{text: "install the release package on the clients (config management),\nthen confirm it for each active key and target:\nrepoctl sign deployed KEY --release NVR --target T", command: "repoctl sign deployed KEY --release NVR --target T", action: "other", human: true},
		{text: "repoctl check --install-timer", command: "repoctl check --install-timer", action: "other"},
	}
}

// setVersion gives "26.11" of the set name slurm-26.11: the Version of the
// release package.
func setVersion(name string) string {
	if i := strings.LastIndex(name, "-"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// parseTargets reads the values of --add-target ID=MOCKCFG.
func parseTargets(values []string) ([]config.Target, error) {
	var out []config.Target
	for _, v := range values {
		id, mock, ok := strings.Cut(v, "=")
		if !ok || id == "" || mock == "" {
			return nil, usagef("invalid --add-target value %q: want ID=MOCKCFG, for example el10=rocky+epel-10-x86_64", v)
		}
		out = append(out, config.Target{ID: id, Mock: mock, Plugins: []string{"rpmautospec"}})
	}
	return out, nil
}

func hInit(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	dir, err := a.startDir()
	if err != nil {
		return nil, err
	}
	fl := cmd.Flags()
	name, _ := fl.GetString("name")
	repo, _ := fl.GetString("repo")
	from, _ := fl.GetString("from")
	force, _ := fl.GetBool("force")
	addTargets, _ := fl.GetStringArray("add-target")
	targets, err := parseTargets(addTargets)
	if err != nil {
		return nil, err
	}
	if from != "" {
		if name == "" {
			return nil, usagef("repoctl init --from %s needs --name NAME, the name of the new set", from)
		}
		if fl.Changed("repo") || fl.Changed("add-target") {
			return nil, usagef("repoctl init --from takes the repo and the targets of %s; leave out --repo and --add-target", from)
		}
	}
	rep, err := workspace.Init(ctx, workspace.InitOptions{Dir: dir, Name: name, Repo: repo, Targets: targets, From: from, Force: force})
	if err != nil {
		return nil, err
	}
	res := &Result{Kind: api.KindFile, NextShown: true}
	for _, f := range rep.Files {
		res.Items = append(res.Items, f)
	}
	var steps []step
	head := ""
	first := 1
	if from != "" {
		steps = newSetSteps(from, rep.OldName, rep.Set.Name)
		head = fmt.Sprintf("steps 3 to %d of \"repoctl help new-set\":", 2+len(steps))
		first = 3
	} else {
		steps = packageSteps
		head = "add the first package, with the steps of \"repoctl help packages\":"
	}
	res.Next = nextOf(steps)
	res.Human = func(o *Out) error {
		var main []api.File
		for _, f := range rep.Files {
			if f.Path == config.SetFile || f.Path == lock.FileName {
				main = append(main, f)
			}
		}
		for _, l := range fileLines(main, func(f api.File) string {
			if f.Path == config.SetFile {
				return initSummary(rep)
			}
			return "empty"
		}) {
			o.Printf("%s\n", l)
		}
		if len(rep.Copied) > 0 {
			o.Printf("copied %s from %s\n", strings.Join(rep.Copied, ", "), from)
		}
		if len(rep.Skipped) > 0 {
			o.Note("kept %s: the files exist (--force writes over them)", strings.Join(rep.Skipped, ", "))
		}
		for _, k := range rep.Dropped {
			o.Printf("dropped key %s (%s): no RPM of the new set carries it\n", k.ID, k.State)
		}
		if len(rep.Deleted) > 0 {
			o.Printf("deleted %s\n", strings.Join(rep.Deleted, ", "))
		}
		wsWriteSteps(o, head, first, steps)
		return nil
	}
	return res, nil
}

func initSummary(rep *workspace.InitReport) string {
	s := rep.Set
	if rep.From == "" {
		var ids []string
		for _, t := range s.Targets {
			ids = append(ids, t.ID)
		}
		return fmt.Sprintf("set %s, repo %s, %s (%s), %d packages", s.Name, s.Repo.Name, wsPlural(len(s.Targets), "target"), strings.Join(ids, " "), len(s.Packages))
	}
	return fmt.Sprintf("set %s, %s renamed (%s → %s), %s", s.Name, wsPlural(rep.Renamed, "branch"), rep.OldName, s.Name, wsPlural(len(rep.Shared), "shared branch"))
}

// wsPlural gives "1 branch", "2 branches", "0 targets".
func wsPlural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if strings.HasSuffix(word, "ch") || strings.HasSuffix(word, "s") {
		return fmt.Sprintf("%d %ses", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// migrateSteps are the Next steps of migrate.
var migrateSteps = []step{
	{text: "add the targets, store.oci and repo.url; check the release package and sign.keys", action: "other", human: true},
	{text: "update CLAUDE.md: it still tells agents to run ./scripts/build", action: "other", human: true},
	{text: "repoctl doctor; repoctl plan", command: "repoctl doctor", action: "other"},
	{text: "import the published repo: https://dennisklein.github.io/repoctl/manual/runbooks/import.html\nkeep packages.lock and scripts/ until the first publish with repoctl", action: "other", human: true},
}

func hMigrate(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	fl := cmd.Flags()
	dry, _ := fl.GetBool("dry-run")
	if moveAPI, _ := fl.GetBool("api"); moveAPI {
		if fl.Changed("name") || fl.Changed("force") {
			return nil, usagef("repoctl migrate --api takes only --dry-run")
		}
		return migrateAPI(ctx, a, dry)
	}
	dir, err := a.startDir()
	if err != nil {
		return nil, err
	}
	name, _ := fl.GetString("name")
	force, _ := fl.GetBool("force")
	rep, err := workspace.Migrate(ctx, workspace.MigrateOptions{Dir: dir, Name: name, DryRun: dry, Force: force})
	if err != nil {
		return nil, err
	}
	for _, w := range rep.Warnings {
		a.Warn(w)
	}
	res := &Result{Kind: api.KindFile, NextShown: true, ShownWarnings: true, Next: nextOf(migrateSteps)}
	for _, f := range rep.Files {
		res.Items = append(res.Items, f)
	}
	res.Human = func(o *Out) error {
		if dry {
			for _, f := range rep.Files {
				o.Printf("--- %s\n%s", f.Path, rep.Texts[f.Path])
			}
			o.Printf("\n")
		}
		for _, line := range fileLines(rep.Files, func(f api.File) string { return rep.Summary[f.Path] }) {
			if dry {
				o.Errorf("%s\n", o.Style.Dim(line))
			} else {
				o.Printf("%s\n", line)
			}
		}
		for _, w := range a.warnings {
			o.Warning(w)
		}
		if dry {
			o.Note("dry run: nothing is written")
		}
		wsWriteSteps(o, "check the files, then:", 1, migrateSteps)
		return nil
	}
	return res, nil
}

// setFiles are the four stored set files (contract.html#stored).
var setFiles = []struct{ path, kind string }{
	{config.SetFile, "config"},
	{lock.FileName, "lock"},
	{"keys/deployed.yaml", "other"},
	{".repoctl/advisories.yaml", "other"},
}

// migrateAPI moves the four set files to the newest apiVersion. This
// release knows one apiVersion, so each file that exists is unchanged.
func migrateAPI(ctx context.Context, a *App, dry bool) (*Result, error) {
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	l, err := a.SetLock(ctx, set, false)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	res := &Result{Kind: api.KindFile}
	var lines []string
	for _, f := range setFiles {
		data, err := os.ReadFile(filepath.Join(setDir, filepath.FromSlash(f.path)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var head struct {
			APIVersion string `yaml:"apiVersion"`
		}
		_ = yaml.Unmarshal(data, &head)
		if head.APIVersion != "" && head.APIVersion != api.APIVersion {
			return nil, api.Errorf(api.PContractNewerFile, "%s: apiVersion %s is newer than this repoctl (writes %s)", f.path, head.APIVersion, api.APIVersion).
				WithPath(f.path).WithHint("upgrade repoctl on this host", "repoctl help upgrade")
		}
		res.Items = append(res.Items, api.File{Path: f.path, Action: "unchanged", Kind: f.kind, Bytes: len(data)})
		lines = append(lines, f.path)
	}
	res.Human = func(o *Out) error {
		width := 0
		for _, p := range lines {
			width = max(width, len(p))
		}
		for _, p := range lines {
			o.Printf("kept %s%s  apiVersion %s is the newest\n", p, strings.Repeat(" ", width-len(p)), api.APIVersion)
		}
		if dry {
			o.Note("dry run: nothing is written")
		}
		return nil
	}
	return res, nil
}

// packageArgs gives the packages that the arguments select, or nil for
// all packages when there is no argument.
func (a *App) packageArgs(set *config.Set, setDir string, args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, nil
	}
	return plan.PackageSelectors(set, args, a.Here(set, setDir))
}

func hCheckout(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	fl := cmd.Flags()
	create, _ := fl.GetBool("create")
	fromSet, _ := fl.GetString("from-set")
	from, _ := fl.GetString("from")
	orphan, _ := fl.GetBool("orphan")
	fetch, _ := fl.GetBool("fetch")
	force, _ := fl.GetBool("force")
	starts := 0
	for _, given := range []bool{fromSet != "", from != "", orphan} {
		if given {
			starts++
		}
	}
	switch {
	case create && starts != 1:
		return nil, usagef("repoctl checkout --create needs exactly one start point: say --from-set, --from, --from fedora or --orphan")
	case !create && starts > 0:
		return nil, usagef("--from-set, --from and --orphan need --create")
	case create && force:
		return nil, usagef("repoctl checkout --create does not take --force")
	}
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	ids, err := a.packageArgs(set, setDir, args)
	if err != nil {
		return nil, err
	}
	l, err := a.SetLock(ctx, set, false)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	lf, err := lock.Read(setDir)
	if err != nil {
		return nil, err
	}
	e := a.workspaceEnv(cmd, set, setDir)
	if create {
		ref, fedora, fbranch := workspace.ParseFrom(from)
		rep, err := workspace.Create(ctx, e, lf, workspace.CreateOptions{
			Packages: ids, FromSet: fromSet, From: ref, FromFedora: fedora, FedoraBranch: fbranch, Orphan: orphan,
			FedoraURL: a.Getenv(workspace.EnvTestFedoraURL),
		})
		if err != nil {
			return nil, err
		}
		return a.createResult(ctx, e, lf, rep)
	}
	sess, pctx := a.startProgress(ctx, "checkout")
	rep, err := workspace.Checkout(pctx, e, lf, workspace.CheckoutOptions{Packages: ids, Fetch: fetch, Force: force})
	sess.finish(err)
	if err != nil {
		return nil, err
	}
	return a.checkoutResult(ctx, e, lf, rep)
}

// progressSession is the progress display of a command with steps.
type progressSession struct {
	s   *cliprogress.Session
	cmd *progress.Span
}

// startProgress starts the progress display on stderr (terminal.html#checkout).
func (a *App) startProgress(ctx context.Context, name string) (*progressSession, context.Context) {
	ps := &progressSession{}
	if a.jsonOut || a.quiet {
		return ps, ctx
	}
	s, err := cliprogress.Start(ctx, a.ProgressOptions("repoctl"))
	if err != nil {
		return ps, ctx
	}
	ps.s = s
	pctx, span := progress.Start(s.Context(), progress.KindCommand, name)
	ps.cmd = span
	return ps, pctx
}

func (ps *progressSession) finish(err error) {
	ps.cmd.End(err)
	if ps.s != nil {
		ps.s.Finish(err != nil)
	}
}

// rowEntry gives the Lock item of a row.
func rowEntry(ctx context.Context, e workspace.Env, lf *lock.File, r workspace.Row) api.LockEntry {
	if r.State.Package != "" {
		return r.State.Entry()
	}
	if pk, ok := e.Set.Package(r.Package); ok {
		if st, err := workspace.State(ctx, e.Paths, e.SetDir, *pk, lf); err == nil {
			return st.Entry()
		}
		if pk.InTree() {
			return api.LockEntry{Package: pk.ID, Path: pk.Path, State: api.LockStateInTree}
		}
		return api.LockEntry{Package: pk.ID, Branch: pk.Branch, URL: pk.URL(), State: api.LockStateMissing}
	}
	return api.LockEntry{Package: r.Package, State: api.LockStateMissing}
}

// failedError gives the error of a checkout with failed packages: the
// problem of the first one, exit 1.
func failedError(failed []workspace.Row) error {
	if len(failed) == 0 {
		return nil
	}
	first := *failed[0].Err
	first.ExitCode = api.ExitFailed
	if len(failed) > 1 {
		var rest []string
		for _, r := range failed[1:] {
			rest = append(rest, r.Package+" ("+r.Err.Code+")")
		}
		first.Message += "; also failed: " + strings.Join(rest, ", ")
	}
	return &first
}

func (a *App) checkoutResult(ctx context.Context, e workspace.Env, lf *lock.File, rep *workspace.Report) (*Result, error) {
	res := &Result{Kind: api.KindLock}
	for _, r := range rep.Rows {
		res.Items = append(res.Items, rowEntry(ctx, e, lf, r))
		if r.Warning != nil {
			a.Warn(*r.Warning)
		}
	}
	verbose := a.verbose > 0
	res.Human = func(o *Out) error {
		t := NewTable("PKG", "BRANCH", "LOCK", "WORKTREE", "RESULT")
		folded := 0
		for _, r := range rep.Rows {
			exception := r.Action == workspace.ActKept || r.Action == workspace.ActFailed || r.Action == workspace.ActReset ||
				r.Note != "" || r.Warning != nil
			if !exception && !verbose {
				folded++
				continue
			}
			t.RowCells(checkoutCells(o.StdoutStyle, e, r)...)
		}
		if len(t.Rows) > 0 {
			if err := o.Table(t); err != nil {
				return err
			}
		}
		if !o.Quiet {
			o.Printf("%s\n", checkoutTotals(rep, folded > 0))
		}
		return nil
	}
	return res, failedError(rep.Failed())
}

func checkoutCells(st Style, e workspace.Env, r workspace.Row) []Cell {
	branch, lockC, head := "", "-", "-"
	if pk, ok := e.Set.Package(r.Package); ok {
		branch = pk.Branch
		if pk.InTree() {
			branch = "in tree"
		} else if pk.External() {
			branch += " (external)"
		}
	}
	if r.State.LockCommit != "" {
		lockC = api.ShortCommit(r.State.LockCommit)
	}
	if r.State.Head != "" {
		head = api.ShortCommit(r.State.Head)
	}
	text, color := r.Action, func(s string) string { return s }
	switch r.Action {
	case workspace.ActKept:
		text, color = "kept: "+r.State.Describe(), st.Amber
	case workspace.ActFailed:
		text, color = "failed: "+r.Err.Code, st.Red
	case workspace.ActReset:
		text = "reset to the lock commit"
	case workspace.ActInTree:
		text = "in tree"
	case workspace.ActCreated:
		if r.Warning != nil {
			text, color = "created: "+r.State.Describe(), st.Amber
		}
	}
	if r.Note != "" {
		text += "; " + r.Note
	}
	return []Cell{{Text: r.Package}, {Text: branch}, {Text: lockC}, {Text: head}, C(text, color)}
}

// checkoutTotals gives the totals line: "11 packages: 10 worktrees
// created, 1 in tree  (-v shows all)".
func checkoutTotals(rep *workspace.Report, folded bool) string {
	order := []string{workspace.ActCreated, workspace.ActMoved, workspace.ActReset, workspace.ActKept, workspace.ActFailed, workspace.ActInSync, workspace.ActInTree}
	names := map[string][]string{}
	for _, r := range rep.Rows {
		names[r.Action] = append(names[r.Action], r.Package)
	}
	var parts []string
	for _, a := range order {
		n := len(names[a])
		if n == 0 {
			continue
		}
		var p string
		switch a {
		case workspace.ActCreated:
			p = wsPlural(n, "worktree") + " created"
		case workspace.ActInTree:
			p = fmt.Sprintf("%d in tree", n)
		default:
			p = fmt.Sprintf("%d %s", n, a)
		}
		if a != workspace.ActInSync && a != workspace.ActInTree && a != workspace.ActCreated {
			if list := strings.Join(names[a], " "); len(list) <= 60 {
				p += " (" + list + ")"
			}
		}
		parts = append(parts, p)
	}
	line := wsPlural(len(rep.Rows), "package") + ": " + strings.Join(parts, ", ")
	if folded {
		line += "  (-v shows all)"
	}
	return line
}

func (a *App) createResult(ctx context.Context, e workspace.Env, lf *lock.File, rep *workspace.Report) (*Result, error) {
	res := &Result{Kind: api.KindLock, NextShown: false}
	created, onRemote := 0, 0
	var pushSet []string
	var pushOther []string
	var orphans []string
	for _, r := range rep.Rows {
		res.Items = append(res.Items, rowEntry(ctx, e, lf, r))
		if r.Warning != nil {
			a.Warn(*r.Warning)
		}
		if r.Action == workspace.ActFailed && r.Err.Code == api.PBranchExistsRemote {
			onRemote++
		}
		if r.Action != workspace.ActBranch {
			continue
		}
		created++
		pk, _ := e.Set.Package(r.Package)
		switch {
		case r.Orphan:
			orphans = append(orphans, r.Package)
		case pk.External():
			pushOther = append(pushOther, "git -C "+workspace.RelWorktree(pk.ID)+" push origin "+pk.Branch)
		default:
			pushSet = append(pushSet, pk.Branch)
		}
	}
	if created > 0 {
		for _, id := range orphans {
			pk, _ := e.Set.Package(id)
			res.Next = append(res.Next, api.NextStep{Action: "commit", Human: true,
				Reason:  "write the spec of " + id + " and commit it in " + workspace.RelWorktree(id),
				Command: "git -C " + workspace.RelWorktree(id) + " commit"})
			res.Next = append(res.Next, api.NextStep{Action: "push", Human: true, Reason: "push the new branch of " + id,
				Command: "git -C " + workspace.RelWorktree(id) + " push " + e.RemoteOf(*pk) + " " + pk.Branch})
		}
		if len(pushSet) > 0 {
			res.Next = append(res.Next, api.NextStep{Action: "push", Human: true, Reason: "push the new package branches",
				Command: "git push " + e.Set.Remote() + " " + strings.Join(pushSet, " ")})
		}
		for _, c := range pushOther {
			res.Next = append(res.Next, api.NextStep{Action: "push", Human: true, Reason: "push the new branch to its external server", Command: c})
		}
		res.Next = append(res.Next, api.NextStep{Action: "pin", Reason: "pin the new commits", Command: "repoctl pin"})
	}
	res.Human = func(o *Out) error {
		var rows [][]Cell
		for _, r := range rep.Rows {
			branch := ""
			if pk, ok := e.Set.Package(r.Package); ok {
				branch = pk.Branch
				if pk.External() {
					host := workspace.CloneHost(pk.URL())
					if host == "" {
						host = "external"
					}
					branch += " (" + host + ")"
				}
			}
			text, color := r.Start, func(s string) string { return s }
			switch r.Action {
			case workspace.ActFailed:
				text, color = "failed: "+r.Err.Code+": "+r.Err.Message, o.StdoutStyle.Red
			case workspace.ActExists:
				text = r.Note
			case workspace.ActInTree:
				text = r.Note
			}
			rows = append(rows, []Cell{{Text: r.Package}, {Text: branch}, C(text, color)})
		}
		writeRows(o, rows)
		if !o.Quiet {
			line := fmt.Sprintf("%s created, %d exist on the remote", wsPlural(created, "branch"), onRemote)
			if n := len(rep.Failed()) - onRemote; n > 0 {
				line += fmt.Sprintf(", %d failed", n)
			}
			o.Printf("%s\n", line)
		}
		return nil
	}
	return res, failedError(rep.Failed())
}

// writeRows writes rows without a heading, aligned like a table.
func writeRows(o *Out, rows [][]Cell) {
	t := NewTable()
	for _, r := range rows {
		for len(t.Heads) < len(r) {
			t.Heads = append(t.Heads, "")
		}
		t.RowCells(r...)
	}
	if len(t.Heads) == 0 {
		return
	}
	var b strings.Builder
	_ = t.Write(&b, true)
	// The first line is the empty heading.
	text := b.String()
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	o.Printf("%s", text)
}

func hPin(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	fl := cmd.Flags()
	older, _ := fl.GetBool("older")
	orphans, _ := fl.GetBool("orphans")
	fetch, _ := fl.GetBool("fetch")
	if older && len(args) == 0 {
		return nil, usagef("repoctl pin --older needs PKG: it pins an older commit only on purpose")
	}
	if orphans && (len(args) > 0 || older) {
		return nil, usagef("repoctl pin --orphans takes no PKG and no --older")
	}
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	var ids []string
	if len(args) > 0 {
		if err := pinArgsKnown(set, args); err != nil {
			return nil, err
		}
		if ids, err = a.packageArgs(set, setDir, args); err != nil {
			return nil, err
		}
	}
	l, err := a.SetLock(ctx, set, false)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	e := a.workspaceEnv(cmd, set, setDir)
	rep, err := workspace.Pin(ctx, e, workspace.PinOptions{Packages: ids, Older: older, Orphans: orphans, Fetch: fetch})
	if err != nil {
		return nil, err
	}
	for _, w := range rep.Warnings {
		a.Warn(w)
	}
	res := &Result{Kind: api.KindLock}
	for _, r := range rep.Rows {
		res.Items = append(res.Items, r.Entry)
	}
	if rep.Written {
		res.Next = []api.NextStep{{Action: "commit", Human: true, Reason: "commit and push repoctl.lock", Command: "git add repoctl.lock && git commit && git push"}}
	}
	res.Human = func(o *Out) error {
		var rows [][]Cell
		for _, r := range rep.Rows {
			if !r.Entry.Changed {
				continue
			}
			rows = append(rows, pinCells(o.StdoutStyle, r))
		}
		writeRows(o, rows)
		if !rep.Written {
			o.Note("repoctl.lock has no change: nothing to pin")
		}
		return nil
	}
	return res, nil
}

// pinArgsKnown gives the usage error of pin PKG for a package that
// repoctl.yaml does not have, also an orphaned one.
func pinArgsKnown(set *config.Set, args []string) error {
	for _, a := range args {
		for _, s := range strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == ' ' }) {
			if strings.ContainsAny(s, "*?[!.@") {
				continue
			}
			if _, ok := set.Package(s); !ok {
				return usagef("repoctl pin %s: %s is not in repoctl.yaml (repoctl pin --orphans removes an orphaned line)", s, s)
			}
		}
	}
	return nil
}

func pinCells(st Style, r workspace.PinRow) []Cell {
	e := r.Entry
	arrow := st.Arrow()
	switch {
	case r.Removed:
		return []Cell{{Text: e.Package}, {Text: api.ShortCommit(e.Previous) + " " + arrow + " removed"}, {Text: e.Branch}, {Text: "orphaned: not in repoctl.yaml"}}
	case r.New:
		return []Cell{{Text: e.Package}, {Text: "new " + arrow + " " + api.ShortCommit(e.Commit)}, {Text: e.Branch}, {Text: quoted(r.Subject)}}
	case e.Commit != e.Previous:
		return []Cell{{Text: e.Package}, {Text: api.ShortCommit(e.Previous) + " " + arrow + " " + api.ShortCommit(e.Commit)}, {Text: e.Branch}, {Text: quoted(r.Subject)}}
	}
	var what []string
	if r.OldBranch != "" {
		what = append(what, "branch "+r.OldBranch+" "+arrow+" "+e.Branch)
	}
	if r.OldURL != "" || e.URL != "" {
		if r.OldURL != e.URL {
			what = append(what, "url "+orDash(r.OldURL)+" "+arrow+" "+orDash(e.URL))
		}
	}
	return []Cell{{Text: e.Package}, {Text: api.ShortCommit(e.Commit)}, {Text: e.Branch}, {Text: strings.Join(what, ", ")}}
}

func quoted(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf("%q", s)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func hRebuild(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	fl := cmd.Flags()
	reason, _ := fl.GetString("reason")
	dry, _ := fl.GetBool("dry-run")
	reason = strings.TrimSpace(reason)
	switch {
	case reason == "":
		return nil, usagef("repoctl rebuild needs --reason TEXT: why the packages build again, %d characters or more", workspace.MinReasonLength)
	case len([]rune(reason)) < workspace.MinReasonLength:
		return nil, usagef("--reason %q is too short: want %d characters or more", reason, workspace.MinReasonLength)
	}
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	ids, err := a.packageArgs(set, setDir, args)
	if err != nil {
		return nil, err
	}
	// A named in-tree package stops the rebuild; a glob leaves it out.
	explicit := map[string]bool{}
	sels, _ := plan.ParseSelectors(args)
	for _, s := range sels {
		if !s.Not && !strings.ContainsAny(s.Pkg, "*?[") {
			p := s.Pkg
			if s.Here {
				p = a.Here(set, setDir)
			}
			explicit[p] = true
		}
	}
	var pkgs []string
	for _, id := range ids {
		pk, _ := set.Package(id)
		if pk.InTree() {
			if explicit[id] {
				return nil, workspace.InTreeError(*pk)
			}
			a.Warn(workspace.InTreeWarning(*pk))
			continue
		}
		pkgs = append(pkgs, id)
	}
	if len(pkgs) == 0 {
		return nil, usagef("repoctl rebuild: the selectors select no package with a branch")
	}
	l, err := a.SetLock(ctx, set, false)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	e := a.workspaceEnv(cmd, set, setDir)
	rep, err := workspace.Rebuild(ctx, e, workspace.RebuildOptions{Packages: pkgs, Reason: reason, DryRun: dry})
	if err != nil {
		return nil, err
	}
	for _, w := range rep.Warnings {
		a.Warn(w)
	}
	res := &Result{Kind: api.KindLock, NextShown: true}
	for _, r := range rep.Rows {
		res.Items = append(res.Items, r.Entry)
	}
	if dry {
		quoted := make([]string, len(args))
		for i, s := range args {
			quoted[i] = shellQuote(s)
		}
		cmdline := "repoctl rebuild " + strings.Join(quoted, " ") + " --reason " + shellQuote(reason)
		res.Next = []api.NextStep{{Action: "rebuild", Reason: "make the rebuild commits and pin them", Command: cmdline}}
	} else {
		for _, r := range rep.Rows {
			res.Next = append(res.Next, api.NextStep{Action: "push", Human: true, Reason: "push the rebuild commit of " + r.Package, Command: r.PushCommand()})
		}
		res.Next = append(res.Next,
			api.NextStep{Action: "commit", Human: true, Reason: "commit and push repoctl.lock", Command: "git add repoctl.lock && git commit && git push"},
			api.NextStep{Action: "build", Reason: "build the new releases", Command: "repoctl build"})
	}
	res.Human = func(o *Out) error {
		arrow := o.StdoutStyle.Arrow()
		var rows [][]Cell
		for _, r := range rep.Rows {
			if dry {
				rows = append(rows, []Cell{{Text: r.Package}, {Text: api.ShortCommit(r.Previous)}, {Text: r.OldNVR() + " " + arrow + " " + r.NewNVR()}})
				continue
			}
			rows = append(rows, []Cell{{Text: r.Package}, {Text: api.ShortCommit(r.Previous) + " " + arrow + " " + api.ShortCommit(r.Commit)},
				{Text: r.OldVR + " " + arrow + " " + r.NewVR}})
		}
		writeRows(o, rows)
		for _, w := range a.warnings {
			o.Warning(w)
		}
		if o.Quiet {
			return nil
		}
		if dry {
			o.Note("dry run: no commit is made and repoctl.lock is not written")
			o.Next(res.Next)
			return nil
		}
		var lines []string
		if len(rep.Rows) > 0 {
			first := rep.Rows[0].PushCommand()
			if len(rep.Rows) > 1 {
				var rest []string
				for _, r := range rep.Rows[1:] {
					rest = append(rest, r.Package)
				}
				first += "   (the same for " + strings.Join(rest, ", ") + ")"
			}
			lines = append(lines, first)
		}
		lines = append(lines, "git add repoctl.lock && git commit && git push", "repoctl build")
		var steps []api.NextStep
		for _, l := range lines {
			steps = append(steps, api.NextStep{Command: l})
		}
		o.Next(steps)
		return nil
	}
	res.ShownWarnings = true
	return res, nil
}

// shellQuote quotes a value for a command line to copy.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"$\\*?;&|<>()`") {
		return s
	}
	return "\"" + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(s) + "\""
}
