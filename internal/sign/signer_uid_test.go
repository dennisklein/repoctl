package sign

import (
	"context"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// sign.key can be a user ID of gpg, for example the mail address of
// %_gpg_name, when it matches exactly one secret key.
func TestNewSignerUserID(t *testing.T) {
	ctx := context.Background()
	k := testkit.GenKey(t, "d.klein")
	other := testkit.GenKey(t, "a.admin")
	home := gnupgHome(t, k, other)
	signer := func(value string) (*Signer, error) {
		t.Helper()
		uf := &config.UserFile{Top: map[string]any{"sign.key": value, "sign.gpg.homedir": home}}
		eff, err := config.Resolve(uf, "", func(string) string { return "" }, nil)
		if err != nil {
			t.Fatal(err)
		}
		return NewSigner(ctx, nil, eff)
	}

	s, err := signer("d.klein@example.org")
	if err != nil || s.FPR != k.Fingerprint {
		t.Fatalf("mail address: %v %+v, want %s", err, s, k.Fingerprint)
	}
	s, err = signer(k.Fingerprint)
	if err != nil || s.FPR != k.Fingerprint {
		t.Fatalf("fingerprint: %v %+v", err, s)
	}

	_, err = signer("nobody@example.org")
	e, ok := api.AsError(err)
	if !ok || e.Code != api.PSignNotActive || len(e.Hints) == 0 || !strings.Contains(e.Hints[0].Run, "gpg --list-secret-keys") {
		t.Fatalf("no match: %v", err)
	}
	_, err = signer("example.org")
	if e, ok := api.AsError(err); !ok || !strings.Contains(e.Message, "matches 2 secret keys") {
		t.Fatalf("two matches: %v", err)
	}
}
