package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/localcopy"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/publish/tree"
	"github.com/dennisklein/repoctl/internal/records"
	"github.com/dennisklein/repoctl/internal/snapshot"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// runner holds the state of one publish run.
type runner struct {
	env   *Env
	p     *Plan
	run   *api.Run
	j     Journal
	files map[string][]snapshot.File
	metas map[string]snapshot.Meta
	descs map[string]ocispec.Descriptor
	// cur is the target of the tree that a step works on.
	cur string
}

// Result is the result of Execute and Complete.
type Result struct {
	Run *api.Run
	// Stats gives the push counts per target.
	Stats    map[string]snapshot.PushStats
	Warnings []api.Problem
}

func (r *runner) value(t string) api.RunPublishValue { return r.run.Publish[t] }

func (r *runner) setValue(t string, v api.RunPublishValue) { r.run.Publish[t] = v }

func (r *runner) save() error { return WriteState(r.env.Paths, r.env.Set.Name, r.p.ID, r.run) }

// step records the end of a step of a target in the state file and the
// journal.
func (r *runner) step(t, name, state string, start time.Time, count int, dg api.Digest) {
	d := r.env.Now().Sub(start)
	v := r.value(t)
	v.Steps = append(v.Steps, api.RunPublishValueStep{Step: name, State: state, DurationMs: api.DurationMs(d.Milliseconds()), Count: count})
	r.setValue(t, v)
	_ = r.j.Append(api.EventTypePublishStep, api.PublishStep{Target: t, Step: name, Phase: "ended", Status: state, Count: count, Digest: dg})
	_ = r.save()
	if r.env.Progress != nil {
		r.env.Progress(t, name, state, d)
	}
}

func (r *runner) begin(t, name string) time.Time {
	_ = r.j.Append(api.EventTypePublishStep, api.PublishStep{Target: t, Step: name, Phase: "started"})
	return r.env.Now()
}

// newRun makes the Run of the state file.
func newRun(env *Env, p *Plan) *api.Run {
	run := &api.Run{
		ID: p.RunID, Kind: "publish", Set: env.Set.Name, State: api.RunStateRunning,
		Origin:    api.RunOriginItem{Via: env.Via, User: env.User, Host: env.Host, PID: os.Getpid()},
		SetCommit: p.SetCommit, StartedAt: api.FormatTime(p.Started),
		Files:  api.RunFiles{Journal: env.Paths.Journal(env.Set.Name, p.RunID)},
		Signer: &p.Signer, PlanDigest: p.Digest, Publish: map[string]api.RunPublishValue{}, PublishKind: p.Kind,
		Nodes: []api.NodeResult{},
	}
	if run.Origin.Via == "" {
		run.Origin.Via = "cli"
	}
	run.FromRuns = append(run.FromRuns, p.FromRuns...)
	for _, t := range p.Targets {
		v := api.RunPublishValue{State: "running", Store: t.Store.Ref, Held: t.Held, Sign: &api.RunPublishValueSign{New: t.Sign.New, Keep: t.Sign.Keep, Resign: t.Sign.Resign}}
		if t.Base != nil {
			v.Base = t.Base.Digest()
		}
		if t.Advisory != nil {
			v.Gate = t.Advisory.Gate
		}
		v.Rebuild = append(v.Rebuild, t.Rebuild...)
		run.Publish[t.Target] = v
		for _, n := range t.Take {
			nr := api.NodeResult{Node: n.Node, Package: n.Package, Target: n.Target, State: api.NodeStateStaged, Commit: n.Commit, SRPM: n.Draft.SRPM}
			for _, f := range n.Files {
				if f.Kind == "rpm" {
					nr.NVRs = append(nr.NVRs, f.NVRA)
				}
			}
			sort.Strings(nr.NVRs)
			run.Nodes = append(run.Nodes, nr)
			run.Counts.Staged++
		}
	}
	return run
}

// Execute runs steps 2 to 8 of a prepared plan. Steps 2 to 5 run for each
// target before the first push and stop all targets on a failure; steps 6
// to 8 run for one target after the other.
func Execute(ctx context.Context, env *Env, p *Plan) (*Result, error) {
	j, err := OpenJournal(env.Paths.Journal(env.Set.Name, p.RunID), p.RunID, env.Now)
	if err != nil {
		return nil, err
	}
	defer j.Close()
	r := &runner{env: env, p: p, run: newRun(env, p), j: j, files: map[string][]snapshot.File{}, metas: map[string]snapshot.Meta{}, descs: map[string]ocispec.Descriptor{}}
	res := &Result{Run: r.run, Stats: map[string]snapshot.PushStats{}}
	trace := traceID(p.RunID)
	_ = j.Append(api.EventTypeRunStarted, map[string]any{
		"set": env.Set.Name, "kind": "publish", "trace": trace, "publishKind": p.Kind, "setCommit": p.SetCommit,
		"repoctl": env.Repoctl, "origin": map[string]any{"via": string(r.run.Origin.Via), "user": env.User, "host": env.Host, "pid": os.Getpid()},
	})
	ps := api.PublishStarted{Targets: toTargets(p.TargetIDs()), By: env.User + "@" + env.Host, Signer: p.Signer, Base: map[string]api.Digest{}, Unattended: p.Req.Unattended}
	ps.FromRuns = append(ps.FromRuns, p.FromRuns...)
	for _, t := range p.Targets {
		if t.Base != nil {
			ps.Base[t.Target] = t.Base.Digest()
		}
	}
	if len(ps.Base) == 0 {
		ps.Base = nil
	}
	_ = j.Append(api.EventTypePublishStarted, ps)
	for _, t := range p.Targets {
		s := r.begin(t.Target, "check")
		r.step(t.Target, "check", StepOK, s, 0, "")
	}
	// Steps 2 to 5 for each target.
	for _, t := range p.Targets {
		if err := r.prepareTree(ctx, t); err != nil {
			return r.finish(res, err)
		}
	}
	// Steps 6 to 8 for one target after the other.
	var failed []error
	for _, t := range p.Targets {
		if ctx.Err() != nil {
			v := r.value(t.Target)
			v.State, v.Pushable = StepCanceled, true
			r.setValue(t.Target, v)
			failed = append(failed, ctx.Err())
			continue
		}
		base := api.Digest("")
		if t.Base != nil {
			base = t.Base.Digest()
		}
		st, warn, err := r.pushTarget(context.WithoutCancel(ctx), t.Target, base)
		res.Stats[t.Target] = st
		if warn != nil {
			res.Warnings = append(res.Warnings, *warn)
		}
		if err != nil {
			failed = append(failed, err)
		}
	}
	if len(failed) > 0 {
		err := failed[0]
		if len(failed) < len(p.Targets) {
			if e, ok := api.AsError(err); ok {
				err = e.WithExit(api.ExitStateConflict)
			}
		}
		return r.finish(res, err)
	}
	return r.finish(res, nil)
}

func toTargets(ids []string) []api.TargetId { return append([]api.TargetId{}, ids...) }

// traceID derives the trace id of a run from its id.
func traceID(run string) string {
	return tree.SHA256Bytes([]byte("trace/" + run))[:32]
}

// finish writes the end of the run.
func (r *runner) finish(res *Result, err error) (*Result, error) {
	now := r.env.Now()
	r.run.FinishedAt = api.FormatTime(now)
	r.run.DurationMs = api.DurationMs(now.Sub(r.p.Started).Milliseconds())
	state := api.RunStateOk
	code := 0
	if err != nil {
		state = api.RunStateFailed
		code = exitOf(err)
		if errors.Is(err, context.Canceled) {
			state, code = api.RunStateCanceled, api.ExitInterrupted
		}
	}
	r.run.State, r.run.ExitCode = state, code
	snaps := map[string]api.PublishFinishedSnapshotsValue{}
	for t, v := range r.run.Publish {
		if v.State == "running" {
			v.State = StepFailed
			if err != nil && errors.Is(err, context.Canceled) {
				v.State = StepCanceled
			}
			r.run.Publish[t] = v
		}
		if v.State == StepOK && v.Snapshot != nil {
			snaps[t] = api.PublishFinishedSnapshotsValue{Tag: v.Snapshot.Tag, Digest: v.Snapshot.Digest}
		}
	}
	pf := api.PublishFinished{State: string(state), Targets: toTargets(r.p.TargetIDs()), ExitCode: code}
	if len(snaps) > 0 {
		pf.Snapshots = snaps
	}
	_ = r.j.Append(api.EventTypePublishFinished, pf)
	_ = r.j.Append(api.EventTypeRunFinished, api.RunFinished{State: state, Counts: r.run.Counts, DurationMs: r.run.DurationMs, ExitCode: code})
	if serr := r.save(); serr != nil && err == nil {
		err = serr
	}
	return res, err
}

func exitOf(err error) int {
	if e, ok := api.AsError(err); ok {
		return e.ExitCode
	}
	if errors.Is(err, context.Canceled) {
		return api.ExitInterrupted
	}
	return api.ExitFailed
}

// prepareTree runs steps 2 to 5 for one target. A failure of sign or
// repodata deletes the tree; a failure of verify or verify-el keeps it.
func (r *runner) prepareTree(ctx context.Context, t *TargetPlan) error {
	env, p := r.env, r.p
	dir := TreeDir(env.Paths, env.Set.Name, p.ID, t.Target)
	r.cur = t.Target
	fail := func(step string, start time.Time, err error, keep bool) error {
		state := StepFailed
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			state, keep = StepCanceled, false
		}
		r.step(t.Target, step, state, start, 0, "")
		if !keep {
			_ = os.RemoveAll(dir)
		}
		if e, ok := api.AsError(err); ok && e.Target == "" {
			e.Target = t.Target
		}
		return err
	}
	s := r.begin(t.Target, "sign")
	signed, err := r.signStep(ctx, t, dir)
	if err != nil {
		return fail("sign", s, err, false)
	}
	r.step(t.Target, "sign", StepOK, s, signed, "")
	s = r.begin(t.Target, "repodata")
	if err := r.repodataStep(ctx, t, dir); err != nil {
		return fail("repodata", s, err, false)
	}
	r.step(t.Target, "repodata", StepOK, s, 0, "")
	s = r.begin(t.Target, "verify")
	n, err := r.verifyStep(ctx, t, dir)
	if err != nil {
		return fail("verify", s, err, true)
	}
	r.step(t.Target, "verify", StepOK, s, n, "")
	s = r.begin(t.Target, "verify-el")
	keys := []string{}
	if t.Gate != nil {
		keys = append(keys, t.Gate.ClientKeys...)
	}
	if len(keys) == 0 || t.First {
		if kf := tree.KeyfileName(env.Set); kf != "" && fileExists(filepath.Join(dir, kf)) {
			keys = []string{filepath.Join(dir, kf)}
		}
	}
	if err := env.EL.VerifyEL(ctx, t.Target, dir, keys); err != nil {
		return fail("verify-el", s, err, true)
	}
	// The manifest of the snapshot: its digest is known before the push.
	files, err := snapshot.Scan(dir)
	if err != nil {
		return fail("verify-el", s, err, true)
	}
	meta := r.meta(t)
	_, desc, err := snapshot.Encode(snapshot.NewManifest(files, meta))
	if err != nil {
		return fail("verify-el", s, err, true)
	}
	if err := writeMeta(env, p.ID, t.Target, meta); err != nil {
		return fail("verify-el", s, err, true)
	}
	r.files[t.Target], r.metas[t.Target], r.descs[t.Target] = files, meta, desc
	v := r.value(t.Target)
	v.Snapshot = &api.RunPublishValueSnapshot{Tag: p.ID, Digest: desc.Digest.String(), Parent: v.Base}
	r.setValue(t.Target, v)
	r.step(t.Target, "verify-el", StepOK, s, 0, "")
	v = r.value(t.Target)
	v.Pushable = true
	r.setValue(t.Target, v)
	return r.save()
}

// meta gives the annotations of the new snapshot of a target.
func (r *runner) meta(t *TargetPlan) snapshot.Meta {
	env, p := r.env, r.p
	m := snapshot.Meta{Set: env.Set.Name, Target: t.Target, Publish: p.ID, Kind: p.Kind, SetCommit: p.SetCommit,
		Signer: p.Signer.Fingerprint, User: env.User, Host: env.Host, Created: p.Started.UTC().Truncate(time.Second), Repoctl: env.Repoctl,
		Unattended: p.Req.Unattended, TraceParent: p.Req.TraceParent, Origin: string(r.run.Origin.Via)}
	if t.Base != nil {
		m.Parent = t.Base.Digest()
	}
	if u := setSource(env.Set.Remote(), env.SetDir); u != "" {
		m.Source = u
	}
	if t.Advisory != nil {
		for _, a := range t.Advisory.Accepted {
			m.Accepted = append(m.Accepted, api.SnapshotAcceptedItem{ID: a.ID, FactsDigest: a.FactsDigest, Reason: a.Reason, User: env.User, Host: env.Host})
		}
	}
	for _, h := range t.Held {
		m.Held = append(m.Held, api.SnapshotHeldItem(h))
	}
	return m
}

// setSource gives the URL of the set repository for
// org.opencontainers.image.source, or "".
func setSource(remote, setDir string) string {
	out, err := git.New(setDir).Run(context.Background(), "config", "--get", "remote."+remote+".url")
	if err != nil {
		return ""
	}
	u := strings.TrimSpace(out)
	if strings.HasPrefix(u, "https://") {
		return strings.TrimSuffix(u, ".git")
	}
	return ""
}

func metaPath(env *Env, id, target string) string {
	return filepath.Join(env.Paths.Publish(env.Set.Name, id), "meta-"+target+".json")
}

func writeMeta(env *Env, id, target string, m snapshot.Meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath(env, id, target), append(b, '\n'), 0o644)
}

func readMeta(env *Env, id, target string) (snapshot.Meta, error) {
	var m snapshot.Meta
	b, err := os.ReadFile(metaPath(env, id, target))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// signStep is step 2: it makes the new tree from hard links of the source
// tree, adds the new RPMs and signs them.
func (r *runner) signStep(ctx context.Context, t *TargetPlan, dir string) (int, error) {
	env, p := r.env, r.p
	layout := tree.LayoutOf(env.Set)
	if err := os.RemoveAll(dir); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	var sign []string
	switch {
	case t.RefSnap != nil:
		have, err := localcopy.Have(t.BaseDir)
		if err != nil {
			return 0, err
		}
		if _, err := t.Store.Download(ctx, t.RefSnap, dir, have); err != nil {
			return 0, err
		}
		for _, f := range t.Base.Files {
			if !t.Remove[f.Path] {
				if _, err := os.Stat(filepath.Join(dir, f.Path)); os.IsNotExist(err) {
					if err := linkInto(filepath.Join(t.BaseDir, f.Path), filepath.Join(dir, f.Path)); err != nil {
						return 0, err
					}
				}
			}
		}
		resign, err := removedSigners(ctx, env, dir, layout)
		if err != nil {
			return 0, err
		}
		t.Sign.Resign = len(resign)
		sign = append(sign, resign...)
	case p.Kind == KindImport:
		err := filepath.WalkDir(t.SourceDir, func(src string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".rpm") {
				return err
			}
			rel := layout.PathOf(d.Name())
			if rel == "" {
				return nil
			}
			return linkInto(src, filepath.Join(dir, rel))
		})
		if err != nil {
			return 0, err
		}
	case t.SourceDir != "":
		err := filepath.WalkDir(t.SourceDir, func(src string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(t.SourceDir, src)
			rel = filepath.ToSlash(rel)
			if snapshot.Skip(rel) || t.Remove[rel] || strings.Contains(rel, "/repodata/") {
				return nil
			}
			return linkInto(src, filepath.Join(dir, rel))
		})
		if err != nil {
			return 0, err
		}
	}
	for _, n := range t.Take {
		for _, f := range n.Files {
			rel := layout.PathOf(f.File)
			if rel == "" || t.SkipFiles[f.File] {
				continue
			}
			got, err := tree.SHA256File(f.Path)
			if err != nil || got != f.SHA256 {
				return 0, api.Errorf(api.PRunStagingMissing, "%s of %s changed in the staging of %s", f.File, n.Node, n.Run).WithNode(n.Node)
			}
			dst := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return 0, err
			}
			if err := snapshot.CopyFile(f.Path, dst); err != nil {
				return 0, err
			}
			sign = append(sign, dst)
		}
	}
	for _, rel := range t.Resign {
		dst := filepath.Join(dir, rel)
		if err := breakLink(dst); err != nil {
			return 0, err
		}
		sign = append(sign, dst)
	}
	if len(sign) > 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := env.Signer.SignRPMs(ctx, sign); err != nil {
			return 0, err
		}
	}
	return len(sign), nil
}

// removedSigners gives the RPMs of a tree that a key in state removed or
// revoked signed (restore re-signs them).
func removedSigners(ctx context.Context, env *Env, dir string, l tree.Layout) ([]string, error) {
	bad := map[string]bool{}
	for _, k := range env.Set.Sign.Keys {
		if k.State == "removed" || k.State == "revoked" {
			bad[normFpr(k.Fingerprint)] = true
		}
	}
	if len(bad) == 0 {
		return nil, nil
	}
	hs, err := baseRPMs(ctx, dir, l)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, h := range hs {
		for f := range bad {
			if h.KeyID != "" && strings.HasSuffix(f, strings.ToUpper(h.KeyID)) {
				p := filepath.Join(dir, h.rel)
				if err := breakLink(p); err != nil {
					return nil, err
				}
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// linkInto links src to dst, making the directory.
func linkInto(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return snapshot.LinkOrCopy(src, dst)
}

// breakLink replaces a hard link by a copy, so that a new signature does
// not change the file of the local copy.
func breakLink(p string) error {
	tmp := p + ".copy"
	if err := snapshot.CopyFile(p, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// rpmInfo is one RPM of the new tree.
type rpmInfo struct {
	tree.Header
	rel    string
	sha256 string
}

// treeRPMs reads the RPMs of the new tree, by file name.
func treeRPMs(ctx context.Context, dir string, l tree.Layout) (map[string]rpmInfo, error) {
	hs, err := baseRPMs(ctx, dir, l)
	if err != nil {
		return nil, err
	}
	out := map[string]rpmInfo{}
	for _, h := range hs {
		s, err := tree.SHA256File(filepath.Join(dir, h.rel))
		if err != nil {
			return nil, err
		}
		out[path.Base(h.rel)] = rpmInfo{Header: h.Header, rel: h.rel, sha256: s}
	}
	return out, nil
}

// buildFiles gives the files of a build in the tree: the SRPM, the binary
// RPMs and the debuginfo RPMs whose source RPM is the SRPM.
func buildFiles(b manifest.Build, rpms map[string]rpmInfo) []rpmInfo {
	var out []rpmInfo
	seen := map[string]bool{}
	add := func(f string) {
		if i, ok := rpms[f]; ok && !seen[f] {
			seen[f] = true
			out = append(out, i)
		}
	}
	add(b.SRPM)
	for _, f := range b.RPMs {
		add(f)
	}
	names := make([]string, 0, len(rpms))
	for n := range rpms {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if rpms[n].Sourcerpm == b.SRPM && strings.Contains(n, "-debug") {
			add(n)
		}
	}
	return out
}

// repodataStep is step 3: the records, the RepoManifest, the root files,
// createrepo_c, modifyrepo_c and the signature of each repomd.xml.
func (r *runner) repodataStep(ctx context.Context, t *TargetPlan, dir string) error {
	env, p := r.env, r.p
	layout := tree.LayoutOf(env.Set)
	bin := filepath.Join(dir, layout.Name)
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	rpms, err := treeRPMs(ctx, dir, layout)
	if err != nil {
		return err
	}
	m := r.newManifest(t)
	ns := records.Namespace(env.Set.Store.OCI)
	writeRec := func(rel string, v any) (*manifest.Record, error) {
		b, err := records.Marshal(v)
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(bin, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return nil, err
		}
		return &manifest.Record{Path: rel, SHA256: tree.SHA256Bytes(b)}, nil
	}
	setRPM := func(nevra string) string { return "" }
	// New builds of the staged nodes.
	for _, n := range t.Take {
		b := n.Draft
		b.Package, b.Commit, b.Run = n.Package, n.Commit, n.Run
		b.RPMs = nil
		for _, f := range n.Files {
			switch f.Kind {
			case "rpm":
				b.RPMs = append(b.RPMs, f.File)
			case "srpm":
				b.SRPM = f.File
			}
		}
		sort.Strings(b.RPMs)
		b.Publish, b.Signer, b.Records = p.ID, p.Signer.Fingerprint, nil
		nvr := manifest.NVR(b.SRPM)
		var br *records.BuildrootFile
		brSum := ""
		if n.Buildroot != "" {
			if raw, err := os.ReadFile(n.Buildroot); err == nil {
				if br, err = records.ParseBuildroot(raw); err == nil {
					rel := records.BuildrootPath(nvr)
					dst := filepath.Join(bin, rel)
					if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
						return err
					}
					if err := os.WriteFile(dst, raw, 0o644); err != nil {
						return err
					}
					b.BuildrootFile, brSum = rel, tree.SHA256Bytes(raw)
				}
			}
		}
		if err := r.addRecords(&b, rpms, ns, n.Facts, n.SourceURIs, br, brSum, setRPM, writeRec, true); err != nil {
			return err
		}
		m.Builds = append(m.Builds, b)
	}
	// Builds whose RPMs a re-sign changed get new records.
	if p.Kind == KindResign || p.Kind == KindRestore {
		changed := map[string]bool{}
		for _, rel := range t.Resign {
			changed[path.Base(rel)] = true
		}
		for i := range m.Builds {
			b := &m.Builds[i]
			touched := false
			for _, f := range buildFiles(*b, rpms) {
				if changed[path.Base(f.rel)] {
					touched = true
				}
			}
			if touched {
				b.Signer = p.Signer.Fingerprint
				prov := b.Records != nil && b.Records.Provenance != nil
				if err := r.addRecords(b, rpms, ns, records.Facts{}, nil, nil, "", setRPM, writeRec, prov); err != nil {
					return err
				}
			}
		}
	}
	// Adopted builds of an import get an SBOM and no provenance.
	if p.Kind == KindImport {
		for i := range m.Builds {
			if err := r.addRecords(&m.Builds[i], rpms, ns, records.Facts{}, nil, nil, "", setRPM, writeRec, false); err != nil {
				return err
			}
		}
	}
	// The repository SBOM.
	var refs []records.BuildSBOMRef
	for _, b := range m.Builds {
		if b.Records != nil && b.Records.SBOM != nil {
			refs = append(refs, records.BuildSBOMRef{NVR: manifest.NVR(b.SRPM), Path: b.Records.SBOM.Path, SHA256: b.Records.SBOM.SHA256})
		}
	}
	rec, err := writeRec(records.RepositorySBOMPath, records.NewRepositorySBOM(records.RepoSBOMInput{
		Set: env.Set.Name, Target: t.Target, Namespace: ns, Store: env.Set.Store.OCI, Publish: p.ID, Created: p.Started, Repoctl: env.Repoctl, Builds: refs}))
	if err != nil {
		return err
	}
	m.Records = &manifest.Records{SBOM: rec}
	m.History = append(m.History, r.history(t))
	if err := m.Validate(); err != nil {
		return err
	}
	// Root files.
	if rel, kf := releaseName(env.Set), tree.KeyfileName(env.Set); rel != "" {
		for _, old := range []string{filepath.Join(dir, rel+".rpm"), filepath.Join(dir, kf)} {
			_ = os.Remove(old)
		}
		newest, err := tree.NewestRelease(ctx, bin, rel)
		if err != nil {
			return err
		}
		if newest != "" {
			if err := snapshot.LinkOrCopy(newest, filepath.Join(dir, rel+".rpm")); err != nil {
				return err
			}
			if kf != "" {
				key, ok, err := tree.ExtractFile(ctx, newest, kf)
				if err != nil {
					return err
				}
				if ok {
					if err := os.WriteFile(filepath.Join(dir, kf), key, 0o644); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, repo := range layout.Repos() {
		rd := filepath.Join(dir, repo.Dir)
		_ = os.RemoveAll(filepath.Join(rd, "repodata"))
		if err := tree.Createrepo(ctx, rd); err != nil {
			return err
		}
	}
	raw, err := m.Marshal()
	if err != nil {
		return err
	}
	mdDir, err := os.MkdirTemp(env.Paths.Publish(env.Set.Name, p.ID), "md-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mdDir)
	mf := filepath.Join(mdDir, manifest.MDType+".json")
	if err := os.WriteFile(mf, raw, 0o644); err != nil {
		return err
	}
	if err := tree.AddMetadata(ctx, bin, mf, manifest.MDType); err != nil {
		return err
	}
	for _, repo := range layout.Repos() {
		md := filepath.Join(dir, repo.Dir, "repodata", "repomd.xml")
		if err := env.Signer.SignDetached(ctx, md, md+".asc"); err != nil {
			return err
		}
	}
	return nil
}

// addRecords writes the SBOM and, with prov, the provenance record of a
// build, and sets b.Records.
func (r *runner) addRecords(b *manifest.Build, rpms map[string]rpmInfo, ns string, facts records.Facts, uris map[string]string,
	br *records.BuildrootFile, brSum string, setRPM func(string) string, write func(string, any) (*manifest.Record, error), prov bool) error {
	env, p := r.env, r.p
	t := r.currentTarget(b)
	nvr := manifest.NVR(b.SRPM)
	files := buildFiles(*b, rpms)
	var subjects []records.Subject
	in := records.SBOMInput{Set: env.Set.Name, Target: t, Namespace: ns, Publish: p.ID, Created: p.Started, Repoctl: env.Repoctl, Build: *b, SourceURIs: uris}
	for _, f := range files {
		subjects = append(subjects, records.Subject{Name: path.Base(f.rel), SHA256: f.sha256})
		info := records.RPMInfo{File: path.Base(f.rel), SHA256: f.sha256, Name: f.Name, Version: f.Version, Release: f.Release, Arch: f.Arch,
			License: f.License, Kind: tree.KindOfFile(path.Base(f.rel)), Provides: f.Provides, Requires: f.Requires}
		if info.Kind == "srpm" {
			in.SRPM = info
		} else {
			in.RPMs = append(in.RPMs, info)
		}
	}
	if in.SRPM.File == "" {
		n, v, rel := manifest.SplitNVR(nvr)
		in.SRPM = records.RPMInfo{File: b.SRPM, Name: n, Version: v, Release: rel, Kind: "srpm"}
	}
	b.Records = &manifest.Records{}
	sb, err := write(records.SBOMPath(nvr), records.NewBuildSBOM(in))
	if err != nil {
		return err
	}
	b.Records.SBOM = sb
	if !prov {
		return nil
	}
	if facts.Repoctl == "" {
		facts.Repoctl = env.Repoctl
	}
	repo := env.SetDir
	if u := setSource(env.Set.Remote(), env.SetDir); u != "" {
		repo = u
	}
	if b.URL != nil && *b.URL != "" {
		repo = *b.URL
	}
	st := records.NewProvenance(records.BuildInput{Set: env.Set.Name, Target: t, Namespace: ns, Repo: repo, Build: *b, Facts: facts,
		Subjects: subjects, SourceURIs: uris, Buildroot: br, BuildrootSHA256: brSum, SetRPM: setRPM, Distro: distroOf(env, t)})
	pr, err := write(records.ProvenancePath(nvr), st)
	if err != nil {
		return err
	}
	b.Records.Provenance = pr
	return nil
}

// currentTarget is the target of the tree that repodataStep works on.
func (r *runner) currentTarget(_ *manifest.Build) string { return r.cur }

func distroOf(env *Env, target string) string {
	t, ok := env.Set.Target(target)
	if !ok {
		return ""
	}
	if i := strings.IndexAny(t.Mock, "+-"); i > 0 {
		return t.Mock[:i]
	}
	return ""
}

// newManifest gives the RepoManifest of the new tree before the new
// builds: the base, REF of a restore or the RepoManifest of an import, with
// removed[] updated.
func (r *runner) newManifest(t *TargetPlan) *manifest.Manifest {
	env, p := r.env, r.p
	m := manifest.New(env.Set.Name, t.Target)
	src := t.BaseManifest
	switch {
	case t.Import != nil:
		src = t.Import
	case t.RefManifest != nil:
		m.Builds = append(m.Builds, t.RefManifest.Builds...)
		for _, b := range t.BaseManifest.Builds {
			if containsStr(t.KeptRelease, manifest.NVR(b.SRPM)) {
				m.Builds = append(m.Builds, b)
			}
		}
	}
	if src != nil {
		if t.RefManifest == nil {
			m.Builds = append(m.Builds, src.Builds...)
		}
		m.Removed = append(m.Removed, src.Removed...)
		m.History = append(m.History, src.History...)
	}
	if t.BaseManifest != nil && t.RefManifest != nil {
		m.Removed = append([]manifest.Removed{}, t.BaseManifest.Removed...)
		m.History = append([]manifest.History{}, t.BaseManifest.History...)
	}
	if len(t.Unremoved) > 0 {
		var keep []manifest.Removed
		for _, rm := range m.Removed {
			if !containsStr(t.Unremoved, rm.NVR) {
				keep = append(keep, rm)
			}
		}
		m.Removed = keep
	}
	m.Removed = append(m.Removed, t.Removed...)
	if p.Kind == KindPrune {
		gone := map[string]bool{}
		for _, rm := range t.Removed {
			gone[rm.NVR] = true
		}
		var keep []manifest.Build
		for _, b := range m.Builds {
			if !gone[manifest.NVR(b.SRPM)] {
				keep = append(keep, b)
				continue
			}
			left := []string{}
			for _, f := range b.RPMs {
				if !t.Remove[tree.LayoutOf(env.Set).PathOf(f)] {
					left = append(left, f)
				}
			}
			if len(left) > 0 {
				b.RPMs = left
				keep = append(keep, b)
			}
		}
		m.Builds = keep
	}
	if m.Builds == nil {
		m.Builds = []manifest.Build{}
	}
	if m.Removed == nil {
		m.Removed = []manifest.Removed{}
	}
	m.Publish = p.ID
	return m
}

// history gives the history entry of this publish.
func (r *runner) history(t *TargetPlan) manifest.History {
	env, p := r.env, r.p
	h := manifest.History{Publish: p.ID, Kind: p.Kind, User: env.User, Host: env.Host, Signer: p.Signer.Fingerprint,
		SetCommit: p.SetCommit, Created: api.FormatTime(p.Started)}
	if t.Base != nil {
		h.Parent = t.Base.Digest()
	}
	if t.Advisory != nil {
		h.Accepted = t.Advisory.Accepted
	}
	for _, x := range t.Held {
		h.Held = append(h.Held, manifest.Held{Node: x.Node, Reason: x.Reason})
	}
	if t.Gate != nil {
		h.Trust = t.Gate.Trust
	}
	return h
}

// verifyStep is step 4: the tree on the host with only the set keys.
func (r *runner) verifyStep(ctx context.Context, t *TargetPlan, dir string) (int, error) {
	env, p := r.env, r.p
	kr, err := tree.NewKeyring(ctx, localcopy.Keys(env.Set, env.SetDir))
	if err != nil {
		return 0, err
	}
	defer kr.Close()
	o := tree.VerifyOptions{Layout: tree.LayoutOf(env.Set), Keyring: kr, Records: true}
	rel := releaseName(env.Set)
	if rel != "" && fileExists(filepath.Join(dir, rel+".rpm")) {
		o.Keyfile, o.Release = tree.KeyfileName(env.Set), rel
	}
	if t.Base != nil && t.RefSnap == nil && p.Kind != KindImport {
		o.Base = map[string]string{}
		for _, f := range t.Base.Files {
			o.Base[f.Path] = f.SHA256()
		}
		resign := map[string]bool{}
		for _, x := range t.Resign {
			resign[x] = true
		}
		o.Changed = func(rel string) bool {
			return t.Remove[rel] || resign[rel] || !strings.Contains(rel, "/") ||
				rel == env.Set.Repo.Name+"/"+records.RepositorySBOMPath ||
				(p.Kind == KindResign && strings.Contains(rel, "/"+records.Dir+"/"))
		}
	}
	res, err := tree.Verify(ctx, dir, o)
	if err != nil {
		return 0, err
	}
	if err := res.Err(); err != nil {
		if e, ok := api.AsError(err); ok {
			e.Target = t.Target
			_ = e.WithHint("the tree stays for a look", "ls "+dir)
		}
		return 0, err
	}
	t.Warnings = append(t.Warnings, res.Warnings...)
	return res.RPMs, nil
}

// pushTarget runs steps 6 to 8 for one target. base is the digest of the
// base, "" for a first publish.
func (r *runner) pushTarget(ctx context.Context, target string, base api.Digest) (snapshot.PushStats, *api.Problem, error) {
	env, p := r.env, r.p
	var st snapshot.PushStats
	store, err := env.Store(target)
	if err != nil {
		return st, nil, err
	}
	desc, files, meta := r.descs[target], r.files[target], r.metas[target]
	hint := func(e *api.Error) *api.Error {
		return e.WithTarget(target).WithHint("complete the publish", "repoctl repo push "+p.ID+" --target "+target)
	}
	failed := func(step string, s time.Time, err error) (snapshot.PushStats, *api.Problem, error) {
		r.step(target, step, StepFailed, s, 0, "")
		v := r.value(target)
		v.State = StepFailed
		r.setValue(target, v)
		_ = r.save()
		if e, ok := api.AsError(err); ok {
			if e.Code != api.PSnapshotDiverged {
				e = hint(e)
			}
			return st, nil, e
		}
		return st, nil, hint(api.Errorf(api.PSnapshotPushFailed, "%s: %v", step, err).WithCause(err))
	}
	diverged := func() error {
		return api.Errorf(api.PSnapshotDiverged, "latest of %s moved since the base %s: another publish is in latest; %s stays an unserved tag", target, api.ShortDigest(base), p.ID).
			WithTarget(target).WithHint("pull, then plan and publish again", "repoctl repo pull --target "+target)
	}
	// isBase reports whether latest is still the base.
	isBase := func(d ocispec.Descriptor, ok bool) bool {
		if base == "" {
			return !ok
		}
		return ok && d.Digest.String() == base
	}
	latest, ok, err := store.Latest(ctx)
	if err != nil {
		return failed("push", env.Now(), err)
	}
	if ok && latest.Digest == desc.Digest {
		// Step 7 is done: only the cache step runs.
		s := r.begin(target, "push")
		r.step(target, "push", StepSkipped, s, 0, "")
		s = r.begin(target, "tag")
		r.step(target, "tag", StepSkipped, s, 0, desc.Digest.String())
	} else {
		if !isBase(latest, ok) {
			s := r.begin(target, "push")
			r.step(target, "push", StepSkipped, s, 0, "")
			return failed("tag", env.Now(), diverged())
		}
		s := r.begin(target, "push")
		d, stats, err := store.Push(ctx, files, meta, p.ID)
		st = stats
		if err != nil {
			return failed("push", s, err)
		}
		if d.Digest != desc.Digest {
			return failed("push", s, api.Errorf(api.PSnapshotPushFailed, "the pushed manifest %s is not %s", d.Digest, desc.Digest))
		}
		r.step(target, "push", StepOK, s, stats.New, d.Digest.String())
		s = r.begin(target, "tag")
		latest, ok, err = store.Latest(ctx)
		if err != nil {
			return failed("tag", s, err)
		}
		if !isBase(latest, ok) && (!ok || latest.Digest != desc.Digest) {
			return failed("tag", s, diverged())
		}
		terr := store.Tag(ctx, desc, snapshot.TagLatest)
		again, ok2, err2 := store.Latest(ctx)
		switch {
		case err2 == nil && ok2 && again.Digest == desc.Digest:
		case terr != nil:
			return failed("tag", s, terr)
		case err2 != nil:
			return failed("tag", s, err2)
		default:
			return failed("tag", s, diverged())
		}
		r.step(target, "tag", StepOK, s, 0, desc.Digest.String())
	}
	v := r.value(target)
	v.State = StepOK
	r.setValue(target, v)
	s := r.begin(target, "cache")
	if warn := r.cache(store, target, desc, files, meta); warn != nil {
		r.step(target, "cache", StepFailed, s, 0, "")
		return st, warn, nil
	}
	v = r.value(target)
	v.Pushable = false
	r.setValue(target, v)
	r.step(target, "cache", StepOK, s, 0, "")
	return st, nil, nil
}

// Complete runs repo push ID: steps 6 to 8 of each pushable target of a
// kept tree, or of one target (publish.html#complete). Without an id it
// takes the newest pushable publish.
func Complete(ctx context.Context, env *Env, id, target string) (*Result, error) {
	if id == "" {
		kept, err := KeptTrees(env.Paths, env.Set.Name)
		if err != nil {
			return nil, err
		}
		for _, k := range kept {
			for t, v := range k.Run.Publish {
				if IsPushable(v) && (target == "" || t == target) {
					id = k.ID
				}
			}
			if id != "" {
				break
			}
		}
		if id == "" {
			return nil, api.Errorf(api.PPublishNothingToPublish, "no kept tree is pushable").WithHint("", "repoctl status")
		}
	}
	run, err := ReadState(env.Paths, env.Set.Name, id)
	if err != nil {
		return nil, err
	}
	var targets []string
	for _, t := range env.Set.Targets {
		v, ok := run.Publish[t.ID]
		if !ok || (target != "" && t.ID != target) {
			continue
		}
		if StepState(v, "verify") != StepOK || StepState(v, "verify-el") != StepOK {
			return nil, api.Errorf(api.PPublishNotVerified, "%s of %s did not pass verify and verify-el; repoctl never pushes it", id, t.ID).
				WithTarget(t.ID).WithHint("discard the tree", "repoctl clean --publish "+id)
		}
		if StepState(v, "cache") == StepOK {
			continue
		}
		targets = append(targets, t.ID)
	}
	if target != "" && len(targets) == 0 {
		if _, ok := run.Publish[target]; !ok {
			return nil, api.Errorf(api.PSnapshotNotFound, "%s has no tree of %s", id, target).WithTarget(target)
		}
	}
	if len(targets) == 0 {
		return nil, api.Errorf(api.PPublishNothingToPublish, "%s is complete", id).WithHint("", "repoctl status")
	}
	started, _ := api.ParseTime(run.StartedAt)
	p := &Plan{ID: id, RunID: run.ID, Kind: run.PublishKind, Started: started}
	if run.Signer != nil {
		p.Signer = *run.Signer
	}
	r := &runner{env: env, p: p, run: run, files: map[string][]snapshot.File{}, metas: map[string]snapshot.Meta{}, descs: map[string]ocispec.Descriptor{}}
	for _, t := range targets {
		p.Targets = append(p.Targets, &TargetPlan{Target: t})
		v := run.Publish[t]
		dir := TreeDir(env.Paths, env.Set.Name, id, t)
		files, err := snapshot.Scan(dir)
		if err != nil {
			return nil, api.Errorf(api.PVerifyFailed, "the tree of %s for %s is gone: %v", id, t, err).WithTarget(t)
		}
		meta, err := readMeta(env, id, t)
		if err != nil {
			return nil, err
		}
		_, desc, err := snapshot.Encode(snapshot.NewManifest(files, meta))
		if err != nil {
			return nil, err
		}
		if v.Snapshot == nil || desc.Digest.String() != v.Snapshot.Digest {
			return nil, api.Errorf(api.PVerifyFailed, "the files of the tree of %s for %s do not give the manifest digest of the state file", id, t).WithTarget(t)
		}
		r.files[t], r.metas[t], r.descs[t] = files, meta, desc
	}
	j, err := OpenJournal(env.Paths.Journal(env.Set.Name, p.RunID), p.RunID, env.Now)
	if err != nil {
		return nil, err
	}
	defer j.Close()
	r.j = j
	run.State = api.RunStateRunning
	res := &Result{Run: run, Stats: map[string]snapshot.PushStats{}}
	var failed []error
	for _, t := range targets {
		st, warn, err := r.pushTarget(context.WithoutCancel(ctx), t, run.Publish[t].Base)
		res.Stats[t] = st
		if warn != nil {
			res.Warnings = append(res.Warnings, *warn)
		}
		if err != nil {
			failed = append(failed, err)
		}
	}
	// The run is ok when each target of the publish is ok.
	var ferr error
	if len(failed) > 0 {
		ferr = failed[0]
	} else {
		for t, v := range run.Publish {
			if v.State != StepOK {
				ferr = api.Errorf(api.PStateUnpushed, "%s of %s is not tagged", id, t).WithTarget(t)
			}
		}
	}
	all := p.Targets
	p.Targets = nil
	for t := range run.Publish {
		p.Targets = append(p.Targets, &TargetPlan{Target: t})
	}
	sort.Slice(p.Targets, func(i, j int) bool { return p.Targets[i].Target < p.Targets[j].Target })
	out, err := r.finish(res, ferr)
	p.Targets = all
	if len(failed) == 0 && ferr != nil {
		// Other targets of the publish still wait: this push did its part.
		return out, nil
	}
	return out, err
}

// cache is step 8: the new tree becomes the local copy.
func (r *runner) cache(store *snapshot.Store, target string, desc ocispec.Descriptor, files []snapshot.File, meta snapshot.Meta) *api.Problem {
	env, p := r.env, r.p
	dir := TreeDir(env.Paths, env.Set.Name, p.ID, target)
	snap := &snapshot.Snapshot{Desc: desc, Meta: meta, Files: files}
	keyID := func(fpr string) string {
		for _, k := range env.Set.Sign.Keys {
			if normFpr(k.Fingerprint) == fpr {
				return k.ID
			}
		}
		return ""
	}
	s := snap.API(store.Ref, env.Set.Repo.Name, keyID)
	s.Tags = []string{snapshot.TagLatest, p.ID}
	if err := localcopy.WriteSnapshot(dir, s); err != nil {
		return cacheWarning(target, p.ID, err)
	}
	if err := localcopy.Install(dir, localcopy.Dir(env.Paths, env.Set.Name, target)); err != nil {
		return cacheWarning(target, p.ID, err)
	}
	return nil
}

func cacheWarning(target, id string, err error) *api.Problem {
	return &api.Problem{Code: api.PStateBehind, Target: target, Message: fmt.Sprintf("the local copy of %s did not change: %v", target, err),
		Hint: api.HintText("", "repoctl repo push "+id+" --target "+target)}
}
