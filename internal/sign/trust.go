package sign

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// The defaults of sign.checks (signing.html#expiry).
const (
	DefaultKeyExpiryWarn  = 180 * 24 * time.Hour
	DefaultKeyMinValidity = 30 * 24 * time.Hour
)

// TrustedKey is one entry of sign.keys with its public key from
// keys/ID.asc.
type TrustedKey struct {
	config.Key
	// Cert is the public key of the file; nil when the file is missing or
	// does not parse.
	Cert *Cert
	// Err says why the file is missing, does not parse, or has another
	// fingerprint than sign.keys.
	Err error
}

// UID gives the user id of the key, or "".
func (k *TrustedKey) UID() string {
	if k.Cert == nil {
		return ""
	}
	return k.Cert.UID
}

// Path gives the key file of the entry relative to the set: file, default
// keys/ID.asc.
func (k *TrustedKey) Path() string {
	if k.File != "" {
		return k.File
	}
	return KeyPath(k.ID)
}

// InKeyfile reports a state whose key the key file holds: active or
// retiring.
func (k *TrustedKey) InKeyfile() bool {
	return k.State == config.KeyActive || k.State == config.KeyRetiring
}

// KeyPath gives keys/ID.asc.
func KeyPath(id string) string { return filepath.Join("keys", id+".asc") }

// Trust is the trust set of a set: sign.keys of repoctl.yaml and the public
// keys in keys/.
type Trust struct {
	Set *config.Set
	// Dir is the set directory.
	Dir string
	// Keys are the entries of sign.keys in file order.
	Keys []*TrustedKey
}

// LoadTrust reads keys/ID.asc of each entry of sign.keys. A key file that
// is missing, does not parse or has another fingerprint gives the entry an
// Err; LoadTrust itself does not fail.
func LoadTrust(set *config.Set, dir string) *Trust {
	t := &Trust{Set: set, Dir: dir}
	for _, k := range set.Sign.Keys {
		tk := &TrustedKey{Key: k}
		tk.Fingerprint = NormalizeFingerprint(k.Fingerprint)
		data, err := os.ReadFile(filepath.Join(dir, tk.Path()))
		switch {
		case err != nil:
			tk.Err = fmt.Errorf("%s: %w", tk.Path(), err)
		default:
			c, err := ParseCert(data)
			switch {
			case err != nil:
				tk.Err = fmt.Errorf("%s: %w", tk.Path(), err)
			case c.Fingerprint != tk.Fingerprint:
				tk.Cert = c
				tk.Err = fmt.Errorf("%s has the fingerprint %s, but sign.keys has %s", tk.Path(), c.Fingerprint, tk.Fingerprint)
			default:
				tk.Cert = c
			}
		}
		t.Keys = append(t.Keys, tk)
	}
	return t
}

// Key gives the entry with the id.
func (t *Trust) Key(id string) *TrustedKey {
	for _, k := range t.Keys {
		if k.ID == id {
			return k
		}
	}
	return nil
}

// ByFingerprint gives the entry with the full fingerprint.
func (t *Trust) ByFingerprint(fpr string) *TrustedKey {
	fpr = NormalizeFingerprint(fpr)
	for _, k := range t.Keys {
		if k.Fingerprint == fpr {
			return k
		}
	}
	return nil
}

// Resolve gives the entry of a value of sign.key: an id or a full
// fingerprint.
func (t *Trust) Resolve(value string) *TrustedKey {
	if k := t.Key(value); k != nil {
		return k
	}
	return t.ByFingerprint(value)
}

// ForIssuer gives the entry whose key or subkey made a signature with the
// issuer fingerprint or key id.
func (t *Trust) ForIssuer(issuer string) *TrustedKey {
	if k := t.ByFingerprint(issuer); k != nil {
		return k
	}
	for _, k := range t.Keys {
		if k.Cert != nil && k.Cert.HasKey(issuer) {
			return k
		}
	}
	return nil
}

// InState gives the entries in the states, in file order.
func (t *Trust) InState(states ...string) []*TrustedKey {
	var out []*TrustedKey
	for _, k := range t.Keys {
		for _, s := range states {
			if k.State == s {
				out = append(out, k)
				break
			}
		}
	}
	return out
}

// KeyfileKeys gives the keys of the key file: the states active and
// retiring, sorted by id.
func (t *Trust) KeyfileKeys() []*TrustedKey {
	out := t.InState(config.KeyActive, config.KeyRetiring)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Keyfile gives the key file of the release package: one armored block per
// key of KeyfileKeys, in id order. A key whose keys/ID.asc does not parse or
// does not match stops it with config.invalid.
func (t *Trust) Keyfile() ([]byte, error) {
	var b bytes.Buffer
	for _, k := range t.KeyfileKeys() {
		if k.Err != nil {
			return nil, api.Errorf(api.PConfigInvalid, "sign.keys %s: %v", k.ID, k.Err).WithPath(k.Path()).
				WithHint("export the public key again", "repoctl sign ls")
		}
		b.Write(Armor(k.Cert.Raw, "PUBLIC KEY BLOCK"))
	}
	return b.Bytes(), nil
}

// KeyfileName gives sign.release.keyfile, else RPM-GPG-KEY-<repo.name>.
func KeyfileName(set *config.Set) string {
	if set.Sign.Release.Keyfile != "" {
		return set.Sign.Release.Keyfile
	}
	return "RPM-GPG-KEY-" + set.Repo.Name
}

// Checks gives sign.checks.key_expiry_warn and key_min_validity with their
// defaults.
func Checks(set *config.Set) (expiryWarn, minValidity time.Duration) {
	expiryWarn, minValidity = DefaultKeyExpiryWarn, DefaultKeyMinValidity
	if d := time.Duration(set.Sign.Checks.KeyExpiryWarn); d > 0 {
		expiryWarn = d
	}
	if d := time.Duration(set.Sign.Checks.KeyMinValidity); d > 0 {
		minValidity = d
	}
	return expiryWarn, minValidity
}

// KeysOfKeyfile gives the fingerprints of the primary keys of a key file.
func KeysOfKeyfile(data []byte) ([]string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	certs, err := ParseCerts(data)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(certs))
	for i, c := range certs {
		out[i] = c.Fingerprint
	}
	return out, nil
}

// KeyfileHolds reports whether a key file holds the fingerprint.
func KeyfileHolds(data []byte, fpr string) bool {
	fprs, err := KeysOfKeyfile(data)
	if err != nil {
		return false
	}
	fpr = NormalizeFingerprint(fpr)
	for _, f := range fprs {
		if f == fpr {
			return true
		}
	}
	return false
}
