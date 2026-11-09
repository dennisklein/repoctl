package sign

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// The sign checks of repoctl doctor (signing.html#doctor). Each function
// gives the checks of one id, in the group sign; the doctor frame registers
// them. A check without its input (no set, no sign.key) is skipped.

// DoctorGroup is the doctor group of the sign checks.
const DoctorGroup = "sign"

// MaxAgentSocket is the longest gpg-agent socket path that works.
const MaxAgentSocket = 107

// DoctorInput is what the sign checks read.
type DoctorInput struct {
	// View is the set, nil outside a set.
	View *View
	// Signer is the key of sign.key, nil when it is not set or not
	// resolved; SignerErr says why.
	Signer    *Signer
	SignerErr error
	// GPG is the backend; default the backend of the signer.
	GPG *GPG
	// ReleaseCheck is the stale check of the release package files
	// (release.Diff); nil skips sign.keyfile, release.repo and
	// release.url.
	ReleaseCheck func() (keyfile bool, repofiles []string, err error)
}

func check(name string, status api.CheckStatus, detail, do, cmd string) api.DoctorReportCheck {
	c := api.DoctorReportCheck{Name: name, Group: DoctorGroup, Status: status, Detail: detail}
	if do != "" || cmd != "" {
		c.Remediation = api.HintText(do, cmd)
	}
	return c
}

func ok(name, detail string) api.DoctorReportCheck {
	return check(name, api.CheckStatusOk, detail, "", "")
}

func (in DoctorInput) gpg() *GPG {
	if in.GPG != nil {
		return in.GPG
	}
	if in.Signer != nil {
		return in.Signer.GPG
	}
	return NewGPG("")
}

// DoctorChecks gives all sign checks that the input allows.
func DoctorChecks(ctx context.Context, in DoctorInput) []api.DoctorReportCheck {
	var out []api.DoctorReportCheck
	out = append(out, CheckKey(ctx, in))
	if in.View != nil {
		out = append(out, CheckTrusted(in), CheckKeyfiles(in), CheckExpiry(in), CheckAlgorithm(in))
		out = append(out, CheckDeployed(ctx, in)...)
		out = append(out, CheckCrossSet(ctx, in)...)
		out = append(out, CheckRelease(ctx, in)...)
		out = append(out, CheckRetiring(in), CheckRepomd(in), CheckVerifyEL(in))
	}
	out = append(out, CheckAgent(ctx, in), CheckPinentry())
	if c, has := CheckCard(ctx, in); has {
		out = append(out, c)
	}
	return out
}

// CheckKey is sign.key: the key of sign.key has a secret key in gpg or on
// an OpenPGP card. A missing secret key is a warning: a host can only
// build.
func CheckKey(ctx context.Context, in DoctorInput) api.DoctorReportCheck {
	const name = "sign.key"
	if in.Signer == nil {
		msg := "sign.key is not set: this host can build, but not publish"
		if in.SignerErr != nil {
			msg = in.SignerErr.Error()
		}
		return check(name, api.CheckStatusWarning, msg, "set sign.key in the user config", "repoctl config show")
	}
	has, err := in.Signer.GPG.HasSecret(ctx, in.Signer.FPR)
	if err != nil {
		return check(name, api.CheckStatusWarning, "gpg: "+err.Error(), "check gpg", "gpg --list-secret-keys")
	}
	if !has {
		return check(name, api.CheckStatusWarning, fmt.Sprintf("no secret key of %s (%s) on this host: it can build, but not publish", in.Signer.KeyID(), AbbrevFingerprint(in.Signer.FPR)),
			"insert the card or import the key", "gpg --card-status")
	}
	return ok(name, fmt.Sprintf("%s %s", in.Signer.KeyID(), GroupFingerprint(in.Signer.FPR)))
}

// CheckTrusted is sign.trusted: the key of sign.key is active in sign.keys.
func CheckTrusted(in DoctorInput) api.DoctorReportCheck {
	const name = "sign.trusted"
	if in.Signer == nil {
		return check(name, api.CheckStatusWarning, "sign.key is not set", "set sign.key in the user config", "repoctl config show")
	}
	k := in.View.Trust.ByFingerprint(in.Signer.FPR)
	switch {
	case k == nil:
		return check(name, api.CheckStatusWarning, fmt.Sprintf("the key %s of sign.key is not in sign.keys", AbbrevFingerprint(in.Signer.FPR)),
			"add the key", "repoctl sign add ID --fingerprint "+in.Signer.FPR)
	case k.State != config.KeyActive:
		return check(name, api.CheckStatusWarning, fmt.Sprintf("the key %s is %s in sign.keys: it cannot sign", k.ID, k.State),
			"sign with an active key", "repoctl sign ls")
	}
	return ok(name, k.ID+" is active")
}

// CheckKeyfiles is sign.keyfiles: each keys/ID.asc parses, and its
// fingerprint matches repoctl.yaml.
func CheckKeyfiles(in DoctorInput) api.DoctorReportCheck {
	const name = "sign.keyfiles"
	var bad []string
	for _, k := range in.View.Trust.Keys {
		if k.Err != nil {
			bad = append(bad, k.ID+": "+k.Err.Error())
		}
	}
	if len(bad) > 0 {
		return check(name, api.CheckStatusFailed, strings.Join(bad, "; "), "export the public key of each key again", "gpg --armor --export FPR")
	}
	return ok(name, plural(len(in.View.Trust.Keys), "key")+" in keys/")
}

// CheckExpiry is sign.expiry: no key of the key file expires inside
// sign.checks.key_expiry_warn.
func CheckExpiry(in DoctorInput) api.DoctorReportCheck {
	const name = "sign.expiry"
	warn, _ := Checks(in.View.Set)
	var soon []string
	for _, k := range in.View.Trust.KeyfileKeys() {
		if k.Cert != nil && k.Cert.ExpiresIn(in.View.Now, warn) {
			soon = append(soon, fmt.Sprintf("%s expires on %s", k.ID, k.Cert.Expires.Format("2006-01-02")))
		}
	}
	if len(soon) > 0 {
		return check(name, api.CheckStatusWarning, strings.Join(soon, "; ")+" (key_expiry_warn "+config.FormatDuration(warn)+")",
			"add and deploy a new key, then retire the old key", "repoctl sign add NEW-ID --fingerprint FPR")
	}
	return ok(name, "no key expires within "+config.FormatDuration(warn))
}

// CheckAlgorithm is sign.algorithm: each key of the key file meets gate 3.
func CheckAlgorithm(in DoctorInput) api.DoctorReportCheck {
	const name = "sign.algorithm"
	var bad []string
	status := api.CheckStatusOk
	for _, k := range in.View.Trust.KeyfileKeys() {
		if k.Cert == nil {
			continue
		}
		if r := k.Cert.CheckFormat(); r != "" {
			bad = append(bad, k.ID+" is "+r)
			if k.State == config.KeyActive {
				status = api.CheckStatusFailed
			} else if status == api.CheckStatusOk {
				status = api.CheckStatusWarning
			}
		}
	}
	if len(bad) > 0 {
		return check(name, status, strings.Join(bad, "; "), "make a new key: RSA 4096 or Ed25519", "repoctl help key")
	}
	return ok(name, "each key is OpenPGP v4, RSA 2048 or more, or Ed25519")
}

// CheckDeployed is sign.deployed: the key of this human has a deployment
// record for each target with a latest, and the release of the record is in
// latest.
func CheckDeployed(ctx context.Context, in DoctorInput) []api.DoctorReportCheck {
	const name = "sign.deployed"
	if in.Signer == nil {
		return nil
	}
	k := in.View.Trust.ByFingerprint(in.Signer.FPR)
	if k == nil {
		return nil
	}
	var missing []string
	for _, t := range in.View.Targets {
		if in.View.latest(t) == nil {
			continue
		}
		if passes, reason := in.View.RecordPasses(ctx, k, t); !passes {
			missing = append(missing, t+": "+reason)
		}
	}
	if len(missing) > 0 {
		return []api.DoctorReportCheck{check(name, api.CheckStatusWarning, k.ID+" cannot sign for "+strings.Join(missing, "; "),
			"see the onboarding steps", "repoctl sign onboard "+k.ID)}
	}
	return []api.DoctorReportCheck{ok(name, k.ID+" has a deployment record for each target with a latest")}
}

// CheckRelease gives sign.keyfile, sign.release, release.url, release.repo
// and release.version.
func CheckRelease(ctx context.Context, in DoctorInput) []api.DoctorReportCheck {
	v := in.View
	p, has := v.Set.ReleasePackage()
	if !has {
		return []api.DoctorReportCheck{check("sign.keyfile", api.CheckStatusWarning, "the set has no package with role: release",
			"add the in-tree release package", "repoctl help new-set")}
	}
	var out []api.DoctorReportCheck
	if in.ReleaseCheck != nil {
		keyfile, repofiles, err := in.ReleaseCheck()
		switch {
		case err != nil:
			out = append(out, check("sign.keyfile", api.CheckStatusWarning, err.Error(), "correct the set config", "repoctl release write"))
		case keyfile:
			out = append(out, check("sign.keyfile", api.CheckStatusWarning, filepath.Join(p.Path, KeyfileName(v.Set))+" differs from sign.keys",
				"write the key file, then commit it", "repoctl release write"))
		default:
			out = append(out, ok("sign.keyfile", filepath.Join(p.Path, KeyfileName(v.Set))+" equals sign.keys"))
		}
		if err == nil {
			out = append(out, checkRepoURL(v, p), checkRepoFiles(repofiles))
		}
	}
	// sign.release: the release package in latest has the same key file.
	kf, _ := v.treeKeyfile()
	var behind []string
	for _, t := range v.Targets {
		if v.latest(t) == nil {
			continue
		}
		if data, okk := v.keyfileOfTree(t); !okk || !bytes.Equal(data, kf) {
			behind = append(behind, t)
		}
	}
	if len(behind) > 0 {
		out = append(out, check("sign.release", api.CheckStatusWarning, "the key file in latest of "+strings.Join(behind, ", ")+" differs from the key file of the set",
			"build and publish the release package", "repoctl plan "+p.ID))
	} else {
		out = append(out, ok("sign.release", "latest has the key file of the set"))
	}
	out = append(out, checkReleaseVersion(v, p))
	return out
}

func checkRepoURL(v *View, p *config.Package) api.DoctorReportCheck {
	const name = "release.url"
	var bad []string
	for _, t := range v.Set.ActiveTargets() {
		path := filepath.Join(v.Dir, p.Path, v.Set.Repo.Name+"-"+t.ID+".repo")
		data, err := os.ReadFile(path)
		if err != nil {
			bad = append(bad, t.ID+": no .repo file")
			continue
		}
		want := strings.TrimSuffix(v.Set.Repo.URL, "/") + "/" + t.ClientPath() + "/"
		for _, l := range strings.Split(string(data), "\n") {
			if u, found := strings.CutPrefix(l, "baseurl="); found && !strings.HasPrefix(u, want) {
				bad = append(bad, t.ID+": "+u)
			}
		}
	}
	if len(bad) > 0 {
		return check(name, api.CheckStatusWarning, "baseurl differs from repo.url: "+strings.Join(bad, "; "), "write the .repo files, then commit them", "repoctl release write")
	}
	return ok(name, "each baseurl is below "+v.Set.Repo.URL)
}

func checkRepoFiles(stale []string) api.DoctorReportCheck {
	const name = "release.repo"
	if len(stale) > 0 {
		return check(name, api.CheckStatusWarning, strings.Join(stale, ", ")+" differ from the files that release write writes",
			"write the .repo files, then commit them", "repoctl release write")
	}
	return ok(name, "each .repo file equals the file of release write")
}

var setVersion = regexp.MustCompile(`-(\d+(?:\.\d+)*)$`)

func checkReleaseVersion(v *View, p *config.Package) api.DoctorReportCheck {
	const name = "release.version"
	m := setVersion.FindStringSubmatch(v.Set.Name)
	if m == nil {
		return ok(name, "the set name has no version; the check is off")
	}
	spec, err := filepath.Glob(filepath.Join(v.Dir, p.Path, "*.spec"))
	if err != nil || len(spec) == 0 {
		return check(name, api.CheckStatusWarning, "the release package has no spec", "", "")
	}
	data, _ := os.ReadFile(spec[0])
	ver := ""
	for _, x := range specVR.FindAllSubmatch(data, -1) {
		if string(x[1]) == "Version" {
			ver = string(x[2])
			break
		}
	}
	if ver != m[1] {
		return check(name, api.CheckStatusWarning, fmt.Sprintf("the release package has Version %s, the set name %s", ver, m[1]),
			"set the Version of the release package to "+m[1], "")
	}
	return ok(name, "Version "+ver)
}

// CheckRetiring is sign.retiring: no RPM in latest carries a key in state
// retiring.
func CheckRetiring(in DoctorInput) api.DoctorReportCheck {
	const name = "sign.retiring"
	v := in.View
	for _, k := range v.Trust.InState(config.KeyRetiring) {
		for _, t := range v.Targets {
			if n := v.RPMs(k, t); n > 0 {
				return check(name, api.CheckStatusWarning, fmt.Sprintf("%s in latest of %s %s the retiring key %s", plural(n, "RPM"), t, Agree(n, "carries", "carry"), k.ID),
					"re-sign them with an active key", fmt.Sprintf("repoctl repo resign --signer %s --target %s", k.ID, t))
			}
		}
	}
	return ok(name, "no RPM in latest carries a retiring key")
}

// CheckRepomd is sign.repomd: each repomd.xml.asc in latest has exactly one
// signature packet.
func CheckRepomd(in DoctorInput) api.DoctorReportCheck {
	const name = "sign.repomd"
	v := in.View
	var bad []string
	n := 0
	for _, t := range v.Targets {
		tree := v.latest(t)
		if tree == nil {
			continue
		}
		for _, repo := range []string{v.Set.Repo.Name, v.Set.Repo.Name + "-source", v.Set.Repo.Name + "-debuginfo"} {
			asc := filepath.Join(tree.Dir, repo, "repodata", "repomd.xml.asc")
			data, err := os.ReadFile(asc)
			if err != nil {
				continue
			}
			n++
			if c, err := CountSignatures(data); err != nil || c != 1 {
				bad = append(bad, fmt.Sprintf("%s/%s has %d", t, repo, c))
			}
		}
	}
	if len(bad) > 0 {
		return check(name, api.CheckStatusFailed, "repomd.xml.asc needs exactly one signature packet: "+strings.Join(bad, ", "),
			"publish again; EL10 librepo refuses a second packet", "repoctl repo verify")
	}
	return ok(name, fmt.Sprintf("%d repomd.xml.asc with one signature each", n))
}

// CheckVerifyEL is sign.verify-el: a verify root exists for each target
// (build.html#mock), the root of mock --uniqueext repoctl-UID-SET-verify-
// TARGET below the mock dir. repoctl mock update makes it.
func CheckVerifyEL(in DoctorInput) api.DoctorReportCheck {
	const name = "sign.verify-el"
	v := in.View
	if len(v.Targets) == 0 {
		return ok(name, "no target")
	}
	var have []string
	if v.MockDir != "" {
		entries, _ := os.ReadDir(v.MockDir)
		for _, e := range entries {
			if e.IsDir() {
				have = append(have, e.Name())
			}
		}
	}
	var missing []string
	for _, t := range v.Targets {
		if !hasVerifyRoot(have, mock.VerifyExt(v.Set.Name, t)) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return check(name, api.CheckStatusWarning, "no verify root for "+strings.Join(missing, ", ")+" in "+v.MockDir,
			"make the verify roots before a publish", "repoctl mock update")
	}
	return ok(name, "verify roots for "+strings.Join(v.Targets, ", ")+" ready")
}

// hasVerifyRoot says whether a root directory CONFIG-EXT is among the
// names; the bootstrap root CONFIG-bootstrap-EXT does not count.
func hasVerifyRoot(names []string, ext string) bool {
	for _, n := range names {
		if root, found := strings.CutSuffix(n, "-"+ext); found && root != "" && !strings.HasSuffix(root, "-bootstrap") {
			return true
		}
	}
	return false
}

// CheckAgent is sign.agent: the gpg-agent socket path is short enough.
func CheckAgent(ctx context.Context, in DoctorInput) api.DoctorReportCheck {
	const name = "sign.agent"
	sock, err := in.gpg().AgentSocket(ctx)
	if err != nil {
		return check(name, api.CheckStatusWarning, "gpgconf: "+err.Error(), "install gnupg2", "sudo dnf install gnupg2")
	}
	if len(sock) > MaxAgentSocket {
		return check(name, api.CheckStatusFailed, fmt.Sprintf("the gpg-agent socket path has %d bytes (limit %d): %s", len(sock), MaxAgentSocket, sock),
			"use a shorter GnuPG home (sign.gpg.homedir)", "repoctl config show")
	}
	return ok(name, sock)
}

// CheckPinentry is sign.pinentry: a pinentry for the terminal or the
// desktop is installed.
func CheckPinentry() api.DoctorReportCheck {
	const name = "sign.pinentry"
	for _, p := range []string{"pinentry", "pinentry-curses", "pinentry-tty", "pinentry-gnome3", "pinentry-qt", "pinentry-gtk-2"} {
		if path, err := exec.LookPath(p); err == nil {
			return ok(name, path)
		}
	}
	return check(name, api.CheckStatusWarning, "no pinentry is installed: gpg-agent cannot ask for the PIN",
		"install a pinentry", "sudo dnf install pinentry")
}

// CheckCard is sign.card: the OpenPGP card of sign.key is present and has
// PIN retries left. It reports has=false when the key is not on a card.
func CheckCard(ctx context.Context, in DoctorInput) (api.DoctorReportCheck, bool) {
	const name = "sign.card"
	if in.Signer == nil {
		return api.DoctorReportCheck{}, false
	}
	keys, err := in.Signer.GPG.SecretKeys(ctx)
	if err != nil {
		return api.DoctorReportCheck{}, false
	}
	card := ""
	for _, k := range keys {
		if k.Fingerprint == in.Signer.FPR {
			card = k.Card
		}
	}
	if card == "" {
		return api.DoctorReportCheck{}, false
	}
	out, err := in.Signer.GPG.cmd("--with-colons", "--card-status").Output(ctx)
	if err != nil {
		return check(name, api.CheckStatusFailed, "the card "+card+" is not present", "insert the card", "gpg --card-status"), true
	}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Split(l, ":")
		if f[0] == "pinretry" && len(f) >= 4 {
			if f[1] == "0" {
				return check(name, api.CheckStatusFailed, "the PIN of card "+card+" is blocked", "unblock the PIN with the reset code or the Admin PIN", "gpg --card-edit"), true
			}
			if f[1] == "1" || f[3] == "1" || f[3] == "0" {
				return check(name, api.CheckStatusWarning, fmt.Sprintf("card %s: PIN retries %s, reset code %s, Admin PIN %s", card, f[1], f[2], f[3]),
					"reset the retry counters", "gpg --card-edit"), true
			}
			if f[2] == "0" {
				return check(name, api.CheckStatusWarning, fmt.Sprintf("card %s has no reset code", card),
					"set a reset code", "ykman openpgp access change-reset-code"), true
			}
			return ok(name, fmt.Sprintf("card %s: PIN retries %s, reset code %s, Admin PIN %s", card, f[1], f[2], f[3])), true
		}
	}
	return ok(name, "card "+card), true
}

// CheckCrossSet is sign.cross-set: each other set branch on the remote has
// the same keys in sign.keys, with the same states. It reads the last
// fetched branches.
func CheckCrossSet(ctx context.Context, in DoctorInput) []api.DoctorReportCheck {
	const name = "sign.cross-set"
	v := in.View
	g := git.New(v.Dir)
	remote := v.Set.Remote()
	refs, err := g.Lines(ctx, "for-each-ref", "--format=%(refname:short)", "refs/remotes/"+remote+"/set/")
	if err != nil {
		return nil
	}
	mine := keyStates(v.Set)
	var diff []string
	sets := 0
	for _, ref := range refs {
		setName := strings.TrimPrefix(ref, remote+"/set/")
		if setName == v.Set.Name {
			continue
		}
		c := &run.Cmd{Name: "git", Args: []string{"-C", v.Dir, "show", ref + ":" + config.SetFile}}
		data, err := c.Output(ctx)
		if err != nil {
			continue
		}
		other, err := config.ParseSet(data, ref+":"+config.SetFile)
		if err != nil || len(other.ActiveTargets()) == 0 {
			continue
		}
		sets++
		theirs := keyStates(other)
		for _, id := range sortedKeys(mine) {
			t, found := theirs[id]
			switch {
			case !found:
				diff = append(diff, fmt.Sprintf("%s has no key %s", setName, id))
			case t.fpr != mine[id].fpr:
				diff = append(diff, fmt.Sprintf("%s has another fingerprint for %s", setName, id))
			case t.state != mine[id].state:
				diff = append(diff, fmt.Sprintf("%s: %s is %s there, %s here", setName, id, t.state, mine[id].state))
			}
		}
		for _, id := range sortedKeys(theirs) {
			if _, found := mine[id]; !found {
				diff = append(diff, fmt.Sprintf("%s has the key %s (%s), this set has not", setName, id, theirs[id].state))
			}
		}
	}
	if len(diff) > 0 {
		return []api.DoctorReportCheck{check(name, api.CheckStatusWarning, strings.Join(diff, "; "),
			"do the onboarding or offboarding steps in each set", "repoctl help signing")}
	}
	return []api.DoctorReportCheck{ok(name, fmt.Sprintf("%d other sets have the same keys", sets))}
}

type keyState struct{ fpr, state string }

func keyStates(s *config.Set) map[string]keyState {
	out := map[string]keyState{}
	for _, k := range s.Sign.Keys {
		out[k.ID] = keyState{NormalizeFingerprint(k.Fingerprint), k.State}
	}
	return out
}

// plural gives "1 RPM" or "3 RPMs".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// Agree gives the verb that agrees with a count: one for 1, else many.
func Agree(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
