package advisory

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/sign"
)

// sign.expiring (advisories.html#codes, signing.html#expiry): the RPMs in
// latest of a target carry a signer whose key expires inside
// sign.checks.key_expiry_warn (level warn) or key_min_validity (level
// required). The subject is the key id, so the advisories of the targets
// merge into one per key. It has no closure: it never blocks a publish.
func init() {
	RegisterCode(CodeCheck{Code: api.AdvisoryCodeSignExpiring, Run: checkSignExpiring})
}

// keys gives the trust set of the set: sign.keys and keys/ID.asc, read
// once per check.
func (in *Inputs) keys() *sign.Trust {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.trust == nil {
		in.trust = sign.LoadTrust(in.Set, in.SetDir)
	}
	return in.trust
}

// checkSignExpiring gives sign.expiring for each key that signed RPMs in
// latest of the target and expires soon.
func checkSignExpiring(ctx context.Context, in *Inputs, target string) (Outcome, error) {
	l, err := in.Latest(ctx, target)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{Latest: l.Digest()}
	if l.Manifest == nil {
		return out, nil
	}
	rpms := map[string]int{}
	for _, b := range l.Manifest.Builds {
		if b.Signer == "" {
			continue
		}
		n := len(b.RPMs)
		if b.SRPM != "" {
			n++
		}
		rpms[sign.NormalizeFingerprint(b.Signer)] += n
	}
	warn, minValidity := sign.Checks(in.Set)
	out.Advisories, err = SignExpiring(in.keys().Keys, rpms, target, in.now(), warn, minValidity)
	return out, err
}

// SignExpiring gives sign.expiring for each key of keys that signed RPMs
// (rpms counts them by full fingerprint) and expires before now+warn:
// level required inside minValidity, else warn. A key without a readable
// key file or without an expiry gives nothing.
func SignExpiring(keys []*sign.TrustedKey, rpms map[string]int, target string, now time.Time, warn, minValidity time.Duration) ([]api.Advisory, error) {
	var out []api.Advisory
	for _, k := range keys {
		if k.Cert == nil || k.Cert.Expires.IsZero() || rpms[k.Fingerprint] == 0 || !k.Cert.ExpiresIn(now, warn) {
			continue
		}
		level := api.AdvisoryLevelWarn
		if k.Cert.ExpiresIn(now, minValidity) {
			level = api.AdvisoryLevelRequired
		}
		expires := k.Cert.Expires.UTC().Format("2006-01-02")
		a, err := New(api.AdvisoryCodeSignExpiring, k.ID, level, []api.TargetId{target},
			api.FactsSignExpiring{Fingerprint: k.Fingerprint, Expires: expires})
		if err != nil {
			return nil, err
		}
		verb := "expires"
		if !now.Before(k.Cert.Expires) {
			verb = "expired"
		}
		a.Title = fmt.Sprintf("the signer key %s %s on %s", k.ID, verb, expires)
		a.Suggestion = &api.AdvisorySuggestion{
			Text: fmt.Sprintf("Treat it as a key change: add and deploy a new key, retire %s, and re-sign its RPMs before %s (key expiry).",
				k.ID, expires),
			Command: "repoctl repo resign --signer " + k.ID,
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
