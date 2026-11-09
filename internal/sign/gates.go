package sign

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// TargetBase is what gate 2 reads of the base snapshot of a target: the
// release NVRs and the key file of each. *Tree implements it.
type TargetBase interface {
	// HasRelease finds a release NVR in the base (with or without the
	// dist tag) and gives the full NVR.
	HasRelease(nvr string) (string, bool)
	// ReleaseKeyfile gives the key file of the release NVR.
	ReleaseKeyfile(ctx context.Context, nvr string) ([]byte, error)
}

// KeyfileAt reads the key file of the release package at a set commit
// (git show COMMIT:PATH), for a key file record.
type KeyfileAt func(ctx context.Context, commit string) ([]byte, error)

// GateInput is the input of the gates 1 to 3 (signing.html#gates).
type GateInput struct {
	Trust       *Trust
	Deployments *Deployments
	// Signer is the key of sign.key.
	Signer *Signer
	// Targets are the selected targets of the publish.
	Targets []string
	// Bases gives the base snapshot of each selected target. A target
	// without a base is a first publish: gate 2 does not apply to it.
	Bases map[string]TargetBase
	// KeyfileAt reads the key file at the commit of a key file record.
	KeyfileAt KeyfileAt
	Now       time.Time
}

// CheckGates runs gate 1, gate 2 and gate 3 and gives the first failure.
func CheckGates(ctx context.Context, in GateInput) error {
	if err := Gate1(in.Trust, in.Signer, in.Now); err != nil {
		return err
	}
	if err := Gate2(ctx, in); err != nil {
		return err
	}
	return Gate3(in.Signer)
}

// Gate1 checks that the signer key is in sign.keys with state active and
// valid for at least sign.checks.key_min_validity: else sign.not-active or
// sign.key-expiring (exit 12).
func Gate1(trust *Trust, s *Signer, now time.Time) error {
	k := s.Key
	if k == nil && trust != nil {
		k = trust.ByFingerprint(s.FPR)
	}
	if k == nil {
		return api.Errorf(api.PSignNotActive, "gate 1: the key %s of sign.key is not in sign.keys", AbbrevFingerprint(s.FPR)).
			WithHint("add the key and deploy it", "repoctl sign add ID --fingerprint "+s.FPR)
	}
	if k.State != config.KeyActive {
		e := api.Errorf(api.PSignNotActive, "gate 1: the key %s is %s; only an active key signs", k.ID, k.State)
		if k.State == config.KeyRetiring {
			return e.WithHint("activate the key again, or sign with an active key", "repoctl sign activate "+k.ID)
		}
		return e.WithHint("sign with another active key", "repoctl sign ls")
	}
	if k.Cert != nil {
		_, minValidity := Checks(trust.Set)
		if k.Cert.ExpiresIn(now, minValidity) {
			return api.Errorf(api.PSignKeyExpiring, "gate 1: the key %s expires on %s, within key_min_validity %s",
				k.ID, k.Cert.Expires.Format("2006-01-02"), config.FormatDuration(minValidity)).
				WithHint("add a new key, then retire this key", "repoctl sign add NEW-ID --fingerprint FPR")
		}
	}
	return nil
}

// Gate3 checks the key format: OpenPGP v4, RSA 2048 or more, or Ed25519,
// and no SHA-1 self-signature: else sign.key-format (exit 9).
func Gate3(s *Signer) error {
	k := s.Key
	if k == nil {
		return api.Errorf(api.PSignKeyFormat, "gate 3: the key %s is not in sign.keys", AbbrevFingerprint(s.FPR))
	}
	if k.Cert == nil {
		return api.Errorf(api.PSignKeyFormat, "gate 3: %v", k.Err).WithPath(k.Path()).
			WithHint("export the public key again", "repoctl doctor --only sign")
	}
	if r := k.Cert.CheckFormat(); r != "" {
		return api.Errorf(api.PSignKeyFormat, "gate 3: the key %s is %s", k.ID, r).WithPath(k.Path()).
			WithHint("make a new key: RSA 4096 or Ed25519", "repoctl help key")
	}
	return nil
}

// Gate2 checks that keys/deployed.yaml has a passing record of the signer
// for each selected target with a base: else sign.not-deployed (exit 12).
func Gate2(ctx context.Context, in GateInput) error {
	s := in.Signer
	if s.Key == nil {
		return api.Errorf(api.PSignNotDeployed, "gate 2: the key %s is not in sign.keys", AbbrevFingerprint(s.FPR))
	}
	for _, t := range in.Targets {
		base := in.Bases[t]
		if base == nil {
			continue
		}
		ok, reason := RecordPasses(ctx, in.Deployments.Find(s.Key.ID, t), s.Key.Fingerprint, base, in.KeyfileAt)
		if !ok {
			return api.Errorf(api.PSignNotDeployed, "gate 2: %s has no deployment record that passes for %s: %s", s.Key.ID, t, reason).
				WithTarget(t).WithHint("check the onboarding steps of the key", "repoctl sign onboard "+s.Key.ID)
		}
	}
	return nil
}

// RecordPasses checks one deployment record against a base (record forms,
// signing.html#deployment-record). It gives the reason of a failure.
func RecordPasses(ctx context.Context, rec *Deployment, fpr string, base TargetBase, keyfileAt KeyfileAt) (bool, string) {
	if rec == nil {
		return false, "no record in " + DeployedFile
	}
	if NormalizeFingerprint(rec.Fingerprint) != NormalizeFingerprint(fpr) {
		return false, fmt.Sprintf("the record has the fingerprint %s, not %s", AbbrevFingerprint(rec.Fingerprint), AbbrevFingerprint(fpr))
	}
	if rec.KeyfileRecord() {
		if keyfileAt == nil {
			return false, "the key file of the record cannot be read"
		}
		data, err := keyfileAt(ctx, rec.Commit)
		if err != nil {
			return false, fmt.Sprintf("the key file at %s: %v", api.ShortCommit(rec.Commit), err)
		}
		if sum := Sha256Hex(data); sum != rec.Keyfile {
			return false, fmt.Sprintf("the key file at %s has the sha256 %s…, not %s…", api.ShortCommit(rec.Commit), sum[:8], shortSum(rec.Keyfile))
		}
		if !KeyfileHolds(data, fpr) {
			return false, fmt.Sprintf("the key file at %s does not hold the key", api.ShortCommit(rec.Commit))
		}
		return true, ""
	}
	if base == nil {
		return false, "the target has no latest"
	}
	nvr, ok := base.HasRelease(rec.Release)
	if !ok {
		return false, fmt.Sprintf("the release %s of the record is not in the base", rec.Release)
	}
	data, err := base.ReleaseKeyfile(ctx, nvr)
	if err != nil {
		return false, fmt.Sprintf("the key file of %s: %v", nvr, err)
	}
	if !KeyfileHolds(data, fpr) {
		return false, fmt.Sprintf("the key file of %s does not hold the key", nvr)
	}
	return true, ""
}

func shortSum(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// Sha256Hex gives the sha256 of data in hex, as sha256sum prints it.
func Sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// BaseSigners is the result of the re-sign rules for the RPMs of a base
// (signing.html#resign).
type BaseSigners struct {
	// Files are the RPMs per key id.
	Files map[string][]string
	// Retiring counts the RPMs of each retiring key: a normal publish
	// keeps them, and the sign line of the summary shows the count.
	Retiring map[string]int
	// Resign are the RPMs that repo resign --signer ID signs again.
	Resign []string
	// Warnings are sign.expired-signer for the RPMs whose key expired
	// after the signature.
	Warnings []api.Problem
}

// ClassifyBase applies the re-sign rules to the signature of each RPM of a
// base (file -> signature, nil for an unsigned RPM). resign is the key id of
// repo resign --signer ID, "" for a normal publish. A removed or revoked
// signer gives sign.removed-signer (exit 12); an unknown key or no
// signature gives sign.unknown-signer (exit 10) with the file names.
func ClassifyBase(trust *Trust, sigs map[string]*Signature, now time.Time, resign string) (*BaseSigners, error) {
	out := &BaseSigners{Files: map[string][]string{}, Retiring: map[string]int{}}
	files := make([]string, 0, len(sigs))
	for f := range sigs {
		files = append(files, f)
	}
	sort.Strings(files)
	var unknown, removed []string
	expired := map[string]int{}
	for _, f := range files {
		sig := sigs[f]
		var k *TrustedKey
		if sig != nil {
			k = trust.ForIssuer(sig.Issuer())
		}
		if k != nil && k.Cert != nil && !k.Cert.Expires.IsZero() && sig.Created.After(k.Cert.Expires) {
			// A signature counts at the time it was made: after the
			// expiry it does not count.
			k = nil
		}
		if k == nil {
			unknown = append(unknown, f)
			continue
		}
		out.Files[k.ID] = append(out.Files[k.ID], f)
		if k.Cert != nil && !k.Cert.Expires.IsZero() && now.After(k.Cert.Expires) {
			expired[k.ID]++
		}
		switch {
		case resign != "" && k.ID == resign:
			out.Resign = append(out.Resign, f)
		case k.State == config.KeyRetiring:
			out.Retiring[k.ID]++
		case k.State == config.KeyRemoved || k.State == config.KeyRevoked:
			removed = append(removed, f)
		}
	}
	for _, id := range sortedKeys(expired) {
		out.Warnings = append(out.Warnings, api.Warning(api.PSignExpiredSigner,
			"%s %s a signature of %s, which expired after it signed them; EL10 clients refuse them", plural(expired[id], "RPM"), Agree(expired[id], "has", "have"), id))
	}
	if len(unknown) > 0 {
		return out, api.Errorf(api.PSignUnknownSigner, "%s of the base %s no signature of a key in sign.keys: %s",
			plural(len(unknown), "RPM"), Agree(len(unknown), "has", "have"), strings.Join(firstN(baseNames(unknown), 5), ", ")).
			WithHint("check the files; in repo pull, get the set commit of the snapshot first", "git pull origin set/"+trust.Set.Name)
	}
	if len(removed) > 0 {
		ids := map[string]bool{}
		for id, fs := range out.Files {
			if k := trust.Key(id); k != nil && (k.State == config.KeyRemoved || k.State == config.KeyRevoked) && len(fs) > 0 {
				ids[id] = true
			}
		}
		first := sortedKeys(ids)[0]
		return out, api.Errorf(api.PSignRemovedSigner, "%s of the base %s a signature of the %s key %s: %s",
			plural(len(removed), "RPM"), Agree(len(removed), "has", "have"), trust.Key(first).State, first, strings.Join(firstN(baseNames(removed), 5), ", ")).
			WithHint("sign them again with an active key", "repoctl repo resign --signer "+first)
	}
	return out, nil
}

func baseNames(files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		if j := strings.LastIndexByte(f, '/'); j >= 0 {
			out[i] = f[j+1:]
		} else {
			out[i] = f
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
