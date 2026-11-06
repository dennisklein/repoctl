package sign

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// RPMCheck is the result of rpmkeys --checksig for one RPM.
type RPMCheck struct {
	File string
	// OK is true when all digests and signatures pass with the keys.
	OK bool
	// KeyID is the key id of the signature (8 hex digits, lower case), ""
	// for an unsigned RPM.
	KeyID string
	// NoKey is true when the signature has a key outside the keys
	// (NOKEY).
	NoKey bool
	// Output is the text of rpmkeys -v for the file.
	Output string
}

// Keyring is a temporary rpmdb with only the given keys, for rpmkeys
// --checksig. Close deletes it.
type Keyring struct {
	dir string
}

// NewKeyring imports each key file (armored, many keys per file are fine)
// into a new temporary rpmdb.
func NewKeyring(ctx context.Context, keyfiles ...[]byte) (*Keyring, error) {
	dir, err := os.MkdirTemp("", "repoctl-rpmdb-")
	if err != nil {
		return nil, err
	}
	k := &Keyring{dir: dir}
	for i, data := range keyfiles {
		f := filepath.Join(dir, fmt.Sprintf("key-%d.asc", i))
		if err := os.WriteFile(f, data, 0o600); err != nil {
			k.Close()
			return nil, err
		}
		c := &run.Cmd{Name: "rpmkeys", Args: []string{"--dbpath", filepath.Join(dir, "db"), "--import", f}}
		if err := c.Run(ctx); err != nil {
			k.Close()
			return nil, fmt.Errorf("rpmkeys --import: %w", err)
		}
	}
	return k, nil
}

// Close deletes the temporary rpmdb.
func (k *Keyring) Close() { _ = os.RemoveAll(k.dir) }

// Check runs rpmkeys --checksig -v with the keys of the keyring on each
// file.
func (k *Keyring) Check(ctx context.Context, files []string) ([]RPMCheck, error) {
	var out []RPMCheck
	if err := os.MkdirAll(filepath.Join(k.dir, "db"), 0o700); err != nil {
		return nil, err
	}
	for _, f := range files {
		c := &run.Cmd{Name: "rpmkeys", Args: []string{"--dbpath", filepath.Join(k.dir, "db"), "--checksig", "-v", f}}
		var stdout, stderr bytes.Buffer
		c.Stdout, c.Stderr = &stdout, &stderr
		err := c.Run(ctx)
		r := parseChecksig(f, stdout.String()+stderr.String())
		r.OK = err == nil && r.OK
		if err != nil && run.ExitCode(err) < 0 {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// parseChecksig reads the -v lines of rpmkeys --checksig:
// "Header V4 RSA/SHA512 Signature, key ID bbc98e99: OK".
func parseChecksig(file, text string) RPMCheck {
	r := RPMCheck{File: file, Output: text, OK: true}
	signed := false
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if !strings.Contains(l, ":") || strings.HasSuffix(l, file+":") {
			continue
		}
		i := strings.LastIndex(l, ": ")
		if i < 0 {
			continue
		}
		what, res := l[:i], strings.TrimSpace(l[i+2:])
		if j := strings.Index(what, "key ID "); j >= 0 {
			signed = true
			r.KeyID = strings.ToLower(strings.TrimSpace(what[j+len("key ID "):]))
			if res == "NOKEY" {
				r.NoKey = true
			}
		}
		if res != "OK" {
			r.OK = false
		}
	}
	if !signed {
		r.OK = false
	}
	return r
}

// VerifyRPMs checks each RPM with only the keys of the key files: each RPM
// needs one signature and passes rpmkeys --checksig. It gives
// verify.failed with the failed files.
func VerifyRPMs(ctx context.Context, files []string, keyfiles ...[]byte) ([]RPMCheck, error) {
	kr, err := NewKeyring(ctx, keyfiles...)
	if err != nil {
		return nil, err
	}
	defer kr.Close()
	res, err := kr.Check(ctx, files)
	if err != nil {
		return nil, err
	}
	var bad []string
	for _, r := range res {
		if !r.OK {
			bad = append(bad, filepath.Base(r.File))
		}
	}
	if len(bad) > 0 {
		return res, api.Errorf(api.PVerifyFailed, "%s %s not verify with the keys: %s", plural(len(bad), "RPM"), Agree(len(bad), "does", "do"), strings.Join(firstN(bad, 5), ", ")).
			WithHint("check the signers of the RPMs", "repoctl repo verify")
	}
	return res, nil
}

func firstN(l []string, n int) []string {
	if len(l) <= n {
		return l
	}
	return append(append([]string{}, l[:n]...), fmt.Sprintf("and %d more", len(l)-n))
}

// VerifyRepomd checks repomd.xml.asc of repomd.xml with only the keys of
// the key files (gpgv), and that the file has exactly one signature packet
// (EL10 librepo refuses a second one). It gives the fingerprint of the
// signer.
func VerifyRepomd(ctx context.Context, repomd string, keyfiles ...[]byte) (string, error) {
	asc := repomd + ".asc"
	data, err := os.ReadFile(asc)
	if err != nil {
		return "", api.Errorf(api.PVerifyFailed, "%s: %v", asc, err).WithPath(asc)
	}
	n, err := CountSignatures(data)
	if err != nil {
		return "", api.Errorf(api.PVerifyFailed, "%s: %v", asc, err).WithPath(asc)
	}
	if n != 1 {
		return "", api.Errorf(api.PVerifyFailed, "%s has %d signature packets; the clients need exactly one", asc, n).WithPath(asc)
	}
	dir, err := os.MkdirTemp("", "repoctl-gpgv-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	var ring []byte
	for _, kf := range keyfiles {
		b, err := Dearmor(kf)
		if err != nil {
			return "", err
		}
		ring = append(ring, b...)
	}
	keyring := filepath.Join(dir, "keyring.gpg")
	if err := os.WriteFile(keyring, ring, 0o600); err != nil {
		return "", err
	}
	c := &run.Cmd{Name: "gpgv", Args: []string{"--status-fd", "1", "--keyring", keyring, asc, repomd}, Env: []string{"GNUPGHOME=" + dir}}
	out, err := c.Output(ctx)
	fpr := ""
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) >= 3 && f[1] == "VALIDSIG" {
			fpr = f[len(f)-1]
		}
	}
	if err != nil || fpr == "" {
		return "", api.Errorf(api.PVerifyFailed, "%s does not verify with the keys", asc).WithPath(asc).WithCause(err)
	}
	return fpr, nil
}

// RPMSignature reads the OpenPGP signature of an RPM header (RSAHEADER or
// DSAHEADER, else SIGPGP or SIGGPG) without a keyring. It gives nil for an
// unsigned RPM. The signature names the issuer fingerprint (gpg 2.2 and
// later) and the time when it was made.
func RPMSignature(ctx context.Context, file string) (*Signature, error) {
	const qf = "%|RSAHEADER?{%{RSAHEADER}}:{%|DSAHEADER?{%{DSAHEADER}}:{%|SIGPGP?{%{SIGPGP}}:{%|SIGGPG?{%{SIGGPG}}:{-}|}|}|}|"
	c := &run.Cmd{Name: "rpm", Args: []string{"-qp", "--nosignature", "--nodigest", "--qf", qf, file}}
	out, err := c.Output(ctx)
	if err != nil {
		return nil, err
	}
	h := strings.TrimSpace(string(out))
	if h == "-" || h == "" || h == "(none)" {
		return nil, nil
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("%s: the signature tag is not hex: %w", file, err)
	}
	return ParseSignature(b)
}
