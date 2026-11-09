package doctor

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/tools/run"
)

// ToolKeys lists the tool repository keys (tool.html#tool-key), with
// entries separated by commas: FINGERPRINT:STATE:EXPIRES. STATE is current,
// retired or revoked; EXPIRES is the expiry date YYYY-MM-DD. A change of the
// keys changes this list and packaging/RPM-GPG-KEY-repoctl in the same
// commit. A build can set it with
// -ldflags "-X github.com/dennisklein/repoctl/internal/doctor.ToolKeys=…";
// the RPM test of CI does so with its test key.
var ToolKeys = "F4EBF057DF65EF8D26B1E44128A5286144DD6792:current:2028-11-08"

// The states of a tool repository key.
const (
	ToolKeyCurrent = "current"
	ToolKeyRetired = "retired"
	ToolKeyRevoked = "revoked"
)

// ToolKey is one entry of ToolKeys.
type ToolKey struct {
	Fingerprint string
	State       string
	Expires     time.Time
}

// ID gives the long key id: the last 16 hex digits of the fingerprint.
func (k ToolKey) ID() string {
	if len(k.Fingerprint) < 16 {
		return k.Fingerprint
	}
	return k.Fingerprint[len(k.Fingerprint)-16:]
}

// ParseToolKeys parses a list in the form of ToolKeys.
func ParseToolKeys(s string) ([]ToolKey, error) {
	var out []ToolKey
	for _, e := range strings.Split(s, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		f := strings.Split(e, ":")
		if len(f) != 3 {
			return nil, fmt.Errorf("tool key %q: want FINGERPRINT:STATE:EXPIRES", e)
		}
		fpr := strings.ToUpper(f[0])
		if len(fpr) != 40 && len(fpr) != 64 || strings.Trim(fpr, "0123456789ABCDEF") != "" {
			return nil, fmt.Errorf("tool key %q: the fingerprint is not 40 or 64 hex digits", e)
		}
		switch f[1] {
		case ToolKeyCurrent, ToolKeyRetired, ToolKeyRevoked:
		default:
			return nil, fmt.Errorf("tool key %q: the state is not current, retired or revoked", e)
		}
		exp, err := time.Parse(time.DateOnly, f[2])
		if err != nil {
			return nil, fmt.Errorf("tool key %q: the expiry is not YYYY-MM-DD", e)
		}
		out = append(out, ToolKey{Fingerprint: fpr, State: f[1], Expires: exp})
	}
	return out, nil
}

// The inputs of the check tool.repo-key; tests replace them.
var (
	toolRepoFile = "/etc/yum.repos.d/repoctl.repo"
	// rpmKeyIDs gives the versions of the gpg-pubkey packages of the rpm
	// keyring: the short key id with rpm 4, the fingerprint with rpm 6.
	// ok is false when rpm is not on this host.
	rpmKeyIDs = func(ctx context.Context) (ids []string, ok bool, err error) {
		if _, found := run.LookPath("rpm"); !found {
			return nil, false, nil
		}
		out, err := (&run.Cmd{Name: "rpm", Args: []string{"-q", "gpg-pubkey", "--qf", "%{VERSION}\n"}}).Output(ctx)
		if err != nil {
			// rpm -q exits 1 when no key is installed.
			if strings.Contains(string(out), "not installed") || len(strings.TrimSpace(string(out))) == 0 {
				return nil, true, nil
			}
			return nil, true, err
		}
		for _, l := range strings.Fields(string(out)) {
			ids = append(ids, strings.ToUpper(l))
		}
		return ids, true, nil
	}
)

// toolKeyExpiryWarn is the time before the expiry of a current key when
// doctor warns (tool.html#tool-rotation).
const toolKeyExpiryWarn = 180 * 24 * time.Hour

// inKeyring tells whether the rpm keyring has the key: a gpg-pubkey
// version is a suffix of the fingerprint (short id, long id or the full
// fingerprint).
func inKeyring(k ToolKey, ids []string) (string, bool) {
	for _, id := range ids {
		if len(id) >= 8 && strings.HasSuffix(k.Fingerprint, id) {
			return id, true
		}
	}
	return "", false
}

// checkToolRepoKey is the check tool.repo-key (tool.html#tool-repo-key).
func checkToolRepoKey(ctx context.Context, env *Env) []Check {
	const name = "tool.repo-key"
	keys, err := ParseToolKeys(ToolKeys)
	if err != nil {
		return []Check{Fail(name, "the list of tool repository keys of this build is not valid: "+err.Error(), "report it: this build of repoctl is broken")}
	}
	if len(keys) == 0 {
		return []Check{Ok(name, name+": not checked; this build lists no tool repository key")}
	}
	ids, ok, err := rpmKeyIDs(ctx)
	if !ok {
		return []Check{Ok(name, name+": not checked; rpm is not on this host")}
	}
	if err != nil {
		return []Check{Warn(name, "rpm cannot list the keys of its keyring: "+err.Error(), "run: rpm -q gpg-pubkey")}
	}
	now := time.Now()
	if env != nil && env.Now != nil {
		now = env.Now()
	}
	var fails, warns []string
	var failFix, warnFix string
	var have []string
	for _, k := range keys {
		id, in := inKeyring(k, ids)
		switch {
		case k.State == ToolKeyRevoked && in:
			fails = append(fails, "the rpm keyring has the revoked tool repository key "+k.Fingerprint)
			if failFix == "" {
				failFix = "erase the key, then do the first install again (tool.html#tool-compromise). run: sudo rpmkeys --erase " + strings.ToLower(id)
			}
		case k.State == ToolKeyRetired && in:
			warns = append(warns, "the rpm keyring still has the retired tool repository key "+k.Fingerprint)
			if warnFix == "" {
				warnFix = "erase the retired key. run: sudo rpmkeys --erase " + strings.ToLower(id)
			}
		case k.State == ToolKeyCurrent && !in:
			warns = append(warns, "the rpm keyring does not have the tool repository key "+k.Fingerprint)
			if warnFix == "" {
				warnFix = "install the newest release package and import its keys. run: sudo dnf upgrade repoctl-release && sudo rpmkeys --import /etc/pki/rpm-gpg/RPM-GPG-KEY-repoctl"
			}
		case k.State == ToolKeyCurrent:
			have = append(have, k.ID())
			if k.Expires.Sub(now) < toolKeyExpiryWarn {
				warns = append(warns, fmt.Sprintf("the tool repository key %s expires on %s", k.Fingerprint, k.Expires.Format(time.DateOnly)))
				if warnFix == "" {
					warnFix = "a maintainer rotates the tool repository key (tool.html#tool-rotation)"
				}
			}
		}
	}
	if msg, fix := checkToolRepoFile(); msg != "" {
		warns = append(warns, msg)
		if warnFix == "" {
			warnFix = fix
		}
	}
	switch {
	case len(fails) > 0:
		return []Check{Fail(name, strings.Join(append(fails, warns...), "; "), failFix)}
	case len(warns) > 0:
		return []Check{Warn(name, strings.Join(warns, "; "), warnFix)}
	}
	return []Check{Ok(name, name+": the rpm keyring has the tool repository key "+strings.Join(have, ", ")+"; repoctl.repo checks the signatures")}
}

// checkToolRepoFile checks that the repo file of the tool repository has
// gpgcheck=1 and repo_gpgcheck=1. It gives the finding and its fix, or "".
func checkToolRepoFile() (string, string) {
	f, err := os.Open(toolRepoFile)
	if err != nil {
		if os.IsNotExist(err) {
			return toolRepoFile + " is missing", "install the release package (https://dennisklein.github.io/repoctl/manual/install.html#first-install). run: sudo dnf install repoctl-release"
		}
		return "cannot read " + toolRepoFile + ": " + err.Error(), "run: ls -l " + toolRepoFile
	}
	defer func() { _ = f.Close() }()
	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		vals[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	var off []string
	for _, k := range []string{"gpgcheck", "repo_gpgcheck"} {
		if v := vals[k]; v != "1" && v != "true" && v != "yes" && v != "True" {
			off = append(off, k)
		}
	}
	if len(off) > 0 {
		return toolRepoFile + " does not set " + strings.Join(off, " and ") + " to 1",
			"restore the repo file of the release package. run: sudo dnf reinstall repoctl-release"
	}
	return "", ""
}
