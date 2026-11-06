package publish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// GPGSigner is the default Signer: gpg for the detached signatures and
// rpmsign for the RPMs, with one secret key of a GnuPG home.
type GPGSigner struct {
	// Homedir is the GnuPG home, "" for the default (GNUPGHOME or
	// ~/.gnupg).
	Homedir     string
	Fingerprint string
	UID         string
	KeyID       string
	State       string
}

// NewGPGSigner finds the signing key: key is sign.key of the user config,
// a key id of sign.keys or a fingerprint; "" takes the only key of
// sign.keys whose secret key the GnuPG home holds.
func NewGPGSigner(ctx context.Context, set *config.Set, key, homedir string) (*GPGSigner, error) {
	s := &GPGSigner{Homedir: homedir}
	secret, err := s.secretKeys(ctx)
	if err != nil {
		return nil, err
	}
	norm := func(f string) string { return strings.ToUpper(strings.ReplaceAll(f, " ", "")) }
	var cands []config.Key
	for _, k := range set.Sign.Keys {
		if key != "" && k.ID != key && !strings.EqualFold(norm(k.Fingerprint), norm(key)) {
			continue
		}
		if _, ok := secret[norm(k.Fingerprint)]; ok {
			cands = append(cands, k)
		}
	}
	switch {
	case len(cands) == 1:
		k := cands[0]
		s.Fingerprint, s.KeyID, s.State = norm(k.Fingerprint), k.ID, k.State
		s.UID = secret[s.Fingerprint]
		return s, nil
	case len(cands) > 1:
		return nil, api.Errorf(api.PSignNoKey, "the GnuPG home holds the secret keys of %d keys of sign.keys", len(cands)).
			WithHint("choose one in the user config", "repoctl config set sign.key ID")
	}
	if key != "" {
		if uid, ok := secret[norm(key)]; ok {
			s.Fingerprint, s.UID = norm(key), uid
			return s, nil
		}
		return nil, api.Errorf(api.PSignNoKey, "the GnuPG home has no secret key of %s", key).
			WithHint("check the key", "gpg --list-secret-keys")
	}
	return nil, api.Errorf(api.PSignNoKey, "the GnuPG home has no secret key of sign.keys").
		WithHint("add your key to the set", "repoctl sign add ID")
}

// secretKeys gives the uid of each secret key of the home, by fingerprint.
func (s *GPGSigner) secretKeys(ctx context.Context) (map[string]string, error) {
	out, err := (&run.Cmd{Name: "gpg", Args: s.args("--batch", "--with-colons", "--list-secret-keys")}).Output(ctx)
	if err != nil && run.ExitCode(err) < 0 {
		return nil, api.Errorf(api.PSignNoKey, "gpg cannot list the secret keys: %v", err).WithCause(err)
	}
	keys := map[string]string{}
	var cur string
	sec := false
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Split(l, ":")
		switch f[0] {
		case "sec":
			sec, cur = true, ""
		case "ssb":
			sec = false
		case "fpr":
			if sec && len(f) > 9 && cur == "" {
				cur = strings.ToUpper(f[9])
				keys[cur] = ""
			}
		case "uid":
			if cur != "" && keys[cur] == "" && len(f) > 9 {
				keys[cur] = unescapeColons(f[9])
			}
		}
	}
	return keys, nil
}

func unescapeColons(s string) string { return strings.ReplaceAll(s, `\x3a`, ":") }

func (s *GPGSigner) args(extra ...string) []string {
	if s.Homedir == "" {
		return extra
	}
	return append([]string{"--homedir", s.Homedir}, extra...)
}

// Info gives the signer.
func (s *GPGSigner) Info() api.Signer {
	return api.Signer{UID: s.UID, Fingerprint: s.Fingerprint, KeyID: s.KeyID, Backend: "gpg", State: s.State}
}

// Probe signs a small file and checks the signature.
func (s *GPGSigner) Probe(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "rp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "probe")
	if err := os.WriteFile(f, []byte("repoctl signer probe\n"), 0o600); err != nil {
		return err
	}
	if err := s.SignDetached(ctx, f, f+".asc"); err != nil {
		return err
	}
	if err := (&run.Cmd{Name: "gpg", Args: s.args("--batch", "--quiet", "--verify", f+".asc", f)}).Run(ctx); err != nil {
		return api.Errorf(api.PSignFailed, "the signer probe does not verify: %v", err).WithCause(err)
	}
	return nil
}

// SignRPMs runs rpmsign --resign with the key.
func (s *GPGSigner) SignRPMs(ctx context.Context, files []string) error {
	for start := 0; start < len(files); start += 100 {
		end := min(start+100, len(files))
		args := []string{"--define", "_gpg_name " + s.Fingerprint}
		if s.Homedir != "" {
			args = append(args, "--define", "_gpg_path "+s.Homedir)
		}
		args = append(append(args, "--resign"), files[start:end]...)
		// rpmsign writes to stdout and stderr from two goroutines: one
		// locked buffer keeps the order without a race.
		var out lockedBuilder
		if err := (&run.Cmd{Name: "rpmsign", Args: args, Stdout: &out, Stderr: &out}).Run(ctx); err != nil {
			return api.Errorf(api.PSignFailed, "rpmsign failed: %s", firstLines(out.String(), 3)).WithCause(err)
		}
	}
	return nil
}

// SignDetached writes an armored detached signature.
func (s *GPGSigner) SignDetached(ctx context.Context, file, sig string) error {
	var out strings.Builder
	c := &run.Cmd{Name: "gpg", Args: s.args("--batch", "--yes", "--quiet", "--local-user", s.Fingerprint+"!", "--armor", "--detach-sign", "--output", sig, file), Stderr: &out}
	if err := c.Run(ctx); err != nil {
		return api.Errorf(api.PSignFailed, "gpg cannot sign %s: %s", filepath.Base(file), firstLines(out.String(), 3)).WithCause(err)
	}
	return nil
}

// lockedBuilder is a strings.Builder for concurrent writers.
type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "; ")
}

// Ensure GPGSigner is a Signer.
var _ Signer = (*GPGSigner)(nil)

// String names the signer for messages.
func (s *GPGSigner) String() string {
	if s.KeyID != "" {
		return s.KeyID
	}
	return fmt.Sprintf("key %s", api.ShortFingerprint(s.Fingerprint))
}
