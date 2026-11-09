package sign

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// Signer is the key of the human who runs the command: sign.key of the user
// config, resolved against sign.keys, with the backend of sign.backend.
// The publish signs the new RPMs and repomd.xml with it.
type Signer struct {
	GPG *GPG
	// ID is the id of the key in sign.keys; "" when sign.key is a
	// fingerprint outside sign.keys.
	ID string
	// FPR is the full fingerprint.
	FPR string
	// Key is the entry of sign.keys, nil outside sign.keys.
	Key *TrustedKey
	// Backend is sign.backend, gpg.
	Backend string
}

// Fingerprint gives the full fingerprint of the signer.
func (s *Signer) Fingerprint() string { return s.FPR }

// KeyID gives the id of the key in sign.keys, or the fingerprint.
func (s *Signer) KeyID() string {
	if s.ID != "" {
		return s.ID
	}
	return s.FPR
}

// NewSigner resolves the signer of the user settings: sign.key (an id of
// sign.keys, a full fingerprint, or a user ID that matches one secret key
// in gpg; default rpm --eval %{_gpg_name}),
// sign.backend and sign.gpg.homedir. The trust may be nil outside a set.
func NewSigner(ctx context.Context, trust *Trust, eff *config.Effective) (*Signer, error) {
	backend, homedir, value := "gpg", "", ""
	if eff != nil {
		backend = eff.String("sign.backend")
		homedir = eff.String("sign.gpg.homedir")
		value = eff.String("sign.key")
	}
	if backend != "" && backend != "gpg" {
		return nil, api.Errorf(api.PConfigUnsupported, "sign.backend %q: this release has only gpg", backend)
	}
	if value == "" {
		value = rpmGPGName(ctx)
	}
	if value == "" {
		return nil, api.Errorf(api.PSignNoKey, "sign.key is not set: repoctl does not know the key of this human").
			WithHint("set sign.key in ~/.config/repoctl/config.yaml to your id of sign.keys or the fingerprint of your key", "gpg --list-secret-keys")
	}
	s := &Signer{GPG: NewGPG(expandHome(homedir)), Backend: "gpg"}
	if trust != nil {
		if k := trust.Resolve(value); k != nil {
			s.ID, s.FPR, s.Key = k.ID, k.Fingerprint, k
			return s, nil
		}
	}
	fpr := NormalizeFingerprint(value)
	if !IsFingerprint(fpr) {
		// A user ID of gpg, for example the mail address of %_gpg_name.
		found, err := s.GPG.FindSecret(ctx, value)
		if err != nil {
			return nil, api.Errorf(api.PSignNoKey, "gpg cannot list the secret keys: %v", err).WithCause(err).
				WithHint("check gpg", "gpg --list-secret-keys")
		}
		switch len(found) {
		case 1:
			fpr = found[0]
		case 0:
			return nil, api.Errorf(api.PSignNotActive, "sign.key %q is not an id of sign.keys, not a full fingerprint and no user ID of a secret key in gpg", value).
				WithHint("set sign.key in ~/.config/repoctl/config.yaml to your id of sign.keys or the fingerprint of your key (40 hex digits)", "gpg --list-secret-keys")
		default:
			return nil, api.Errorf(api.PSignNotActive, "sign.key %q matches %d secret keys in gpg: %s", value, len(found), strings.Join(found, ", ")).
				WithHint("set sign.key to one full fingerprint in ~/.config/repoctl/config.yaml", "gpg --list-secret-keys "+value)
		}
	}
	s.FPR = fpr
	if trust != nil {
		if k := trust.ByFingerprint(fpr); k != nil {
			s.ID, s.Key = k.ID, k
		}
	}
	return s, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// rpmGPGName gives rpm --eval %{_gpg_name}, the default of sign.key.
func rpmGPGName(ctx context.Context) string {
	out, err := (&run.Cmd{Name: "rpm", Args: []string{"--eval", "%{?_gpg_name}"}}).Output(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// RequireSecret checks that the secret key of the signer is on this host:
// else sign.no-key (exit 9).
func (s *Signer) RequireSecret(ctx context.Context) error {
	ok, err := s.GPG.HasSecret(ctx, s.FPR)
	if err != nil {
		return api.Errorf(api.PSignNoKey, "gpg cannot list the secret keys: %v", err).WithCause(err).
			WithHint("check gpg", "repoctl doctor")
	}
	if !ok {
		return api.Errorf(api.PSignNoKey, "the secret key of sign.key %s (%s) is not on this host", s.KeyID(), AbbrevFingerprint(s.FPR)).
			WithHint("insert the card or import the key, or set sign.key", "repoctl doctor")
	}
	return nil
}

// Probe runs the signer probe (signing.html#probe).
func (s *Signer) Probe(ctx context.Context) error { return s.GPG.Probe(ctx, s.FPR) }

// SignRPMs gives each RPM one new signature of the signer with rpmsign
// --resign. It passes %_gpg_name (rpm 4) and %_openpgp_sign_id (rpm 6) as
// the full fingerprint and never uses --addsign or --rpmv6, so the result
// is the same on rpm 4.16, 4.19 and 6. A failure gives sign.failed with the
// file.
func (s *Signer) SignRPMs(ctx context.Context, files []string) error {
	if len(files) == 0 {
		return nil
	}
	args := []string{
		"--resign",
		"--define", "_gpg_name " + s.FPR,
		"--define", "_openpgp_sign_id " + s.FPR,
		"--define", "_gpg_sign_cmd_extra_args --batch",
	}
	if home := s.GPG.Homedir; home != "" {
		args = append(args, "--define", "_gpg_path "+home)
	}
	// One rpmsign per batch keeps the command line short and names the
	// file of a failure.
	const batch = 64
	for i := 0; i < len(files); i += batch {
		chunk := files[i:min(i+batch, len(files))]
		c := &run.Cmd{Name: "rpmsign", Args: append(append([]string{}, args...), chunk...), Env: s.GPG.Env()}
		if err := c.Run(ctx); err != nil {
			file := chunk[0]
			if len(chunk) > 1 {
				file = failedFile(chunk, err)
			}
			return api.Errorf(api.PSignFailed, "rpmsign %s: %v", filepath.Base(file), err).WithPath(file).WithCause(err).
				WithHint("check the card and the key, then publish again", "repoctl doctor --only sign")
		}
	}
	return nil
}

// failedFile gives the file that rpmsign names in its error, else the
// first file of the batch.
func failedFile(files []string, err error) string {
	msg := err.Error()
	var e *run.Error
	if errors.As(err, &e) {
		msg = e.Stderr
	}
	for _, f := range files {
		if strings.Contains(msg, f) || strings.Contains(msg, filepath.Base(f)) {
			return f
		}
	}
	return files[0]
}

// SignRepomd writes PATH.asc: a detached armored signature of repomd.xml
// with exactly one signature packet (gpg --detach-sign --armor -u FPR!).
func (s *Signer) SignRepomd(ctx context.Context, path string) error {
	asc := path + ".asc"
	_ = os.Remove(asc)
	if err := s.GPG.DetachSign(ctx, s.FPR, path, asc, true); err != nil {
		if e, ok := api.AsError(err); ok && e.Code == api.PSignLocked {
			e.Code = api.PSignFailed
			e.ExitCode = api.Problems[api.PSignFailed].Exit
			e.Path = path
		}
		return err
	}
	data, err := os.ReadFile(asc)
	if err != nil {
		return err
	}
	if n, err := CountSignatures(data); err != nil || n != 1 {
		return api.Errorf(api.PSignFailed, "%s: want exactly one signature packet, got %d", asc, n).WithPath(asc)
	}
	return nil
}
