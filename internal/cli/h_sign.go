package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/release"
	"github.com/dennisklein/repoctl/internal/sign"
	"github.com/dennisklein/repoctl/internal/ui"
)

// The handlers of signing: sign and release (cli.html#group-sign,
// signing.html).

// signEnv is what the sign commands load: the set, the trust set, the
// deployment records, latest of each target and the signer of sign.key.
type signEnv struct {
	set       *config.Set
	dir       string
	eff       *config.Effective
	view      *sign.View
	paths     config.Paths
	signer    *sign.Signer
	signerErr error
	targets   []string
}

func loadSign(ctx context.Context, a *App) (*signEnv, error) {
	set, dir, err := a.Set()
	if err != nil {
		return nil, err
	}
	eff, err := a.Settings()
	if err != nil {
		return nil, err
	}
	targets, err := a.Targets(set, false)
	if err != nil {
		return nil, err
	}
	v, err := sign.NewView(set, dir, a.Paths(), targets)
	if err != nil {
		return nil, err
	}
	v.Now = a.Now()
	e := &signEnv{set: set, dir: dir, eff: eff, view: v, paths: a.Paths(), targets: targets}
	e.signer, e.signerErr = sign.NewSigner(ctx, v.Trust, eff)
	v.Current = e.signer
	return e, nil
}

// gpg gives the backend of the user settings.
func (e *signEnv) gpg() *sign.GPG {
	if e.signer != nil {
		return e.signer.GPG
	}
	return sign.NewGPG(e.eff.String("sign.gpg.homedir"))
}

// committer gives the signer of a set commit: the key of sign.key with its
// secret key on this host, else sign.no-key (exit 9).
func (e *signEnv) committer(ctx context.Context) (*sign.Signer, error) {
	if e.signer == nil {
		if e.signerErr != nil {
			if ae, ok := api.AsError(e.signerErr); ok && ae.Code != api.PSignNoKey {
				return nil, api.Errorf(api.PSignNoKey, "a set commit needs the key of sign.key: %s", ae.Message).
					WithHint("set sign.key to your id of sign.keys", "repoctl config show")
			}
			return nil, e.signerErr
		}
		return nil, api.Errorf(api.PSignNoKey, "a set commit needs the key of sign.key").WithHint("set sign.key", "repoctl config show")
	}
	if err := e.signer.RequireSecret(ctx); err != nil {
		return nil, err
	}
	return e.signer, nil
}

// pushStep is the Next: step after a set commit.
func (e *signEnv) pushStep() api.NextStep {
	return api.NextStep{Action: "push", Human: true, Reason: "push the set commit",
		Command: fmt.Sprintf("git push %s set/%s", e.set.Remote(), e.set.Name)}
}

// keyDisplay gives "a.admin C2F9…52A7".
func keyDisplay(k *sign.TrustedKey) string {
	return k.ID + " " + sign.AbbrevFingerprint(k.Fingerprint)
}

// subjectDisplay gives a commit subject with the fingerprint shortened for
// the terminal.
func subjectDisplay(subject string) string {
	f := strings.Fields(subject)
	for i, w := range f {
		if sign.IsFingerprint(w) {
			f[i] = sign.AbbrevFingerprint(w)
		}
	}
	return strings.Join(f, " ")
}

// isTerminal reports a terminal for a question: stdin and stderr are
// terminals.
func (a *App) isTerminal() bool {
	return cliprogress.IsTerminal(a.Stdin) && cliprogress.IsTerminal(a.Stderr)
}

// confirmKeyID asks the human to type the key id (shared-state rules). No
// terminal, another text, an empty line or EOF give exit 6.
func (a *App) confirmKeyID(question, id string) error {
	if !a.isTerminal() {
		return api.Errorf(api.PPublishNeedsConfirmation, "repoctl %s needs the typed key id %s, and there is no terminal", a.cmdName(), id).
			WithHint("run it in a terminal", "repoctl "+a.cmdName()+" "+id)
	}
	fmt.Fprint(a.Stderr, a.style.Bold(question)+" ")
	line, err := bufio.NewReader(a.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Fprintln(a.Stderr)
	}
	if strings.TrimSpace(line) != id {
		return api.Errorf(api.PPublishNeedsConfirmation, "the answer is not the key id %s; nothing changed", id)
	}
	return nil
}

// signerResult gives the SignerList result of one key after a change.
func signerResult(ctx context.Context, e *signEnv, id string, next []api.NextStep) []any {
	l := api.SignerList{Keys: []any{}, Next: next}
	if e.signer != nil {
		l.Current = e.signer.KeyID()
	}
	tr := sign.LoadTrust(e.set, e.dir)
	e.view.Trust = tr
	if d, err := sign.ReadDeployments(e.dir); err == nil {
		e.view.Deployments = d
	}
	if k := tr.Key(id); k != nil {
		l.Keys = append(l.Keys, e.view.Item(ctx, k))
	}
	return []any{l}
}

// reloadSet reads repoctl.yaml again after an edit.
func (e *signEnv) reloadSet() error {
	s, err := config.LoadSet(e.dir)
	if err != nil {
		return err
	}
	e.set = s
	e.view.Set = s
	e.view.Trust = sign.LoadTrust(s, e.dir)
	return nil
}

// releaseFiles runs release write after a key change. A set without a
// release package gets a warning and no files.
func releaseFiles(ctx context.Context, a *App, e *signEnv, msg string) (*release.Result, error) {
	if _, ok := e.set.ReleasePackage(); !ok {
		a.Warn(api.Warning(api.PConfigInvalid, "the set has no package with role: release: no key file written"))
		return &release.Result{}, nil
	}
	return release.Write(ctx, e.set, e.dir, e.view.Trust, release.Options{Message: msg, Now: a.Now()})
}

// signPaths are the set paths that a key change writes.
func signPaths(set *config.Set, extra ...string) []string {
	paths := append([]string{config.SetFile}, extra...)
	if p, ok := set.ReleasePackage(); ok {
		paths = append(paths, p.Path)
	}
	return paths
}

func hSignLs(ctx context.Context, a *App, _ *cobra.Command, _ []string) (*Result, error) {
	e, err := loadSign(ctx, a)
	if err != nil {
		return nil, err
	}
	for _, k := range e.view.Trust.Keys {
		if k.Err != nil {
			a.Warn(api.Warning(api.PConfigInvalid, "sign.keys %s: %v", k.ID, k.Err))
		}
	}
	list := e.view.SignerList(ctx)
	res := &Result{Kind: api.KindSignerList, Items: []any{list}, Targets: e.targets}
	res.Human = func(o *Out) error { return signTable(o, e, list) }
	return res, nil
}

// signTable writes the table of sign ls. With one target in scope it has
// one row per key; with more, one row per key and target.
func signTable(o *Out, e *signEnv, l api.SignerList) error {
	multi := len(e.targets) > 1
	heads := []string{"ID", "FINGERPRINT", "STATE", "DEPLOYED (release, by, date)", "EXPIRES", "RPMS"}
	if multi {
		heads = []string{"ID", "FINGERPRINT", "STATE", "TARGET", "DEPLOYED (release, by, date)", "EXPIRES", "RPMS"}
	}
	t := NewTable(heads...)
	t.Right[len(heads)-1] = true
	for _, x := range l.Keys {
		it := x.(sign.SignerItem)
		id := it.KeyID
		expires := "never"
		if it.Expires != "" {
			expires = api.MustTime(it.Expires).Format("2006-01-02")
		}
		state := it.State
		color := func(s string) string { return s }
		if state == config.KeyRetiring {
			color = o.StdoutStyle.Amber
		}
		if state == config.KeyRevoked {
			color = o.StdoutStyle.Red
		}
		for i, tg := range e.targets {
			cells := []Cell{{Text: id}, {Text: api.ShortFingerprint(it.Fingerprint)}, C(state, color)}
			if i > 0 {
				cells = []Cell{{}, {}, {}}
			}
			if multi {
				cells = append(cells, Cell{Text: tg})
			}
			exp := expires
			if i > 0 {
				exp = ""
			}
			cells = append(cells, Cell{Text: deployedCell(it, tg)}, Cell{Text: exp}, Cell{Text: fmt.Sprint(it.RPMs[tg])})
			t.RowCells(cells...)
		}
		if len(e.targets) == 0 {
			t.RowCells(Cell{Text: id}, Cell{Text: api.ShortFingerprint(it.Fingerprint)}, C(state, color), Cell{Text: "-"}, Cell{Text: expires}, Cell{Text: "0"})
		}
	}
	if len(l.Keys) == 0 {
		o.Note("sign.keys is empty; add a key with repoctl sign add ID --fingerprint FPR")
		return nil
	}
	return o.Table(t)
}

// deployedCell gives the DEPLOYED cell of a key and a target.
func deployedCell(it sign.SignerItem, target string) string {
	if it.State == config.KeyRemoved || it.State == config.KeyRevoked {
		if r, ok := it.Removals[target]; ok {
			return fmt.Sprintf("clients: removed, %s, %s", r.ConfirmedBy, api.MustTime(r.At).Format("2006-01-02"))
		}
		return "clients: unconfirmed"
	}
	d, ok := it.Deployments[target]
	if !ok {
		return "no record"
	}
	what := sign.StripDist(d.Release)
	if d.Release == "" {
		sum := d.Keyfile
		if len(sum) > 8 {
			sum = sum[:8] + "…"
		}
		what = "key file " + sum
	}
	return fmt.Sprintf("%s, %s, %s", what, d.ConfirmedBy, api.MustTime(d.At).Format("2006-01-02"))
}

func hSignOnboard(ctx context.Context, a *App, _ *cobra.Command, args []string) (*Result, error) {
	e, err := loadSign(ctx, a)
	if err != nil {
		return nil, err
	}
	r := e.view.Onboard(ctx, args[0])
	res := &Result{Kind: api.KindOnboardReport, Items: []any{r}, Targets: e.targets}
	res.Human = func(o *Out) error {
		writeSteps(o, r)
		if r.Next != "" {
			o.Next([]api.NextStep{{Command: r.Next}})
		}
		return nil
	}
	res.NextShown = true
	return res, nil
}

// writeSteps writes the steps of an OnboardReport: n, mark, name, detail.
func writeSteps(o *Out, r api.OnboardReport) {
	width := 0
	for _, s := range r.Steps {
		width = max(width, len(s.Name))
	}
	for _, s := range r.Steps {
		mark := o.StdoutStyle.Mark("ok")
		if !s.Ok {
			mark = o.StdoutStyle.Mark("failed")
		}
		o.Printf("%d %s %s%s  %s\n", s.N, mark, s.Name, strings.Repeat(" ", width-len(s.Name)), o.StdoutStyle.Dim(s.Detail))
	}
}

func hSignOffboard(ctx context.Context, a *App, _ *cobra.Command, args []string) (*Result, error) {
	e, err := loadSign(ctx, a)
	if err != nil {
		return nil, err
	}
	r, err := e.view.Offboard(ctx, args[0])
	if err != nil {
		return nil, err
	}
	k := e.view.Trust.Key(args[0])
	res := &Result{Kind: api.KindOnboardReport, Items: []any{r}, Targets: e.targets, NextShown: true}
	res.Human = func(o *Out) error {
		if k.State == config.KeyRevoked {
			o.Printf("%s  %s  %s\n", o.StdoutStyle.Red("revoked"), k.ID, sign.GroupFingerprint(k.Fingerprint))
		}
		writeSteps(o, r)
		o.Printf("\n")
		if k.State == config.KeyRevoked {
			o.Printf("Run now, on the HTTP server first, then on every client:\n")
		} else {
			o.Printf("Run on every client after \"dnf upgrade %s\":\n", releaseName(e.set))
		}
		for _, c := range r.ClientCommands {
			line := fmt.Sprintf("  %-7s %s", c.OS+":", c.Command)
			if c.Note != "" && c.OS != "all" {
				line = fmt.Sprintf("%-40s # %s", line, c.Note)
			}
			o.Printf("%s\n", line)
		}
		if len(r.SignedSnapshots) > 0 {
			o.Printf("\nSnapshots in the registry with files of %s:\n", k.ID)
			for _, s := range r.SignedSnapshots {
				who := s.User
				if s.Host != "" {
					who += "@" + s.Host
				}
				what := chkPlural(s.RPMs, "RPM")
				if s.Repomd {
					what += ", repomd.xml.asc"
				}
				latest := ""
				if s.Latest {
					latest = "latest"
				}
				o.Printf("  %-5s %s   %s   %s   %s\n", s.Target, s.Publish, who, what, latest)
			}
		}
		if r.Next != "" {
			o.Next([]api.NextStep{{Command: r.Next}})
		}
		return nil
	}
	return res, nil
}

func releaseName(set *config.Set) string {
	if p, ok := set.ReleasePackage(); ok {
		return p.ID
	}
	return "the release package"
}

func hSignAdd(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	id := args[0]
	fpr, _ := cmd.Flags().GetString("fingerprint")
	file, _ := cmd.Flags().GetString("file")
	fromGPG, _ := cmd.Flags().GetBool("from-gpg")
	if fpr == "" {
		return nil, usagef("repoctl sign add needs --fingerprint FPR, the full fingerprint of the key")
	}
	fpr = sign.NormalizeFingerprint(fpr)
	if !sign.IsFingerprint(fpr) {
		return nil, usagef("--fingerprint %s: want the full fingerprint, 40 hex digits", fpr)
	}
	if file != "" && fromGPG {
		return nil, usagef("give one of --from-gpg and --file")
	}
	if !keyIDRE.MatchString(id) {
		return nil, usagef("key id %q: want lowercase letters, digits, ., _ and -", id)
	}
	e, err := loadSign(ctx, a)
	if err != nil {
		return nil, err
	}
	for _, k := range e.set.Sign.Keys {
		if k.ID == id || sign.NormalizeFingerprint(k.Fingerprint) == fpr {
			return nil, api.Errorf(api.PSignKeyExists, "sign.keys has the key %s (%s, state %s)", k.ID, sign.AbbrevFingerprint(k.Fingerprint), k.State).
				WithHint("use a new id for a new key", "repoctl sign ls")
		}
	}
	// The public key.
	var data []byte
	if file != "" {
		data, err = os.ReadFile(file)
		if err != nil {
			return nil, api.Errorf(api.PConfigInvalid, "--file %s: %v", file, err).WithPath(file)
		}
	} else {
		data, err = e.gpg().Export(ctx, fpr)
		if err != nil {
			return nil, err
		}
	}
	certs, err := sign.ParseCerts(data)
	if err != nil {
		return nil, api.Errorf(api.PSignKeyFormat, "the key of %s: %v", id, err)
	}
	var cert *sign.Cert
	for _, c := range certs {
		if c.Fingerprint == fpr {
			cert = c
		}
	}
	if cert == nil {
		return nil, api.Errorf(api.PSignKeyFormat, "the key has the fingerprint %s, not %s", certs[0].Fingerprint, fpr).
			WithHint("check the full fingerprint", "gpg --list-keys --with-fingerprint")
	}
	if r := cert.CheckFormat(); r != "" {
		return nil, api.Errorf(api.PSignKeyFormat, "gate 3: the key %s is %s", id, r).
			WithHint("make a new key: RSA 4096 or Ed25519", "repoctl help key")
	}
	keyPath := sign.KeyPath(id)
	paths := signPaths(e.set, keyPath)
	l, err := a.SetLock(ctx, e.set, false)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	if err := sign.CheckClean(ctx, e.dir, paths...); err != nil {
		return nil, err
	}
	// The commit signer: sign.key after the change, so that a new admin
	// signs the commit with the new key (security.html#set-inputs).
	newKey := config.Key{ID: id, File: keyPath, Fingerprint: fpr, State: config.KeyActive}
	after := *e.set
	after.Sign.Keys = append(append([]config.Key{}, e.set.Sign.Keys...), newKey)
	committer, err := sign.NewSigner(ctx, sign.LoadTrust(&after, e.dir), e.eff)
	if err == nil && committer.Key == nil && committer.FPR == fpr {
		committer.ID = id
	}
	if err != nil {
		return nil, api.Errorf(api.PSignNoKey, "a set commit needs the key of sign.key: %v", err).
			WithHint("set sign.key to your id", "repoctl config show")
	}
	if err := committer.RequireSecret(ctx); err != nil {
		return nil, err
	}
	// Write keys/ID.asc, repoctl.yaml and the release files.
	if err := writeFileAtomic(filepath.Join(e.dir, keyPath), sign.Armor(cert.Raw, "PUBLIC KEY BLOCK")); err != nil {
		return nil, err
	}
	yml, err := os.ReadFile(filepath.Join(e.dir, config.SetFile))
	if err != nil {
		return nil, err
	}
	keyfile := ""
	if e.set.Sign.Release.Keyfile == "" {
		keyfile = sign.KeyfileName(e.set)
	}
	out, err := sign.AddKeyEntry(yml, newKey, keyfile)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(e.dir, config.SetFile), out); err != nil {
		return nil, err
	}
	if err := e.reloadSet(); err != nil {
		return nil, err
	}
	msg := sign.CommitMessage("add", id, fpr)
	rel, err := releaseFiles(ctx, a, e, msg)
	if err != nil {
		return nil, err
	}
	commit, err := sign.SetCommit(ctx, e.dir, committer, paths, msg)
	if err != nil {
		return nil, err
	}
	next := []api.NextStep{e.pushStep()}
	res := &Result{Kind: api.KindSignerList, Items: signerResult(ctx, e, id, next), Next: next, Targets: e.targets}
	res.Human = func(o *Out) error {
		width := len(keyPath)
		for _, f := range rel.Files {
			width = max(width, len(f.Path))
		}
		exp := "no expiry"
		if !cert.Expires.IsZero() {
			exp = "expires " + cert.Expires.Format("2006-01-02")
		}
		o.Printf("wrote %-*s  %s · %s\n", width, keyPath, cert.UID, exp)
		writeReleaseLines(o, rel, width)
		o.Printf("committed %s %q, signed by %s\n", api.ShortCommit(commit), subjectDisplay(msg), committer.KeyID())
		return nil
	}
	return res, nil
}

// keyIDRE is the form of a key id of sign.keys.
var keyIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// sha256RE is the form of the sum of sign deployed --keyfile.
var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeReleaseLines writes one "wrote" line per file that release write
// changed.
func writeReleaseLines(o *Out, rel *release.Result, width int) {
	for _, f := range rel.Files {
		if f.Action != "created" && f.Action != "updated" {
			continue
		}
		detail := ""
		switch f.Kind {
		case "keyfile":
			detail = chkPlural(len(rel.Keys), "key") + ": " + strings.Join(rel.Keys, ", ")
		case "other":
			if rel.Release != "" {
				detail = "Release " + rel.Release
			}
		}
		o.Printf("%s\n", strings.TrimRight(fmt.Sprintf("wrote %-*s  %s", width, f.Path, detail), " "))
	}
}

func hSignLifecycle(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	verb := cmd.Name()
	id := args[0]
	e, err := loadSign(ctx, a)
	if err != nil {
		return nil, err
	}
	k := e.view.Trust.Key(id)
	if k == nil {
		return nil, api.Errorf(api.PSignWrongState, "sign %s %s: sign.keys has no key %s", verb, id, id).
			WithHint("list the keys", "repoctl sign ls")
	}
	if err := sign.CheckTransition(verb, k); err != nil {
		return nil, err
	}
	paths := signPaths(e.set)
	l, err := a.SetLock(ctx, e.set, false)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	if err := sign.CheckClean(ctx, e.dir, paths...); err != nil {
		return nil, err
	}
	// The refusals of the state change.
	switch verb {
	case sign.CmdRetire:
		if t := lastSigner(ctx, e, k); t != "" {
			return nil, api.Errorf(api.PSignLastSigner, "sign retire %s: no other active key has a deployment record for %s, which has a latest", id, t).
				WithTarget(t).WithHint("deploy a second key first", "repoctl sign onboard NEW-ID")
		}
	case sign.CmdRemove:
		for _, t := range activeLatest(e) {
			if n := e.view.RPMs(k, t); n > 0 {
				return nil, api.Errorf(api.PSignKeyInUse, "sign remove %s: %s in latest of %s %s the key", id, chkPlural(n, "RPM"), t, sign.Agree(n, "carries", "carry")).
					WithTarget(t).WithHint("sign them again with an active key", fmt.Sprintf("repoctl repo resign --signer %s --target %s", id, t))
			}
		}
	}
	// The commit signer. revoke commits also without the secret key.
	committer, cerr := e.committer(ctx)
	if cerr != nil && verb != sign.CmdRevoke {
		return nil, cerr
	}
	if verb == sign.CmdActivate || verb == sign.CmdRevoke {
		q := fmt.Sprintf("Type the key id to %s %s:", verb, keyDisplay(k))
		if err := a.confirmKeyID(q, id); err != nil {
			return nil, err
		}
	}
	yml, err := os.ReadFile(filepath.Join(e.dir, config.SetFile))
	if err != nil {
		return nil, err
	}
	out, err := sign.SetKeyState(yml, id, sign.NewState(verb))
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(e.dir, config.SetFile), out); err != nil {
		return nil, err
	}
	if err := e.reloadSet(); err != nil {
		return nil, err
	}
	msg := sign.CommitMessage(verb, id, k.Fingerprint)
	rel, err := releaseFiles(ctx, a, e, msg)
	if err != nil {
		return nil, err
	}
	var commit, signedBy string
	if committer != nil {
		commit, err = sign.SetCommit(ctx, e.dir, committer, paths, msg)
		signedBy = committer.KeyID()
	} else {
		a.Warn(api.Warning(api.PSignNoKey, "the commit is not signed: %v", cerr))
		commit, err = sign.SetCommit(ctx, e.dir, nil, paths, msg)
	}
	if err != nil {
		return nil, err
	}
	next := []api.NextStep{e.pushStep()}
	switch verb {
	case sign.CmdRetire:
		next = append(next, api.NextStep{Action: "other", Human: true, Reason: "re-sign the RPMs of the retiring key", Command: "repoctl repo resign --signer " + id})
	case sign.CmdRemove:
		next = append(next, api.NextStep{Action: "build", Human: true, Reason: "build and publish the release package without the key", Command: "repoctl build " + releaseName(e.set)})
	case sign.CmdRevoke:
		if t := noDeployedSigner(ctx, e, id); t != "" {
			a.Warn(api.Warning(api.PSignLastSigner, "no other key is deployed for %s: add a new key and deploy it with a key file record (repoctl help emergency)", t))
			next = append(next, api.NextStep{Action: "other", Human: true, Reason: "recover without a deployed signer", Command: "repoctl sign add NEW-ID --fingerprint FPR"})
		} else {
			next = append(next, api.NextStep{Action: "other", Human: true, Reason: "end the trust on the clients", Command: "repoctl sign offboard " + id})
		}
	}
	res := &Result{Kind: api.KindSignerList, Items: signerResult(ctx, e, id, next), Next: next, Targets: e.targets}
	res.Human = func(o *Out) error {
		width := len(config.SetFile)
		for _, f := range rel.Files {
			width = max(width, len(f.Path))
		}
		o.Printf("wrote %-*s  %s %s\n", width, config.SetFile, keyDisplay(k), sign.NewState(verb))
		writeReleaseLines(o, rel, width)
		if signedBy != "" {
			o.Printf("committed %s %q, signed by %s\n", api.ShortCommit(commit), subjectDisplay(msg), signedBy)
		} else {
			o.Printf("committed %s %q, not signed\n", api.ShortCommit(commit), subjectDisplay(msg))
		}
		return nil
	}
	return res, nil
}

// otherDeployed reports whether an active key other than id has a passing
// deployment record for the target.
func otherDeployed(ctx context.Context, e *signEnv, id, target string) bool {
	for _, o := range e.view.Trust.InState(config.KeyActive) {
		if o.ID == id {
			continue
		}
		if ok, _ := e.view.RecordPasses(ctx, o, target); ok {
			return true
		}
	}
	return false
}

// activeLatest gives the active targets of the set with a latest. A key
// change applies to the whole set, whatever --target says.
func activeLatest(e *signEnv) []string {
	var out []string
	for _, t := range e.set.ActiveTargets() {
		tree := e.view.Trees[t.ID]
		if tree == nil {
			tree = sign.LocalTree(e.paths, e.set, t.ID)
			e.view.Trees[t.ID] = tree
		}
		if tree.Exists() {
			out = append(out, t.ID)
		}
	}
	return out
}

// lastSigner gives a target with a latest where k is the last deployed
// signer: k has a passing record and no other active key has one; or "".
func lastSigner(ctx context.Context, e *signEnv, k *sign.TrustedKey) string {
	for _, t := range activeLatest(e) {
		if ok, _ := e.view.RecordPasses(ctx, k, t); ok && !otherDeployed(ctx, e, k.ID, t) {
			return t
		}
	}
	return ""
}

// noDeployedSigner gives a target with a latest without another active and
// deployed key than id, or "".
func noDeployedSigner(ctx context.Context, e *signEnv, id string) string {
	for _, t := range activeLatest(e) {
		if !otherDeployed(ctx, e, id, t) {
			return t
		}
	}
	return ""
}

func hSignDeployed(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	id := args[0]
	fl := cmd.Flags()
	rel, _ := fl.GetString("release")
	sum, _ := fl.GetString("keyfile")
	path, _ := fl.GetString("path")
	withdraw, _ := fl.GetBool("withdraw")
	note, _ := fl.GetString("note")
	dry, _ := fl.GetBool("dry-run")
	if len(a.TargetArgs()) == 0 {
		return nil, usagef("repoctl sign deployed needs --target: the record is per target")
	}
	n := 0
	for _, b := range []bool{rel != "", sum != "", withdraw} {
		if b {
			n++
		}
	}
	if n != 1 {
		return nil, usagef("give exactly one of --release NVR, --keyfile SHA256 and --withdraw")
	}
	if path != "" && rel == "" {
		return nil, usagef("--path needs --release")
	}
	sum = strings.ToLower(strings.TrimSpace(sum))
	if sum != "" && !sha256RE.MatchString(sum) {
		return nil, usagef("--keyfile %s: want the sha256 of repoctl sign keyfile --checksum, 64 hex digits", sum)
	}
	// A child of the web UI reads its UI confirmation once, at the start.
	fd, _ := fl.GetInt("confirm-fd")
	var confirmation []byte
	if fd >= 0 {
		confirmation = readConfirmFD(fd)
	}
	e, err := loadSign(ctx, a)
	if err != nil {
		return nil, err
	}
	k := e.view.Trust.Key(id)
	if k == nil {
		return nil, api.Errorf(api.PSignNotActive, "sign deployed %s: sign.keys has no key %s", id, id).
			WithHint("add the key first", "repoctl sign add "+id+" --fingerprint FPR")
	}
	if !withdraw && k.State != config.KeyActive {
		return nil, api.Errorf(api.PSignNotActive, "sign deployed %s: the key is %s; only an active key gets a deployment record", id, k.State).
			WithHint("check the state of the key", "repoctl sign ls")
	}
	l, err := a.SetLock(ctx, e.set, false)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	// The confirmer: the key of sign.key, active, not the key of the
	// record while another key is active.
	if e.signer == nil {
		return nil, e.signerErr
	}
	ck := e.signer.Key
	if ck == nil || ck.State != config.KeyActive {
		return nil, api.Errorf(api.PSignNotActive, "sign deployed: the key %s of sign.key is not active; an active key confirms a deployment", e.signer.KeyID()).
			WithHint("an admin with an active key confirms it", "repoctl sign ls")
	}
	if !withdraw && ck.ID == id {
		for _, o := range e.view.Trust.InState(config.KeyActive) {
			if o.ID != id {
				return nil, api.Errorf(api.PSignSelfConfirm, "sign deployed %s: the key of sign.key is the key of the record, and %s is active", id, o.ID).
					WithHint("an admin with another active key confirms the deployment", "")
			}
		}
	}
	committer, err := e.committer(ctx)
	if err != nil {
		return nil, err
	}
	if err := sign.CheckClean(ctx, e.dir, sign.DeployedFile); err != nil {
		return nil, err
	}
	// The checks of each target.
	targets := e.targets
	now := a.Now()
	var summary []string
	var entries []sign.Deployment
	pins := map[string]string{}
	var keyfileData []byte
	var keyfileCommit string
	if sum != "" {
		p, ok := e.set.ReleasePackage()
		if !ok {
			return nil, api.Errorf(api.PConfigInvalid, "the set has no release package with a key file")
		}
		kfPath := filepath.Join(p.Path, sign.KeyfileName(e.set))
		keyfileCommit = sign.LastCommit(ctx, e.dir, kfPath)
		if keyfileCommit == "" {
			return nil, api.Errorf(api.PSignNotDeployed, "%s is not committed", kfPath).WithHint("write and commit the key file", "repoctl release write")
		}
		keyfileData, err = sign.KeyfileAtCommit(e.dir, e.set)(ctx, keyfileCommit)
		if err != nil {
			return nil, err
		}
		if got := sign.Sha256Hex(keyfileData); got != sum {
			return nil, api.Errorf(api.PSignNotDeployed, "--keyfile %s… does not match the committed key file %s (%s…)", sum[:8], kfPath, got[:8]).
				WithHint("give the sum of the committed key file", "repoctl sign keyfile --tree --checksum")
		}
		if !sign.KeyfileHolds(keyfileData, k.Fingerprint) {
			return nil, api.Errorf(api.PSignNotDeployed, "the key file %s… does not hold %s", sum[:8], keyDisplay(k))
		}
		summary = append(summary, fmt.Sprintf("keyfile  %s %s… of set commit %s · holds %s", sign.KeyfileName(e.set), sum[:8], api.ShortCommit(keyfileCommit), keyDisplay(k)))
	}
	for _, t := range targets {
		tree := e.view.Trees[t]
		if path != "" {
			tree = sign.NewTree(path, e.set, t)
		}
		switch {
		case withdraw:
			d := e.view.Deployments.Find(id, t)
			if d == nil {
				return nil, api.Errorf(api.PSignNotDeployed, "sign deployed %s --withdraw: %s has no record of %s for %s", id, sign.DeployedFile, id, t).WithTarget(t)
			}
			what := d.Release
			if what == "" {
				what = "key file " + d.Keyfile[:8] + "…"
			}
			summary = append(summary, fmt.Sprintf("withdraw %s on %s: %s, confirmed by %s on %s", id, t, what, d.ConfirmedBy, d.Time().Format("2006-01-02")))
			if tree.Exists() && e.view.RPMs(k, t) > 0 {
				a.Warn(api.Warning(api.PSignNotDeployed, "latest of %s holds %s of %s; re-sign them with another deployed key: repoctl repo resign --signer %s --target %s", t, chkPlural(e.view.RPMs(k, t), "RPM"), id, id, t))
			}
		case !tree.Exists():
			return nil, api.Errorf(api.PSignNotDeployed, "sign deployed %s: %s has no latest", id, t).WithTarget(t).
				WithHint("a target without a latest has no clients; its first publish needs no record", "repoctl sign ls")
		case rel != "":
			nvr, ok := tree.HasRelease(rel)
			if !ok {
				return nil, api.Errorf(api.PSignNotDeployed, "the release %s is not in latest of %s (latest has %s)", rel, t, orNone(tree.NewestRelease())).WithTarget(t).
					WithHint("publish the release package first", "repoctl sign onboard "+id)
			}
			data, err := tree.ReleaseKeyfile(ctx, nvr)
			if err != nil {
				return nil, api.Errorf(api.PSignNotDeployed, "the key file of %s: %v", nvr, err).WithTarget(t)
			}
			if !sign.KeyfileHolds(data, k.Fingerprint) {
				return nil, api.Errorf(api.PSignNotDeployed, "the key file of %s does not hold %s", nvr, keyDisplay(k)).WithTarget(t).
					WithHint("build and publish a release package with the key", "repoctl sign onboard "+id)
			}
			summary = append(summary, fmt.Sprintf("release  %s in latest of %s · key file holds %s", nvr, t, keyDisplay(k)))
			entries = append(entries, sign.Deployment{Key: id, Target: t, Fingerprint: k.Fingerprint, Release: nvr, ConfirmedBy: ck.ID, At: sign.FormatAt(now), Note: note})
			if root, err := tree.RootKeyfile(); err == nil {
				pins[t] = sign.Sha256Hex(root)
			}
		default:
			entries = append(entries, sign.Deployment{Key: id, Target: t, Fingerprint: k.Fingerprint, Keyfile: sum, Commit: keyfileCommit, ConfirmedBy: ck.ID, At: sign.FormatAt(now), Note: note})
		}
	}
	msg := deployedMessage(id, targets, rel, sum, withdraw)
	res := &Result{Kind: api.KindSignerList, Targets: targets}
	printSummary := func(o *Out) {
		for _, s := range summary {
			o.Printf("%s\n", s)
		}
	}
	if dry {
		for _, en := range entries {
			summary = append(summary, fmt.Sprintf("entry    %s %s %s, confirmed by %s", en.Key, en.Target, orNone(en.Release+en.Keyfile), en.ConfirmedBy))
		}
		res.Items = signerResult(ctx, e, id, nil)
		res.Human = func(o *Out) error {
			printSummary(o)
			o.Note("dry run: nothing written")
			return nil
		}
		return res, nil
	}
	// The summary goes to stdout before the question.
	if !a.jsonOut {
		printSummary(a.out())
	}
	q := "Type the key id to confirm that all clients have this release:"
	switch {
	case withdraw:
		q = "Type the key id to withdraw its deployment record:"
	case sum != "":
		q = "Type the key id to confirm that all clients have this key file:"
	}
	if fd >= 0 {
		err = checkUIConfirmation(confirmation, a.paths.UIJSON(), os.Getppid(), "sign deployed", e.set.Name, targets, id)
	} else {
		err = a.confirmKeyID(q, id)
	}
	if err != nil {
		return nil, err
	}
	d := e.view.Deployments
	for _, en := range entries {
		d.Put(en)
	}
	if withdraw {
		for _, t := range targets {
			d.Withdraw(id, t)
		}
	}
	if err := d.Write(e.dir); err != nil {
		return nil, err
	}
	if _, err := sign.SetCommit(ctx, e.dir, committer, []string{sign.DeployedFile}, msg); err != nil {
		return nil, err
	}
	a.signJournal(ctx, "sign deployed", msg, targets)
	next := []api.NextStep{e.pushStep()}
	res.Items = signerResult(ctx, e, id, next)
	res.Next = next
	res.Human = func(o *Out) error {
		o.Printf("committed %s %q, signed by %s\n", sign.DeployedFile, msg, committer.KeyID())
		for _, t := range sortedKeys(pins) {
			p := pins[t]
			path := e.set.Repo.Name
			if tg, ok := e.set.Target(t); ok {
				path = tg.ClientPath()
			}
			o.Printf("pin      %s/%s %s…  (client bootstrap: update the pin in config management)\n", path, sign.KeyfileName(e.set), p[:8])
		}
		return nil
	}
	return res, nil
}

// signJournal records a shared-state change of the deployment record in
// the journal of the commands without a run: the go-clikit event log
// activity.jsonl of the state directory, which rotates at 8 MiB
// (contract.html#journal). The command span carries the commit subject,
// and one target span names each target. It draws no display; a log that
// cannot be written fails nothing.
func (a *App) signJournal(ctx context.Context, command, subject string, targets []string) {
	o := a.ProgressOptions("repoctl")
	o.Mode = cliprogress.Setting{Flag: "--progress", Value: "none", Given: true}
	o.DefaultLog = a.paths.Activity()
	o.RotateAt = 8 << 20
	if err := os.MkdirAll(filepath.Dir(o.DefaultLog), 0o700); err != nil {
		return
	}
	s, err := cliprogress.Start(ctx, o)
	if err != nil {
		return
	}
	pctx, span := progress.Start(s.Context(), progress.KindCommand, command, progress.Message(subject))
	for _, t := range targets {
		_, ts := progress.Start(pctx, progress.KindTarget, t, progress.Node(t), progress.Role("target"))
		ts.End(nil)
	}
	span.End(nil)
	s.Finish(false)
}

// readConfirmFD reads the UI confirmation from the file descriptor of
// --confirm-fd once and closes it (cli.html#ui-confirmation). A descriptor
// that cannot be read gives nothing, which checkUIConfirmation refuses.
func readConfirmFD(fd int) []byte {
	f := os.NewFile(uintptr(fd), "confirm-fd")
	if f == nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil
	}
	return b
}

// checkUIConfirmation checks the UI confirmation of a child of the web UI
// (cli.html#ui-confirmation). The parent process must be the server that
// ui.json names, and the object must name the command, the set, the typed
// key id and, when it gives them, the targets. Else
// publish.needs-confirmation (exit 6).
func checkUIConfirmation(body []byte, uiJSON string, ppid int, command, set string, targets []string, id string) error {
	refuse := func(format string, args ...any) error {
		return api.Errorf(api.PPublishNeedsConfirmation, "repoctl %s %s --confirm-fd: %s; nothing changed", command, id, fmt.Sprintf(format, args...)).
			WithHint("confirm it again in the web UI, or run the command in a terminal", "")
	}
	rec, held := ui.ReadRecord(uiJSON)
	if !held || rec.PID != ppid {
		return refuse("the parent process is not the web UI server of %s", uiJSON)
	}
	var c ui.Confirmation
	if len(body) == 0 || json.Unmarshal(body, &c) != nil || c.V != 1 {
		return refuse("no valid UI confirmation")
	}
	switch {
	case c.Command != command:
		return refuse("the UI confirmation is for %q", c.Command)
	case c.Set != set:
		return refuse("the UI confirmation names the set %q, not %s", c.Set, set)
	case len(c.Keys) != 1 || c.Keys[0] != id:
		return refuse("the typed key ids [%s] are not %s", strings.Join(c.Keys, " "), id)
	case len(c.Targets) > 0 && !sameSet(c.Targets, targets):
		return refuse("the UI confirmation names the targets %s, not %s", strings.Join(c.Targets, ", "), strings.Join(targets, ", "))
	}
	return nil
}

// deployedMessage gives the set commit of sign deployed.
func deployedMessage(id string, targets []string, rel, sum string, withdraw bool) string {
	on := strings.Join(targets, ", ")
	switch {
	case withdraw:
		return fmt.Sprintf("Withdraw deployment of %s on %s", id, on)
	case sum != "":
		return fmt.Sprintf("Deployed %s on %s with key file %s", id, on, sum[:12])
	}
	return fmt.Sprintf("Deployed %s on %s with %s", id, on, sign.StripDist(rel))
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hSignRemoved(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	id := args[0]
	note, _ := cmd.Flags().GetString("note")
	dry, _ := cmd.Flags().GetBool("dry-run")
	if len(a.TargetArgs()) == 0 {
		return nil, usagef("repoctl sign removed needs --target: the record is per target")
	}
	e, err := loadSign(ctx, a)
	if err != nil {
		return nil, err
	}
	k := e.view.Trust.Key(id)
	if k == nil {
		return nil, api.Errorf(api.PSignNotRemoved, "sign removed %s: sign.keys has no key %s", id, id)
	}
	if k.State != config.KeyRemoved && k.State != config.KeyRevoked {
		return nil, api.Errorf(api.PSignNotRemoved, "sign removed %s: the key is %s, not removed", id, k.State).
			WithHint("see the offboarding steps", "repoctl sign offboard "+id)
	}
	l, err := a.SetLock(ctx, e.set, false)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	if e.signer == nil {
		return nil, e.signerErr
	}
	ck := e.signer.Key
	if ck == nil || ck.State != config.KeyActive {
		return nil, api.Errorf(api.PSignNotActive, "sign removed: the key %s of sign.key is not active", e.signer.KeyID())
	}
	committer, err := e.committer(ctx)
	if err != nil {
		return nil, err
	}
	if err := sign.CheckClean(ctx, e.dir, sign.DeployedFile); err != nil {
		return nil, err
	}
	var summary []string
	var entries []sign.Removal
	for _, t := range e.targets {
		tree := e.view.Trees[t]
		if tree.Exists() {
			if data, err := tree.RootKeyfile(); err == nil && sign.KeyfileHolds(data, k.Fingerprint) {
				return nil, api.Errorf(api.PSignNotRemoved, "sign removed %s: the key file in latest of %s still holds the key", id, t).WithTarget(t).
					WithHint("publish the release package without the key first", "repoctl sign offboard "+id)
			}
		}
		summary = append(summary, fmt.Sprintf("removed  %s on the clients of %s", keyDisplay(k), t))
		entries = append(entries, sign.Removal{Key: id, Target: t, Fingerprint: k.Fingerprint, ConfirmedBy: ck.ID, At: sign.FormatAt(a.Now()), Note: note})
	}
	res := &Result{Kind: api.KindSignerList, Targets: e.targets}
	if dry {
		res.Items = signerResult(ctx, e, id, nil)
		res.Human = func(o *Out) error {
			for _, s := range summary {
				o.Printf("%s\n", s)
			}
			o.Note("dry run: nothing written")
			return nil
		}
		return res, nil
	}
	if !a.jsonOut {
		for _, s := range summary {
			a.out().Printf("%s\n", s)
		}
	}
	if err := a.confirmKeyID("Type the key id to confirm that no client has the key:", id); err != nil {
		return nil, err
	}
	for _, r := range entries {
		e.view.Deployments.PutRemoval(r)
	}
	if err := e.view.Deployments.Write(e.dir); err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("Removed key %s from the clients of %s", id, strings.Join(e.targets, ", "))
	if _, err := sign.SetCommit(ctx, e.dir, committer, []string{sign.DeployedFile}, msg); err != nil {
		return nil, err
	}
	a.signJournal(ctx, "sign removed", msg, e.targets)
	next := []api.NextStep{e.pushStep()}
	res.Items = signerResult(ctx, e, id, next)
	res.Next = next
	res.Human = func(o *Out) error {
		o.Printf("committed %s %q, signed by %s\n", sign.DeployedFile, msg, committer.KeyID())
		return nil
	}
	return res, nil
}

func hSignKeyfile(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	tree, _ := cmd.Flags().GetBool("tree")
	checksum, _ := cmd.Flags().GetBool("checksum")
	e, err := loadSign(ctx, a)
	if err != nil {
		return nil, err
	}
	type entry struct {
		label, info string
		data        []byte
		file        api.File
	}
	var entries []entry
	if tree {
		p, err := release.Package(e.set)
		if err != nil {
			return nil, err
		}
		if err := release.Check(e.set, e.dir, e.view.Trust); err != nil {
			if ae, ok := api.AsError(err); ok && ae.Code == api.PSignKeyfileStale {
				return nil, err
			}
		}
		path := release.KeyfilePath(e.set, p)
		data, err := os.ReadFile(filepath.Join(e.dir, path))
		if err != nil {
			return nil, api.Errorf(api.PSignKeyfileStale, "%s: %v", path, err).WithPath(path).WithHint("write the key file", "repoctl release write")
		}
		entries = append(entries, entry{label: path, data: data,
			file: api.File{Path: path, Action: "unchanged", Kind: "keyfile", Bytes: len(data), Sha256: sign.Sha256Hex(data)}})
	} else {
		for _, t := range e.targets {
			tr := e.view.Trees[t]
			data, err := tr.RootKeyfile()
			if err != nil || !tr.Exists() {
				return nil, api.Errorf(api.PSnapshotNotFound, "%s has no snapshot with a key file in the local copy of latest", t).WithTarget(t).
					WithHint("get the local copy of latest", "repoctl repo pull --target "+t)
			}
			tg, _ := e.set.Target(t)
			label := tg.ClientPath() + "/" + sign.KeyfileName(e.set)
			info := tr.NewestRelease()
			if pub := tr.PublishID(); pub != "" {
				info += " in " + pub
			}
			entries = append(entries, entry{label: label, info: strings.TrimSpace(info), data: data,
				file: api.File{Path: label, Action: "unchanged", Kind: "keyfile", Bytes: len(data), Sha256: sign.Sha256Hex(data)}})
		}
	}
	res := &Result{Kind: api.KindFile, Targets: e.targets}
	for _, en := range entries {
		res.Items = append(res.Items, en.file)
	}
	res.Human = func(o *Out) error {
		if checksum {
			width := 0
			for _, en := range entries {
				width = max(width, len(en.label))
			}
			for _, en := range entries {
				o.Printf("%s\n", strings.TrimRight(fmt.Sprintf("%s  %-*s   %s", en.file.Sha256, width, en.label, en.info), " "))
			}
			return nil
		}
		var shown []entry
		for _, en := range entries {
			same := false
			for _, s := range shown {
				if s.file.Sha256 == en.file.Sha256 {
					same = true
				}
			}
			if same {
				o.Note("%s is the same file", en.label)
				continue
			}
			shown = append(shown, en)
			if _, err := o.Stdout.Write(en.data); err != nil {
				return err
			}
		}
		return nil
	}
	return res, nil
}

func hReleaseWrite(ctx context.Context, a *App, _ *cobra.Command, _ []string) (*Result, error) {
	set, dir, err := a.Set()
	if err != nil {
		return nil, err
	}
	if _, err := release.Package(set); err != nil {
		return nil, err
	}
	l, err := a.SetLock(ctx, set, false)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	trust := sign.LoadTrust(set, dir)
	rel, err := release.Write(ctx, set, dir, trust, release.Options{Now: a.Now()})
	if err != nil {
		return nil, err
	}
	res := &Result{Kind: api.KindFile}
	for _, f := range rel.Files {
		res.Items = append(res.Items, f)
	}
	if len(rel.Changed) > 0 {
		res.Next = []api.NextStep{{Action: "commit", Human: true, Reason: "commit the files of the release package",
			Command: fmt.Sprintf("git commit -S -m %q -- %s", release.DefaultMessage, strings.Join(rel.Changed, " "))}}
	}
	res.Human = func(o *Out) error {
		width := 0
		for _, f := range rel.Files {
			width = max(width, len(f.Path))
		}
		writeReleaseLines(o, rel, width)
		if o.Verbose > 0 {
			for _, f := range rel.Files {
				if f.Action == "unchanged" {
					o.Printf("unchanged %s\n", f.Path)
				}
			}
		}
		if len(rel.Changed) == 0 {
			o.Note("the key file and the .repo files are up to date")
		}
		return nil
	}
	return res, nil
}
