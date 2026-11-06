package mock

import (
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/config"
)

func TestWrapperConfig(t *testing.T) {
	m := New(config.Paths{MockConfigDir: "/etc/mock"}, Options{NoSync: true})
	got := m.WrapperConfig(Target{ID: "el9", Config: "rocky+epel-9-x86_64", Plugins: []string{"rpmautospec"}})
	for _, want := range []string{
		"include('/etc/mock/rocky+epel-9-x86_64.cfg')",
		"config_opts['plugin_conf']['rpmautospec_enable'] = True",
		"config_opts['plugin_conf']['buildroot_lock_enable'] = True",
		"config_opts['plugin_conf']['chroot_scan_opts']['only_failed'] = True",
		"config_opts['nosync'] = True",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapper config misses %q:\n%s", want, got)
		}
	}
}

func TestHashDebugConfig(t *testing.T) {
	a := HashDebugConfig([]byte("config_opts['root'] = 'x'\nconfig_opts['mock_run_uuid'] = '1'\nconfig_opts['verbose'] = 1\n"))
	b := HashDebugConfig([]byte("config_opts['root'] = 'x'\nconfig_opts['mock_run_uuid'] = '2'\nconfig_opts['verbose'] = 2\n"))
	c := HashDebugConfig([]byte("config_opts['root'] = 'y'\n"))
	if a != b || a == c {
		t.Fatalf("hash: %s %s %s", a, b, c)
	}
}

func TestParseBuildroot(t *testing.T) {
	out := "noise\nPKG bash-0:5.1-1.el9.x86_64\tabc\tRSA/SHA256, Mon 05 Jan 2026, Key ID 702d426d350d275d\nPKG gpg-pubkey-0:1-2.(none)\tdef\t(none)\n"
	got := ParseBuildroot([]byte(out))
	if len(got) != 2 || got[0].Signer != "702d426d350d275d" || got[1].Signer != "" || got[0].HeaderSHA256 != "abc" {
		t.Fatalf("%+v", got)
	}
}
