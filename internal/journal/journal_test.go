package journal

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/testkit"
)

var example = filepath.Join("..", "..", "docs", "design", "contract", "examples", "journal.jsonl")

func TestNewID(t *testing.T) {
	now := time.Date(2026, 11, 2, 17, 50, 4, 0, time.Local)
	id := NewID(KindBuild, now, t.TempDir())
	if !IsRunID(id) || !strings.HasPrefix(id, "r-20261102-1750-") {
		t.Fatalf("id %q", id)
	}
	if p := NewID(KindPublish, now, ""); !strings.HasPrefix(p, "p-") || !IsRunID(p) {
		t.Fatalf("publish id %q", p)
	}
	if got := IDTime(id); !got.Equal(now.Truncate(time.Minute)) {
		t.Fatalf("IDTime %v", got)
	}
	if IsRunID("p-import-20260828") || IsJournal("r.progress.jsonl") || !IsJournal("r.jsonl") {
		t.Fatal("patterns")
	}
}

// TestFoldExample folds the journal example of the contract and compares
// it with the run of build.json.
func TestFoldExample(t *testing.T) {
	lines, err := Read(example)
	if err != nil {
		t.Fatal(err)
	}
	f := FoldLines(lines, FoldOptions{Journal: "/home/dklein/.local/state/repoctl/slurm-26.05/runs/r-20261102-1750-a3f.jsonl"})
	r := f.Run()
	if r.ID != "r-20261102-1750-a3f" || r.State != api.RunStateFailed || r.ExitCode != 3 {
		t.Fatalf("run %s %s %d", r.ID, r.State, r.ExitCode)
	}
	want := api.StateCounts{Ok: 6, Current: 13, Failed: 1, Blocked: 1}
	if r.Counts != want {
		t.Fatalf("counts %+v, want %+v", r.Counts, want)
	}
	if len(r.Publishable) != 1 || r.Publishable[0] != "el9" {
		t.Fatalf("publishable %v", r.Publishable)
	}
	var slurm, sse *api.NodeResult
	for i := range r.Nodes {
		switch r.Nodes[i].Node {
		case "slurm@el10":
			slurm = &r.Nodes[i]
		case "slurm-singularity-exec@el10":
			sse = &r.Nodes[i]
		}
	}
	if slurm == nil || slurm.Error == nil || slurm.Error.Class != api.FailureClassBuilddep || slurm.Commit == "" {
		t.Fatalf("slurm@el10 %+v", slurm)
	}
	if sse == nil || sse.State != api.NodeStateBlocked || len(sse.BlockedBy) != 1 {
		t.Fatalf("slurm-singularity-exec@el10 %+v", sse)
	}
	if len(r.PublishBlockers) != 1 || r.PublishBlockers[0].Target != "el10" || len(r.PublishBlockers[0].Nodes) != 2 {
		t.Fatalf("blockers %+v", r.PublishBlockers)
	}
	if r.Files.Logs == "" || slurm.Logs == nil || !strings.HasSuffix(slurm.Logs.Node, "/logs/r-20261102-1750-a3f/slurm@el10.log") {
		t.Fatalf("files %+v logs %+v", r.Files, slurm.Logs)
	}
	b, _ := json.Marshal(r)
	if err := testkit.Validate("Run", b); err != nil {
		t.Fatalf("Run: %v\n%s", err, b)
	}
	if len(f.Warnings()) != 1 || f.Warnings()[0].Code != api.PDepNotForTarget {
		t.Fatalf("warnings %+v", f.Warnings())
	}
}

func TestWriterAndAbandoned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs", "r-20261102-1750-a3f.jsonl")
	now := time.Date(2026, 11, 2, 17, 50, 4, 0, time.UTC)
	w, err := Create(path, "r-20261102-1750-a3f", Options{Now: func() time.Time { return now }, FlushEvery: -1, MaxLogBytes: 600})
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	c := strings.Repeat("a", 40)
	must(w.Write(&api.RunStarted{Set: "s", Kind: KindBuild, Trace: strings.Repeat("1", 32),
		Origin:    api.RunStartedOrigin{Via: api.RunOriginCli, User: "u", Host: "h", PID: 7},
		Selection: &api.Selection{Targets: []string{"el9"}, From: "lock", Jobs: 2}}))
	must(w.Write(&api.PlanReady{Summary: api.PlanReadySummary{Build: 2},
		Build:   []api.PlanReadyBuildItem{{Node: "a@el9", Reasons: []api.PlanReasonCode{api.PlanReasonCodeNewPackage}}, {Node: "b@el9", Reasons: []api.PlanReasonCode{api.PlanReasonCodeNewPackage}, Deps: []string{"a@el9"}}},
		Commits: map[string]string{"a@el9": c, "b@el9": c}}))
	must(w.Write(&api.NodeStateEvent{EventHeader: api.EventHeader{Node: "a@el9"}, State: api.NodeStateRunning, Prev: api.NodeStateWaiting}))
	for i := 0; i < 20; i++ {
		must(w.Write(&api.NodeLogEvent{EventHeader: api.EventHeader{Node: "a@el9"}, Step: api.StepRPM, Line: "line\x1b[31m red", Offset: i}))
	}
	must(w.Flush())
	f, err := FoldFile(FoldOptions{Journal: path, Host: "h", Alive: func(*api.Run) bool { return false }})
	must(err)
	r := f.Run()
	if r.State != api.RunStateAbandoned || r.Counts.Canceled != 2 {
		t.Fatalf("abandoned run %s %+v", r.State, r.Counts)
	}
	// A run of another host is never abandoned.
	f2, _ := FoldFile(FoldOptions{Journal: path, Host: "other", Alive: func(*api.Run) bool { return false }})
	if f2.Run().State != api.RunStateRunning {
		t.Fatalf("other host: %s", f2.Run().State)
	}
	if r.Nodes[1].State != api.NodeStateCanceled {
		t.Fatalf("b@el9 %s", r.Nodes[1].State)
	}
	must(w.Write(&api.NodeStateEvent{EventHeader: api.EventHeader{Node: "a@el9"}, State: api.NodeStateOk, Prev: api.NodeStateRunning, DurationMs: 10}))
	must(w.Write(&api.RunFinished{State: api.RunStateOk, DurationMs: 20}))
	must(w.Close())
	// Every line validates, seq has no gaps, and the sampling stopped.
	fh, _ := os.Open(path)
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	seq, sampling := 0, 0
	for sc.Scan() {
		if err := testkit.Validate("events", sc.Bytes()); err != nil {
			t.Fatalf("line %d: %v\n%s", seq+1, err, sc.Text())
		}
		l, err := Decode(sc.Bytes())
		must(err)
		seq++
		if l.Seq != seq {
			t.Fatalf("seq %d, want %d", l.Seq, seq)
		}
		if l.Type == api.EventTypeWarning {
			sampling++
		}
		if l.Type == api.EventTypeNodeLog && strings.Contains(sc.Text(), "\x1b") {
			t.Fatal("node.log not sanitized")
		}
	}
	if sampling != 1 || seq >= 25 {
		t.Fatalf("sampling warnings %d, lines %d", sampling, seq)
	}
	f, _ = FoldFile(FoldOptions{Journal: path, Alive: func(*api.Run) bool { return false }})
	if r := f.Run(); r.State != api.RunStateOk || r.Nodes[0].State != api.NodeStateOk {
		t.Fatalf("finished run %s", r.State)
	}
}

func TestListFindRetain(t *testing.T) {
	home := t.TempDir()
	p := config.NewPaths(func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	})
	o := FoldOptions{Paths: &p, Host: "h"}
	mk := func(id, kind string, at time.Time, finished bool) {
		w, err := Create(p.Journal("s", id), id, Options{Now: func() time.Time { return at }, FlushEvery: -1})
		if err != nil {
			t.Fatal(err)
		}
		_ = w.Write(&api.RunStarted{Set: "s", Kind: kind, Trace: strings.Repeat("2", 32), Origin: api.RunStartedOrigin{Via: api.RunOriginCli, User: "u", Host: "h"}})
		if finished {
			_ = w.Write(&api.RunFinished{State: api.RunStateOk})
		}
		_ = w.Close()
	}
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		mk(NewID(KindBuild, base.Add(time.Duration(i)*time.Hour), p.Runs("s")), KindBuild, base.Add(time.Duration(i)*time.Hour), true)
	}
	mk("p-20260901-1500-abc", KindPublish, base.Add(6*time.Hour), true)
	mk("r-20260901-1600-abc", KindBuild, base.Add(7*time.Hour), false)
	es, err := List(o, "s", false)
	if err != nil || len(es) != 7 {
		t.Fatalf("list %d %v", len(es), err)
	}
	if es[0].ID != "r-20260901-1600-abc" {
		t.Fatalf("newest %s", es[0].ID)
	}
	e, err := Find(o, "s", "latest", KindPublish)
	if err != nil || e.ID != "p-20260901-1500-abc" {
		t.Fatalf("find latest publish %v %v", e.ID, err)
	}
	if _, err := Find(o, "s", "r-20990101-0000-xyz", ""); err == nil {
		t.Fatal("find unknown run")
	} else if ae, _ := api.AsError(err); ae.Code != api.PRunNotFound {
		t.Fatalf("err %v", err)
	}
	gone, err := Retain(o, "s", Retention{KeepRuns: 2, KeepPublishes: 1})
	if err != nil || len(gone) != 3 {
		t.Fatalf("retain %d %v", len(gone), err)
	}
	es, _ = List(o, "s", false)
	if len(es) != 4 {
		t.Fatalf("after retain %d", len(es))
	}
	// The start of a check keeps the newest state.keep_checks check runs
	// and leaves the build and publish runs alone.
	for i := 0; i < 3; i++ {
		at := base.Add(time.Duration(10+i) * time.Hour)
		mk(NewID(KindCheck, at, p.Runs("s")), KindCheck, at, true)
	}
	gone, err = Retain(o, "s", Retention{KeepChecks: 1, OnlyChecks: true})
	if err != nil || len(gone) != 2 {
		t.Fatalf("retain checks %d %v", len(gone), err)
	}
	for _, e := range gone {
		if e.Fold.Run().Kind != KindCheck {
			t.Errorf("the check retention deleted %s of kind %s", e.ID, e.Fold.Run().Kind)
		}
	}
	if es, _ = List(o, "s", false); len(es) != 5 {
		t.Fatalf("after the check retention %d", len(es))
	}
	// Without Checks, the build retention never deletes a check run.
	if gone, _ = Retain(o, "s", Retention{KeepRuns: 2, KeepPublishes: 1}); len(gone) != 0 {
		t.Errorf("the build retention deleted %d runs", len(gone))
	}
}
