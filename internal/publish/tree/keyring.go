package tree

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// Key is one public key of a keyring.
type Key struct {
	// ID is the key id in sign.keys, if known.
	ID string
	// Fingerprint is the full fingerprint, upper case.
	Fingerprint string
	// State is the key state in sign.keys, if known.
	State string
	// File is the armored key file.
	File string
}

// Keyring is a temporary GnuPG home and rpmdb with a set of public keys.
// It verifies detached signatures and RPM signatures with only these keys.
type Keyring struct {
	dir  string
	Keys []Key
}

// KeyFingerprints gives the fingerprints of the keys in an armored key
// file.
func KeyFingerprints(ctx context.Context, file string) ([]string, error) {
	home, err := os.MkdirTemp("", "rk-")
	if err != nil {
		return nil, err
	}
	defer cleanupHome(home)
	out, err := (&run.Cmd{Name: "gpg", Args: []string{"--homedir", home, "--batch", "--with-colons", "--import-options", "show-only", "--import", file}}).Output(ctx)
	if err != nil {
		return nil, fmt.Errorf("gpg: the key file %s cannot be read: %w", file, err)
	}
	var fprs []string
	pub := false
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Split(l, ":")
		switch f[0] {
		case "pub":
			pub = true
		case "sub":
			pub = false
		case "fpr":
			if pub && len(f) > 9 {
				fprs = append(fprs, strings.ToUpper(f[9]))
				pub = false
			}
		}
	}
	return fprs, nil
}

func cleanupHome(home string) {
	_ = exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run()
	_ = os.RemoveAll(home)
}

// NewKeyring makes a keyring of the keys. Each key needs File; its
// fingerprint comes from the file when Fingerprint is empty.
func NewKeyring(ctx context.Context, keys []Key) (*Keyring, error) {
	dir, err := os.MkdirTemp("", "rk-")
	if err != nil {
		return nil, err
	}
	k := &Keyring{dir: dir}
	if err := os.MkdirAll(k.gpgHome(), 0o700); err != nil {
		k.Close()
		return nil, err
	}
	if err := os.MkdirAll(k.rpmdb(), 0o755); err != nil {
		k.Close()
		return nil, err
	}
	for _, key := range keys {
		fprs, err := KeyFingerprints(ctx, key.File)
		if err != nil {
			k.Close()
			return nil, err
		}
		if len(fprs) == 0 {
			k.Close()
			return nil, fmt.Errorf("the key file %s holds no public key", key.File)
		}
		if err := (&run.Cmd{Name: "gpg", Args: []string{"--homedir", k.gpgHome(), "--batch", "--quiet", "--import", key.File}}).Run(ctx); err != nil {
			k.Close()
			return nil, err
		}
		if err := (&run.Cmd{Name: "rpmkeys", Args: []string{"--dbpath", k.rpmdb(), "--import", key.File}}).Run(ctx); err != nil {
			k.Close()
			return nil, err
		}
		for _, f := range fprs {
			kk := key
			kk.Fingerprint = f
			if key.Fingerprint != "" && !strings.EqualFold(key.Fingerprint, f) {
				// A key file with several keys: keep the id only for the
				// named one.
				kk.ID, kk.State = "", ""
			}
			k.Keys = append(k.Keys, kk)
		}
	}
	return k, nil
}

func (k *Keyring) gpgHome() string { return filepath.Join(k.dir, "g") }
func (k *Keyring) rpmdb() string   { return filepath.Join(k.dir, "r") }

// Close removes the keyring.
func (k *Keyring) Close() {
	if k != nil && k.dir != "" {
		cleanupHome(k.gpgHome())
		_ = os.RemoveAll(k.dir)
	}
}

// ByKeyID gives the key whose fingerprint ends with the key id.
func (k *Keyring) ByKeyID(id string) (Key, bool) {
	id = strings.ToUpper(id)
	if id == "" {
		return Key{}, false
	}
	for _, key := range k.Keys {
		if strings.HasSuffix(key.Fingerprint, id) {
			return key, true
		}
	}
	return Key{}, false
}

// ByFingerprint gives the key of a fingerprint.
func (k *Keyring) ByFingerprint(fpr string) (Key, bool) {
	for _, key := range k.Keys {
		if strings.EqualFold(key.Fingerprint, fpr) {
			return key, true
		}
	}
	return Key{}, false
}

// VerifyDetached checks a detached signature of data: it must hold
// exactly one signature, made by a key of the keyring. It gives the
// fingerprint of the signing key.
func (k *Keyring) VerifyDetached(ctx context.Context, sig, data string) (string, error) {
	var status bytes.Buffer
	c := &run.Cmd{Name: "gpg", Args: []string{"--homedir", k.gpgHome(), "--batch", "--status-fd", "1", "--verify", sig, data}, Stdout: &status}
	err := c.Run(ctx)
	sigs, valid := 0, ""
	for _, l := range strings.Split(status.String(), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || f[0] != "[GNUPG:]" {
			continue
		}
		switch f[1] {
		case "NEWSIG":
			sigs++
		case "VALIDSIG":
			if len(f) > 2 {
				valid = strings.ToUpper(f[len(f)-1])
			}
		}
	}
	if sigs != 1 {
		return "", api.Errorf(api.PVerifyFailed, "%s holds %d signatures; it must hold exactly one", sig, sigs)
	}
	if err != nil || valid == "" {
		return "", api.Errorf(api.PVerifyFailed, "%s does not verify with the keys %s", sig, k.fingerprints())
	}
	return valid, nil
}

func (k *Keyring) fingerprints() string {
	var out []string
	for _, key := range k.Keys {
		name := key.ID
		if name == "" {
			name = api.ShortFingerprint(key.Fingerprint)
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return "(none)"
	}
	return strings.Join(out, ", ")
}

// CheckRPMs runs rpmkeys --checksig on RPM files with the keys of the
// keyring. It gives the files that do not verify, with the reason.
func (k *Keyring) CheckRPMs(ctx context.Context, files []string) (map[string]string, error) {
	bad := map[string]string{}
	for start := 0; start < len(files); start += 200 {
		end := min(start+200, len(files))
		var out bytes.Buffer
		c := &run.Cmd{Name: "rpmkeys", Args: append([]string{"--dbpath", k.rpmdb(), "--checksig"}, files[start:end]...), Stdout: &out}
		err := c.Run(ctx)
		if err != nil && run.ExitCode(err) < 0 {
			return nil, err
		}
		got := map[string]string{}
		for _, l := range strings.Split(out.String(), "\n") {
			name, res, ok := strings.Cut(l, ": ")
			if ok {
				got[name] = res
			}
		}
		for _, f := range files[start:end] {
			res, ok := got[f]
			switch {
			case !ok:
				bad[f] = "rpmkeys gave no result"
			case !strings.Contains(res, "signatures OK"):
				if strings.Contains(strings.ToUpper(res), "NOT OK") {
					bad[f] = "the signature does not verify: " + res
				} else {
					bad[f] = "the RPM has no signature: " + res
				}
			}
		}
	}
	return bad, nil
}
