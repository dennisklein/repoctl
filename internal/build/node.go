package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/mock"
	"github.com/dennisklein/repoctl/internal/publish"
	"github.com/dennisklein/repoctl/internal/records"
	"github.com/dennisklein/repoctl/internal/staging"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// runNode runs the steps srpm, rpm and stage of a node in its goroutine
// and tells the scheduler the end (build.html#node-steps).
func (r *runner) runNode(n *node) {
	d := &nodeDone{n: n}
	defer func() {
		close(n.finished)
		r.done <- d
	}()
	tr := r.targets[n.target]
	kctx, kill := context.WithCancel(r.ctx)
	defer kill()
	n.stop.mu.Lock()
	n.stop.kill = kill
	n.stop.mu.Unlock()
	n.log = r.openNodeLog(n)
	src := r.plan.Sources[n.pkg]
	n.log.Marker(fmt.Sprintf("node %s: commit %s in root %s-%s (slot %d)", n.id, src.Commit, tr.mt.Config, n.ext, n.slot))
	root := tr.mt.Config + "-" + n.ext
	base := mock.Call{Files: tr.files, Uniqueext: n.ext, AddRepos: tr.addRepos}

	// Step srpm.
	var srpm *SRPMResult
	resSRPM := r.resultDir(n, api.StepSRPM)
	err := r.step(n, api.StepSRPM, root, r.o.Timeouts.SRPM, func(out io.Writer) error {
		c := base
		c.ResultDir, c.Log = resSRPM, out
		res, err := r.o.SRPM.BuildSRPM(kctx, SRPMRequest{Node: n.id, Set: r.o.Set, SetDir: r.o.SetDir, Paths: r.o.Paths, Source: src,
			Clone: filepath.Join(r.work, "src", n.id), Work: filepath.Join(r.work, "src"), Mock: r.mock, Call: c})
		r.keepLogs(n, api.StepSRPM, resSRPM)
		srpm = res
		return err
	})
	if err != nil {
		d.err, d.short = r.failure(n, api.StepSRPM, err)
		return
	}
	for _, w := range srpm.Warnings {
		r.warn(w, n.span)
	}

	// Step rpm.
	resRPM := r.resultDir(n, api.StepRPM)
	var rpms []string
	var facts []byte
	var provides, requires []string
	err = r.step(n, api.StepRPM, root, r.o.Timeouts.RPM, func(out io.Writer) error {
		c := base
		c.ResultDir, c.Log, c.Timeout = resRPM, out, r.o.Timeouts.RPM
		err := r.mock.Rebuild(kctx, c, srpm.SRPM)
		r.keepLogs(n, api.StepRPM, resRPM)
		if err != nil {
			return err
		}
		rpms = builtRPMs(resRPM)
		if len(rpms) == 0 {
			return errors.New("mock --rebuild made no RPM")
		}
		provides, requires = rpmDeps(kctx, rpms)
		d.checks = r.sonameCheck(n, provides)
		q, err := r.mock.Chroot(kctx, mock.Call{Files: tr.files, Uniqueext: n.ext}, mock.BuildrootQuery...)
		if err != nil {
			return &factsError{err}
		}
		facts, err = buildrootFile(filepath.Join(resRPM, "buildroot_lock.json"), mock.ParseBuildroot(q))
		if err != nil {
			return &factsError{err}
		}
		if err := r.mock.Clean(kctx, mock.Call{Files: tr.files, Uniqueext: n.ext}); err != nil {
			n.log.Marker("mock --clean of the root failed: " + err.Error())
		}
		return nil
	})
	if err != nil {
		d.err, d.short = r.failure(n, api.StepRPM, err)
		return
	}

	// Step stage.
	var updated []*api.StagingUpdated
	err = r.step(n, api.StepStage, "", r.o.Timeouts.Stage, func(io.Writer) error {
		var err error
		updated, err = r.stage(kctx, n, tr, srpm, rpms, facts, provides, requires)
		return err
	})
	for _, u := range updated {
		r.emit(nil, nil, nil, u)
	}
	if err != nil {
		d.err, d.short = r.failure(n, api.StepStage, err)
		return
	}
	for _, f := range rpms {
		name := filepath.Base(f)
		if staging.Kind(name) == "rpm" {
			d.nvrs = append(d.nvrs, strings.TrimSuffix(name, ".rpm"))
		}
	}
	sort.Strings(d.nvrs)
	if d.checks == nil {
		d.checks = &api.NodeChecks{Soname: "none"}
	}
}

// factsError is a failure of the buildroot facts: class mock.
type factsError struct{ err error }

func (e *factsError) Error() string { return "the buildroot facts: " + e.err.Error() }
func (e *factsError) Unwrap() error { return e.err }

func (r *runner) resultDir(n *node, s api.Step) string {
	d := filepath.Join(r.work, "results", n.id, string(s))
	_ = os.RemoveAll(d)
	_ = os.MkdirAll(d, 0o755)
	return d
}

// keepLogs copies the mock logs of a step into logs/RUN/NODE/STEP/.
func (r *runner) keepLogs(n *node, s api.Step, resdir string) {
	dst := r.o.Paths.MockLogs(r.o.Set.Name, r.o.RunID, n.id, string(s))
	for _, name := range []string{"root.log", "build.log", "state.log", "hw_info.log"} {
		b, err := os.ReadFile(filepath.Join(resdir, name))
		if err != nil {
			continue
		}
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return
		}
		_ = os.WriteFile(filepath.Join(dst, name), b, 0o600)
	}
}

// step runs one step of a node: node.step started and ended, the call
// span, the markers of the node log and the time limits.
func (r *runner) step(n *node, s api.Step, root string, limit time.Duration, fn func(out io.Writer) error) error {
	callCtx, span := progress.Start(n.ctx, progress.KindCall, string(s))
	n.log.setStep(s, span)
	n.span.Update(progress.Message(string(s)))
	r.emit(n, span, n.span, &api.NodeStepEvent{Step: s, Phase: "started", Root: root})
	n.log.Marker(string(s) + " started")
	start := r.now()
	stop := make(chan struct{})
	go r.watch(n, s, start, limit, stop)
	out := progress.Tee(callCtx, n.log, progress.StreamStdout, nil)
	err := fn(out)
	close(stop)
	exit := 0
	status := "ok"
	if err != nil {
		exit = run.ExitCode(err)
		if exit < 0 {
			exit = 1
		}
		status = "failed"
		if c, _ := n.stop.cause(); c == api.FailureClassCanceled {
			status = "canceled"
		}
	}
	r.emit(n, span, n.span, &api.NodeStepEvent{Step: s, Phase: "ended", Status: status, ExitCode: exit, DurationMs: r.since(start)})
	switch status {
	case "ok":
		span.End(nil, progress.Exit(exit))
	case "canceled":
		span.End(context.Canceled, progress.Exit(exit))
	default:
		span.End(err, progress.Exit(exit))
	}
	return err
}

// watch stops a step at its limit or at the idle limit.
func (r *runner) watch(n *node, s api.Step, start time.Time, limit time.Duration, stop <-chan struct{}) {
	idle := r.o.Timeouts.Idle
	if limit <= 0 && idle <= 0 {
		return
	}
	tick := time.Second
	for _, d := range []time.Duration{limit, idle} {
		if d > 0 && d/10 < tick {
			tick = max(d/10, 10*time.Millisecond)
		}
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		now := r.now()
		switch {
		case limit > 0 && now.Sub(start) > limit:
			r.timeout(n, s, fmt.Sprintf("build.timeout.%s (%s)", s, limit))
			return
		case idle > 0 && now.Sub(n.log.Last()) > idle:
			r.timeout(n, s, fmt.Sprintf("build.timeout.idle (%s)", idle))
			return
		}
	}
}

// timeout writes the time limit to the journal, then stops the root.
func (r *runner) timeout(n *node, s api.Step, what string) {
	msg := fmt.Sprintf("step %s passed %s", s, what)
	n.log.errorLine(msg + "; repoctl stops the root with mock --orphanskill")
	r.stopNode(n, api.FailureClassTimeout, msg, true)
}

// builtRPMs gives the RPMs of a result directory: the binary and
// debuginfo RPMs, without the SRPM.
func builtRPMs(dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, "*.rpm"))
	var out []string
	for _, f := range m {
		if !strings.HasSuffix(f, ".src.rpm") {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// rpmDeps gives the names of the provides and the requires of the binary
// RPMs (record draft).
func rpmDeps(ctx context.Context, rpms []string) (provides, requires []string) {
	seenP, seenR := map[string]bool{}, map[string]bool{}
	for _, f := range rpms {
		if staging.Kind(filepath.Base(f)) != "rpm" {
			continue
		}
		for _, q := range []struct {
			flag string
			seen map[string]bool
			out  *[]string
		}{{"--provides", seenP, &provides}, {"--requires", seenR, &requires}} {
			b, err := (&run.Cmd{Name: "rpm", Args: []string{"-qp", "--nosignature", q.flag, f}}).Output(ctx)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(b), "\n") {
				name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
				if name == "" || strings.HasPrefix(name, "rpmlib(") || q.seen[name] {
					continue
				}
				q.seen[name] = true
				*q.out = append(*q.out, name)
			}
		}
	}
	sort.Strings(provides)
	sort.Strings(requires)
	return provides, requires
}

// buildrootFile adds the header digest and the signer of each RPM of the
// root to buildroot_lock.json (build.html#buildroot-facts).
func buildrootFile(lockPath string, facts []mock.BuildrootRPM) ([]byte, error) {
	b, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(lockPath), err)
	}
	byNEVRA := map[string]mock.BuildrootRPM{}
	for _, f := range facts {
		byNEVRA[f.NEVRA] = f
	}
	if br, ok := doc["buildroot"].(map[string]any); ok {
		if list, ok := br["rpms"].([]any); ok {
			for _, x := range list {
				e, ok := x.(map[string]any)
				if !ok {
					continue
				}
				epoch := "0"
				if s, ok := e["epoch"].(string); ok && s != "" {
					epoch = s
				}
				key := fmt.Sprintf("%v-%s:%v-%v.%v", e["name"], epoch, e["version"], e["release"], e["arch"])
				f, found := byNEVRA[key]
				if !found {
					continue
				}
				// The key of records.BuildrootRPM.HeaderDigest.
				e["sha256header"] = f.HeaderSHA256
				if f.Signer == "" {
					e["signer"] = nil
				} else {
					e["signer"] = f.Signer
				}
			}
		}
	}
	return json.MarshalIndent(doc, "", "  ")
}

// stage is step stage: the RPMs and the SRPM go into the staging of the
// run, the repodata of each sub-repo is updated, and the buildroot file and
// the record draft are written. It gives the staging.updated events.
func (r *runner) stage(ctx context.Context, n *node, tr *targetRun, srpm *SRPMResult, rpms []string, facts []byte,
	provides, requires []string) ([]*api.StagingUpdated, error) {
	unlock := tr.dir.Lock()
	defer unlock()
	files := append(append([]string{}, rpms...), srpm.SRPM)
	staged, err := tr.dir.Copy(files)
	if err != nil {
		return nil, err
	}
	undo := func() {
		tr.dir.Remove(staged)
		for _, repo := range touched(staged) {
			_, _ = staging.Createrepo(ctx, tr.dir.Repo(repo))
		}
	}
	nvr := manifest.NVR(filepath.Base(srpm.SRPM))
	abs, rel := tr.dir.BuildrootFile(nvr)
	if err := staging.WriteFileAtomic(abs, facts, 0o644); err != nil {
		undo()
		return nil, err
	}
	span := n.log.curSpan()
	for _, f := range staged {
		r.emit(n, span, nil, &api.ArtifactAdded{Kind: f.Kind, Repo: f.Repo, NVRA: f.NVRA, File: f.File, SizeBytes: int(f.Size), Sha256: f.SHA256})
	}
	var out []*api.StagingUpdated
	for _, repo := range touched(staged) {
		rd, err := staging.Createrepo(ctx, tr.dir.Repo(repo))
		if err != nil {
			undo()
			return nil, err
		}
		out = append(out, &api.StagingUpdated{Target: n.target, Repo: repo, Packages: rd.Packages, Revision: rd.Revision})
	}
	sum := sha256.Sum256(facts)
	b := manifest.Build{Package: n.pkg, Branch: r.plan.Sources[n.pkg].Branch, Commit: n.pn.Commit, Reason: reasonText(n.pn),
		SRPM: filepath.Base(srpm.SRPM), Provides: provides, Requires: requires, MockConfig: tr.hash,
		Macros: "sha256:" + tr.files.MacrosSHA256, Buildroot: "sha256:" + hex.EncodeToString(sum[:]), BuildrootFile: rel,
		Run: r.o.RunID, Built: api.FormatTime(r.now()), Sources: srpm.Sources}
	if pk, ok := r.o.Set.Package(n.pkg); ok && pk.External() {
		u := pk.URL()
		b.URL = &u
	}
	for _, f := range staged {
		if f.Kind == "rpm" {
			b.RPMs = append(b.RPMs, f.File)
		}
	}
	sort.Strings(b.RPMs)
	dr := publish.Draft{Build: b, Facts: records.Facts{Started: n.started, Finished: r.now(), Mock: r.mockVersion(ctx),
		MockConfig: tr.mt.Config, Repoctl: r.o.Version, User: r.o.Origin.User, Host: r.o.Origin.Host}}
	dr.Buildroot = abs
	if relp, err := filepath.Rel(filepath.Dir(tr.dir.DraftPath(n.pkg)), abs); err == nil {
		dr.Buildroot = relp
	}
	if err := tr.dir.WriteDraft(n.pkg, dr); err != nil {
		undo()
		return nil, err
	}
	return out, nil
}

// mockVersion gives the output of mock --version, once per run.
func (r *runner) mockVersion(ctx context.Context) string {
	r.mockVerOnce.Do(func() {
		out, err := (&run.Cmd{Name: r.mock.Program, Args: []string{"--version"}}).Output(ctx)
		if err == nil {
			r.mockVer = strings.TrimSpace(string(out))
		}
	})
	return r.mockVer
}

// touched gives the sub-repos of staged files, in the order binary,
// source, debuginfo.
func touched(files []staging.File) []string {
	var out []string
	for _, f := range files {
		found := false
		for _, x := range out {
			if x == f.Repo {
				found = true
			}
		}
		if !found {
			out = append(out, f.Repo)
		}
	}
	return out
}

// reasonText gives the manifest reason of a plan node.
func reasonText(pn *api.PlanNode) string {
	for _, rs := range pn.Reasons {
		switch rs.Code {
		case api.PlanReasonCodeForced, api.PlanReasonCodeStagingMissing:
			continue
		case api.PlanReasonCodeRebuild:
			return "rebuild: " + rs.Text
		default:
			return string(rs.Code)
		}
	}
	if len(pn.Reasons) > 0 {
		return string(pn.Reasons[0].Code)
	}
	return ""
}
