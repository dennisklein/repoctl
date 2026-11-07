package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
)

const (
	fprA = "7D3A1F0C9B2E4D6A8F1C3E5B7A9D0F2C4E6B8A13"
	fprB = "1111222233334444555566667777888899990000"
	fprC = "AAAABBBBCCCCDDDDEEEEFFFF0000111122223333"
)

func TestParseToolKeys(t *testing.T) {
	keys, err := ParseToolKeys(" " + strings.ToLower(fprA) + ":current:2028-08-01, " + fprB + ":retired:2027-01-01,")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].Fingerprint != fprA || keys[0].State != ToolKeyCurrent || keys[1].State != ToolKeyRetired {
		t.Fatalf("keys = %+v", keys)
	}
	if got := keys[0].ID(); got != "7A9D0F2C4E6B8A13" {
		t.Errorf("ID = %s", got)
	}
	for _, bad := range []string{fprA, fprA + ":gone:2028-01-01", "XYZ:current:2028-01-01", fprA + ":current:soon"} {
		if _, err := ParseToolKeys(bad); err == nil {
			t.Errorf("ParseToolKeys(%q) gives no error", bad)
		}
	}
}

func TestCheckToolRepoKey(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.repo")
	if err := os.WriteFile(good, []byte("[repoctl]\nbaseurl=https://example.org/rpm/$releasever/$basearch/\ngpgcheck=1\nrepo_gpgcheck=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	off := filepath.Join(dir, "off.repo")
	if err := os.WriteFile(off, []byte("[repoctl]\ngpgcheck=1\nrepo_gpgcheck=0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	env := &Env{Now: func() time.Time { return now }}
	keyList := fprA + ":current:2028-08-01," + fprB + ":retired:2027-01-01," + fprC + ":revoked:2027-06-01"
	for _, tc := range []struct {
		name   string
		keys   string
		ids    []string
		noRPM  bool
		repo   string
		status api.CheckStatus
		want   string
	}{
		{"no keys in the build", "", nil, false, good, api.CheckStatusOk, "lists no tool repository key"},
		{"no rpm", keyList, nil, true, good, api.CheckStatusOk, "rpm is not on this host"},
		{"pass, short id", keyList, []string{"4E6B8A13"}, false, good, api.CheckStatusOk, "has the tool repository key 7A9D0F2C4E6B8A13"},
		{"pass, fingerprint", keyList, []string{fprA}, false, good, api.CheckStatusOk, "repoctl.repo checks the signatures"},
		{"current missing", keyList, []string{"12345678"}, false, good, api.CheckStatusWarning, "does not have the tool repository key " + fprA},
		{"expires soon", fprA + ":current:2027-03-01", []string{"4E6B8A13"}, false, good, api.CheckStatusWarning, "expires on 2027-03-01"},
		{"retired present", keyList, []string{"4E6B8A13", "99990000"}, false, good, api.CheckStatusWarning, "still has the retired"},
		{"revoked present", keyList, []string{"4E6B8A13", "22223333"}, false, good, api.CheckStatusFailed, "revoked tool repository key " + fprC},
		{"repo_gpgcheck off", keyList, []string{"4E6B8A13"}, false, off, api.CheckStatusWarning, "does not set repo_gpgcheck to 1"},
		{"repo file missing", keyList, []string{"4E6B8A13"}, false, filepath.Join(dir, "none.repo"), api.CheckStatusWarning, "none.repo is missing"},
		{"bad list", "nonsense", nil, false, good, api.CheckStatusFailed, "is not valid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldKeys, oldFile, oldIDs := ToolKeys, toolRepoFile, rpmKeyIDs
			t.Cleanup(func() { ToolKeys, toolRepoFile, rpmKeyIDs = oldKeys, oldFile, oldIDs })
			ToolKeys, toolRepoFile = tc.keys, tc.repo
			rpmKeyIDs = func(context.Context) ([]string, bool, error) { return tc.ids, !tc.noRPM, nil }
			got := checkToolRepoKey(context.Background(), env)
			if len(got) != 1 {
				t.Fatalf("got %d checks", len(got))
			}
			c := got[0]
			text := c.Text + c.Detail
			if c.Name != "tool.repo-key" || c.Status != tc.status || !strings.Contains(text, tc.want) {
				t.Errorf("got %s %s %q (fix %q), want %s with %q", c.Name, c.Status, text, c.Fix, tc.status, tc.want)
			}
			if c.Status != api.CheckStatusOk && c.Fix == "" {
				t.Errorf("a %s has no fix", c.Status)
			}
		})
	}
}
