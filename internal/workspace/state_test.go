package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
)

// fixture is a set directory with package branches and worktrees.
type fixture struct {
	t   *testing.T
	dir string
	set *config.Set
	lf  *lock.File
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

const stateYAML = `apiVersion: repoctl/v1alpha1
name: test-1
repo: {name: test}
targets:
  - {id: el9, mock: rocky+epel-9-x86_64, plugins: [rpmautospec]}
packages:
  - {id: insync, branch: insync/b}
  - {id: ahead, branch: ahead/b}
  - {id: behind, branch: behind/b}
  - {id: diverged, branch: diverged/b}
  - {id: dirty, branch: dirty/b}
  - {id: missing, branch: missing/b}
  - {id: unpinned, branch: unpinned/b}
  - {id: unknown, branch: unknown/b}
  - {id: release, path: packages/release}
`

// newFixture makes the set directory: one branch with two commits per
// package, a worktree per package, and a lock at the first commit.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	gitcfg := filepath.Join(home, ".gitconfig")
	write(t, gitcfg, "[user]\n\tname = T\n\temail = t@example.org\n[init]\n\tdefaultBranch = main\n[advice]\n\tdetachedHead = false\n")
	t.Setenv("GIT_CONFIG_GLOBAL", gitcfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", home)
	dir := filepath.Join(t.TempDir(), "set")
	gitIn(t, filepath.Dir(dir), "init", "-q", dir)
	write(t, filepath.Join(dir, "repoctl.yaml"), stateYAML)
	write(t, filepath.Join(dir, "packages/release/release.spec"), "Name: release\n")
	write(t, filepath.Join(dir, ".gitignore"), "/packages/*\n!/packages/release/\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "set")
	set, err := config.ParseSet([]byte(stateYAML), "repoctl.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: dir, set: set, lf: lock.Empty()}
	for _, pk := range set.Packages {
		if pk.InTree() {
			continue
		}
		gitIn(t, dir, "branch", "-q", pk.Branch, "main")
		wt := WorktreePath(dir, pk.ID)
		gitIn(t, dir, "worktree", "add", "-q", wt, pk.Branch)
		write(t, filepath.Join(wt, pk.ID+".spec"), "Name: "+pk.ID+"\nVersion: 1\n")
		gitIn(t, wt, "add", "-A")
		gitIn(t, wt, "commit", "-qm", "Import "+pk.ID)
		if pk.ID != "unpinned" {
			f.lf.Set(pk.ID, lock.Entry{Branch: pk.Branch, Commit: gitIn(t, wt, "rev-parse", "HEAD")})
		}
	}
	return f
}

func (f *fixture) commit(pkg, msg string) string {
	wt := WorktreePath(f.dir, pkg)
	gitIn(f.t, wt, "commit", "-q", "--allow-empty", "-m", msg)
	return gitIn(f.t, wt, "rev-parse", "HEAD")
}

func TestStates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// ahead: a new commit in the worktree.
	f.commit("ahead", "Bump")
	// behind: the lock has a newer commit than the worktree.
	newer := f.commit("behind", "Bump")
	gitIn(t, WorktreePath(f.dir, "behind"), "reset", "-q", "--hard", "HEAD~1")
	f.lf.Set("behind", lock.Entry{Branch: "behind/b", Commit: newer})
	// diverged: the lock and the worktree have one commit each.
	lockSide := f.commit("diverged", "Lock side")
	gitIn(t, WorktreePath(f.dir, "diverged"), "reset", "-q", "--hard", "HEAD~1")
	f.commit("diverged", "Worktree side")
	f.lf.Set("diverged", lock.Entry{Branch: "diverged/b", Commit: lockSide})
	// dirty: a changed tracked file.
	write(t, filepath.Join(WorktreePath(f.dir, "dirty"), "dirty.spec"), "changed\n")
	// missing: no worktree.
	gitIn(t, f.dir, "worktree", "remove", "--force", WorktreePath(f.dir, "missing"))
	// unknown: the lock commit is not in the repository.
	f.lf.Set("unknown", lock.Entry{Branch: "unknown/b", Commit: strings.Repeat("ab", 20)})
	// orphaned: a lock line without an entry.
	f.lf.Set("gone", lock.Entry{Branch: "gone/b", Commit: strings.Repeat("cd", 20)})

	states, err := States(ctx, config.Paths{}, f.set, f.dir, f.lf)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]api.LockState{
		"insync": api.LockStateInSync, "ahead": api.LockStateAhead, "behind": api.LockStateBehind,
		"diverged": api.LockStateDiverged, "dirty": api.LockStateDirty, "missing": api.LockStateMissing,
		"unpinned": api.LockStateUnpinned, "unknown": api.LockStateBehind, "release": api.LockStateInTree,
		"gone": api.LockStateOrphaned,
	}
	var order []string
	for _, s := range states {
		order = append(order, s.Package)
		if s.State != want[s.Package] {
			t.Errorf("%s: %s, want %s", s.Package, s.State, want[s.Package])
		}
		e := s.Entry()
		if e.Package != s.Package || e.State != s.State {
			t.Errorf("%s: entry %+v", s.Package, e)
		}
	}
	if got := strings.Join(order, " "); got != "insync ahead behind diverged dirty missing unpinned unknown release gone" {
		t.Errorf("order %s", got)
	}
	m := StateMap(states)
	if s := m["ahead"]; s.Ahead != 1 || s.Behind != 0 || s.HeadSubject != "Bump" {
		t.Errorf("ahead: %+v", s)
	}
	if s := m["diverged"]; s.Ahead != 1 || s.Behind != 1 {
		t.Errorf("diverged: %+v", s)
	}
	if s := m["dirty"]; strings.Join(s.DirtyFiles, " ") != "dirty.spec" {
		t.Errorf("dirty: %+v", s.DirtyFiles)
	}
	if s := m["release"]; s.LockCommit == "" || s.Path != "packages/release" {
		t.Errorf("release: %+v", s)
	}
	for _, s := range states {
		if s.State != api.LockStateInSync && s.State != api.LockStateInTree && s.Message() == "" {
			t.Errorf("%s: no message", s.Package)
		}
	}

	// An uncommitted change in the in-tree path makes it dirty.
	write(t, filepath.Join(f.dir, "packages/release/release.spec"), "Name: release\nVersion: 2\n")
	pk, _ := f.set.Package("release")
	s, err := State(ctx, config.Paths{}, f.dir, *pk, f.lf)
	if err != nil || s.State != api.LockStateDirty {
		t.Errorf("in-tree with a change: %s %v", s.State, err)
	}
}

func TestCheckClean(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.commit("ahead", "Bump")
	gitIn(t, f.dir, "worktree", "remove", "--force", WorktreePath(f.dir, "missing"))
	exit := func(err error) (api.ProblemCode, int) {
		if err == nil {
			return "", 0
		}
		e, ok := api.AsError(err)
		if !ok {
			t.Fatalf("not an api error: %v", err)
		}
		return e.Code, e.ExitCode
	}
	for _, tc := range []struct {
		pkgs []string
		code api.ProblemCode
	}{
		{[]string{"insync", "missing", "release"}, ""},
		{[]string{"insync", "ahead"}, api.PLockDirty},
		{[]string{"unpinned"}, api.PLockMissingPackage},
		{nil, api.PLockDirty},
	} {
		code, ex := exit(CheckClean(ctx, config.Paths{}, f.set, f.dir, f.lf, tc.pkgs))
		if code != tc.code || (code != "" && ex != api.ExitLockDirty) {
			t.Errorf("CheckClean %v: %s exit %d, want %s", tc.pkgs, code, ex, tc.code)
		}
	}
	err := CheckClean(ctx, config.Paths{}, f.set, f.dir, f.lf, []string{"ahead"})
	e, _ := api.AsError(err)
	if e == nil || e.HumanAction == nil || e.HumanAction.Action != "commit-lock" || e.Path != "packages/ahead" {
		t.Errorf("ahead error: %+v", e)
	}
	// A committed lock passes; a changed one does not.
	if err := f.lf.Write(f.dir, nil); err != nil {
		t.Fatal(err)
	}
	if err := CheckLockCommitted(ctx, f.dir); err == nil {
		t.Error("CheckLockCommitted takes an untracked repoctl.lock")
	}
	gitIn(t, f.dir, "add", "repoctl.lock")
	gitIn(t, f.dir, "commit", "-qm", "pin")
	if err := CheckLockCommitted(ctx, f.dir); err != nil {
		t.Errorf("CheckLockCommitted: %v", err)
	}
}
