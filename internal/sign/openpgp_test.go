package sign

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/testkit"
)

func TestParseCertRSA(t *testing.T) {
	k := testkit.GenKey(t, "d.klein")
	c, err := ParseCert(k.Public)
	if err != nil {
		t.Fatal(err)
	}
	if c.Fingerprint != k.Fingerprint {
		t.Errorf("fingerprint %s, want %s", c.Fingerprint, k.Fingerprint)
	}
	if c.KeyID != k.Fingerprint[24:] {
		t.Errorf("key id %s", c.KeyID)
	}
	if c.Version != 4 || c.Algo != AlgoRSA || c.Bits != 3072 {
		t.Errorf("version %d algo %d bits %d", c.Version, c.Algo, c.Bits)
	}
	if c.AlgorithmName() != "rsa3072" {
		t.Errorf("algorithm %s", c.AlgorithmName())
	}
	if c.UID != "Dennis Klein <d.klein@example.org>" {
		t.Errorf("uid %q", c.UID)
	}
	if !c.Expires.IsZero() {
		t.Errorf("expires %v, want never", c.Expires)
	}
	if r := c.CheckFormat(); r != "" {
		t.Errorf("gate 3: %s", r)
	}
	if !c.HasKey(k.Fingerprint) || !c.HasKey(strings.ToLower(k.Fingerprint[24:])) || !c.HasKey(k.Fingerprint[32:]) {
		t.Error("HasKey misses the key")
	}
	if c.RPMKeyID() != strings.ToLower(k.Fingerprint[32:]) {
		t.Errorf("rpm key id %s", c.RPMKeyID())
	}
}

func TestCheckFormat(t *testing.T) {
	for _, tc := range []struct {
		spec testkit.KeySpec
		bad  string
	}{
		{testkit.KeySpec{ID: "ed", Algo: "ed25519"}, ""},
		{testkit.KeySpec{ID: "small", Algo: "rsa1024"}, "rsa1024"},
		{testkit.KeySpec{ID: "sha1", Algo: "rsa2048", SHA1: true}, "SHA-1"},
	} {
		k := testkit.GenKeySpec(t, tc.spec)
		c, err := ParseCert(k.Public)
		if err != nil {
			t.Fatal(err)
		}
		r := c.CheckFormat()
		if tc.bad == "" && r != "" || tc.bad != "" && !strings.Contains(r, tc.bad) {
			t.Errorf("%s: CheckFormat %q, want %q", tc.spec.ID, r, tc.bad)
		}
	}
}

func TestExpiry(t *testing.T) {
	k := testkit.GenKeySpec(t, testkit.KeySpec{ID: "soon", Algo: "ed25519", Expire: "10d"})
	c, err := ParseCert(k.Public)
	if err != nil {
		t.Fatal(err)
	}
	if c.Expires.IsZero() {
		t.Fatal("no expiry")
	}
	if d := c.Expires.Sub(c.Created); d < 9*24*time.Hour || d > 11*24*time.Hour {
		t.Errorf("expiry %v after creation", d)
	}
	if !c.ExpiresIn(time.Now(), 30*24*time.Hour) || c.ExpiresIn(time.Now(), time.Hour) {
		t.Error("ExpiresIn")
	}
}

func TestArmorRoundTrip(t *testing.T) {
	k := testkit.GenKey(t, "d.klein")
	bin, err := Dearmor(k.Public)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Dearmor(Armor(bin, "PUBLIC KEY BLOCK"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bin, again) {
		t.Error("armor round trip changed the data")
	}
	// Two blocks in one file give both keys.
	a := testkit.GenKey(t, "a.admin")
	certs, err := ParseCerts(append(append([]byte{}, k.Public...), a.Public...))
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 2 || certs[1].Fingerprint != a.Fingerprint {
		t.Fatalf("two blocks: %d certs", len(certs))
	}
	// One block with both keys.
	binA, _ := Dearmor(a.Public)
	certs, err = ParseCerts(Armor(append(bin, binA...), "PUBLIC KEY BLOCK"))
	if err != nil || len(certs) != 2 {
		t.Fatalf("one block: %v, %d", err, len(certs))
	}
	if !bytes.Equal(certs[0].Raw, bin) {
		t.Error("Raw of the first cert differs from its packets")
	}
	bad := bytes.Replace(k.Public, []byte("\n="), []byte("\n=AAAA\n#"), 1)
	if _, err := Dearmor(bad); err == nil {
		t.Error("a wrong checksum passes")
	}
	if _, err := ParseCerts(k.Secret); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Errorf("a secret key: %v", err)
	}
}

func TestFingerprintForms(t *testing.T) {
	f := "C2F9771305ABE4D618C39F2B70DEA5C1E93F52A7"
	if g := GroupFingerprint(f); g != "C2F9 7713 05AB E4D6 18C3  9F2B 70DE A5C1 E93F 52A7" {
		t.Errorf("group %q", g)
	}
	if a := AbbrevFingerprint(f); a != "C2F9…52A7" {
		t.Errorf("abbrev %q", a)
	}
	if n := NormalizeFingerprint("0xc2f9 7713 05ab e4d6 18c3  9f2b 70de a5c1 e93f 52a7!"); n != f || !IsFingerprint(n) {
		t.Errorf("normalize %q", n)
	}
}
