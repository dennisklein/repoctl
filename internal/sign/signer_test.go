package sign

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/testkit"
)

func needTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, x := range tools {
		if _, err := exec.LookPath(x); err != nil {
			t.Skip(x + " is not installed")
		}
	}
}

func TestSignAndVerifyRPM(t *testing.T) {
	needTools(t, "rpmsign", "rpmkeys", "gpg")
	ctx := context.Background()
	k := testkit.GenKey(t, "d.klein")
	other := testkit.GenKey(t, "a.admin")
	s := &Signer{GPG: NewGPG(gnupgHome(t, k)), ID: "d.klein", FPR: k.Fingerprint}
	rpm := buildRPM(t, "ucx", "1.22.1", "1", map[string]string{"/usr/share/ucx/README": "ucx\n"})

	if sig, err := RPMSignature(ctx, rpm); err != nil || sig != nil {
		t.Fatalf("an unsigned RPM: %v %v", sig, err)
	}
	if err := s.SignRPMs(ctx, []string{rpm}); err != nil {
		t.Fatal(err)
	}
	sig, err := RPMSignature(ctx, rpm)
	if err != nil || sig == nil {
		t.Fatalf("signature: %v", err)
	}
	if sig.IssuerFingerprint != k.Fingerprint {
		t.Errorf("issuer %s, want %s", sig.IssuerFingerprint, k.Fingerprint)
	}
	if sig.Created.IsZero() {
		t.Error("no creation time")
	}
	res, err := VerifyRPMs(ctx, []string{rpm}, k.Public)
	if err != nil {
		t.Fatalf("verify with the key: %v", err)
	}
	if !res[0].OK || res[0].KeyID != k.Fingerprint[32:40] && res[0].KeyID != toLower(k.Fingerprint[32:40]) {
		t.Errorf("check %+v", res[0])
	}
	// Only the keys of the clients count: another key gives NOKEY.
	res, err = VerifyRPMs(ctx, []string{rpm}, other.Public)
	if err == nil || !res[0].NoKey {
		t.Errorf("verify with another key: %v %+v", err, res)
	}
	if e, ok := api.AsError(err); !ok || e.Code != api.PVerifyFailed {
		t.Errorf("error %v", err)
	}
	// A key file with both keys passes (F14).
	if _, err := VerifyRPMs(ctx, []string{rpm}, append(append([]byte{}, other.Public...), k.Public...)); err != nil {
		t.Errorf("verify with a key file of two keys: %v", err)
	}
	// --resign replaces the signature: one signature of the new signer.
	s2 := &Signer{GPG: NewGPG(gnupgHome(t, other)), ID: "a.admin", FPR: other.Fingerprint}
	if err := s2.SignRPMs(ctx, []string{rpm}); err != nil {
		t.Fatal(err)
	}
	if sig, _ := RPMSignature(ctx, rpm); sig == nil || sig.IssuerFingerprint != other.Fingerprint {
		t.Errorf("after resign: %+v", sig)
	}
	if _, err := VerifyRPMs(ctx, []string{rpm}, k.Public); err == nil {
		t.Error("the old signature is still there")
	}
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func TestSignRepomdAndProbe(t *testing.T) {
	needTools(t, "gpg", "gpgv")
	ctx := context.Background()
	k := testkit.GenKey(t, "d.klein")
	other := testkit.GenKey(t, "a.admin")
	home := gnupgHome(t, k)
	s := &Signer{GPG: NewGPG(home), ID: "d.klein", FPR: k.Fingerprint}
	if err := s.Probe(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if ok, err := s.GPG.HasSecret(ctx, k.Fingerprint); err != nil || !ok {
		t.Errorf("HasSecret %v %v", ok, err)
	}
	if ok, _ := s.GPG.HasSecret(ctx, other.Fingerprint); ok {
		t.Error("HasSecret of a key that is not there")
	}
	dir := t.TempDir()
	repomd := filepath.Join(dir, "repomd.xml")
	if err := os.WriteFile(repomd, []byte("<repomd/>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.SignRepomd(ctx, repomd); err != nil {
		t.Fatal(err)
	}
	fpr, err := VerifyRepomd(ctx, repomd, k.Public)
	if err != nil || fpr != k.Fingerprint {
		t.Fatalf("verify: %s %v", fpr, err)
	}
	if _, err := VerifyRepomd(ctx, repomd, other.Public); err == nil {
		t.Error("repomd verifies with another key")
	}
	// A second signature packet is refused (F9).
	data, _ := os.ReadFile(repomd + ".asc")
	bin, _ := Dearmor(data)
	if err := os.WriteFile(repomd+".asc", Armor(append(bin, bin...), "SIGNATURE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRepomd(ctx, repomd, k.Public); err == nil {
		t.Error("two signature packets pass")
	}
	// A key without its secret key fails the probe with sign.no-key.
	s3 := &Signer{GPG: NewGPG(home), ID: "a.admin", FPR: other.Fingerprint}
	err = s3.Probe(ctx)
	if e, ok := api.AsError(err); !ok || e.Code != api.PSignNoKey {
		t.Errorf("probe without the secret key: %v", err)
	}
	if err := s3.RequireSecret(ctx); err == nil {
		t.Error("RequireSecret passes without the key")
	}
}

func TestExtractKeyfileFromRPM(t *testing.T) {
	needTools(t, "rpm2cpio")
	ctx := context.Background()
	k := testkit.GenKey(t, "d.klein")
	rpm := buildRPM(t, "slurm-release", "26.05", "2.el9", map[string]string{
		"/etc/pki/rpm-gpg/RPM-GPG-KEY-slurm": string(k.Public),
		"/etc/yum.repos.d/slurm.repo":        "[slurm]\n",
	})
	data, err := ExtractFile(ctx, rpm, "/etc/pki/rpm-gpg/RPM-GPG-KEY-slurm")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(k.Public) {
		t.Error("the extracted key file differs")
	}
	if _, err := ExtractFile(ctx, rpm, "/etc/nothing"); err == nil {
		t.Error("a missing file passes")
	}
}

func TestCompareNVR(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"slurm-release-26.05-2.el9", "slurm-release-26.05-10.el9", -1},
		{"slurm-release-26.05-3.el9", "slurm-release-26.05-3.el9", 0},
		{"a-1.0-1", "a-1.0~rc1-1", 1},
		{"a-1.0^git1-1", "a-1.0-1", 1},
		{"a-1.0a-1", "a-1.0-1", 1},
		{"a-1.01-1", "a-1.1-1", 0},
	} {
		if got := CompareNVR(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareNVR(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
