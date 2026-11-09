package publish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/publish/tree"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// HostEL is an ELVerifier that runs the check of the verify root on the
// host, with the host rpmkeys. Tests use it in place of MockEL.
type HostEL struct{}

// VerifyEL runs the check on the host.
func (HostEL) VerifyEL(ctx context.Context, target, treeDir string, keyFiles []string) error {
	if len(keyFiles) == 0 {
		return api.Errorf(api.PVerifyELFailed, "%s: the client trust set has no key file", target).WithTarget(target)
	}
	var out strings.Builder
	args := append([]string{"-c", elScript, "sh", treeDir}, keyFiles...)
	if err := (&run.Cmd{Name: "sh", Args: args, Stdout: &out}).Run(ctx); err != nil {
		return api.Errorf(api.PVerifyELFailed, "%s: the check of the client trust set failed: %s", target, firstLines(out.String(), 3)).WithTarget(target).WithCause(err)
	}
	return nil
}

// BasicGates is the default Gates until the gates of the sign package are
// wired. It checks the signer state and the signers of the base with the
// re-sign rules (signing.html#resign), gives the trust block when the key
// file changes and takes the root key file of the base as the client trust
// set. It does not read keys/deployed.yaml: gate 2 is not run.
type BasicGates struct{}

// Check runs the basic gates.
func (BasicGates) Check(ctx context.Context, in GateInput) (*GateResult, error) {
	res := &GateResult{}
	if in.Signer.State != "" && in.Signer.State != config.KeyActive {
		return nil, api.Errorf(api.PSignNotActive, "the signing key %s is in state %s, not active", in.Signer.KeyID, in.Signer.State).WithTarget(in.Target)
	}
	keys := map[string]config.Key{}
	for _, k := range in.Set.Sign.Keys {
		keys[normFpr(k.Fingerprint)] = k
	}
	var unknown []string
	for fpr, n := range in.BaseSigners {
		k, ok := keys[fpr]
		switch {
		case !ok:
			if in.Kind != KindImport {
				unknown = append(unknown, fmt.Sprintf("%d RPMs of %s", n, shortOr(fpr)))
			}
		case k.State == config.KeyRemoved || k.State == config.KeyRevoked:
			if in.Kind == KindPublish || in.Kind == KindPrune {
				return nil, api.Errorf(api.PSignRemovedSigner, "%d RPMs of the base of %s are signed by %s, a key in state %s", n, in.Target, k.ID, k.State).
					WithTarget(in.Target).WithHint("give them a new signature", "repoctl repo resign --signer "+k.ID+" --target "+in.Target)
			}
		case k.State == config.KeyRetiring:
			res.Warnings = append(res.Warnings, api.Problem{Code: api.PSignRetiring, Target: in.Target,
				Message: fmt.Sprintf("%d RPMs of %s are signed by the retiring key %s", n, in.Target, k.ID),
				Hint:    api.HintText("", "repoctl repo resign --signer "+k.ID+" --target "+in.Target)})
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, api.Errorf(api.PSignUnknownSigner, "the base of %s has RPMs of a key that is not in sign.keys: %s", in.Target, strings.Join(unknown, ", ")).
			WithTarget(in.Target).WithHint("pull the set branch, then the local copy", "git pull && repoctl repo pull --target "+in.Target)
	}
	baseKey := ""
	if in.BaseDir != "" {
		if p := filepath.Join(in.BaseDir, tree.KeyfileName(in.Set)); fileExists(p) {
			baseKey = p
		}
	}
	switch {
	case baseKey != "":
		res.ClientKeys = []string{baseKey}
	case in.NewKeyfile != "":
		res.ClientKeys = []string{in.NewKeyfile}
	}
	if baseKey != "" && in.NewKeyfile != "" {
		old, err := tree.KeyFingerprints(ctx, baseKey)
		if err != nil {
			return nil, err
		}
		cur, err := tree.KeyFingerprints(ctx, in.NewKeyfile)
		if err != nil {
			return nil, err
		}
		t := &manifest.Trust{Added: diff(cur, old), Removed: diff(old, cur)}
		if len(t.Added) > 0 || len(t.Removed) > 0 {
			res.Trust = t
		}
	}
	return res, nil
}

func diff(a, b []string) []string {
	out := []string{}
	for _, x := range a {
		if !containsStr(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func shortOr(fpr string) string {
	if fpr == "" {
		return "no signature"
	}
	return "key " + api.ShortFingerprint(fpr)
}

func normFpr(f string) string { return strings.ToUpper(strings.ReplaceAll(f, " ", "")) }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// NoAdvisoryGate is the default AdvisoryGate until the advisory package is
// wired: it blocks nothing and accepts nothing.
type NoAdvisoryGate struct{}

// Gate gives an empty result; an --accept-advisory id is unknown.
func (NoAdvisoryGate) Gate(_ context.Context, in AdvisoryInput) (*AdvisoryResult, error) {
	if len(in.Accept) > 0 {
		return nil, api.Errorf(api.PAdvisoryUnknown, "no open advisory has the id %s", in.Accept[0]).WithTarget(in.Target)
	}
	return &AdvisoryResult{Line: "advisory gate ok"}, nil
}

// MockEL is the default ELVerifier: it runs rpmkeys --checksig on each RPM
// of the tree in the verify root of the target (mock --chroot), with a
// temporary rpmdb that holds only the client trust set. The tree and the
// key files are bind mounted read-only at the same paths.
type MockEL struct {
	Mock   *mock.Mock
	Set    *config.Set
	SetDir string
	// Work is the directory of the wrapper config.
	Work string
}

const elScript = `set -e
tree="$1"; shift
db=$(mktemp -d)
trap 'rm -rf "$db"' EXIT
for k in "$@"; do rpmkeys --dbpath "$db" --import "$k"; done
n=0; bad=0
for f in $(find "$tree" -mindepth 2 -name '*.rpm' | sort); do
  n=$((n+1))
  out=$(rpmkeys --dbpath "$db" --checksig "$f" 2>&1) || true
  case "$out" in
    *"signatures OK"*|*"signatures ok"*) ;;
    *) bad=$((bad+1)); echo "BAD $out" ;;
  esac
done
echo "CHECKED $n BAD $bad"
test "$bad" -eq 0
`

// VerifyEL runs the check in the verify root.
func (m *MockEL) VerifyEL(ctx context.Context, target, treeDir string, keyFiles []string) error {
	t, ok := m.Set.Target(target)
	if !ok {
		return fmt.Errorf("unknown target %s", target)
	}
	if len(keyFiles) == 0 {
		return api.Errorf(api.PVerifyELFailed, "%s: the client trust set has no key file", target).WithTarget(target)
	}
	mt := mock.FromConfig(*t, m.SetDir)
	files, err := m.Mock.WriteFiles(filepath.Join(m.Work, "verify"), mt)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("config_opts['plugin_conf']['bind_mount_enable'] = True\n")
	for _, d := range append([]string{treeDir}, keyFiles...) {
		fmt.Fprintf(&b, "config_opts['plugin_conf']['bind_mount_opts']['dirs'].append(('%s', '%s'))\n", d, d)
	}
	f, err := os.OpenFile(files.Config, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(b.String())
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	args := append([]string{"sh", "-c", elScript, "sh", treeDir}, keyFiles...)
	out, err := m.Mock.Chroot(ctx, mock.Call{Files: files, Uniqueext: mock.VerifyExt(m.Set.Name, target)}, args...)
	if err != nil {
		lines := []string{}
		for _, l := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(l, "BAD ") {
				lines = append(lines, strings.TrimPrefix(l, "BAD "))
			}
		}
		msg := err.Error()
		if len(lines) > 0 {
			msg = firstLines(strings.Join(lines, "\n"), 3)
		}
		return api.Errorf(api.PVerifyELFailed, "%s: the check in the verify root failed: %s", target, msg).WithTarget(target).WithCause(err)
	}
	return nil
}
