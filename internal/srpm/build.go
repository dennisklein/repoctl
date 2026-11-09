package srpm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// Input is one SRPM build of a node.
type Input struct {
	Node   api.NodeId
	Source workspace.Source
	Target *Target
	// Work is a directory of the caller. Build makes the source clone in
	// Work/NODE/clone and the mock result directory Work/NODE/result.
	Work string
	// Log gets the output of mock. nil discards it.
	Log io.Writer
	// Uniqueext names the mock root: the plan root (mock.PlanExt) or the
	// build root of a job slot (mock.BuildExt).
	Uniqueext string
	// Fedora is upstream.fedora, the name of the package in the Fedora
	// lookaside cache; "" does not ask the lookaside cache.
	Fedora string
	// Remote is defaults.git.remote of the set, for a commit that the set
	// repository does not have yet.
	Remote string
	// Cache reads and writes the SRPM cache. With Cache false, Build always
	// runs mock and Result.SRPM is in Work.
	Cache bool
	// Plan is the plan mode of graph, plan, list and show: an in-tree spec
	// with %autorelease gives the warning source.in-tree-autorelease in
	// Result.Warnings, and the SRPM still builds. Without Plan it is an
	// error, as step srpm of a build gives it.
	Plan bool
	// KeepClone keeps the source clone in Work. Else Build removes it.
	KeepClone bool
}

// Result is the SRPM of a node and what the graph needs of it.
type Result struct {
	Node api.NodeId `json:"node"`
	// SRPM is the path of the SRPM: in the SRPM cache, or in Work.
	SRPM    string `json:"-"`
	File    string `json:"file"`
	Name    string `json:"name"`
	Epoch   string `json:"epoch,omitempty"`
	Version string `json:"version"`
	Release string `json:"release"`
	// Requires are the requires of the SRPM (the BuildRequires in the
	// chroot of the target), without rpmlib(), sorted.
	Requires []string `json:"requires"`
	// Dynamic reports %generate_buildrequires: edges can be missing until
	// the build.
	Dynamic bool `json:"dynamic,omitempty"`
	// Provides are the capabilities of the binary packages, without
	// version, sorted.
	Provides []string `json:"provides"`
	// RPMs are the names of the binary packages.
	RPMs []string `json:"rpms"`
	// Spec is the file name of the spec.
	Spec string `json:"spec"`
	// Sources are the sha256 of the Source and Patch files, by file name.
	Sources map[string]string `json:"sources,omitempty"`
	// Key is the SRPM cache key.
	Key string `json:"key"`
	// Cached reports an SRPM from the cache.
	Cached bool `json:"-"`
	// Warnings are the plan warnings of the node, for example
	// source.in-tree-autorelease.
	Warnings []api.Problem `json:"warnings,omitempty"`
}

// NVR gives NAME-VERSION-RELEASE.
func (r *Result) NVR() string { return r.Name + "-" + r.Version + "-" + r.Release }

// EVR gives [EPOCH:]VERSION-RELEASE.
func (r *Result) EVR() string {
	evr := r.Version + "-" + r.Release
	if r.Epoch != "" && r.Epoch != "0" {
		evr = r.Epoch + ":" + evr
	}
	return evr
}

// Key gives the SRPM cache key of a node: the sha256 of the package commit
// (and the in-tree path), the mock config hash, the sha256 of the macro
// file, and the versions of mock and its plugins (build.html#graph).
func (h *Host) Key(ctx context.Context, src workspace.Source, t *Target) (string, error) {
	mv, pv, err := h.versions(ctx)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "repoctl-srpm-v1\ncommit %s\npath %s\nmock-config %s\nmacros %s\nmock %s\nplugins %s\n%s\n",
		src.Commit, src.Path, t.ConfigHash, t.Files.MacrosSHA256, mv, strings.Join(t.Mock.Plugins, ","), pv)
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// Cached reports whether the SRPM cache has the SRPM of the input.
func (h *Host) Cached(ctx context.Context, in Input) bool {
	key, err := h.Key(ctx, in.Source, in.Target)
	return err == nil && h.readEntry(key) != nil
}

const infoFile = "srpm.json"

func (h *Host) entryDir(key string) string { return filepath.Join(h.Paths.SRPMCache(), key) }

// readEntry reads an entry of the SRPM cache, or nil.
func (h *Host) readEntry(key string) *Result {
	dir := h.entryDir(key)
	data, err := os.ReadFile(filepath.Join(dir, infoFile))
	if err != nil {
		return nil
	}
	var r Result
	if json.Unmarshal(data, &r) != nil || r.File == "" || r.Key != key {
		return nil
	}
	r.SRPM = filepath.Join(dir, r.File)
	if !fileExists(r.SRPM) {
		return nil
	}
	r.Cached = true
	return &r
}

// writeEntry writes an entry: into a temporary directory, then a rename.
// A reader never sees a part of an entry; of two writers of one key, the
// first rename wins and the second uses its entry.
func (h *Host) writeEntry(r *Result) (*Result, error) {
	root := h.Paths.SRPMCache()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(root, ".tmp-"+r.Key[:12]+"-")
	if err != nil {
		return nil, err
	}
	defer removeAll(tmp)
	if err := linkOrCopy(r.SRPM, filepath.Join(tmp, r.File)); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tmp, infoFile), append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, h.entryDir(r.Key)); err != nil {
		if e := h.readEntry(r.Key); e != nil {
			return e, nil
		}
		return nil, err
	}
	out := *r
	out.SRPM = filepath.Join(h.entryDir(r.Key), r.File)
	return &out, nil
}

// Build builds the SRPM of a node (step srpm, build.html#node-steps):
//
//  1. EnsureCommit fetches the commit into the package repository when it
//     is missing.
//  2. Clone makes the source clone Work/NODE/clone.
//  3. rpmspec reads the Source and Patch lines in the chroot of the target,
//     and PutSources puts the files next to the spec.
//  4. mock -r WORK/TARGET.cfg --uniqueext EXT --resultdir Work/NODE/result
//     --macro-file WORK/TARGET.macros --buildsrpm --spec SPEC --sources DIR.
//  5. rpm -qp reads the NVR and the requires of the SRPM; rpmspec reads the
//     provides of the binary packages.
//
// With Cache, an entry of the SRPM cache skips steps 1 to 5, and a new
// SRPM goes into the cache. A source problem gives an *api.Error with its
// code (source.mismatch, source.unlisted, source.unreachable, and
// source.in-tree-autorelease without Plan). A failed mock gives an error
// that wraps the *run.Error of mock (mock.ClassOf reads its exit code).
func (h *Host) Build(ctx context.Context, in Input) (*Result, error) {
	key, err := h.Key(ctx, in.Source, in.Target)
	if err != nil {
		return nil, err
	}
	if in.Cache {
		if r := h.readEntry(key); r != nil {
			if !in.Plan {
				for _, w := range r.Warnings {
					if w.Code == api.PSourceInTreeAutorelease {
						return nil, (&api.Error{Problem: w}).WithExit(api.ExitFailed)
					}
				}
			}
			r.Node = in.Node
			return r, nil
		}
	}
	if err := EnsureCommit(ctx, h.Paths, in.Source, in.Remote); err != nil {
		return nil, err
	}
	nodeDir := filepath.Join(in.Work, in.Node)
	cloneDir := filepath.Join(nodeDir, "clone")
	if !in.KeepClone {
		defer removeAll(cloneDir)
	}
	pkgDir, err := Clone(ctx, in.Source, cloneDir)
	if err != nil {
		return nil, err
	}
	spec, err := FindSpec(pkgDir, in.Source.Package)
	if err != nil {
		return nil, err
	}
	r := &Result{Node: in.Node, Spec: filepath.Base(spec), Key: key}
	if in.Source.Path != "" {
		text, err := os.ReadFile(spec)
		if err != nil {
			return nil, err
		}
		if UsesAutospec(text) {
			p := api.Problem{
				Code: api.PSourceInTreeAutorelease, Node: in.Node, Package: in.Source.Package,
				Message: fmt.Sprintf("%s of the in-tree package %s uses %%autorelease or %%autochangelog; the release stays 1", r.Spec, in.Source.Package),
				Hint:    "write a plain Release and %changelog, and raise the Release with each change",
			}
			_, p.Target = api.SplitNode(in.Node)
			if !in.Plan {
				return nil, (&api.Error{Problem: p}).WithExit(api.ExitFailed)
			}
			r.Warnings = append(r.Warnings, p)
		}
	}
	facts, err := readSpec(ctx, in.Target, pkgDir, spec)
	if err != nil {
		return nil, err
	}
	if err := h.Fetch.PutSources(ctx, pkgDir, facts.Sources, in.Fedora); err != nil {
		if e, ok := api.AsError(err); ok {
			return nil, e.WithNode(in.Node)
		}
		return nil, err
	}
	r.Sources = sourceDigests(pkgDir, facts.Sources)
	resultDir := filepath.Join(nodeDir, "result")
	removeAll(resultDir)
	log := in.Log
	if log == nil {
		log = io.Discard
	}
	call := mock.Call{Files: in.Target.Files, Uniqueext: in.Uniqueext, ResultDir: resultDir, Log: log}
	h.noteRoot(call)
	if err := h.Mock.BuildSRPM(ctx, call, spec, pkgDir); err != nil {
		return nil, fmt.Errorf("mock --buildsrpm %s: %w", r.Spec, err)
	}
	srpms, _ := filepath.Glob(filepath.Join(resultDir, "*.src.rpm"))
	if len(srpms) != 1 {
		return nil, fmt.Errorf("mock --buildsrpm %s gave %d SRPMs in %s", r.Spec, len(srpms), resultDir)
	}
	hdr, err := readSRPM(ctx, srpms[0])
	if err != nil {
		return nil, err
	}
	r.SRPM, r.File = srpms[0], filepath.Base(srpms[0])
	r.Name, r.Epoch, r.Version, r.Release = hdr.Name, hdr.Epoch, hdr.Version, hdr.Release
	r.Requires, r.Dynamic = hdr.Requires, hdr.Dynamic || facts.Dynamic
	r.Provides, r.RPMs = facts.Provides, facts.RPMs
	if r.Provides == nil {
		r.Provides = []string{}
	}
	if !in.Cache {
		return r, nil
	}
	return h.writeEntry(r)
}

// Inspect reads the name, the version and the provides of a node from its
// spec with rpmspec, without mock: for a node of a target that the command
// does not build, whose provides tell dep.not-for-target. It reads a
// cache entry of Build first, and keeps its own result in the SRPM cache
// as KEY.spec.json.
func (h *Host) Inspect(ctx context.Context, in Input) (*Result, error) {
	key, err := h.Key(ctx, in.Source, in.Target)
	if err != nil {
		return nil, err
	}
	if r := h.readEntry(key); r != nil {
		r.Node = in.Node
		return r, nil
	}
	specCache := filepath.Join(h.Paths.SRPMCache(), key+".spec.json")
	if data, err := os.ReadFile(specCache); err == nil {
		var r Result
		if json.Unmarshal(data, &r) == nil && r.Key == key {
			r.Node, r.Cached = in.Node, true
			return &r, nil
		}
	}
	if err := EnsureCommit(ctx, h.Paths, in.Source, in.Remote); err != nil {
		return nil, err
	}
	cloneDir := filepath.Join(in.Work, in.Node, "inspect")
	defer removeAll(cloneDir)
	pkgDir, err := Clone(ctx, in.Source, cloneDir)
	if err != nil {
		return nil, err
	}
	spec, err := FindSpec(pkgDir, in.Source.Package)
	if err != nil {
		return nil, err
	}
	facts, err := readSpec(ctx, in.Target, pkgDir, spec)
	if err != nil {
		return nil, err
	}
	r := &Result{Node: in.Node, Spec: filepath.Base(spec), Key: key, Name: facts.Name, Version: facts.Version,
		Provides: facts.Provides, RPMs: facts.RPMs, Dynamic: facts.Dynamic, Requires: []string{}}
	if data, err := json.Marshal(r); err == nil {
		_ = writeFileAtomic(specCache, data) // a cache entry; the result stands without it
	}
	return r, nil
}

func writeFileAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
