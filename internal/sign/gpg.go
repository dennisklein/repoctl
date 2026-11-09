package sign

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// GPG is the signing backend gpg (sign.backend: gpg): the local gpg with a
// key file or an OpenPGP card, in the GnuPG home of sign.gpg.homedir, else
// GNUPGHOME, else ~/.gnupg.
type GPG struct {
	// Homedir is sign.gpg.homedir; "" uses GNUPGHOME or ~/.gnupg.
	Homedir string
	// Program is the gpg binary, default gpg.
	Program string
}

// NewGPG gives the backend for a GnuPG home ("" for the default).
func NewGPG(homedir string) *GPG { return &GPG{Homedir: homedir} }

func (g *GPG) program() string {
	if g.Program != "" {
		return g.Program
	}
	return "gpg"
}

// Env gives the variables that point a child (git, rpmsign) at the GnuPG
// home of the backend.
func (g *GPG) Env() []string {
	if g.Homedir == "" {
		return nil
	}
	return []string{"GNUPGHOME=" + g.Homedir}
}

// Home gives the effective GnuPG home directory.
func (g *GPG) Home() string {
	if g.Homedir != "" {
		return g.Homedir
	}
	if h := os.Getenv("GNUPGHOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gnupg")
}

func (g *GPG) cmd(args ...string) *run.Cmd {
	full := []string{"--batch", "--no-tty"}
	if g.Homedir != "" {
		full = append(full, "--homedir", g.Homedir)
	}
	return &run.Cmd{Name: g.program(), Args: append(full, args...), Env: g.Env()}
}

// Export gives the armored public key of the fingerprint, from the local
// keyring or the card stub.
func (g *GPG) Export(ctx context.Context, fpr string) ([]byte, error) {
	out, err := g.cmd("--armor", "--export", fpr).Output(ctx)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, api.Errorf(api.PSignNoKey, "gpg has no public key %s", fpr).
			WithHint("import the key, or give the file", "repoctl sign add ID --fingerprint FPR --file FILE")
	}
	return out, nil
}

// Import imports keys into the keyring.
func (g *GPG) Import(ctx context.Context, data []byte) error {
	c := g.cmd("--import")
	c.Stdin = bytes.NewReader(data)
	return c.Run(ctx)
}

// SecretKey is one secret key of gpg --list-secret-keys.
type SecretKey struct {
	Fingerprint string
	// Card is the serial number of the OpenPGP card that holds the key,
	// "" for a key in the keyring.
	Card string
	// Available is false for a stub without the secret key (gpg shows #).
	Available bool
	// Subkeys are the fingerprints of the subkeys whose secret key is
	// available.
	Subkeys []string
}

// SecretKeys lists the secret keys of the keyring and the cards.
func (g *GPG) SecretKeys(ctx context.Context) ([]SecretKey, error) {
	out, err := g.cmd("--with-colons", "--fixed-list-mode", "--list-secret-keys").Output(ctx)
	if err != nil {
		// gpg exits 2 when the keyring is empty or missing.
		if run.ExitCode(err) == 2 {
			return nil, nil
		}
		return nil, err
	}
	var keys []SecretKey
	var cur *SecretKey
	var pending, pendingSub string
	sub := false
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "sec":
			keys = append(keys, SecretKey{})
			cur = &keys[len(keys)-1]
			pending, sub = field(f, 14), false
		case "ssb":
			pendingSub, sub = field(f, 14), true
		case "fpr":
			if cur == nil {
				continue
			}
			if !sub && cur.Fingerprint == "" {
				cur.Fingerprint = field(f, 9)
				cur.Available, cur.Card = cardField(pending)
			} else if sub {
				if ok, _ := cardField(pendingSub); ok {
					cur.Subkeys = append(cur.Subkeys, field(f, 9))
				}
			}
		}
	}
	return keys, nil
}

func field(f []string, i int) string {
	if i < len(f) {
		return f[i]
	}
	return ""
}

// cardField reads field 15 of a sec or ssb line: "+" a secret key in the
// keyring, "#" a stub, a serial number a key on a card.
func cardField(s string) (available bool, card string) {
	switch s {
	case "#":
		return false, ""
	case "+", "":
		return true, ""
	}
	return true, s
}

// FindSecret gives the fingerprints of the primary keys with a secret key
// whose user ID matches query, as gpg --list-secret-keys QUERY matches it
// (a mail address, a name, a part of the user ID).
func (g *GPG) FindSecret(ctx context.Context, query string) ([]string, error) {
	out, err := g.cmd("--with-colons", "--fixed-list-mode", "--list-secret-keys", "--", query).Output(ctx)
	if err != nil {
		// gpg exits 2 when no key matches.
		if run.ExitCode(err) == 2 {
			return nil, nil
		}
		return nil, err
	}
	var fprs []string
	primary := false
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, ":")
		switch f[0] {
		case "sec":
			primary = true
		case "ssb":
			primary = false
		case "fpr":
			if primary {
				fprs = append(fprs, field(f, 9))
				primary = false
			}
		}
	}
	return fprs, nil
}

// HasSecret reports whether the secret key of the fingerprint (or one of
// its subkeys) is in the keyring or on a card.
func (g *GPG) HasSecret(ctx context.Context, fpr string) (bool, error) {
	keys, err := g.SecretKeys(ctx)
	if err != nil {
		return false, err
	}
	fpr = NormalizeFingerprint(fpr)
	for _, k := range keys {
		if k.Fingerprint == fpr && (k.Available || len(k.Subkeys) > 0) {
			return true, nil
		}
	}
	return false, nil
}

// DetachSign writes a detached signature of in to out with exactly one
// signature packet of the key: gpg --detach-sign -u FPR!.
func (g *GPG) DetachSign(ctx context.Context, fpr, in, out string, armor bool) error {
	args := []string{"--status-fd", "2", "--yes", "--local-user", fpr + "!", "--detach-sign"}
	if armor {
		args = append(args, "--armor")
	}
	args = append(args, "--output", out, in)
	c := g.cmd(args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(ctx); err != nil {
		return probeError(fpr, stderr.String(), err)
	}
	return nil
}

// Probe is the signer probe (signing.html#probe): it signs a random
// 32-byte value with gpg --detach-sign -u FPR! and verifies it. Thus
// gpg-agent asks for the PIN at the start of a publish. A failure gives
// sign.locked, sign.card-absent or sign.no-pinentry (exit 9).
func (g *GPG) Probe(ctx context.Context, fpr string) error {
	dir, err := os.MkdirTemp("", "repoctl-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return err
	}
	in, sig := filepath.Join(dir, "probe"), filepath.Join(dir, "probe.sig")
	if err := os.WriteFile(in, value, 0o600); err != nil {
		return err
	}
	if err := g.DetachSign(ctx, fpr, in, sig, false); err != nil {
		return err
	}
	c := g.cmd("--status-fd", "1", "--verify", sig, in)
	out, err := c.Output(ctx)
	if err != nil || !validSig(string(out), fpr) {
		return api.Errorf(api.PSignLocked, "signer probe: the signature of %s does not verify", fpr).
			WithHint("check the card and the key", "gpg --card-status")
	}
	return nil
}

// validSig reports a VALIDSIG line of the primary key fpr in gpg status
// output.
func validSig(status, fpr string) bool {
	for _, l := range strings.Split(status, "\n") {
		f := strings.Fields(l)
		if len(f) >= 3 && f[0] == "[GNUPG:]" && f[1] == "VALIDSIG" {
			primary := f[len(f)-1]
			if f[2] == fpr || primary == fpr {
				return true
			}
		}
	}
	return false
}

// probeError maps a failed gpg signature to the problem of the probe.
func probeError(fpr, stderr string, err error) error {
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "no pinentry"), strings.Contains(low, "pinentry") && strings.Contains(low, "not found"):
		return api.Errorf(api.PSignNoPinentry, "signer probe: no pinentry works for gpg-agent").WithCause(err).
			WithHint("install a pinentry for the terminal or the desktop", "sudo dnf install pinentry")
	case strings.Contains(low, "card not present"), strings.Contains(low, "card removed"),
		strings.Contains(low, "no such device"), strings.Contains(low, "cardctrl 4"), strings.Contains(low, "card error"):
		return api.Errorf(api.PSignCardAbsent, "signer probe: the card of %s is not present", fpr).WithCause(err).
			WithHint("insert the card", "gpg --card-status")
	case strings.Contains(low, "no secret key"), strings.Contains(low, "inv_sgnr"):
		return api.Errorf(api.PSignNoKey, "the secret key %s is not on this host", fpr).WithCause(err).
			WithHint("check the keys of gpg and sign.key", "repoctl doctor")
	}
	msg := lastLine(stderr)
	if msg == "" {
		msg = err.Error()
	}
	return api.Errorf(api.PSignLocked, "signer probe: gpg cannot sign with %s: %s", fpr, msg).WithCause(err).
		WithHint("unlock the card or the key", "gpg --card-status")
}

func lastLine(s string) string {
	var last string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if !strings.HasPrefix(l, "[GNUPG:]") && strings.TrimSpace(l) != "" {
			last = strings.TrimSpace(l)
		}
	}
	return last
}

// AgentSocket gives the path of the gpg-agent socket of the home.
func (g *GPG) AgentSocket(ctx context.Context) (string, error) {
	args := []string{"--list-dirs", "agent-socket"}
	if g.Homedir != "" {
		args = append([]string{"--homedir", g.Homedir}, args...)
	}
	out, err := (&run.Cmd{Name: "gpgconf", Args: args, Env: g.Env()}).Output(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
