package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/localcopy"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/plan"
	"github.com/dennisklein/repoctl/internal/publish"
	"github.com/dennisklein/repoctl/internal/publish/tree"
	"github.com/dennisklein/repoctl/internal/records"
)

// recordSource is where repo provenance and repo sbom read the records:
// the local copy of latest or a snapshot.
type recordSource struct {
	target  string
	publish string
	// m is the RepoManifest of the binary repository.
	m *manifest.Manifest
	// read gives a file of the binary repository.
	read func(rel string) ([]byte, error)
	// sums gives the sha256 of an RPM of the tree by its base name.
	sums records.FileSums
}

// openRecords opens the local copy of a target, or the snapshot REF.
func openRecords(ctx context.Context, a *App, set *config.Set, target, ref string) (*recordSource, error) {
	repo := set.Repo.Name
	if ref != "" {
		store, err := openStore(a, set, target)
		if err != nil {
			return nil, err
		}
		s, err := store.Get(ctx, ref)
		if err != nil {
			return nil, err
		}
		m, err := publish.SnapshotManifest(ctx, store, s, repo)
		if err != nil {
			return nil, err
		}
		sums := map[string]string{}
		for _, f := range s.Files {
			if strings.HasSuffix(f.Path, ".rpm") {
				sums[path.Base(f.Path)] = f.Digest.Encoded()
			}
		}
		return &recordSource{target: target, publish: s.Meta.Publish, m: m,
			read: func(rel string) ([]byte, error) {
				f, ok := s.File(repo + "/" + rel)
				if !ok {
					return nil, fs.ErrNotExist
				}
				return store.FetchBlob(ctx, f)
			},
			sums: func(base string) (string, bool) { s, ok := sums[base]; return s, ok },
		}, nil
	}
	dir := localcopy.Dir(a.paths, set.Name, target)
	m, err := manifest.ReadRepo(filepath.Join(dir, repo))
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, api.Errorf(api.PSnapshotNotFound, "%s has no local copy", target).WithTarget(target).
			WithHint("pull the local copy", "repoctl repo pull --target "+target)
	}
	src := &recordSource{target: target, m: m,
		read: func(rel string) ([]byte, error) {
			return os.ReadFile(filepath.Join(dir, repo, filepath.FromSlash(rel)))
		}}
	if s, _ := localcopy.ReadSnapshot(dir); s != nil {
		src.publish = s.Publish
	}
	var files map[string]string
	src.sums = func(base string) (string, bool) {
		if files == nil {
			files = map[string]string{}
			_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.HasSuffix(p, ".rpm") {
					files[d.Name()] = p
				}
				return nil
			})
		}
		p, ok := files[base]
		if !ok {
			return "", false
		}
		s, err := tree.SHA256File(p)
		return s, err == nil
	}
	return src, nil
}

// record reads a record and checks it against its sha256 in the
// RepoManifest: records.missing or records.mismatch.
func (r *recordSource) record(rec *manifest.Record) ([]byte, error) {
	b, err := r.read(rec.Path)
	if err != nil {
		where := "the local copy of " + r.target
		if r.publish != "" {
			where = r.publish
		}
		return nil, api.Errorf(api.PRecordsMissing, "%s has no record %s", where, rec.Path).WithTarget(r.target)
	}
	if rec.SHA256 != "" && tree.SHA256Bytes(b) != rec.SHA256 {
		return nil, api.Errorf(api.PRecordsMismatch, "the record %s does not have the sha256 of the RepoManifest", rec.Path).WithTarget(r.target)
	}
	return b, nil
}

// findBuild gives the index of the newest build of an NVR or a package in
// the RepoManifest.
func (r *recordSource) findBuild(what string) (int, error) {
	for i := len(r.m.Builds) - 1; i >= 0; i-- {
		b := &r.m.Builds[i]
		nvr := manifest.NVR(b.SRPM)
		if nvr == what || b.Package == what || strings.HasPrefix(nvr, what+".") {
			return i, nil
		}
	}
	return 0, api.Errorf(api.PRecordsMissing, "the RepoManifest of %s has no build %s", r.target, what).WithTarget(r.target)
}

// oneTarget gives the one target of the command: the --target, or the one
// active target of the set.
func oneTarget(a *App, set *config.Set) (string, error) {
	targets, err := a.Targets(set, false)
	if err != nil {
		return "", err
	}
	if len(targets) != 1 {
		return "", usagef("this command needs one --target ID")
	}
	return targets[0], nil
}

// buildArg gives the target and the NVR or package of NVR|PKG@TARGET. The
// target of a node must agree with --target.
func buildArg(a *App, set *config.Set, arg string) (string, string, error) {
	if !strings.Contains(arg, "@") {
		t, err := oneTarget(a, set)
		return t, arg, err
	}
	pkg, target := api.SplitNode(arg)
	if pkg == "" || target == "" {
		return "", "", usagef("%s is not a node PKG@TARGET", arg)
	}
	if _, err := plan.FilterTargets(set, []string{target}, false); err != nil {
		return "", "", err
	}
	if ta := a.TargetArgs(); len(ta) > 0 {
		targets, err := a.Targets(set, false)
		if err != nil {
			return "", "", err
		}
		if len(targets) != 1 || targets[0] != target {
			return "", "", usagef("the node %s is not in --target %s", arg, strings.Join(ta, ","))
		}
	}
	return target, pkg, nil
}

// buildroot gives the buildroot RPMs of a record by name, with
// version-release.
func buildroot(st *records.Statement) map[string]string {
	out := map[string]string{}
	for _, d := range st.Predicate.BuildDefinition.ResolvedDependencies {
		rest, ok := strings.CutPrefix(d.URI, "pkg:rpm/")
		if d.Name != "" || !ok {
			continue
		}
		_, nv, _ := strings.Cut(rest, "/")
		nv, _, _ = strings.Cut(nv, "?")
		if name, vr, ok := strings.Cut(nv, "@"); ok {
			out[name] = vr
		}
	}
	return out
}

// hRepoProvenance runs repo provenance NVR|PKG@TARGET.
func hRepoProvenance(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error) {
	if len(args) != 1 {
		return nil, usagef("repo provenance needs an NVR or a node PKG@TARGET")
	}
	set, _, err := a.Set()
	if err != nil {
		return nil, err
	}
	t, what, err := buildArg(a, set, args[0])
	if err != nil {
		return nil, err
	}
	src, err := openRecords(ctx, a, set, t, flagStr(cmd, "snapshot"))
	if err != nil {
		return nil, err
	}
	i, err := src.findBuild(what)
	if err != nil {
		return nil, err
	}
	b := &src.m.Builds[i]
	if b.Records == nil || b.Records.Provenance == nil {
		return nil, api.Errorf(api.PRecordsMissing, "%s has no provenance record (adopted)", manifest.NVR(b.SRPM)).WithTarget(t)
	}
	raw, err := src.record(b.Records.Provenance)
	if err != nil {
		return nil, err
	}
	if flagBool(cmd, "raw") {
		return &Result{Raw: raw}, nil
	}
	st, err := records.ParseStatement(raw)
	if err != nil {
		return nil, api.Errorf(api.PRecordsMismatch, "%s: %v", b.Records.Provenance.Path, err).WithTarget(t)
	}
	var stmt map[string]any
	_ = json.Unmarshal(raw, &stmt)
	ext, intl := st.Predicate.BuildDefinition.ExternalParameters, st.Predicate.BuildDefinition.InternalParameters
	md := st.Predicate.RunDetails.Metadata
	runID, _, _ := strings.Cut(md.InvocationID, "/")
	p := api.Provenance{Target: t, Node: api.Node(b.Package, t), NVR: manifest.NVR(b.SRPM), Publish: src.publish,
		Record: api.RecordFile{Path: b.Records.Provenance.Path, Sha256: b.Records.Provenance.SHA256},
		Source: api.ProvenanceSource{Branch: b.Branch, URL: b.URL, Commit: b.Commit, Reason: b.Reason},
		Run:    api.ProvenanceRun{ID: runID, User: intl["user"], Host: intl["host"], StartedOn: md.StartedOn, FinishedOn: md.FinishedOn},
		Builder: api.ProvenanceBuilder{ID: st.Predicate.RunDetails.Builder.ID, Repoctl: st.Predicate.RunDetails.Builder.Version["repoctl"],
			Mock: st.Predicate.RunDetails.Builder.Version["mock"], MockConfig: ext["mockConfig"], MockConfigSha256: strings.TrimPrefix(intl["mockConfig"], "sha256:")},
		Subjects: []api.ProvenanceSubject{}, Statement: stmt}
	if p.Record.Sha256 == "" {
		p.Record.Sha256 = tree.SHA256Bytes(raw)
	}
	var bad []string
	for _, s := range st.Subject {
		got, ok := src.sums(s.Name)
		match := ok && got == s.Digest["sha256"]
		if !match {
			bad = append(bad, s.Name)
		}
		p.Subjects = append(p.Subjects, api.ProvenanceSubject{Name: s.Name, Sha256: s.Digest["sha256"], Match: match})
	}
	for _, d := range st.Predicate.BuildDefinition.ResolvedDependencies {
		if d.Name != "" && d.Digest["sha256"] != "" {
			p.Sources = append(p.Sources, api.ProvenanceSourceItem{Name: d.Name, URI: d.URI, Sha256: d.Digest["sha256"]})
		}
	}
	if br := buildroot(st); len(br) > 0 {
		p.Buildroot = &api.ProvenanceBuildroot{RPMs: len(br)}
		if prev := src.previousRecord(i); prev != nil {
			since := manifest.NVR(prev.build.SRPM)
			p.Buildroot.Since = &since
			old := buildroot(prev.st)
			for name, vr := range br {
				if ovr, ok := old[name]; ok && ovr != vr {
					p.Buildroot.Changed = append(p.Buildroot.Changed, name)
				}
			}
			sort.Strings(p.Buildroot.Changed)
		}
	}
	res := &Result{Kind: api.KindProvenance, Items: []any{p}, Targets: []api.TargetId{t}, Human: func(o *Out) error {
		kv := func(k, format string, args ...any) {
			o.Printf("%s %s\n", o.StdoutStyle.Dim(fmt.Sprintf("%-9s", k)), fmt.Sprintf(format, args...))
		}
		head := p.NVR + "  " + t + " of " + set.Name
		if p.Publish != "" {
			head += "  publish " + p.Publish
		}
		o.Printf("%s\n", o.StdoutStyle.Bold(head))
		source := p.Source.Branch
		if p.Source.URL != nil {
			source = *p.Source.URL
		}
		line := source + " at " + api.ShortCommit(p.Source.Commit)
		if p.Source.Reason != "" {
			line += "  reason " + p.Source.Reason
		}
		kv("source", "%s", line)
		kv("run", "%s  %s@%s  %s → %s", p.Run.ID, p.Run.User, p.Run.Host, clock(p.Run.StartedOn), clock(p.Run.FinishedOn))
		builder := "repoctl " + strings.TrimPrefix(p.Builder.Repoctl, "v") + ", mock " + p.Builder.Mock
		if p.Builder.MockConfig != "" {
			builder += ", " + p.Builder.MockConfig
			if p.Builder.MockConfigSha256 != "" {
				builder += " (" + api.ShortDigest("sha256:"+p.Builder.MockConfigSha256) + ")"
			}
		}
		kv("builder", "%s", builder)
		if len(bad) == 0 {
			kv("subjects", "%s, sha256 equal to the files", countOf(len(p.Subjects), "RPM", "RPMs"))
		} else {
			kv("subjects", "%s, %d not equal to the files: %s", countOf(len(p.Subjects), "RPM", "RPMs"), len(bad), strings.Join(bad, ", "))
		}
		for _, s := range p.Sources {
			line := s.Name + "  " + api.ShortDigest("sha256:"+s.Sha256)
			if s.URI != "" {
				line += "  " + shortURI(s.URI)
			}
			kv("sources", "%s", line)
		}
		if br := p.Buildroot; br != nil {
			line := countOf(br.RPMs, "RPM", "RPMs")
			if br.Since != nil {
				line += fmt.Sprintf(", %d changed since %s", len(br.Changed), *br.Since)
				if len(br.Changed) > 0 {
					line += ": " + strings.Join(br.Changed, ", ")
				}
			}
			kv("buildroot", "%s", line)
		}
		return nil
	}}
	if len(bad) > 0 {
		return res, api.Errorf(api.PRecordsMismatch, "the subject %s of %s does not have the sha256 of its file", bad[0], p.Record.Path).WithTarget(t)
	}
	return res, nil
}

type prevRecord struct {
	build *manifest.Build
	st    *records.Statement
}

// previousRecord gives the provenance of the build of the same package
// before the build i, if the tree still holds it.
func (r *recordSource) previousRecord(i int) *prevRecord {
	pkg := r.m.Builds[i].Package
	for j := i - 1; j >= 0; j-- {
		b := &r.m.Builds[j]
		if b.Package != pkg || b.Records == nil || b.Records.Provenance == nil {
			continue
		}
		raw, err := r.record(b.Records.Provenance)
		if err != nil {
			return nil
		}
		st, err := records.ParseStatement(raw)
		if err != nil {
			return nil
		}
		return &prevRecord{build: b, st: st}
	}
	return nil
}

// clock gives the local time of day of a contract time.
func clock(t api.Time) string {
	tm, err := api.ParseTime(t)
	if err != nil {
		return t
	}
	return tm.Local().Format("15:04:05")
}

// shortURI drops the scheme of a URI.
func shortURI(u string) string {
	if _, rest, ok := strings.Cut(u, "://"); ok {
		return rest
	}
	return u
}

func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// hRepoSBOM runs repo sbom: the repository SBOM of each target, or the
// SBOM of a build.
func hRepoSBOM(ctx context.Context, a *App, cmd *cobra.Command, _ []string) (*Result, error) {
	set, _, err := a.Set()
	if err != nil {
		return nil, err
	}
	bname, ref := flagStr(cmd, "build"), flagStr(cmd, "snapshot")
	var targets []string
	if bname != "" || ref != "" || flagBool(cmd, "raw") {
		t, err := oneTarget(a, set)
		if err != nil {
			return nil, err
		}
		targets = []string{t}
	} else if targets, err = a.Targets(set, false); err != nil {
		return nil, err
	}
	var items []any
	var sboms []api.Sbom
	var shown []string
	for _, t := range targets {
		src, err := openRecords(ctx, a, set, t, ref)
		if e, ok := api.AsError(err); ok && e.Code == api.PSnapshotNotFound && len(targets) > 1 {
			// Of all targets, one that was never published has no SBOM.
			a.Warn(api.Problem{Code: e.Code, Message: e.Message, Target: t, Hint: e.Hint})
			continue
		}
		if err != nil {
			return nil, err
		}
		shown = append(shown, t)
		rec := &manifest.Record{Path: records.RepositorySBOMPath}
		var build *string
		if bname != "" {
			i, err := src.findBuild(bname)
			if err != nil {
				return nil, err
			}
			b := &src.m.Builds[i]
			if b.Records == nil || b.Records.SBOM == nil {
				return nil, api.Errorf(api.PRecordsMissing, "%s has no SBOM", manifest.NVR(b.SRPM)).WithTarget(t)
			}
			rec = b.Records.SBOM
			nvr := manifest.NVR(b.SRPM)
			build = &nvr
		} else if src.m.Records != nil && src.m.Records.SBOM != nil {
			rec = src.m.Records.SBOM
		}
		raw, err := src.record(rec)
		if err != nil {
			return nil, err
		}
		if flagBool(cmd, "raw") {
			return &Result{Raw: raw}, nil
		}
		s, err := sbomItem(t, src.publish, rec, raw, build)
		if err != nil {
			return nil, err
		}
		items = append(items, s)
		sboms = append(sboms, s)
	}
	if len(shown) == 0 {
		return nil, api.Errorf(api.PSnapshotNotFound, "no target has a local copy").WithHint("pull the local copies", "repoctl repo pull")
	}
	return &Result{Kind: api.KindSBOM, Items: items, Targets: shown, Human: func(o *Out) error {
		for i, s := range sboms {
			if i > 0 {
				o.Printf("\n")
			}
			if len(sboms) > 1 {
				o.Printf("%s\n", o.StdoutStyle.Bold(s.Target+"  publish "+s.Publish))
			}
			tb := NewTable("COMPONENT", "VERSION", "PURL", "LICENSE")
			n := map[string]int{}
			for _, c := range s.Components {
				n[c.Type]++
				name := c.Name
				switch {
				case c.Type == "upstream" || c.Type == "bundled":
					name += " (" + c.Type + ")"
				case c.Type == "srpm" && s.Build != nil:
					name += " (source)"
				}
				tb.Row(name, c.Version, c.PURL, c.License)
			}
			_ = o.Table(tb)
			if s.Build != nil {
				o.Printf("%s, %s, %s\n", countOf(n["srpm"]+n["rpm"]+n["debuginfo"], "RPM", "RPMs"),
					countOf(n["upstream"], "upstream source", "upstream sources"), countOf(n["bundled"], "bundled library", "bundled libraries"))
			} else {
				o.Printf("%s in %s, CycloneDX %s\n", countOf(n["srpm"], "build", "builds"), s.Record.Path, s.SpecVersion)
			}
		}
		return nil
	}}, nil
}

// sbomItem gives the Sbom item of a CycloneDX file: a build SBOM lists the
// upstream sources, the SRPM and each RPM with the components it bundles;
// the repository SBOM lists the builds.
func sbomItem(t, pub string, rec *manifest.Record, raw []byte, build *string) (api.Sbom, error) {
	bom, err := records.ParseBOM(raw)
	if err != nil {
		return api.Sbom{}, api.Errorf(api.PRecordsMismatch, "%s: %v", rec.Path, err).WithTarget(t)
	}
	sum := rec.SHA256
	if sum == "" {
		sum = tree.SHA256Bytes(raw)
	}
	s := api.Sbom{Target: t, Build: build, Publish: pub, Record: api.RecordFile{Path: rec.Path, Sha256: sum}, SpecVersion: bom.SpecVersion, Components: []api.SbomComponent{}}
	var flat []records.Component
	if m := bom.Metadata.Component; m != nil && m.Prop(records.PropType) == "srpm" {
		if m.Pedigree != nil {
			flat = append(flat, m.Pedigree.Ancestors...)
		}
		flat = append(flat, *m)
	}
	for _, c := range bom.Components {
		flat = append(flat, c)
		flat = append(flat, c.Components...)
	}
	for _, c := range flat {
		name, typ := c.Name, c.Prop(records.PropType)
		switch typ {
		case "upstream", "srpm", "rpm", "debuginfo", "bundled":
		default:
			// The repository SBOM lists the SBOM file of each build: the
			// component is the build.
			typ = "rpm"
			if nvr := c.Prop("repoctl:build"); nvr != "" {
				typ = "srpm"
				name, _, _ = manifest.SplitNVR(nvr)
			}
		}
		s.Components = append(s.Components, api.SbomComponent{Name: name, Version: c.Version, PURL: c.Purl, Type: typ, License: c.LicenseText(), Sha256: c.SHA256()})
	}
	return s, nil
}
