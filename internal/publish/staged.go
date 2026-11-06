package publish

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/records"
)

// DraftDir is the directory of the record drafts in the staging of a run
// and target: STAGING/RUN/TARGET/drafts/PACKAGE.json holds one Draft.
const DraftDir = "drafts"

// Draft is the record draft of one node build, which the build step
// writes next to the staging of the target.
type Draft struct {
	Build      manifest.Build    `json:"build"`
	Facts      records.Facts     `json:"facts"`
	SourceURIs map[string]string `json:"sourceURIs,omitempty"`
	// Buildroot is the buildroot file of the node, absolute or relative
	// to the drafts directory.
	Buildroot string `json:"buildroot,omitempty"`
}

// DraftPath gives the path of the draft of a package in the staging of a
// run and target.
func DraftPath(p config.Paths, set, run, target, pkg string) string {
	return filepath.Join(p.StagingTarget(set, run, target), DraftDir, pkg+".json")
}

// JournalStager is the default Stager: it reads the journals of the build
// runs of this user (runs/*.jsonl) and their staging. A node is staged
// when a run of this host built the commit of the lock, the node ended ok
// and each file of its artifact.added events is in the staging. A run from
// worktrees (--from worktree) is never staged.
type JournalStager struct {
	Set   *config.Set
	Paths config.Paths
	// Commits gives the commit of each package in the lock.
	Commits map[string]string
	// Host limits the runs to this origin host, if not "".
	Host string

	runs []*runInfo
}

type runInfo struct {
	ID      string
	Host    string
	User    string
	Commits map[string]string
	Reasons map[string][]string
	State   map[string]string
	Ended   map[string]time.Time
	Files   map[string][]StagedFile
}

// Nodes gives the plan state of the nodes of a target.
func (s *JournalStager) Nodes(ctx context.Context, target string, base *manifest.Manifest, run string) (*TargetNodes, error) {
	if s.runs == nil {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	out := &TargetNodes{}
	for _, p := range s.Set.Packages {
		if !containsStr(s.Set.PackageTargets(p), target) {
			continue
		}
		node := api.Node(p.ID, target)
		commit := s.Commits[p.ID]
		if commit == "" {
			out.NotReady = append(out.NotReady, NotReady{Node: node, State: "skip", Reason: "the package has no lock line"})
			continue
		}
		if base != nil {
			if b, ok := base.Newest(p.ID, ""); ok && b.Commit == commit {
				out.Current = append(out.Current, node)
				continue
			}
		}
		staged, last := s.find(node, commit, target, run)
		if staged != nil {
			out.Staged = append(out.Staged, *staged)
			continue
		}
		nr := NotReady{Node: node, State: "build", Reason: "no run built " + api.ShortCommit(commit)}
		if last != "" {
			nr.State, nr.Reason = last, "the node is "+last
		}
		out.NotReady = append(out.NotReady, nr)
	}
	return out, nil
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// find gives the staged node of the newest run that built the commit, and
// the state of the node in the newest run that planned it.
func (s *JournalStager) find(node api.NodeId, commit, target, only string) (*StagedNode, string) {
	last := ""
	for _, r := range s.runs {
		if only != "" && r.ID != only {
			continue
		}
		if r.Commits[node] != commit {
			continue
		}
		st := r.State[node]
		if last == "" {
			last = st
		}
		if st != string(api.NodeStateOk) || len(r.Files[node]) == 0 {
			continue
		}
		sn, err := s.staged(r, node, commit, target)
		if err != nil {
			last = "build"
			continue
		}
		return sn, ""
	}
	return nil, last
}

func (s *JournalStager) staged(r *runInfo, node api.NodeId, commit, target string) (*StagedNode, error) {
	pkg, _ := api.SplitNode(node)
	dir := s.Paths.StagingTarget(s.Set.Name, r.ID, target)
	sn := &StagedNode{Node: node, Package: pkg, Target: target, Run: r.ID, Commit: commit}
	for _, f := range r.Files[node] {
		p := locate(dir, f.Repo, f.File)
		if p == "" {
			return nil, fmt.Errorf("%s is gone from the staging of %s", f.File, r.ID)
		}
		f.Path = p
		sn.Files = append(sn.Files, f)
	}
	dp := DraftPath(s.Paths, s.Set.Name, r.ID, target, pkg)
	if b, err := os.ReadFile(dp); err == nil {
		var d Draft
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("%s: %w", dp, err)
		}
		sn.Draft, sn.Facts, sn.SourceURIs = d.Build, d.Facts, d.SourceURIs
		if d.Buildroot != "" {
			sn.Buildroot = d.Buildroot
			if !filepath.IsAbs(sn.Buildroot) {
				sn.Buildroot = filepath.Join(filepath.Dir(dp), sn.Buildroot)
			}
		}
	}
	d := &sn.Draft
	d.Package, d.Commit, d.Run = pkg, commit, r.ID
	if d.Reason == "" {
		d.Reason = "commit-changed"
		if rs := r.Reasons[node]; len(rs) > 0 {
			d.Reason = rs[0]
		}
	}
	if d.Built == "" && !r.Ended[node].IsZero() {
		d.Built = api.FormatTime(r.Ended[node])
	}
	if p, ok := s.Set.Package(pkg); ok {
		if d.Branch == "" {
			d.Branch = p.Branch
		}
		if p.External() && d.URL == nil {
			u := p.URL()
			d.URL = &u
		}
	}
	for _, f := range sn.Files {
		if f.Kind == "srpm" && d.SRPM == "" {
			d.SRPM = f.File
		}
	}
	if sn.Facts.Started.IsZero() {
		sn.Facts.Finished = r.Ended[node]
	}
	return sn, nil
}

// locate finds a file in the staging of a target: REPO/FILE,
// REPO/Packages/X/FILE or anywhere below REPO.
func locate(dir, repo, file string) string {
	for _, p := range []string{
		filepath.Join(dir, repo, file),
		filepath.Join(dir, repo, "Packages", strings.ToLower(file[:1]), file),
	} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	found := ""
	_ = filepath.WalkDir(filepath.Join(dir, repo), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == file && found == "" {
			found = p
		}
		return nil
	})
	return found
}

// load reads the journals of the build runs, newest first.
func (s *JournalStager) load() error {
	s.runs = []*runInfo{}
	dir := s.Paths.Runs(s.Set.Name)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var names []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "r-") && strings.HasSuffix(e.Name(), ".jsonl") && !strings.HasSuffix(e.Name(), ".progress.jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names {
		r, err := readRun(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		if s.Host != "" && r.Host != "" && r.Host != s.Host {
			continue
		}
		s.runs = append(s.runs, r)
	}
	return nil
}

func readRun(path string) (*runInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := &runInfo{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Commits: map[string]string{}, Reasons: map[string][]string{},
		State: map[string]string{}, Ended: map[string]time.Time{}, Files: map[string][]StagedFile{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var ev struct {
			Type    string            `json:"type"`
			Time    string            `json:"time"`
			Node    string            `json:"node"`
			Kind    string            `json:"kind"`
			State   string            `json:"state"`
			Commits map[string]string `json:"commits"`
			Build   []struct {
				Node    string   `json:"node"`
				Reasons []string `json:"reasons"`
			} `json:"build"`
			Origin struct {
				User string `json:"user"`
				Host string `json:"host"`
			} `json:"origin"`
			Selection struct {
				From string `json:"from"`
			} `json:"selection"`
			Repo      string `json:"repo"`
			NVRA      string `json:"nvra"`
			File      string `json:"file"`
			SizeBytes int64  `json:"sizeBytes"`
			SHA256    string `json:"sha256"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case string(api.EventTypeRunStarted):
			if ev.Kind != "build" {
				return nil, fmt.Errorf("not a build run")
			}
			// A run from worktrees is unpinned: it is never staged or
			// publishable (build.html).
			if ev.Selection.From == "worktree" {
				return nil, fmt.Errorf("an unpinned run")
			}
			r.User, r.Host = ev.Origin.User, ev.Origin.Host
		case string(api.EventTypePlanReady):
			for k, v := range ev.Commits {
				r.Commits[k] = v
			}
			for _, b := range ev.Build {
				r.Reasons[b.Node] = b.Reasons
			}
		case string(api.EventTypeNodeState):
			r.State[ev.Node] = ev.State
			if t, err := time.Parse(time.RFC3339, ev.Time); err == nil {
				r.Ended[ev.Node] = t
			}
		case string(api.EventTypeArtifactAdded):
			file := ev.File
			if file == "" {
				file = ev.NVRA + ".rpm"
			}
			r.Files[ev.Node] = append(r.Files[ev.Node], StagedFile{Kind: ev.Kind, Repo: ev.Repo, NVRA: ev.NVRA, File: file, SHA256: ev.SHA256, Size: ev.SizeBytes})
		}
	}
	return r, sc.Err()
}
