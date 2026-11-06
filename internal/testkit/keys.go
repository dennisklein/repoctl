package testkit

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestKey is a generated OpenPGP signing key of the tests. It has no
// passphrase, so gpg-agent never starts a pinentry.
type TestKey struct {
	// ID is the id of the key in sign.keys, for example d.klein.
	ID          string
	UID         string
	Fingerprint string
	// Public is the armored public key, Secret the armored secret key.
	Public []byte
	Secret []byte
}

// KeySpec says how to make a test key.
type KeySpec struct {
	ID string
	// Algo is the algorithm of gpg --quick-gen-key: rsa3072 (default),
	// ed25519, rsa1024.
	Algo string
	// Expire is the expiry of gpg --quick-gen-key: never (default), 10d,
	// 2y.
	Expire string
	// SHA1 makes the self-signature with SHA-1 (gate 3 refuses it).
	SHA1 bool
}

func (k KeySpec) key() string {
	return fmt.Sprintf("%s|%s|%s|%t", k.ID, k.Algo, k.Expire, k.SHA1)
}

var (
	keysMu sync.Mutex
	keys   = map[string]*TestKey{}
)

// KeyUID gives the user id of the test key of an id.
func KeyUID(id string) string {
	switch id {
	case "d.klein":
		return "Dennis Klein <d.klein@example.org>"
	case "a.admin":
		return "A. Admin <a.admin@example.org>"
	case "hpc-repo":
		return "GSI HPC repo <hpc-repo@example.org>"
	}
	return id + " <" + id + "@example.org>"
}

// GenKey gives the RSA 3072 test key of an id without an expiry. It makes
// each key once per test process.
func GenKey(t testing.TB, id string) *TestKey {
	t.Helper()
	return GenKeySpec(t, KeySpec{ID: id})
}

// GenKeySpec gives the test key of a spec, made once per test process.
// It skips the test when gpg is missing.
func GenKeySpec(t testing.TB, spec KeySpec) *TestKey {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg is not installed")
	}
	if spec.Algo == "" {
		spec.Algo = "rsa3072"
	}
	if spec.Expire == "" {
		spec.Expire = "never"
	}
	keysMu.Lock()
	defer keysMu.Unlock()
	if k, ok := keys[spec.key()]; ok {
		return k
	}
	k, err := genKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	keys[spec.key()] = k
	return k
}

func genKey(spec KeySpec) (*TestKey, error) {
	// A short path: the socket of gpg-agent breaks beyond 107 bytes.
	home, err := os.MkdirTemp("", "gk-")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run()
		_ = os.RemoveAll(home)
	}()
	gpg := func(stdin []byte, args ...string) ([]byte, error) {
		cmd := exec.Command("gpg", append([]string{"--homedir", home, "--batch", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)...)
		cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("gpg %s: %w\n%s", strings.Join(args, " "), err, errb.String())
		}
		return out.Bytes(), nil
	}
	uid := KeyUID(spec.ID)
	args := []string{}
	if spec.SHA1 {
		args = append(args, "--cert-digest-algo", "SHA1")
	}
	args = append(args, "--quick-gen-key", uid, spec.Algo, "sign", spec.Expire)
	if _, err := gpg(nil, args...); err != nil {
		return nil, err
	}
	out, err := gpg(nil, "--with-colons", "--list-keys", uid)
	if err != nil {
		return nil, err
	}
	fpr := ""
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Split(l, ":")
		if len(f) > 9 && f[0] == "fpr" {
			fpr = f[9]
			break
		}
	}
	if fpr == "" {
		return nil, fmt.Errorf("gpg made no key for %s", uid)
	}
	pub, err := gpg(nil, "--armor", "--export", fpr)
	if err != nil {
		return nil, err
	}
	sec, err := gpg(nil, "--armor", "--export-secret-keys", fpr)
	if err != nil {
		return nil, err
	}
	return &TestKey{ID: spec.ID, UID: uid, Fingerprint: fpr, Public: pub, Secret: sec}, nil
}

// ImportKeys imports the secret keys into the GnuPG home directory.
func ImportKeys(t testing.TB, homedir string, ks ...*TestKey) {
	t.Helper()
	if err := os.MkdirAll(homedir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, k := range ks {
		cmd := exec.Command("gpg", "--homedir", homedir, "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--import")
		cmd.Env = append(os.Environ(), "GNUPGHOME="+homedir)
		cmd.Stdin = bytes.NewReader(k.Secret)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("gpg --import %s: %v\n%s", k.ID, err, out)
		}
	}
}

// GPGKeys makes the test keys of the ids, imports their secret keys into
// the GnuPG home of the story ($HOME/.gnupg) and gives them by id.
func GPGKeys(t testing.TB, s *Story, ids ...string) map[string]*TestKey {
	t.Helper()
	out := map[string]*TestKey{}
	var list []*TestKey
	for _, id := range ids {
		k := GenKey(t, id)
		out[id] = k
		list = append(list, k)
	}
	ImportKeys(t, s.GnuPGHome(), list...)
	return out
}

// GnuPGHome gives the GnuPG home directory of the story.
func (s *Story) GnuPGHome() string { return filepath.Join(s.Home, ".gnupg") }
