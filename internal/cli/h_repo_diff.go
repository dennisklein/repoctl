package cli

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/journal"
	"github.com/dennisklein/repoctl/internal/localcopy"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/plan"
	"github.com/dennisklein/repoctl/internal/publish"
	"github.com/dennisklein/repoctl/internal/publish/tree"
	"github.com/dennisklein/repoctl/internal/snapshot"
)

// diffRPM is one binary RPM of one side of repo diff.
type diffRPM struct {
	// File is the file name NVRA.rpm.
	File    string
	Name    string
	Arch    string
	Epoch   int
	Version string
	Release string
	// Source is the name of the source package; SourceV and SourceR its
	// version and release.
	Source, SourceV, SourceR string
	// Sum tells the content: the sha256 of the file between snapshots, the
	// payload digest between a staging and the local copy. "" is unknown.
	Sum string
	// Path is the file on disk, if any.
	Path string
}

func (r diffRPM) nvra() string { return strings.TrimSuffix(r.File, ".rpm") }

func evrText(e int, v, r string) string {
	if e > 0 {
		return fmt.Sprintf("%d:%s-%s", e, v, r)
	}
	return v + "-" + r
}

// diffRow is one source package of repo diff: + added, ~ new version or
// new RPMs, - removed RPMs (only between snapshots).
type diffRow struct {
	Mark     string
	Source   string
	From, To string
	// RPMs are the new RPM files (+, ~) or the removed ones (-).
	RPMs []string
}

// diffResult is the comparison of two sides of one target.
type diffResult struct {
	api.RepoDiff
	Rows []diffRow
	// New and Gone count the new and the removed RPMs.
	New, Gone int
}

// compareRPMs compares the binary RPMs of two sides. withRemoved is false
// for a staging, which holds only the new RPMs: the old versions stay.
// Unchanged counts the source packages that do not change.
func compareRPMs(from, to []diffRPM, withRemoved bool) diffResult {
	var res diffResult
	res.Added, res.Removed, res.Changed, res.Rebuilt = []string{}, []string{}, []api.RepoDiffChangedItem{}, []string{}
	fromBy, toBy := map[string]diffRPM{}, map[string]diffRPM{}
	for _, r := range from {
		fromBy[r.File] = r
	}
	for _, r := range to {
		toBy[r.File] = r
	}
	isNew := func(r diffRPM) bool { _, ok := fromBy[r.File]; return !ok }
	for _, r := range to {
		f, ok := fromBy[r.File]
		switch {
		case !ok:
			res.Added = append(res.Added, r.nvra())
		case f.Sum != "" && r.Sum != "" && f.Sum != r.Sum:
			res.Rebuilt = append(res.Rebuilt, r.nvra())
		}
	}
	if withRemoved {
		for _, r := range from {
			if _, ok := toBy[r.File]; !ok {
				res.Removed = append(res.Removed, r.nvra())
			}
		}
	}
	newest := func(rs []diffRPM) map[string]diffRPM {
		out := map[string]diffRPM{}
		for _, r := range rs {
			k := r.Name + "." + r.Arch
			if o, ok := out[k]; !ok || manifest.CompareEVR(r.Epoch, r.Version, r.Release, o.Epoch, o.Version, o.Release) > 0 {
				out[k] = r
			}
		}
		return out
	}
	nf, nt := newest(from), newest(to)
	for k, t := range nt {
		if f, ok := nf[k]; ok && manifest.CompareEVR(f.Epoch, f.Version, f.Release, t.Epoch, t.Version, t.Release) != 0 {
			res.Changed = append(res.Changed, api.RepoDiffChangedItem{Name: t.Name, Arch: t.Arch,
				From: evrText(f.Epoch, f.Version, f.Release), To: evrText(t.Epoch, t.Version, t.Release)})
		}
	}
	// One row per source package.
	bySource := map[string][2][]diffRPM{}
	for _, r := range from {
		s := bySource[r.Source]
		s[0] = append(s[0], r)
		bySource[r.Source] = s
	}
	for _, r := range to {
		s := bySource[r.Source]
		s[1] = append(s[1], r)
		bySource[r.Source] = s
	}
	newestVR := func(rs []diffRPM) string {
		best := -1
		for i, r := range rs {
			if best < 0 || manifest.CompareEVR(0, r.SourceV, r.SourceR, 0, rs[best].SourceV, rs[best].SourceR) > 0 {
				best = i
			}
		}
		if best < 0 {
			return "-"
		}
		return rs[best].SourceV + "-" + rs[best].SourceR
	}
	for src, s := range bySource {
		var added, gone []string
		for _, r := range s[1] {
			if isNew(r) {
				added = append(added, r.File)
			}
		}
		if withRemoved {
			for _, r := range s[0] {
				if _, ok := toBy[r.File]; !ok {
					gone = append(gone, r.File)
				}
			}
		}
		sort.Strings(added)
		sort.Strings(gone)
		row := diffRow{Source: src, From: newestVR(s[0]), To: newestVR(s[1])}
		switch {
		case len(s[0]) == 0 && len(added) > 0:
			row.Mark, row.RPMs = "+", added
		case len(added) > 0:
			row.Mark, row.RPMs = "~", added
		case len(gone) > 0:
			row.Mark, row.RPMs = "-", gone
		default:
			if len(s[0]) > 0 {
				res.Unchanged++
			}
			continue
		}
		res.Rows = append(res.Rows, row)
	}
	res.New, res.Gone = len(res.Added), len(res.Removed)
	sort.Strings(res.Added)
	sort.Strings(res.Removed)
	sort.Strings(res.Rebuilt)
	sort.Slice(res.Changed, func(i, j int) bool {
		if res.Changed[i].Name != res.Changed[j].Name {
			return res.Changed[i].Name < res.Changed[j].Name
		}
		return res.Changed[i].Arch < res.Changed[j].Arch
	})
	sort.Slice(res.Rows, func(i, j int) bool { return res.Rows[i].Source < res.Rows[j].Source })
	return res
}

// sourceOf splits a source RPM file name N-V-R.src.rpm.
func sourceOf(srpm string) (name, version, release string) {
	return manifest.SplitNVR(manifest.NVR(srpm))
}

// localRPMs reads the RPMs of the repositories of a tree (binary, source,
// debuginfo) from their primary.xml. A directory without repodata has
// none. An SRPM is its own source package.
func localRPMs(ctx context.Context, root string, l tree.Layout) ([]diffRPM, error) {
	var out []diffRPM
	for _, repo := range l.Repos() {
		dir := filepath.Join(root, repo.Dir)
		_, pkgs, err := tree.ReadPrimary(ctx, dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, p := range pkgs {
			r := diffRPM{File: p.File(), Name: p.Name, Arch: p.Arch, Epoch: p.Epoch(), Version: p.Version.Ver, Release: p.Version.Rel,
				Path: filepath.Join(dir, filepath.FromSlash(p.Location.Href))}
			r.Source, r.SourceV, r.SourceR = sourceOf(p.Format.Sourcerpm)
			if p.Arch == "src" || p.Format.Sourcerpm == "" {
				r.Source, r.SourceV, r.SourceR = p.Name, p.Version.Ver, p.Version.Rel
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// stagingRPMs reads the RPMs of the repositories of a staging from their
// headers, with the payload digest.
func stagingRPMs(ctx context.Context, root string, l tree.Layout) ([]diffRPM, error) {
	var files []string
	for _, repo := range l.Repos() {
		err := filepath.WalkDir(filepath.Join(root, repo.Dir), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(d.Name(), ".rpm") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, nil
	}
	hs, err := tree.QueryHeaders(ctx, files)
	if err != nil {
		return nil, err
	}
	out := make([]diffRPM, 0, len(hs))
	for i, h := range hs {
		r := diffRPM{File: filepath.Base(files[i]), Name: h.Name, Arch: h.Arch, Epoch: h.Epoch, Version: h.Version, Release: h.Release,
			Sum: h.PayloadDigest, Path: files[i]}
		r.Source, r.SourceV, r.SourceR = sourceOf(h.Sourcerpm)
		if h.Sourcerpm == "" {
			// An SRPM: its header arch is the build host's.
			r.Arch = "src"
			r.Source, r.SourceV, r.SourceR = h.Name, h.Version, h.Release
		}
		out = append(out, r)
	}
	return out, nil
}

// snapshotRPMs gives the RPMs of a snapshot (all repositories) from its
// layers, with the source package of each from its RepoManifest.
func snapshotRPMs(ctx context.Context, store *snapshot.Store, s *snapshot.Snapshot, repo string) ([]diffRPM, *manifest.Manifest, error) {
	m, err := publish.SnapshotManifest(ctx, store, s, repo)
	if err != nil {
		return nil, nil, err
	}
	srpmOf := map[string]string{}
	for _, b := range m.Builds {
		srpmOf[path.Base(b.SRPM)] = b.SRPM
		for _, f := range b.RPMs {
			srpmOf[path.Base(f)] = b.SRPM
		}
	}
	var out []diffRPM
	for _, f := range s.RPMs(repo) {
		file := path.Base(f.Path)
		nvra := strings.TrimSuffix(file, ".rpm")
		i := strings.LastIndex(nvra, ".")
		if i < 0 {
			continue
		}
		r := diffRPM{File: file, Arch: nvra[i+1:], Sum: f.SHA256()}
		r.Name, r.Version, r.Release = manifest.SplitNVR(nvra[:i])
		if srpm, ok := srpmOf[file]; ok {
			r.Source, r.SourceV, r.SourceR = sourceOf(srpm)
		} else {
			r.Source, r.SourceV, r.SourceR = r.Name, r.Version, r.Release
		}
		out = append(out, r)
	}
	return out, m, nil
}

// newestStagingRun gives the newest finished build run with a staging of
// the target.
func newestStagingRun(a *App, set *config.Set, target string) (string, error) {
	es, err := journal.List(journal.Readers(a.paths, set.Name, hostname()), set.Name, false)
	if err != nil {
		return "", err
	}
	for _, e := range es {
		if !e.Fold.Finished() {
			continue
		}
		r := e.Run()
		if r.Kind == journal.KindBuild && r.Staging != nil && r.Staging.Targets[target] != "" {
			return e.ID, nil
		}
	}
	return "", api.Errorf(api.PRunNotFound, "set %s has no finished build run with a staging of %s", set.Name, target).WithTarget(target).
		WithHint("build the target", "repoctl build --target "+target)
}

// diffPackages maps source package names to package ids with the
// RepoManifests and the nodes of a run, for the selectors.
func diffPackages(ms []*manifest.Manifest, nodes []api.NodeResult) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		if m == nil {
			continue
		}
		for _, b := range m.Builds {
			out[manifest.NameOfSRPM(b.SRPM)] = b.Package
		}
	}
	for _, n := range nodes {
		if n.SRPM != "" {
			out[manifest.NameOfSRPM(n.SRPM)] = n.Package
		}
	}
	return out
}

// hRepoDiff runs repo diff: the staging of a run against the local copy
// of latest, or two snapshots, per target. Rows are source packages.
func hRepoDiff(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	set, setDir, err := a.Set()
	if err != nil {
		return nil, err
	}
	targets, err := a.Targets(set, false)
	if err != nil {
		return nil, err
	}
	fromRef, toRef, runFlag := flagStr(cmd, "from"), flagStr(cmd, "to"), flagStr(cmd, "run")
	snapshots := fromRef != "" || toRef != ""
	if snapshots && toRef == "" {
		toRef = snapshot.TagLatest
	}
	if snapshots && runFlag != "" {
		return nil, usagef("--run and --from/--to exclude each other")
	}
	var sel *plan.Selection
	if len(args) > 0 {
		sels, err := plan.ParseSelectors(args)
		if err != nil {
			return nil, err
		}
		if sel, err = plan.Select(set, sels, targets, a.Here(set, setDir)); err != nil {
			return nil, err
		}
	}
	var items []any
	var diffs []diffResult
	repo := set.Repo.Name
	for _, t := range targets {
		var from, to []diffRPM
		var ms []*manifest.Manifest
		var nodes []api.NodeResult
		d := api.RepoDiff{Target: t, Repo: repo}
		if snapshots {
			store, err := openStore(a, set, t)
			if err != nil {
				return nil, err
			}
			ts, err := store.Get(ctx, toRef)
			if err != nil {
				return nil, err
			}
			var fs *snapshot.Snapshot
			switch {
			case fromRef != "":
				if fs, err = store.Get(ctx, fromRef); err != nil {
					return nil, err
				}
			case ts.Meta.Parent != "":
				if fs, err = store.Get(ctx, ts.Meta.Parent); err != nil {
					return nil, err
				}
			}
			var tm, fm *manifest.Manifest
			if to, tm, err = snapshotRPMs(ctx, store, ts, repo); err != nil {
				return nil, err
			}
			d.To = api.RepoDiffTo{Source: "snapshot", Snapshot: ts.Meta.Publish}
			if fs != nil {
				if from, fm, err = snapshotRPMs(ctx, store, fs, repo); err != nil {
					return nil, err
				}
				d.From = api.RepoDiffFrom{Source: "snapshot", Snapshot: fs.Meta.Publish}
			}
			ms = []*manifest.Manifest{fm, tm}
		} else {
			run := runFlag
			if run == "" {
				if run, err = newestStagingRun(a, set, t); err != nil {
					return nil, err
				}
			}
			sdir := a.paths.StagingTarget(set.Name, run, t)
			if _, err := os.Stat(sdir); err != nil {
				return nil, api.Errorf(api.PRunStagingMissing, "run %s has no staging of %s", run, t).WithTarget(t).WithRun(run).
					WithHint("list the runs", "repoctl runs")
			}
			if e, err := journal.Find(journal.Readers(a.paths, set.Name, hostname()), set.Name, run, ""); err == nil {
				nodes = e.Run().Nodes
			}
			if to, err = stagingRPMs(ctx, sdir, tree.LayoutOf(set)); err != nil {
				return nil, err
			}
			ldir := localcopy.Dir(a.paths, set.Name, t)
			if from, err = localRPMs(ctx, ldir, tree.LayoutOf(set)); err != nil {
				return nil, err
			}
			// A staged RPM with the NVRA of a published one: compare the
			// payload digests.
			staged := map[string]bool{}
			for _, r := range to {
				staged[r.File] = true
			}
			var both []int
			for i, r := range from {
				if staged[r.File] {
					both = append(both, i)
				}
			}
			if len(both) > 0 {
				files := make([]string, len(both))
				for i, j := range both {
					files[i] = from[j].Path
				}
				if hs, err := tree.QueryHeaders(ctx, files); err == nil {
					for i, j := range both {
						from[j].Sum = hs[i].PayloadDigest
					}
				}
			}
			d.From = api.RepoDiffFrom{Source: "local", Path: ldir}
			if s, _ := localcopy.ReadSnapshot(ldir); s != nil {
				d.From.Snapshot = s.Publish
			}
			d.To = api.RepoDiffTo{Source: "staging", Run: run, Path: sdir}
			m, _ := manifest.ReadRepo(filepath.Join(ldir, repo))
			ms = []*manifest.Manifest{m}
		}
		if sel != nil {
			ids := diffPackages(ms, nodes)
			keep := func(rs []diffRPM) []diffRPM {
				var out []diffRPM
				for _, r := range rs {
					if id, ok := ids[r.Source]; ok && sel.Contains(api.Node(id, t)) {
						out = append(out, r)
					}
				}
				return out
			}
			from, to = keep(from), keep(to)
		}
		res := compareRPMs(from, to, snapshots)
		res.Target, res.Repo, res.From, res.To = d.Target, d.Repo, d.From, d.To
		diffs = append(diffs, res)
		items = append(items, res.RepoDiff)
	}
	rpms := flagBool(cmd, "rpms")
	out := &Result{Kind: api.KindRepoDiff, Items: items, Targets: targets, Human: func(o *Out) error {
		for _, d := range diffs {
			writeDiff(o, d, rpms)
		}
		return nil
	}}
	differs := false
	for _, d := range diffs {
		if len(d.Rows) > 0 || len(d.Rebuilt) > 0 {
			differs = true
		}
		if len(d.Rebuilt) > 0 && d.To.Source == "staging" {
			a.Warn(api.Problem{Code: api.PNVRReused, Target: d.Target,
				Message: fmt.Sprintf("%d staged RPMs have the NVRA of a published RPM and other content: %s", len(d.Rebuilt), strings.Join(d.Rebuilt, ", "))})
		}
	}
	if differs && flagBool(cmd, "check") {
		return out, exitOnly(api.ExitDifferences)
	}
	return out, nil
}

// sideText names a side of repo diff: "latest p-…", "staging r-…" or a
// publish id.
func sideText(source, run, snap string) string {
	switch source {
	case "local":
		if snap == "" {
			return "latest none"
		}
		return "latest " + snap
	case "staging":
		return "staging " + run
	case "":
		return "none"
	}
	return snap
}

// writeDiff writes the rows of one target (cli.html repo diff).
func writeDiff(o *Out, d diffResult, rpms bool) {
	o.Printf("%s  %s → %s\n", d.Target, sideText(d.From.Source, d.From.Run, d.From.Snapshot), sideText(d.To.Source, d.To.Run, d.To.Snapshot))
	wName, wFrom, wTo, wN := 0, 0, 0, 0
	for _, r := range d.Rows {
		wName, wFrom, wTo = max(wName, len(r.Source)), max(wFrom, len(r.From)), max(wTo, len(r.To))
		wN = max(wN, len(fmt.Sprint(len(r.RPMs))))
	}
	for _, r := range d.Rows {
		unit := "RPMs"
		if len(r.RPMs) == 1 {
			unit = "RPM"
		}
		o.Printf("%s %-*s  %-*s → %-*s  %*d %s\n", r.Mark, wName, r.Source, wFrom, r.From, wTo, r.To, wN, len(r.RPMs), unit)
		if rpms {
			for _, f := range r.RPMs {
				o.Printf("    %s %s\n", r.Mark, strings.TrimSuffix(f, ".rpm"))
			}
		}
	}
	parts := []string{fmt.Sprintf("%d new RPMs", d.New)}
	if d.To.Source == "snapshot" {
		parts = append(parts, fmt.Sprintf("%d removed RPMs", d.Gone))
	}
	parts = append(parts, fmt.Sprintf("%d packages unchanged", d.Unchanged))
	if d.To.Source == "staging" {
		parts = append(parts, "old versions stay")
	}
	o.Printf("%s\n", strings.Join(parts, " · "))
}
