package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/graph"
	"github.com/dennisklein/repoctl/internal/journal"
	"github.com/dennisklein/repoctl/internal/plan"
	"github.com/dennisklein/repoctl/internal/testkit"
)

func TestDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		4200 * time.Millisecond:               "4.2s",
		25 * time.Second:                      "25s",
		108 * time.Second:                     "1m48s",
		41*time.Minute + 7*time.Second:        "41m07s",
		72 * time.Minute:                      "1h12m",
		2*time.Hour + 30*time.Second:          "2h00m",
		0:                                     "0.0s",
		59*time.Second + 999*time.Millisecond: "59s",
	} {
		if got := Dur(d); got != want {
			t.Errorf("Dur(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestCountWords(t *testing.T) {
	c := api.StateCounts{Ok: 6, Failed: 1, Blocked: 1, Current: 13}
	if got := CountWords(c, true); got != "6 ok, 1 failed, 1 blocked, 13 current" {
		t.Fatal(got)
	}
	if got := CountWords(c, false); got != "6 ok, 1 failed, 1 blocked" {
		t.Fatal(got)
	}
	if got := CountWords(api.StateCounts{}, true); got != "nothing to build" {
		t.Fatal(got)
	}
}

func TestErrorLine(t *testing.T) {
	for line, want := range map[string]bool{
		"DEBUG util.py: No matching package to install: 'rocm-smi-devel'":                    true,
		"error: Failed build dependencies: rocm-smi-devel is needed by slurm-26.05.4-2.el10": true,
		"ERROR: Exception(/builddir/build/SRPMS/slurm-26.05.4-2.el10.src.rpm)":               true,
		"error: Bad exit status from /var/tmp/rpm-tmp.x (%build)":                            true,
		"INFO: Start: build setup for slurm-26.05.4-2.el10.src.rpm":                          false,
		"checking for strerror... yes":                                                       false,
		"gcc -Werror=format-security -c foo.c":                                               false,
	} {
		if got := ErrorLine.MatchString(line); got != want {
			t.Errorf("ErrorLine(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestNotForTarget(t *testing.T) {
	c := &plan.Computed{
		Graphs: map[string]*graph.Graph{"el9": nil, "el10": nil},
		Infos: map[api.NodeId]*graph.Info{
			"rocm-smi@el9": {Node: "rocm-smi@el9", Package: "rocm-smi", Target: "el9",
				Provides: []string{"rocm-smi", "rocm-smi-devel"}, RPMs: []string{"rocm-smi", "rocm-smi-devel"}},
			"slurm@el9": {Node: "slurm@el9", Package: "slurm", Target: "el9",
				Requires: []string{"rocm-smi-devel", "munge-devel >= 0.5"}},
			"slurm@el10": {Node: "slurm@el10", Package: "slurm", Target: "el10",
				Requires: []string{"rocm-smi-devel", "munge-devel >= 0.5", "gcc"}},
			"munge@el10": {Node: "munge@el10", Package: "munge", Target: "el10", Provides: []string{"munge-devel"}},
			"munge@el9":  {Node: "munge@el9", Package: "munge", Target: "el9", Provides: []string{"munge-devel"}},
		},
	}
	got := notForTarget(c)
	want := []NotForTarget{{Node: "slurm@el10", Requires: "rocm-smi-devel", Package: "rocm-smi", Targets: []string{"el9"}}}
	if len(got) != 1 || got[0].Node != want[0].Node || got[0].Requires != want[0].Requires ||
		got[0].Package != want[0].Package || !slices.Equal(got[0].Targets, want[0].Targets) {
		t.Fatalf("notForTarget = %+v", got)
	}
}

func TestParseSpec(t *testing.T) {
	si := parseSpec(`Name: slurm
Version: 26.05.4
BuildRequires: munge-devel, ucx-devel >= 1.22
BuildRequires: pmix-devel
Provides: slurm-libs = 26.05
%package devel
%package -n perl-Slurm
`)
	if !slices.Equal(si.buildRequires, []string{"munge-devel", "ucx-devel", "pmix-devel"}) {
		t.Fatalf("buildRequires %v", si.buildRequires)
	}
	for _, p := range []string{"slurm", "slurm-libs", "slurm-devel", "perl-Slurm"} {
		if !slices.Contains(si.provides, p) {
			t.Fatalf("provides %v has no %s", si.provides, p)
		}
	}
}

func TestClosure(t *testing.T) {
	nodes := []api.NodeId{"a@el9", "b@el9", "c@el9", "d@el9"}
	edges := map[api.NodeId][]api.NodeId{"b@el9": {"a@el9"}, "c@el9": {"b@el9"}}
	if got := closure(nodes, edges, []api.NodeId{"c@el9"}, true, false); !slices.Equal(got, []api.NodeId{"b@el9", "a@el9"}) {
		t.Fatalf("deps %v", got)
	}
	if got := closure(nodes, edges, []api.NodeId{"a@el9"}, false, true); !slices.Equal(got, []api.NodeId{"b@el9", "c@el9"}) {
		t.Fatalf("rdeps %v", got)
	}
}

func TestMoveLogs(t *testing.T) {
	from, to := t.TempDir(), filepath.Join(t.TempDir(), "srpm")
	for _, n := range []string{"root.log", "build.log", "x.src.rpm"} {
		if err := os.WriteFile(filepath.Join(from, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	moveLogs(from, to)
	for _, n := range []string{"root.log", "build.log"} {
		if _, err := os.Stat(filepath.Join(to, n)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(from, "x.src.rpm")); err != nil {
		t.Fatal("moveLogs moved an SRPM")
	}
}

const testSet = `apiVersion: repoctl/v1alpha1
name: demo

repo:
  name: demo
  url: https://repo.example.org/demo

targets:
  - id: el9
    mock: rocky+epel-9-x86_64
    plugins: [rpmautospec]

packages:
  - id: tiny
    branch: tiny/demo
`

// planFunc is a Planner of a test.
type planFunc func(ctx context.Context, r PlanRequest) (*Plan, error)

func (f planFunc) Plan(ctx context.Context, r PlanRequest) (*Plan, error) { return f(ctx, r) }

func testRun(t *testing.T, pl Planner, mods ...func(*Options)) (*Result, []byte, error) {
	t.Helper()
	dir := t.TempDir()
	set, err := config.ParseSet([]byte(testSet), filepath.Join(dir, "repoctl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": dir, "XDG_STATE_HOME": filepath.Join(dir, "state"), "XDG_DATA_HOME": filepath.Join(dir, "data"),
		"XDG_CACHE_HOME": filepath.Join(dir, "cache"), "XDG_CONFIG_HOME": filepath.Join(dir, "config"), "REPOCTL_TEST_HOST": "1"}
	p := config.NewPaths(func(k string) string { return env[k] })
	if err := os.MkdirAll(p.Runs(set.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	id := journal.NewID(journal.KindBuild, time.Now(), p.Runs(set.Name))
	o := Options{
		Set: set, SetDir: dir, Paths: p, RunID: id,
		Origin:    api.RunStartedOrigin{Via: api.RunOriginCli, User: "tester", Host: "host", PID: os.Getpid()},
		Selection: &plan.Selection{Targets: []string{"el9"}}, Planner: pl, SRPM: CloneBuilder{}, Staged: nil,
		Jobs: 2, Version: "test",
	}
	for _, m := range mods {
		m(&o)
	}
	res, err := Run(context.Background(), o)
	data, rerr := os.ReadFile(p.Journal(set.Name, id))
	if rerr != nil {
		t.Fatal(rerr)
	}
	testkit.ValidateJournal(t, data)
	return res, data, err
}

// A plan error ends the run failed with the exit code of the error.
func TestRunPlanError(t *testing.T) {
	res, data, err := testRun(t, planFunc(func(context.Context, PlanRequest) (*Plan, error) {
		return nil, api.Errorf(api.PGraphCycle, "the graph of el9 has a cycle").WithExit(api.ExitFailed)
	}))
	if e, ok := api.AsError(err); !ok || e.Code != api.PGraphCycle {
		t.Fatalf("error %v", err)
	}
	if res == nil || res.Run.State != api.RunStateFailed {
		t.Fatalf("result %+v", res)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"type":"run.started"`) || !strings.Contains(lines[1], `"type":"run.finished"`) {
		t.Fatalf("journal:\n%s", data)
	}
}

// A plan without build nodes ends the run ok at once.
func TestRunNothingToBuild(t *testing.T) {
	res, data, err := testRun(t, planFunc(func(_ context.Context, r PlanRequest) (*Plan, error) {
		p := &Plan{}
		p.Targets = r.Selection.Targets
		p.Nodes = []api.PlanNode{{Node: "tiny@el9", Action: api.PlanActionCurrent, Commit: strings.Repeat("a", 40)}}
		p.Summarize()
		return p, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Run.State != api.RunStateOk || res.Failed() {
		t.Fatalf("run %+v", res.Run)
	}
	if !strings.Contains(string(data), `"type":"plan.ready"`) {
		t.Fatalf("journal has no plan.ready:\n%s", data)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("canceled")
	}
}

// The freshness check runs first in step plan: its warnings go to the
// journal, and state.behind ends the run failed with exit 13 and no
// plan.ready (build.html#run).
func TestRunFreshness(t *testing.T) {
	planned := false
	current := planFunc(func(_ context.Context, r PlanRequest) (*Plan, error) {
		planned = true
		p := &Plan{}
		p.Targets = r.Selection.Targets
		p.Nodes = []api.PlanNode{{Node: "tiny@el9", Action: api.PlanActionCurrent, Commit: strings.Repeat("a", 40)}}
		p.Summarize()
		return p, nil
	})
	var warned []api.Problem
	res, data, err := testRun(t, current, func(o *Options) {
		o.Warn = func(p api.Problem) { warned = append(warned, p) }
		o.Fresh = func(_ context.Context, targets []string) ([]api.Problem, error) {
			return []api.Problem{{Code: api.PRegistryUnreachable, Target: targets[0], Message: "the registry does not answer for el9; repoctl uses the local copy"}}, nil
		}
	})
	if err != nil || res.Run.State != api.RunStateOk || !planned {
		t.Fatalf("unreachable: %v %+v", err, res)
	}
	if len(warned) != 1 || !strings.Contains(string(data), `"code":"registry.unreachable"`) {
		t.Fatalf("the warning is not in the journal and the result: %v\n%s", warned, data)
	}

	planned = false
	res, data, err = testRun(t, current, func(o *Options) {
		o.Fresh = func(context.Context, []string) ([]api.Problem, error) {
			return nil, api.Errorf(api.PStateBehind, "latest of el9 is p-20261103-1412-5e1, no local copy").WithTarget("el9")
		}
	})
	if e, ok := api.AsError(err); !ok || e.Code != api.PStateBehind || planned {
		t.Fatalf("behind: %v, planned %v", err, planned)
	}
	if res.Run.State != api.RunStateFailed || res.Run.ExitCode != api.ExitStateConflict {
		t.Fatalf("behind: run %s exit %d", res.Run.State, res.Run.ExitCode)
	}
	if strings.Contains(string(data), `"type":"plan.ready"`) {
		t.Fatalf("plan.ready after state.behind:\n%s", data)
	}
}
