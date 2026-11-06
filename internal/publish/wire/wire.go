// Package wire connects the publish runs to the parts of repoctl that
// other packages own: the signer and gates 1 to 3 of package sign, the
// advisory gate of package advisory, the verify root of package mock and
// the registry of package snapshot.
package wire

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/advisory"
	"github.com/dennisklein/repoctl/internal/advisory/rpmmd"
	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/publish"
	"github.com/dennisklein/repoctl/internal/sign"
	"github.com/dennisklein/repoctl/internal/snapshot"
)

// Signer is the publish.Signer of a sign.Signer.
type Signer struct {
	S     *sign.Signer
	Trust *sign.Trust
}

// Info gives the signer.
func (s *Signer) Info() api.Signer {
	out := api.Signer{Fingerprint: s.S.Fingerprint(), KeyID: s.S.KeyID(), Backend: s.S.Backend}
	if s.S.Key != nil {
		out.UID, out.State = s.S.Key.UID(), s.S.Key.State
	}
	return out
}

// Probe runs the signer probe.
func (s *Signer) Probe(ctx context.Context) error { return s.S.Probe(ctx) }

// SignRPMs signs RPM files in place.
func (s *Signer) SignRPMs(ctx context.Context, files []string) error { return s.S.SignRPMs(ctx, files) }

// SignDetached writes FILE.asc; sig must be FILE.asc.
func (s *Signer) SignDetached(ctx context.Context, file, sig string) error {
	if sig != file+".asc" {
		return fmt.Errorf("the signature of %s must be %s.asc", file, file)
	}
	return s.S.SignRepomd(ctx, file)
}

// Gates runs gates 1 to 3 of package sign after the basic checks of the
// base signers.
type Gates struct {
	Trust  *sign.Trust
	Deps   *sign.Deployments
	Signer *sign.Signer
	SetDir string
	// Tmp holds the key files of the client trust set until verify-el.
	Tmp string
	Now func() time.Time
}

// Check runs the gates for one target.
func (g *Gates) Check(ctx context.Context, in publish.GateInput) (*publish.GateResult, error) {
	res, err := publish.BasicGates{}.Check(ctx, in)
	if err != nil {
		return nil, err
	}
	gi := sign.GateInput{Trust: g.Trust, Deployments: g.Deps, Signer: g.Signer, Targets: []string{in.Target},
		Bases: map[string]sign.TargetBase{}, KeyfileAt: sign.KeyfileAtCommit(g.SetDir, in.Set), Now: g.Now()}
	var base *sign.Tree
	if in.BaseDir != "" && in.Kind != publish.KindImport {
		base = sign.NewTree(in.BaseDir, in.Set, in.Target)
		gi.Bases[in.Target] = base
	}
	if err := sign.CheckGates(ctx, gi); err != nil {
		if e, ok := api.AsError(err); ok && e.Target == "" {
			e.Target = in.Target
		}
		return nil, err
	}
	if g.Deps != nil && g.Signer != nil {
		if rec := g.Deps.Find(g.Signer.KeyID(), in.Target); rec != nil {
			what := rec.Release
			if what == "" {
				what = "the key file of " + api.ShortCommit(rec.Commit)
			}
			at := rec.At
			if len(at) >= 10 {
				at = at[:10]
			}
			res.Deployed = fmt.Sprintf("deployed with %s (confirmed by %s %s)", what, rec.ConfirmedBy, at)
			if rec.Release != "" && base != nil {
				if kf, err := base.ReleaseKeyfile(ctx, rec.Release); err == nil && len(kf) > 0 {
					p := filepath.Join(g.Tmp, in.Target+"-client.asc")
					if os.WriteFile(p, kf, 0o644) == nil {
						res.ClientKeys = []string{p}
					}
				}
			}
		}
	}
	if res.Trust != nil && base != nil && in.NewKeyfile != "" {
		old, err1 := base.RootKeyfile()
		cur, err2 := os.ReadFile(in.NewKeyfile)
		if err1 == nil && err2 == nil {
			if tc, err := sign.ComputeTrustChange(ctx, g.Trust, g.SetDir, "", old, cur); err == nil && !tc.Empty() {
				res.TrustLines = tc.Lines()
				res.Trust.Added = tc.AddedIDs()
			}
		}
	}
	return res, nil
}

// Advisory is the publish.AdvisoryGate of package advisory.
type Advisory struct {
	Inputs *advisory.Inputs
	Set    *config.Set
	// Command is the publish command of the hints.
	Command string
}

// Gate runs the advisory gate for one target: the base, the result tree
// (the base plus the added RPMs, without the removed paths) and the
// closure repos. A repo that cannot be read stops the publish
// (advisory.gate-unavailable).
func (g *Advisory) Gate(ctx context.Context, in publish.AdvisoryInput) (*publish.AdvisoryResult, error) {
	name := g.Set.Repo.Name
	rd := &rpmmd.Reader{}
	var base []*rpmmd.Package
	var bm *manifest.Manifest
	if in.BaseDir != "" {
		var err error
		if base, err = rd.Read(ctx, filepath.Join(in.BaseDir, name), name); err != nil {
			return nil, advisory.GateUnavailable(in.Target, name, err)
		}
		bm, _ = manifest.ReadRepo(filepath.Join(in.BaseDir, name))
	}
	removed := map[string]bool{}
	for _, r := range in.Remove {
		removed[filepath.Base(r)] = true
	}
	var tr []*rpmmd.Package
	for _, p := range base {
		if !removed[filepath.Base(p.Location)] {
			tr = append(tr, p)
		}
	}
	added, err := stagedPackages(ctx, rd, in.Add)
	if err != nil {
		return nil, advisory.GateUnavailable(in.Target, "staging", err)
	}
	tr = append(tr, added...)
	repos, err := g.Inputs.ClosurePackages(ctx, in.Target)
	if err != nil {
		return nil, advisory.GateUnavailable(in.Target, "closure repos", err)
	}
	kind := in.Kind
	if kind == publish.KindReseed {
		kind = advisory.GateResign
	}
	res, err := advisory.Gate(advisory.GateInput{Target: in.Target, Kind: kind, Base: base, Tree: tr, Repos: repos,
		Owner: advisory.NewOwner(g.Set, bm, in.Owners), Packages: in.Packages, Accept: in.Accept, Reason: in.Reason,
		User: in.User, Host: in.Host, Command: g.Command})
	if err != nil {
		return nil, err
	}
	out := &publish.AdvisoryResult{}
	blocks, open := 0, 0
	for _, r := range res.Rows {
		if r.Advisory.ID == "" {
			continue
		}
		out.Gate = append(out.Gate, api.RunPublishValueGateItem{ID: r.Advisory.ID, Blocks: r.Blocks, Accepted: r.Accepted, Reason: r.Status, Affected: r.Affected})
		if r.Blocks {
			blocks++
		} else if r.Status == advisory.RowOpen {
			open++
		}
	}
	for _, a := range res.Accepted {
		out.Accepted = append(out.Accepted, manifest.Accepted{ID: a.ID, FactsDigest: a.FactsDigest, Reason: a.Reason})
	}
	switch {
	case blocks > 0:
		out.Line = fmt.Sprintf("advisory gate: %d accepted", len(res.Accepted))
	case open > 0:
		out.Line = fmt.Sprintf("advisory gate ok: %d open, not changed by this publish", open)
	default:
		out.Line = "advisory gate ok"
	}
	return out, nil
}

// stagedPackages reads the packages of the added binary RPMs from the
// repodata of their staging repositories.
func stagedPackages(ctx context.Context, rd *rpmmd.Reader, files []string) ([]*rpmmd.Package, error) {
	want := map[string]map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, ".src.rpm") {
			continue
		}
		dir := filepath.Dir(f)
		for dir != "/" && dir != "." && !rpmmd.HasRepodata(dir) {
			dir = filepath.Dir(dir)
		}
		if !rpmmd.HasRepodata(dir) {
			return nil, fmt.Errorf("%s is in no repository with repodata", f)
		}
		if want[dir] == nil {
			want[dir] = map[string]bool{}
		}
		want[dir][filepath.Base(f)] = true
	}
	var out []*rpmmd.Package
	for dir, names := range want {
		pkgs, err := rd.Read(ctx, dir, filepath.Base(dir))
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			if names[filepath.Base(p.Location)] {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// Options are what Env needs from the CLI.
type Options struct {
	Set      *config.Set
	SetDir   string
	Paths    config.Paths
	Eff      *config.Effective
	Getenv   func(string) string
	Now      func() time.Time
	User     string
	Host     string
	Via      api.RunOrigin
	Mock     mock.Options
	Inputs   *advisory.Inputs
	Command  string
	Progress func(target, step, state string, d time.Duration)
}

// Env wires a publish.Env. The cleanup removes the temporary key files.
func Env(ctx context.Context, o Options) (*publish.Env, func(), error) {
	trust := sign.LoadTrust(o.Set, o.SetDir)
	s, err := sign.NewSigner(ctx, trust, o.Eff)
	if err != nil {
		return nil, func() {}, err
	}
	if err := s.RequireSecret(ctx); err != nil {
		return nil, func() {}, err
	}
	deps, err := sign.ReadDeployments(o.SetDir)
	if err != nil {
		return nil, func() {}, err
	}
	tmp, err := os.MkdirTemp("", "rpw-")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	env := &publish.Env{
		Set: o.Set, SetDir: o.SetDir, Paths: o.Paths, Now: o.Now, Getenv: o.Getenv, User: o.User, Host: o.Host,
		Repoctl: config.Version, Via: o.Via,
		Store: func(target string) (*snapshot.Store, error) {
			return snapshot.Open(o.Set.OCIPackage(target), snapshot.Options{Getenv: o.Getenv, UserAgent: "repoctl/" + config.Version})
		},
		Signer:   &Signer{S: s, Trust: trust},
		Gates:    &Gates{Trust: trust, Deps: deps, Signer: s, SetDir: o.SetDir, Tmp: tmp, Now: o.Now},
		Advisory: &Advisory{Inputs: o.Inputs, Set: o.Set, Command: o.Command},
		Stager:   &publish.JournalStager{Set: o.Set, Paths: o.Paths, Host: o.Host},
		EL:       &publish.MockEL{Mock: mock.New(o.Paths, o.Mock), Set: o.Set, SetDir: o.SetDir, Work: tmp},
		Git:      &publish.SetGit{Set: o.Set, SetDir: o.SetDir, Paths: o.Paths},
		Deployed: func(target string) bool {
			for _, d := range deps.Deployments {
				if d.Target == target {
					return true
				}
			}
			return false
		},
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Progress: o.Progress,
	}
	if o.Inputs == nil {
		env.Advisory = publish.NoAdvisoryGate{}
	}
	return env, cleanup, nil
}
