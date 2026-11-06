//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// TestRunViews: logs into a pipe writes the whole log to stdout and the
// header and the hint to stderr, so logs … > FILE holds only the log
// (cli.html#logs). graph --run latest shows the newest build run, as runs
// latest, with the versions of that run, not of the lock.
func TestRunViews(t *testing.T) {
	needTools(t, "createrepo_c")
	s := testkit.NewStory(t, testkit.WithPackages(storyPkgs("munge", "pmix")...),
		testkit.Modify("pmix", func(p *testkit.Pkg) { p.Build = "false" }))
	if r := s.Run("build", "--target", "el9"); r.Code != 3 {
		t.Fatalf("build: exit %d, want 3\n%s", r.Code, r.Stderr)
	}
	r := s.Run("logs", "pmix@el9")
	if r.Code != 0 {
		t.Fatalf("logs: exit %d\n%s", r.Code, r.Stderr)
	}
	t.Logf("%s", r.Stderr)
	mustContain(t, "logs stderr", r.Stderr, "pmix@el9 failed in rpm", "  run r-", "log  ", "hint: read the rpmbuild output. run: repoctl logs pmix@el9 --file build")
	mustContain(t, "logs stdout", r.Stdout, "── repoctl ── rpm failed")
	if strings.Contains(r.Stdout, "hint:") || strings.Contains(r.Stdout, "failed in rpm after") {
		t.Errorf("the header or the hint is on stdout:\n%s", r.Stdout)
	}

	// A new version of munge in the lock: graph --run latest shows the
	// version that the run built.
	old := s.BranchCommit("munge")
	bumped := s.Bump("munge", "0.5.19")
	lockFile := filepath.Join(s.SetDir, "repoctl.lock")
	writeFile(t, lockFile, strings.ReplaceAll(readFile(t, lockFile), old, bumped))
	s.Git(s.SetDir, "commit", "-qam", "Pin munge 0.5.19")
	env, _ := s.RunJSON("runs", "latest")
	runID := items(t, env)[0]["id"].(string)
	r = s.MustRun("graph", "--target", "el9", "--run", "latest")
	t.Logf("%s", r.Stdout)
	mustContain(t, "graph --run latest", r.Stdout, "run "+runID)
	for _, l := range strings.Split(r.Stdout, "\n") {
		switch {
		case strings.Contains(l, " munge "):
			if !strings.Contains(l, "0.5.18") || !strings.Contains(l, "ok") {
				t.Errorf("munge row: %q, want the version of the run 0.5.18 and ok", l)
			}
		case strings.Contains(l, " pmix "):
			if !strings.Contains(l, "failed") {
				t.Errorf("pmix row: %q, want failed", l)
			}
		}
	}
	if strings.Contains(r.Stdout, "0.5.19") {
		t.Errorf("graph --run shows the lock version 0.5.19:\n%s", r.Stdout)
	}
}

// TestBuildUsageFirst: a usage error of build comes before the set lock and
// the lock check, and the set.busy hint keeps the selectors (cli.html#exit,
// build.html#set-lock).
func TestBuildUsageFirst(t *testing.T) {
	s := testkit.NewStory(t, testkit.WithPackages(storyPkgs("munge")...))
	s.MustRun("checkout", "munge")
	spec := filepath.Join(s.SetDir, "packages", "munge", "munge.spec")
	writeFile(t, spec, readFile(t, spec)+"\n# a change\n")

	// The worktree is dirty: build stops with exit 5, but --progress tree
	// without a terminal is a usage error first.
	if r := s.Run("build", "munge"); r.Code != 5 {
		t.Fatalf("build on a dirty worktree: exit %d, want 5\n%s", r.Code, r.Stderr)
	}
	r := s.Run("build", "munge", "--progress", "tree")
	if r.Code != 2 {
		t.Fatalf("build --progress tree: exit %d, want 2\n%s", r.Code, r.Stderr)
	}
	mustContain(t, "build --progress tree", r.Stderr, "asks for a live tree")

	// Another process holds the set lock.
	paths := config.NewPaths(os.Getenv)
	l, err := lock.Acquire(context.Background(), paths.SetLock(testkit.SetName),
		lock.Holder{Command: "publish", PID: os.Getpid(), Host: "hpc-a2", User: "d.klein", Started: time.Now()}, 0, false, lock.What{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	r = s.Run("build", "munge")
	t.Logf("%s", r.Stderr)
	if r.Code != 7 {
		t.Fatalf("build on a busy set: exit %d, want 7\n%s", r.Code, r.Stderr)
	}
	// The selectors in the retry come from App.SetLock (retryCommand on the
	// integration branch).
	mustContain(t, "build stderr", r.Stderr, "set slurm-26.05 is busy", "run: repoctl build munge --lock-wait 2h")
}
