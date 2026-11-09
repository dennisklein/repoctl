package publish

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/publish/tree"
	"github.com/dennisklein/repoctl/internal/snapshot"
)

func vr(nvr string) string {
	_, v, r := manifest.SplitNVR(nvr)
	if v == "" {
		return ""
	}
	return v + "-" + r
}

// planPublish takes the staged nodes of a target and runs publish.not-ready
// and the nvr checks.
func planPublish(ctx context.Context, env *Env, p *Plan, tp *TargetPlan, base []baseRPM) (*Skipped, error) {
	nodes, err := env.Stager.Nodes(ctx, tp.Target, tp.BaseManifest, p.Req.Run)
	if err != nil {
		return nil, err
	}
	tp.Current = len(nodes.Current)
	held := map[api.NodeId]bool{}
	for _, h := range p.Req.Hold {
		n := h
		if !strings.Contains(h, "@") {
			n = api.Node(h, tp.Target)
		}
		if _, t := api.SplitNode(n); t != tp.Target {
			continue
		}
		for _, c := range nodes.Current {
			if c == n {
				return nil, &UsageError{Msg: fmt.Sprintf("--hold %s: the node is current; nobody holds in advance", n)}
			}
		}
		for _, s := range nodes.Staged {
			if s.Node == n {
				return nil, &UsageError{Msg: fmt.Sprintf("--hold %s: the node is staged; nobody holds in advance", n)}
			}
		}
		held[n] = true
	}
	var notReady []NotReady
	for _, n := range nodes.NotReady {
		if !held[n.Node] {
			notReady = append(notReady, n)
			continue
		}
		pkg, _ := api.SplitNode(n.Node)
		h := api.RunPublishValueHeldItem{Node: n.Node, Reason: p.Req.Reason}
		if tp.BaseManifest != nil {
			if b, ok := tp.BaseManifest.Newest(pkg, ""); ok {
				h.NVR = manifest.NVR(b.SRPM)
			}
		}
		tp.Held = append(tp.Held, h)
		delete(held, n.Node)
	}
	for n := range held {
		return nil, &UsageError{Msg: fmt.Sprintf("--hold %s: the target %s has no such node", n, tp.Target)}
	}
	if len(notReady) > 0 {
		return &Skipped{Target: tp.Target, Code: api.PPublishNotReady, NotReady: notReady,
			Message: fmt.Sprintf("%s is not publishable: %s", tp.Target, NotReadyText(notReady))}, nil
	}
	if len(nodes.Staged) == 0 {
		return &Skipped{Target: tp.Target, Code: api.PPublishNothingToPublish, Message: tp.Target + " has no staged node"}, nil
	}
	layout := tree.LayoutOf(env.Set)
	byPath := map[string]baseRPM{}
	for _, b := range base {
		byPath[b.rel] = b
	}
	for _, n := range nodes.Staged {
		var paths []string
		for _, f := range n.Files {
			got, err := tree.SHA256File(f.Path)
			if err != nil || got != f.SHA256 {
				return nil, api.Errorf(api.PRunStagingMissing, "%s of %s is gone from the staging of %s or changed", f.File, n.Node, n.Run).
					WithNode(n.Node).WithRun(n.Run).WithHint("build the node again", "repoctl plan --target "+tp.Target)
			}
			paths = append(paths, f.Path)
		}
		hs, err := tree.QueryHeaders(ctx, paths)
		if err != nil {
			return nil, err
		}
		srpm := n.Draft.SRPM
		count := 0
		for i, f := range n.Files {
			rel := layout.PathOf(f.File)
			if rel == "" {
				continue
			}
			if b, ok := byPath[rel]; ok {
				if b.PayloadDigest != "" && b.PayloadDigest == hs[i].PayloadDigest {
					tp.SkipFiles[f.File] = true
					continue
				}
				return nil, api.Errorf(api.PNVRReused, "%s gives %s again with new content", n.Node, f.File).WithNode(n.Node).
					WithHint("give the build a new release", "repoctl rebuild "+n.Package+" --reason TEXT")
			}
			count++
		}
		nvr := manifest.NVR(srpm)
		name := manifest.NameOfSRPM(srpm)
		if err := checkNVR(tp, n, nvr, name); err != nil {
			return nil, err
		}
		row := Row{Package: n.Package, New: vr(nvr), RPMs: count, Run: n.Run}
		if tp.BaseManifest != nil {
			if b, ok := tp.BaseManifest.Newest(n.Package, name); ok {
				row.Published = vr(manifest.NVR(b.SRPM))
			}
		}
		tp.Rows = append(tp.Rows, row)
		tp.Sign.New += count
	}
	tp.Take = nodes.Staged
	return nil, nil
}

// checkNVR runs nvr.removed and nvr.older for the SRPM NVR of a node.
func checkNVR(tp *TargetPlan, n StagedNode, nvr, name string) error {
	m := tp.BaseManifest
	if m == nil || nvr == "" {
		return nil
	}
	for _, r := range m.Removed {
		if r.NVR == nvr {
			return api.Errorf(api.PNVRRemoved, "%s gives %s, which %s removed (%s)", n.Node, nvr, r.Publish, r.Reason).WithNode(n.Node).
				WithHint("give the build a new release", "repoctl rebuild "+n.Package+" --reason TEXT")
		}
	}
	newest := ""
	consider := func(other string) {
		if manifest.NameOfSRPM(other+".src.rpm") != name || other == nvr {
			return
		}
		if newest == "" || manifest.CompareNVR(other, newest) > 0 {
			newest = other
		}
	}
	for _, b := range m.Builds {
		consider(manifest.NVR(b.SRPM))
	}
	for _, r := range m.Removed {
		consider(r.NVR)
	}
	if newest != "" && manifest.CompareNVR(nvr, newest) <= 0 {
		return api.Errorf(api.PNVROlder, "%s gives %s, which is not newer than %s", n.Node, nvr, newest).WithNode(n.Node).
			WithHint("raise the release or the epoch, then build", "repoctl rebuild "+n.Package+" --reason TEXT")
	}
	return nil
}

// repoManifestFile finds the RepoManifest layer of the binary repository
// in a snapshot.
func repoManifestFile(s *snapshot.Snapshot, repo string) (snapshot.File, bool) {
	for _, f := range s.Files {
		if strings.HasPrefix(f.Path, repo+"/repodata/") && (strings.HasSuffix(f.Path, manifest.MDType+".json") || strings.HasSuffix(f.Path, manifest.MDType+".json.gz")) {
			return f, true
		}
	}
	return snapshot.File{}, false
}

// planRestore makes the plan of repo restore REF.
func planRestore(ctx context.Context, env *Env, p *Plan, tp *TargetPlan) error {
	ref, err := tp.Store.Get(ctx, p.Req.Ref)
	if err != nil {
		return err
	}
	if ref.Meta.Target != tp.Target || ref.Meta.Set != env.Set.Name {
		return api.Errorf(api.PSnapshotNotFound, "%s is a snapshot of %s %s, not of %s", p.Req.Ref, ref.Meta.Set, ref.Meta.Target, tp.Target).WithTarget(tp.Target)
	}
	if ref.Digest() == tp.Base.Digest() {
		return api.Errorf(api.PPublishNothingToPublish, "%s is latest of %s", p.Req.Ref, tp.Target).WithTarget(tp.Target)
	}
	tp.RefSnap = ref
	f, ok := repoManifestFile(ref, env.Set.Repo.Name)
	if !ok {
		return api.Errorf(api.PManifestInvalid, "%s has no RepoManifest", p.Req.Ref)
	}
	raw, err := tp.Store.FetchBlob(ctx, f)
	if err != nil {
		return err
	}
	if strings.HasSuffix(f.Path, ".gz") {
		if raw, err = gunzip(raw); err != nil {
			return err
		}
	}
	if tp.RefManifest, err = manifest.Parse(raw); err != nil {
		return err
	}
	inRef := map[string]bool{}
	for _, b := range tp.RefManifest.Builds {
		inRef[b.SRPM] = true
	}
	rel := releaseName(env.Set)
	keep := map[string]bool{}
	if rel != "" {
		if b, ok := tp.BaseManifest.Newest(rel, rel); ok && !inRef[b.SRPM] {
			keep[b.SRPM] = true
			tp.KeptRelease = append(tp.KeptRelease, manifest.NVR(b.SRPM))
		}
	}
	refFiles := map[string]string{}
	for _, f := range ref.Files {
		refFiles[f.Path] = f.SHA256()
	}
	layout := tree.LayoutOf(env.Set)
	keepFiles := map[string]bool{}
	for _, b := range tp.BaseManifest.Builds {
		if !keep[b.SRPM] {
			continue
		}
		for _, r := range append([]string{b.SRPM}, b.RPMs...) {
			keepFiles[layout.PathOf(r)] = true
		}
	}
	for _, f := range tp.Base.Files {
		if keepFiles[f.Path] {
			continue
		}
		if s, ok := refFiles[f.Path]; !ok || s != f.SHA256() {
			tp.Remove[f.Path] = true
		}
	}
	removed := map[string]bool{}
	for _, r := range tp.BaseManifest.Removed {
		removed[r.NVR] = true
	}
	for _, b := range tp.RefManifest.Builds {
		if removed[manifest.NVR(b.SRPM)] {
			tp.Unremoved = append(tp.Unremoved, manifest.NVR(b.SRPM))
		}
	}
	pkgs := map[string]bool{}
	for _, b := range tp.BaseManifest.Builds {
		if inRef[b.SRPM] || keep[b.SRPM] {
			continue
		}
		rm := manifest.Removed{NVR: manifest.NVR(b.SRPM), SHA256: map[string]string{}, Publish: p.ID, Reason: "restore"}
		for _, r := range append([]string{b.SRPM}, b.RPMs...) {
			if f, ok := tp.Base.File(layout.PathOf(r)); ok {
				rm.SHA256[r] = f.SHA256()
			}
		}
		tp.Removed = append(tp.Removed, rm)
		row := Row{Package: b.Package, Published: vr(rm.NVR), RPMs: len(b.RPMs)}
		if rb, ok := tp.RefManifest.Newest(b.Package, manifest.NameOfSRPM(b.SRPM)); ok {
			row.New = vr(manifest.NVR(rb.SRPM))
		}
		tp.Rows = append(tp.Rows, row)
		if !pkgs[b.Package] {
			pkgs[b.Package] = true
			tp.Rebuild = append(tp.Rebuild, b.Package)
		}
	}
	sort.Strings(tp.Rebuild)
	tp.SourceDir = ""
	return nil
}

// planPrune selects the NVRs that repo prune removes, with the safety
// rules of prune (publish.html#prune-safety).
func planPrune(env *Env, p *Plan, tp *TargetPlan, base []baseRPM) (*Skipped, error) {
	m := tp.BaseManifest
	if m == nil {
		return &Skipped{Target: tp.Target, Code: api.PPublishNothingToPublish, Message: tp.Target + " has no RepoManifest"}, nil
	}
	spec := p.Req.Prune
	if spec.All {
		pk, ok := env.Set.Package(spec.Package)
		if ok && containsStr(env.Set.PackageTargets(*pk), tp.Target) {
			return nil, api.Errorf(api.PPruneNotRetired, "%s is not retired on %s", spec.Package, tp.Target).WithTarget(tp.Target)
		}
	}
	byName := map[string][]int{}
	for i, b := range m.Builds {
		n := manifest.NameOfSRPM(b.SRPM)
		byName[n] = append(byName[n], i)
	}
	newest := map[int]bool{}
	for _, idx := range byName {
		sort.Slice(idx, func(a, b int) bool {
			return manifest.CompareNVR(manifest.NVR(m.Builds[idx[a]].SRPM), manifest.NVR(m.Builds[idx[b]].SRPM)) > 0
		})
		if !spec.All {
			newest[idx[0]] = true
		}
	}
	now := env.Now()
	var sel []int
	var protected []string
	for name, idx := range byName {
		for rank, i := range idx {
			b := m.Builds[i]
			nvr := manifest.NVR(b.SRPM)
			match := true
			switch {
			case len(spec.NVRs) > 0:
				match = containsStr(spec.NVRs, nvr)
			default:
				if spec.Package != "" && b.Package != spec.Package && name != spec.Package {
					match = false
				}
				if spec.KeepLast > 0 && rank < spec.KeepLast {
					match = false
				}
				if spec.OlderThan > 0 {
					t := snapshot.IDTime(b.Publish)
					if t.IsZero() || now.Sub(t) < spec.OlderThan {
						match = false
					}
				}
				if !spec.All && spec.KeepLast == 0 && spec.OlderThan == 0 && spec.Package == "" {
					match = false
				}
			}
			if !match {
				continue
			}
			if newest[i] {
				if len(spec.NVRs) > 0 || spec.All {
					return nil, api.Errorf(api.PPruneUnsafe, "%s is the newest NVR of %s; prune never deletes it", nvr, name).WithTarget(tp.Target)
				}
				protected = append(protected, nvr)
				continue
			}
			sel = append(sel, i)
		}
	}
	for _, n := range spec.NVRs {
		found := false
		for _, i := range sel {
			if manifest.NVR(m.Builds[i].SRPM) == n {
				found = true
			}
		}
		if !found {
			return nil, api.Errorf(api.PSnapshotNotFound, "the base of %s has no build %s", tp.Target, n).WithTarget(tp.Target)
		}
	}
	for _, n := range protected {
		tp.Warnings = append(tp.Warnings, api.Problem{Code: api.PPruneUnsafe, Target: tp.Target, Message: "prune keeps " + n + ": the newest NVR of a kept package"})
	}
	if len(sel) == 0 {
		return &Skipped{Target: tp.Target, Code: api.PPublishNothingToPublish, Message: "nothing to prune on " + tp.Target}, nil
	}
	sort.Ints(sel)
	layout := tree.LayoutOf(env.Set)
	bySRPM := map[string][]baseRPM{}
	for _, h := range base {
		key := h.Sourcerpm
		if key == "" {
			key = path.Base(h.rel)
		}
		bySRPM[key] = append(bySRPM[key], h)
	}
	for _, i := range sel {
		b := m.Builds[i]
		rm := manifest.Removed{NVR: manifest.NVR(b.SRPM), SHA256: map[string]string{}, Publish: p.ID, Reason: "prune"}
		n := 0
		for _, h := range bySRPM[b.SRPM] {
			if p.Req.Repo != "" && tree.RepoKindOfFile(path.Base(h.rel)) != p.Req.Repo {
				continue
			}
			tp.Remove[h.rel] = true
			if f, ok := tp.Base.File(h.rel); ok {
				rm.SHA256[path.Base(h.rel)] = f.SHA256()
			}
			n++
		}
		_ = layout
		if n == 0 {
			continue
		}
		tp.Removed = append(tp.Removed, rm)
		tp.Rows = append(tp.Rows, Row{Package: b.Package, Published: vr(rm.NVR), RPMs: n})
	}
	return nil, nil
}

// planResign selects the RPMs of the key of repo resign --signer.
func planResign(env *Env, p *Plan, tp *TargetPlan, base []baseRPM, kr *tree.Keyring) (*Skipped, error) {
	found := false
	for _, k := range env.Set.Sign.Keys {
		if k.ID == p.Req.SignerID {
			found = true
		}
	}
	if !found {
		return nil, api.Errorf(api.PSignUnknownSigner, "sign.keys has no key %s", p.Req.SignerID)
	}
	for _, h := range base {
		k, ok := kr.ByKeyID(h.KeyID)
		if !ok || k.ID != p.Req.SignerID {
			continue
		}
		if p.Req.Repo != "" && tree.RepoKindOfFile(path.Base(h.rel)) != p.Req.Repo {
			continue
		}
		tp.Resign = append(tp.Resign, h.rel)
	}
	if len(tp.Resign) == 0 {
		return &Skipped{Target: tp.Target, Code: api.PPublishNothingToPublish, Message: fmt.Sprintf("%s has no RPM of %s", tp.Target, p.Req.SignerID)}, nil
	}
	sort.Strings(tp.Resign)
	tp.Sign.Resign = len(tp.Resign)
	tp.Rows = append(tp.Rows, Row{Package: "(re-sign)", Published: p.Req.SignerID, New: p.Signer.KeyID, RPMs: len(tp.Resign)})
	return nil, nil
}

// planImport reads the RepoManifest of an import, or the local copy of a
// re-seed.
func planImport(ctx context.Context, env *Env, p *Plan, tp *TargetPlan) error {
	if p.Kind == KindReseed {
		dir := filepath.Join(env.Paths.LocalCopy(env.Set.Name, tp.Target))
		m, err := manifest.ReadRepo(filepath.Join(dir, env.Set.Repo.Name))
		if err != nil {
			return err
		}
		if m == nil {
			return api.Errorf(api.PSnapshotNotFound, "%s has no local copy to re-seed from", tp.Target).WithTarget(tp.Target)
		}
		tp.SourceDir, tp.Import = dir, m
		p.ID = snapshot.DayID(KindReseed, p.Started, 0)
		return nil
	}
	if p.Req.ImportDir == "" || p.Req.ImportManifest == "" {
		return &UsageError{Msg: "repo push --from DIR needs --manifest FILE and --target ID"}
	}
	b, err := os.ReadFile(p.Req.ImportManifest)
	if err != nil {
		return err
	}
	m, err := manifest.Parse(b)
	if err != nil {
		return err
	}
	if m.Set != env.Set.Name || m.Target != tp.Target {
		return api.Errorf(api.PManifestInvalid, "the RepoManifest %s is of %s %s, not of %s %s", p.Req.ImportManifest, m.Set, m.Target, env.Set.Name, tp.Target)
	}
	p.ID = snapshot.DayID(KindImport, p.Started, 0)
	for i := range m.Builds {
		if m.Builds[i].Reason == "" {
			m.Builds[i].Reason = "adopted"
		}
		// The manifest is written before the day of the push is known.
		if m.Builds[i].Publish == "" {
			m.Builds[i].Publish = p.ID
		}
	}
	tp.SourceDir, tp.Import = p.Req.ImportDir, m
	tp.Rows = append(tp.Rows, Row{Package: "(import)", New: fmt.Sprintf("%d builds", len(m.Builds))})
	_ = ctx
	return nil
}

// SnapshotManifest reads the RepoManifest of a snapshot from the registry:
// the repodata file of type repoctl of the binary repository repo.
func SnapshotManifest(ctx context.Context, store *snapshot.Store, s *snapshot.Snapshot, repo string) (*manifest.Manifest, error) {
	f, ok := repoManifestFile(s, repo)
	if !ok {
		return nil, api.Errorf(api.PManifestInvalid, "%s has no RepoManifest", s.Meta.Publish)
	}
	raw, err := store.FetchBlob(ctx, f)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(f.Path, ".gz") {
		if raw, err = gunzip(raw); err != nil {
			return nil, api.Errorf(api.PManifestInvalid, "the RepoManifest of %s: %v", s.Meta.Publish, err)
		}
	}
	return manifest.Parse(raw)
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(zr); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// NotReadyText says why each node is not ready, as the refusal of
// publish says it (terminal.html#publish): "slurm@el10 failed,
// slurm-singularity-exec@el10 is blocked".
func NotReadyText(nr []NotReady) string {
	parts := make([]string, len(nr))
	for i, n := range nr {
		switch n.State {
		case "failed":
			parts[i] = n.Node + " failed"
		case "build":
			parts[i] = n.Node + " is not built"
		case "skip":
			parts[i] = n.Node + " has no lock line"
		default:
			parts[i] = n.Node + " is " + n.State
		}
	}
	return strings.Join(parts, ", ")
}

// notReadyHints adds the hints of a not-ready target: the log and the fix
// of the first failed node, else the plan of the target.
func notReadyHints(e *api.Error, target string, nr []NotReady) *api.Error {
	for _, n := range nr {
		if n.State == "failed" {
			pkg, _ := api.SplitNode(n.Node)
			return e.WithHint("read the log", "repoctl logs "+n.Node).
				WithHint("fix "+pkg+".spec, commit and push it", "repoctl pin "+pkg)
		}
	}
	return e.WithHint("see what the target needs", "repoctl plan --target "+target)
}
