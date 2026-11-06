package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/localcopy"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/publish/tree"
	"github.com/dennisklein/repoctl/internal/snapshot"
)

// Env is what a publish run uses: the set, the host and the parts that
// other packages own.
type Env struct {
	Set     *config.Set
	SetDir  string
	Paths   config.Paths
	Now     func() time.Time
	Getenv  func(string) string
	User    string
	Host    string
	Repoctl string
	Via     api.RunOrigin
	// Store opens the OCI package of a target.
	Store    func(target string) (*snapshot.Store, error)
	Signer   Signer
	Gates    Gates
	Advisory AdvisoryGate
	Stager   Stager
	EL       ELVerifier
	Git      GitChecks
	// Deployed reports a record of keys/deployed.yaml for a target.
	Deployed func(target string) bool
	// HTTP reads repo.url for the first publish check.
	HTTP *http.Client
	// Progress gets each step of each target, if not nil.
	Progress func(target, step, state string, d time.Duration)
}

// UsageError is a usage error of a publish request (exit 2).
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// PruneSpec are the selectors of repo prune.
type PruneSpec struct {
	Package   string
	All       bool
	KeepLast  int
	OlderThan time.Duration
	NVRs      []string
}

// Request is one publish run as the human asks it.
type Request struct {
	Kind string
	// Targets are the selected targets; Explicit is true when --target
	// named them.
	Targets  []string
	Explicit bool
	Run      string
	Hold     []string
	Reason   string
	Accept   []string
	// Unattended is --yes without a terminal with
	// REPOCTL_ALLOW_UNATTENDED_PUBLISH=1.
	Unattended  bool
	TraceParent string
	// Ref is the snapshot of a restore.
	Ref   string
	Prune PruneSpec
	// SignerID is the key of repo resign --signer.
	SignerID string
	// Repo limits prune and resign to one repository.
	Repo api.RepoKind
	// ImportDir and ImportManifest are repo push --from and --manifest.
	ImportDir      string
	ImportManifest string
}

// Row is one line of the package table of the summary.
type Row struct {
	Package   string
	Published string
	New       string
	RPMs      int
	Run       string
}

// TargetPlan is the result of step 1 for one target.
type TargetPlan struct {
	Target string
	Store  *snapshot.Store
	Fresh  localcopy.Freshness
	First  bool
	// Base is latest at step 1, nil for a first publish.
	Base         *snapshot.Snapshot
	BaseDir      string
	BaseManifest *manifest.Manifest
	// SourceDir is the tree that step 2 copies: the local copy, the files
	// of REF (restore) or the import directory.
	SourceDir string
	// RefSnap is the snapshot of a restore.
	RefSnap     *snapshot.Snapshot
	RefManifest *manifest.Manifest
	Take        []StagedNode
	Held        []api.RunPublishValueHeldItem
	Current     int
	Rows        []Row
	// Remove are the paths of the base that the new tree leaves out.
	Remove map[string]bool
	// Resign are the paths of the new tree that get a new signature.
	Resign []string
	// Removed are the new entries of removed[], Unremoved the NVRs that
	// leave removed[].
	Removed   []manifest.Removed
	Unremoved []string
	// Rebuild are the packages that need a rebuild after a restore.
	Rebuild     []string
	KeptRelease []string
	Import      *manifest.Manifest
	Sign        api.RunPublishValueSign
	// Keep counts the kept RPMs per signing key id.
	Keep       map[string]int
	Gate       *GateResult
	Advisory   *AdvisoryResult
	NewKeyfile string
	Warnings   []api.Problem
	// SkipFiles are taken files that the base already holds with the same
	// payload: a publish of an orphaned run signs them again.
	SkipFiles map[string]bool
}

// Skipped is a target that is not publishable, and why.
type Skipped struct {
	Target   string
	Code     api.ProblemCode
	Message  string
	NotReady []NotReady
}

// Plan is the result of step 1.
type Plan struct {
	// ID is the publish id (the tag); RunID is the id of the run journal.
	ID        string
	RunID     string
	Kind      string
	Rules     Rules
	Req       Request
	Signer    api.Signer
	SetCommit string
	Started   time.Time
	Targets   []*TargetPlan
	Skipped   []Skipped
	Digest    string
	FromRuns  []string
	Warnings  []api.Problem
}

// SignCount gives the RPMs that the publish signs, over all targets.
func (p *Plan) SignCount() int {
	n := 0
	for _, t := range p.Targets {
		n += t.Sign.New + t.Sign.Resign
	}
	return n
}

// TargetIDs gives the targets of the plan.
func (p *Plan) TargetIDs() []string {
	var out []string
	for _, t := range p.Targets {
		out = append(out, t.Target)
	}
	return out
}

// Prepare runs step 1 of a publish run: all checks before the first
// signature (publish.html#checks). It writes nothing.
func Prepare(ctx context.Context, env *Env, req Request) (*Plan, error) {
	rules, ok := KindRules[req.Kind]
	if !ok {
		return nil, fmt.Errorf("unknown publish kind %q", req.Kind)
	}
	if len(req.Hold) > 0 && len(strings.TrimSpace(req.Reason)) < 8 {
		return nil, &UsageError{Msg: "--hold needs --reason with 8 characters or more"}
	}
	if len(req.Accept) > 0 && len(strings.TrimSpace(req.Reason)) < 8 {
		return nil, &UsageError{Msg: "--accept-advisory needs --reason with 8 characters or more"}
	}
	now := env.Now()
	p := &Plan{Kind: req.Kind, Rules: rules, Req: req, Started: now, RunID: snapshot.NewID(now)}
	p.ID = p.RunID
	setCommit, commits, err := env.Git.Check(ctx, rules, req.Targets)
	if err != nil {
		return nil, err
	}
	p.SetCommit = setCommit
	if st, ok := env.Stager.(*JournalStager); ok && st.Commits == nil {
		st.Commits = commits
	}
	p.Signer = env.Signer.Info()
	kr, err := tree.NewKeyring(ctx, localcopy.Keys(env.Set, env.SetDir))
	if err != nil {
		return nil, err
	}
	defer kr.Close()
	tmp, err := os.MkdirTemp("", "rpk-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	runs := map[string]bool{}
	for _, t := range req.Targets {
		tp, skip, err := prepareTarget(ctx, env, p, t, kr, tmp)
		if err != nil {
			return nil, err
		}
		if skip != nil {
			if req.Explicit {
				e := api.Errorf(skip.Code, "%s", skip.Message).WithTarget(t)
				if skip.Code == api.PPublishNotReady {
					e = notReadyHints(e, t, skip.NotReady)
				}
				return nil, e
			}
			p.Skipped = append(p.Skipped, *skip)
			continue
		}
		for _, n := range tp.Take {
			runs[n.Run] = true
		}
		p.Targets = append(p.Targets, tp)
	}
	if len(p.Targets) == 0 {
		for _, s := range p.Skipped {
			if s.Code != api.PPublishNothingToPublish {
				return nil, api.Errorf(s.Code, "no target is publishable: %s", s.Message).WithTarget(s.Target).
					WithHint("see what the targets need", "repoctl plan")
			}
		}
		return nil, api.Errorf(api.PPublishNothingToPublish, "nothing to publish: each node is current").WithHint("", "repoctl status")
	}
	for r := range runs {
		p.FromRuns = append(p.FromRuns, r)
	}
	sort.Strings(p.FromRuns)
	if err := env.Signer.Probe(ctx); err != nil {
		return nil, err
	}
	p.Digest = planDigest(p)
	return p, nil
}

// prepareTarget runs the checks of one target.
func prepareTarget(ctx context.Context, env *Env, p *Plan, target string, kr *tree.Keyring, tmp string) (*TargetPlan, *Skipped, error) {
	rules := p.Rules
	store, err := env.Store(target)
	if err != nil {
		return nil, nil, err
	}
	tp := &TargetPlan{Target: target, Store: store, Remove: map[string]bool{}, Keep: map[string]int{}, SkipFiles: map[string]bool{}}
	dir := localcopy.Dir(env.Paths, env.Set.Name, target)
	local, err := localcopy.ReadSnapshot(dir)
	if err != nil {
		return nil, nil, err
	}
	deployed := env.Deployed != nil && env.Deployed(target)
	switch rules.Freshness {
	case FreshApplies:
		f := localcopy.Check(ctx, localcopy.FreshInput{Store: store, Target: target, Local: local, Deployed: deployed,
			Pushable: PushableFor(env.Paths, env.Set.Name, target), User: env.User})
		tp.Fresh = f
		if w := f.OrphanedWarning(); w != nil {
			p.Warnings = append(p.Warnings, *w)
		}
		if err := f.PublishErr(); err != nil {
			return nil, nil, err
		}
		if f.Result == localcopy.FreshFirst {
			if p.Kind != KindPublish {
				return nil, nil, api.Errorf(api.PSnapshotNotFound, "%s has no snapshot to %s", target, p.Kind).WithTarget(target)
			}
			if err := firstPublish(ctx, env, target); err != nil {
				return nil, nil, err
			}
			tp.First = true
		} else {
			tp.Base, tp.BaseDir = f.Latest, dir
			if tp.BaseManifest, err = manifest.ReadRepo(filepath.Join(dir, env.Set.Repo.Name)); err != nil {
				return nil, nil, err
			}
		}
	case FreshEmpty:
		_, ok, err := store.Latest(ctx)
		if err != nil {
			return nil, nil, err
		}
		if ok && p.Kind == KindImport {
			return nil, nil, api.Errorf(api.PImportNotEmpty, "the registry has a snapshot of %s; an import makes only the first snapshot", target).WithTarget(target)
		}
		if ok {
			return nil, nil, api.Errorf(api.PSnapshotExists, "the registry has a snapshot of %s; a re-seed is for a lost OCI package", target).WithTarget(target)
		}
		tp.First = true
	}
	tp.SourceDir = tp.BaseDir
	baseHeaders, err := baseRPMs(ctx, tp.BaseDir, tree.LayoutOf(env.Set))
	if err != nil {
		return nil, nil, err
	}
	switch p.Kind {
	case KindPublish:
		if skip, err := planPublish(ctx, env, p, tp, baseHeaders); err != nil || skip != nil {
			return nil, skip, err
		}
	case KindRestore:
		if err := planRestore(ctx, env, p, tp); err != nil {
			return nil, nil, err
		}
	case KindPrune:
		if skip, err := planPrune(env, p, tp, baseHeaders); err != nil || skip != nil {
			return nil, skip, err
		}
	case KindResign:
		if skip, err := planResign(env, p, tp, baseHeaders, kr); err != nil || skip != nil {
			return nil, skip, err
		}
	case KindImport, KindReseed:
		if err := planImport(ctx, env, p, tp); err != nil {
			return nil, nil, err
		}
	}
	signers := map[string]int{}
	for _, h := range baseHeaders {
		if tp.Remove[h.rel] {
			continue
		}
		fpr := h.KeyID
		if k, ok := kr.ByKeyID(h.KeyID); ok {
			fpr = k.Fingerprint
			tp.Keep[k.ID]++
		} else {
			tp.Keep[h.KeyID]++
		}
		signers[fpr]++
	}
	tp.Sign.Keep = len(baseHeaders) - len(tp.Remove) - tp.Sign.Resign
	if tp.Sign.Keep < 0 {
		tp.Sign.Keep = 0
	}
	if rules.Trust != TrustNever || tp.First {
		tp.NewKeyfile = newKeyfile(ctx, env, tp, tmp)
	}
	gin := GateInput{Set: env.Set, SetDir: env.SetDir, Target: target, Kind: p.Kind, Signer: p.Signer, BaseDir: tp.BaseDir, BaseSigners: signers, NewKeyfile: tp.NewKeyfile}
	if tp.Gate, err = env.Gates.Check(ctx, gin); err != nil {
		return nil, nil, err
	}
	tp.Warnings = append(tp.Warnings, tp.Gate.Warnings...)
	if rules.Advisory != AdvisorySkip {
		ain := AdvisoryInput{Set: env.Set, Target: target, Kind: p.Kind, BaseDir: tp.BaseDir, Accept: p.Req.Accept, Reason: p.Req.Reason,
			User: env.User, Host: env.Host, Owners: map[string]string{}}
		if tp.RefSnap != nil {
			ain.BaseDir = ""
		}
		for _, n := range tp.Take {
			ain.Packages = append(ain.Packages, n.Package)
			ain.Owners[n.Draft.SRPM] = n.Package
			for _, f := range n.Files {
				ain.Add = append(ain.Add, f.Path)
			}
		}
		for r := range tp.Remove {
			ain.Remove = append(ain.Remove, r)
		}
		sort.Strings(ain.Remove)
		res, err := env.Advisory.Gate(ctx, ain)
		if err != nil {
			if rules.Advisory == AdvisoryReport {
				tp.Warnings = append(tp.Warnings, api.Problem{Code: api.PAdvisoryOpen, Target: target, Message: err.Error()})
			} else {
				return nil, nil, err
			}
		}
		tp.Advisory = res
		if res != nil {
			tp.Warnings = append(tp.Warnings, res.Warnings...)
		}
	}
	return tp, nil, nil
}

// baseRPM is one RPM of the base with its path in the tree.
type baseRPM struct {
	tree.Header
	rel string
}

// baseRPMs reads the headers of the RPMs of a tree.
func baseRPMs(ctx context.Context, dir string, l tree.Layout) ([]baseRPM, error) {
	if dir == "" {
		return nil, nil
	}
	var rels, abs []string
	for _, r := range l.Repos() {
		_ = filepath.WalkDir(filepath.Join(dir, r.Dir), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".rpm") {
				rel, _ := filepath.Rel(dir, p)
				rels = append(rels, filepath.ToSlash(rel))
				abs = append(abs, p)
			}
			return nil
		})
	}
	hs, err := tree.QueryHeaders(ctx, abs)
	if err != nil {
		return nil, err
	}
	out := make([]baseRPM, len(hs))
	for i, h := range hs {
		out[i] = baseRPM{Header: h, rel: rels[i]}
	}
	return out, nil
}

// firstPublish proves that no client uses the target
// (publish.html#first-publish): the served tree names no other set or
// target, and repo.url serves no repo for the target.
func firstPublish(ctx context.Context, env *Env, target string) error {
	if env.Set.Repo.URL == "" || env.HTTP == nil {
		return nil
	}
	t, _ := env.Set.Target(target)
	base := strings.TrimSuffix(env.Set.Repo.URL, "/") + "/" + strings.Trim(t.ClientPath(), "/")
	get := func(u string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return 0, nil, err
		}
		resp, err := env.HTTP.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		var b []byte
		if resp.StatusCode == http.StatusOK {
			b = make([]byte, 0, 4096)
			buf := make([]byte, 4096)
			for len(b) < 1<<16 {
				n, err := resp.Body.Read(buf)
				b = append(b, buf[:n]...)
				if err != nil {
					break
				}
			}
		}
		return resp.StatusCode, b, nil
	}
	code, body, err := get(base + "/repoctl-snapshot.json")
	if err != nil {
		return api.Errorf(api.PPublishServedExists, "repo.url does not answer for %s: %v", target, err).WithTarget(target)
	}
	if code == http.StatusOK {
		var s struct{ Set, Target string }
		if json.Unmarshal(body, &s) == nil && (s.Set != env.Set.Name || s.Target != target) {
			return api.Errorf(api.PSetRenamed, "the served tree of %s names %s %s", target, s.Set, s.Target).WithTarget(target)
		}
	}
	code, _, err = get(base + "/" + env.Set.Repo.Name + "/repodata/repomd.xml")
	if err != nil || code != http.StatusNotFound {
		why := fmt.Sprintf("answers %d", code)
		if err != nil {
			why = err.Error()
		}
		return api.Errorf(api.PPublishServedExists, "%s/%s/repodata/repomd.xml %s; a first publish needs 404", base, env.Set.Repo.Name, why).
			WithTarget(target).WithHint("import the existing repository", "repoctl repo push --from DIR --manifest FILE --target "+target)
	}
	return nil
}

// newKeyfile extracts the key file of the release RPM with the highest NVR
// that the publish adds, or gives "".
func newKeyfile(ctx context.Context, env *Env, tp *TargetPlan, tmp string) string {
	rel, kf := releaseName(env.Set), tree.KeyfileName(env.Set)
	if rel == "" || kf == "" {
		return ""
	}
	var best *tree.Header
	var bestPath string
	for _, n := range tp.Take {
		for _, f := range n.Files {
			if f.Kind != "rpm" || !strings.HasPrefix(f.File, rel+"-") {
				continue
			}
			h, err := tree.QueryHeader(ctx, f.Path)
			if err != nil || h.Name != rel {
				continue
			}
			if best == nil || manifest.CompareEVR(h.Epoch, h.Version, h.Release, best.Epoch, best.Version, best.Release) > 0 {
				hh := h
				best, bestPath = &hh, f.Path
			}
		}
	}
	if best == nil && tp.SourceDir != "" && tp.SourceDir != tp.BaseDir {
		if p, err := tree.NewestRelease(ctx, filepath.Join(tp.SourceDir, env.Set.Repo.Name), rel); err == nil && p != "" {
			bestPath = p
		}
	}
	if bestPath == "" {
		return ""
	}
	b, ok, err := tree.ExtractFile(ctx, bestPath, kf)
	if err != nil || !ok {
		return ""
	}
	out := filepath.Join(tmp, tp.Target+"-"+kf)
	if os.WriteFile(out, b, 0o644) != nil {
		return ""
	}
	return out
}

// releaseName gives the name of the release package of the set.
func releaseName(set *config.Set) string {
	if p, ok := set.ReleasePackage(); ok {
		return p.ID
	}
	return ""
}

// planDigest gives the sha256 of the summary: per target the base digest
// and the files to sign, the signer, the trust block and the accepted
// advisory ids.
func planDigest(p *Plan) string {
	type tgt struct {
		Base    string          `json:"base"`
		Sign    []string        `json:"sign"`
		Remove  []string        `json:"remove,omitempty"`
		Trust   *manifest.Trust `json:"trust,omitempty"`
		Accept  []string        `json:"accept,omitempty"`
		Held    []string        `json:"held,omitempty"`
		Restore string          `json:"restore,omitempty"`
	}
	d := struct {
		Kind    string         `json:"kind"`
		Signer  string         `json:"signer"`
		Targets map[string]tgt `json:"targets"`
	}{Kind: p.Kind, Signer: p.Signer.Fingerprint, Targets: map[string]tgt{}}
	for _, t := range p.Targets {
		x := tgt{Sign: append([]string{}, t.Resign...)}
		if t.Base != nil {
			x.Base = t.Base.Digest()
		}
		for _, n := range t.Take {
			for _, f := range n.Files {
				x.Sign = append(x.Sign, f.File+"@"+f.SHA256)
			}
		}
		for r := range t.Remove {
			x.Remove = append(x.Remove, r)
		}
		sort.Strings(x.Sign)
		sort.Strings(x.Remove)
		if t.Gate != nil {
			x.Trust = t.Gate.Trust
		}
		x.Accept = p.Req.Accept
		for _, h := range t.Held {
			x.Held = append(x.Held, h.Node)
		}
		if t.RefSnap != nil {
			x.Restore = t.RefSnap.Digest()
		}
		d.Targets[t.Target] = x
	}
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
