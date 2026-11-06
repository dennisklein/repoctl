//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// The workspace commands on the story: init, checkout, pin, rebuild,
// migrate, schema (cli.html#group-workspace).

func wsItems(env map[string]any) []map[string]any {
	var out []map[string]any
	for _, it := range env["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func itemOf(t *testing.T, env map[string]any, pkg string) map[string]any {
	t.Helper()
	for _, it := range wsItems(env) {
		if it["package"] == pkg {
			return it
		}
	}
	t.Fatalf("no item of %s in %v", pkg, env["items"])
	return nil
}

func lockCommit(t *testing.T, s *testkit.Story, pkg string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.SetDir, "repoctl.lock"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	i := strings.Index(text, "  "+pkg+":")
	if i < 0 {
		t.Fatalf("no line of %s in repoctl.lock:\n%s", pkg, text)
	}
	rest := text[i:]
	j := strings.Index(rest, "commit: ")
	return rest[j+8 : j+48]
}

func TestCheckoutStory(t *testing.T) {
	s := testkit.NewStory(t)
	r := s.MustRun("checkout", "--progress", "plain")
	t.Logf("checkout:\n%s%s", r.Stdout, r.Stderr)
	if !strings.Contains(r.Stdout, "11 packages: 10 worktrees created, 1 in tree") {
		t.Errorf("totals line:\n%s", r.Stdout)
	}
	for _, p := range s.Pkgs {
		if p.Path != "" {
			continue
		}
		wt := filepath.Join(s.SetDir, "packages", p.ID)
		if _, err := os.Stat(filepath.Join(wt, p.SpecName())); err != nil {
			t.Errorf("worktree of %s: %v", p.ID, err)
		}
		if got := s.Git(wt, "rev-parse", "HEAD"); got != s.BranchCommit(p.ID) {
			t.Errorf("%s at %s, want %s", p.ID, got, s.BranchCommit(p.ID))
		}
	}
	// rocm-smi is a worktree of the bare clone below XDG_DATA_HOME.
	common := s.Git(filepath.Join(s.SetDir, "packages", "rocm-smi"), "rev-parse", "--git-common-dir")
	if !strings.HasPrefix(common, filepath.Join(s.Home, ".local", "share", "repoctl", "git")) || !strings.HasSuffix(common, ".git") {
		t.Errorf("rocm-smi common dir %s", common)
	}
	// The worktrees do not show in git status of the set.
	if st := s.Git(s.SetDir, "status", "--porcelain"); st != "" {
		t.Errorf("git status of the set:\n%s", st)
	}
	// A second checkout is in sync.
	r = s.MustRun("checkout", "--progress", "plain")
	t.Logf("second checkout:\n%s%s", r.Stdout, r.Stderr)
	if !strings.Contains(r.Stdout, "10 in-sync, 1 in tree") {
		t.Errorf("second checkout:\n%s", r.Stdout)
	}
	r = s.MustRun("checkout", "-v")
	t.Logf("checkout -v:\n%s", r.Stdout)
	if !strings.Contains(r.Stdout, "PKG") || !strings.Contains(r.Stdout, "slurm-26.05 (external)") || strings.Contains(r.Stdout, "-v shows all") {
		t.Errorf("checkout -v:\n%s", r.Stdout)
	}
	env, _ := s.RunJSON("checkout")
	if env["kind"] != "Lock" || len(wsItems(env)) != 11 {
		t.Errorf("checkout --json: %v", env)
	}
	if st := itemOf(t, env, "slurm-release")["state"]; st != "in-tree" {
		t.Errorf("slurm-release state %v", st)
	}
	if st := itemOf(t, env, "ucx")["state"]; st != "in-sync" {
		t.Errorf("ucx state %v", st)
	}
}

func TestPinAndRebuild(t *testing.T) {
	s := testkit.NewStory(t)
	s.MustRun("checkout")
	ucx := filepath.Join(s.SetDir, "packages", "ucx")
	old := lockCommit(t, s, "ucx")

	// A new commit in the worktree: ucx is ahead of the lock.
	spec := filepath.Join(ucx, "ucx.spec")
	data, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec, []byte(strings.Replace(string(data), "1.22.0", "1.22.1", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Git(ucx, "commit", "-qam", "Bump to 1.22.1")
	head := s.Git(ucx, "rev-parse", "HEAD")
	env, r := s.RunJSON("checkout", "ucx")
	if st := itemOf(t, env, "ucx")["state"]; st != "ahead" {
		t.Errorf("ucx after a commit in the worktree: %v\n%s", st, r.Stderr)
	}

	// pin writes the new commit and warns that it is not on origin.
	r = s.MustRun("pin", "ucx")
	if st := s.Git(s.SetDir, "status", "--porcelain"); st != "M repoctl.lock" {
		t.Errorf("git status of the set after pin:\n%s", st)
	}
	t.Logf("pin:\n%s%s", r.Stdout, r.Stderr)
	if got := lockCommit(t, s, "ucx"); got != head {
		t.Fatalf("lock of ucx %s, want %s", got, head)
	}
	if !strings.Contains(r.Stdout, "ucx") || !strings.Contains(r.Stdout, old[:7]) || !strings.Contains(r.Stdout, `"Bump to 1.22.1"`) {
		t.Errorf("pin output:\n%s", r.Stdout)
	}
	if !strings.Contains(r.Stderr, "is not on origin/ucx/slurm-26.05") {
		t.Errorf("pin does not warn about the unpushed commit:\n%s", r.Stderr)
	}
	if !strings.Contains(r.Stderr, "git add repoctl.lock && git commit && git push") {
		t.Errorf("pin Next:\n%s", r.Stderr)
	}
	// A second pin has nothing to do.
	r = s.MustRun("pin", "ucx")
	if strings.Contains(r.Stdout, "→") {
		t.Errorf("second pin:\n%s", r.Stdout)
	}
	s.Git(ucx, "push", "-q", "origin", "ucx/slurm-26.05")
	s.Git(s.SetDir, "commit", "-qam", "Pin ucx 1.22.1")

	// rebuild --dry-run gives the next NVR and changes nothing.
	slurmOld := lockCommit(t, s, "slurm")
	r = s.MustRun("rebuild", "slurm", "--reason", "rebuild for ucx 1.22.1", "--dry-run")
	t.Logf("rebuild --dry-run:\n%s%s", r.Stdout, r.Stderr)
	if !strings.Contains(r.Stdout, "slurm-26.05.4-1") || !strings.Contains(r.Stdout, "slurm-26.05.4-2") {
		t.Errorf("rebuild --dry-run:\n%s", r.Stdout)
	}
	if lockCommit(t, s, "slurm") != slurmOld {
		t.Fatal("rebuild --dry-run wrote repoctl.lock")
	}

	// rebuild makes the empty commit and pins it.
	env, r = s.RunJSON("rebuild", "slurm", "--reason", "rebuild for ucx 1.22.1")
	slurm := filepath.Join(s.SetDir, "packages", "slurm")
	newHead := s.Git(slurm, "rev-parse", "HEAD")
	if newHead == slurmOld || lockCommit(t, s, "slurm") != newHead {
		t.Fatalf("rebuild: head %s, lock %s, old %s", newHead, lockCommit(t, s, "slurm"), slurmOld)
	}
	if msg := s.Git(slurm, "log", "-1", "--format=%s"); msg != "Rebuild: rebuild for ucx 1.22.1" {
		t.Errorf("rebuild commit message %q", msg)
	}
	if files := s.Git(slurm, "show", "--format=", "--name-only", "HEAD"); files != "" {
		t.Errorf("rebuild commit is not empty: %s", files)
	}
	if it := itemOf(t, env, "slurm"); it["commit"] != newHead {
		t.Errorf("rebuild --json item %v", it)
	}
	s.Git(s.SetDir, "commit", "-qam", "Rebuild slurm")
	r = s.Run("rebuild", "slurm", "--reason", "short")
	if r.Code != 2 {
		t.Errorf("rebuild --reason short: exit %d", r.Code)
	}
	env, r = s.RunJSON("rebuild", "slurm-release", "--reason", "rebuild for ucx 1.22.1")
	if r.Code != 1 || !contains(codes(env, "errors"), "rebuild.in-tree") {
		t.Errorf("rebuild of an in-tree package: exit %d\n%s", r.Code, r.Stdout)
	}
	// A glob leaves the in-tree package out with a warning.
	env, r = s.RunJSON("rebuild", "slurm*", "--reason", "rebuild for ucx 1.22.1", "--dry-run")
	if r.Code != 0 || !contains(codes(env, "warnings"), "rebuild.in-tree") || len(wsItems(env)) != 2 {
		t.Errorf("rebuild of a glob: exit %d\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
}

func TestPinOlderAndOrphans(t *testing.T) {
	s := testkit.NewStory(t)
	newer := s.Bump("ucx", "1.22.1")
	s.MustRun("checkout", "--fetch")
	ucx := filepath.Join(s.SetDir, "packages", "ucx")
	old := lockCommit(t, s, "ucx")
	s.Git(ucx, "merge", "-q", "--ff-only", "origin/ucx/slurm-26.05")
	s.MustRun("pin", "ucx")
	if lockCommit(t, s, "ucx") != newer {
		t.Fatal("pin of the fast-forward")
	}
	// The worktree goes back: pin PKG stops, pin keeps the line and warns.
	s.Git(ucx, "reset", "-q", "--hard", old)
	env, r := s.RunJSON("pin", "ucx")
	if r.Code != 5 || !contains(codes(env, "errors"), "lock.behind") {
		t.Errorf("pin of a worktree behind: exit %d\n%s", r.Code, r.Stdout)
	}
	env, r = s.RunJSON("pin")
	if r.Code != 0 || !contains(codes(env, "warnings"), "lock.behind") || lockCommit(t, s, "ucx") != newer {
		t.Errorf("pin of all with a worktree behind: exit %d\n%s", r.Code, r.Stdout)
	}
	r = s.MustRun("pin", "ucx", "--older")
	t.Logf("pin --older:\n%s%s", r.Stdout, r.Stderr)
	if lockCommit(t, s, "ucx") != old {
		t.Errorf("pin --older did not write the older commit")
	}
	if r = s.Run("pin", "--older"); r.Code != 2 {
		t.Errorf("pin --older without PKG: exit %d", r.Code)
	}

	// An entry leaves repoctl.yaml: its line is orphaned.
	yml := filepath.Join(s.SetDir, "repoctl.yaml")
	data, err := os.ReadFile(yml)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	i := strings.Index(text, "  - id: slurm-singularity-exec")
	j := strings.Index(text, "  - id: slurm-release")
	if err := os.WriteFile(yml, []byte(text[:i]+text[j:]), 0o644); err != nil {
		t.Fatal(err)
	}
	if r = s.Run("pin", "slurm-singularity-exec"); r.Code != 2 {
		t.Errorf("pin of an orphaned package: exit %d\n%s", r.Code, r.Stderr)
	}
	env, _ = s.RunJSON("pin", "--orphans")
	if it := itemOf(t, env, "slurm-singularity-exec"); it["state"] != "orphaned" || it["changed"] != true {
		t.Errorf("pin --orphans item: %v", it)
	}
	s.Git(s.SetDir, "checkout", "-q", "repoctl.lock")
	r = s.MustRun("pin", "--orphans")
	t.Logf("pin --orphans:\n%s%s", r.Stdout, r.Stderr)
	if !strings.Contains(r.Stdout, "removed") || lockCommit(t, s, "ucx") != old {
		t.Errorf("pin --orphans:\n%s", r.Stdout)
	}
	data, _ = os.ReadFile(filepath.Join(s.SetDir, "repoctl.lock"))
	if strings.Contains(string(data), "slurm-singularity-exec") {
		t.Errorf("pin --orphans kept the line:\n%s", data)
	}
}

func TestRebuildHuman(t *testing.T) {
	s := testkit.NewStory(t)
	s.MustRun("checkout")
	old := lockCommit(t, s, "slurm")
	r := s.MustRun("rebuild", "slurm", "--reason", "rebuild for ucx 1.22.1")
	t.Logf("rebuild:\n%s%s", r.Stdout, r.Stderr)
	if !strings.Contains(r.Stdout, old[:7]) || !strings.Contains(r.Stdout, "26.05.4-1 → 26.05.4-2") {
		t.Errorf("rebuild output:\n%s", r.Stdout)
	}
	if !strings.Contains(r.Stderr, "git -C packages/slurm push origin slurm/slurm-26.05") {
		t.Errorf("rebuild Next:\n%s", r.Stderr)
	}
	// The second rebuild counts on: -3.
	s.Git(filepath.Join(s.SetDir, "packages", "slurm"), "push", "-q", "origin", "slurm/slurm-26.05")
	r = s.MustRun("rebuild", "slurm", "--reason", "rebuild for pmix 6.1.1", "--dry-run")
	if !strings.Contains(r.Stdout, "slurm-26.05.4-2 → slurm-26.05.4-3") {
		t.Errorf("second rebuild --dry-run:\n%s", r.Stdout)
	}
}

func TestCheckoutKeepsDirty(t *testing.T) {
	s := testkit.NewStory(t)
	s.MustRun("checkout")
	munge := filepath.Join(s.SetDir, "packages", "munge")
	if err := os.WriteFile(filepath.Join(munge, "munge.spec"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := s.MustRun("checkout")
	t.Logf("checkout dirty:\n%s%s", r.Stdout, r.Stderr)
	if !strings.Contains(r.Stdout, "kept: dirty (munge.spec)") || !strings.Contains(r.Stderr, "warning: packages/munge has uncommitted changes") {
		t.Errorf("checkout with a dirty worktree:\n%s%s", r.Stdout, r.Stderr)
	}
	env, _ := s.RunJSON("checkout", "munge")
	if !contains(codes(env, "warnings"), "worktree.dirty") || itemOf(t, env, "munge")["state"] != "dirty" {
		t.Errorf("checkout --json with a dirty worktree: %v", env)
	}
	env, r = s.RunJSON("pin", "munge")
	if r.Code != 5 || !contains(codes(env, "errors"), "lock.dirty") {
		t.Errorf("pin of a dirty worktree: exit %d\n%s", r.Code, r.Stdout)
	}
	r = s.MustRun("checkout", "--force", "munge")
	if st := s.Git(munge, "status", "--porcelain"); st != "" {
		t.Errorf("checkout --force keeps the change: %s", st)
	}
}

func TestCheckoutRemoteMoved(t *testing.T) {
	s := testkit.NewStory(t)
	s.MustRun("checkout")
	// A new commit on origin is not pinned: checkout stays on the lock.
	s.Bump("munge", "0.5.19")
	r := s.MustRun("checkout", "--fetch", "munge")
	head := s.Git(filepath.Join(s.SetDir, "packages", "munge"), "rev-parse", "HEAD")
	if head != lockCommit(t, s, "munge") {
		t.Errorf("checkout moved munge off the lock commit:\n%s%s", r.Stdout, r.Stderr)
	}
	// pin pins the worktree HEAD, not the remote tip.
	old := lockCommit(t, s, "munge")
	r = s.MustRun("pin", "--fetch", "munge")
	if got := lockCommit(t, s, "munge"); got != old {
		t.Errorf("pin --fetch pinned %s, want the worktree HEAD %s", got, old)
	}
	// Another human pinned the new commit: the lock moves, checkout follows.
	lockFile := filepath.Join(s.SetDir, "repoctl.lock")
	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockFile, []byte(strings.Replace(string(data), old, s.BranchCommit("munge"), 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	env, r := s.RunJSON("checkout", "munge")
	if st := itemOf(t, env, "munge")["state"]; st != "in-sync" {
		t.Errorf("checkout of a moved lock: %v\n%s", st, r.Stderr)
	}
	head = s.Git(filepath.Join(s.SetDir, "packages", "munge"), "rev-parse", "HEAD")
	if head != s.BranchCommit("munge") {
		t.Errorf("checkout does not move munge to the new lock commit:\n%s%s", r.Stdout, r.Stderr)
	}
}

func TestCheckoutCreate(t *testing.T) {
	s := testkit.NewStory(t)
	s.MustRun("checkout")

	// A new set in a new set directory: init --from, then the branches of
	// the old set (help new-set).
	dir := filepath.Join(s.Root, "slurm-26.11")
	s.Git(s.SetDir, "worktree", "add", "-q", "-b", "set/slurm-26.11", dir)
	r := s.RunIn(dir, "init", "--from", testkit.SetBranch, "--name", "slurm-26.11")
	t.Logf("init --from:\n%s%s", r.Stdout, r.Stderr)
	if r.Code != 0 {
		t.Fatalf("init --from: exit %d\n%s", r.Code, r.Stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "repoctl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: slurm-26.11") || !strings.Contains(string(data), "branch: ucx/slurm-26.11") {
		t.Fatalf("init --from repoctl.yaml:\n%s", data)
	}
	s.Git(dir, "commit", "-qam", "feat: start set slurm-26.11")
	r = s.RunIn(dir, "checkout", "--create", "--from-set", testkit.SetBranch)
	t.Logf("checkout --create --from-set:\n%s%s", r.Stdout, r.Stderr)
	if r.Code != 0 {
		t.Fatalf("checkout --create: exit %d", r.Code)
	}
	if !strings.Contains(r.Stdout, "forked from ucx/slurm-26.05 at "+lockCommit(t, s, "ucx")[:7]) ||
		!strings.Contains(r.Stdout, "10 branches created, 0 exist on the remote") {
		t.Errorf("checkout --create output:\n%s", r.Stdout)
	}
	if got := s.Git(filepath.Join(dir, "packages", "ucx"), "branch", "--show-current"); got != "ucx/slurm-26.11" {
		t.Errorf("ucx worktree on %s", got)
	}
	if got := s.Git(filepath.Join(dir, "packages", "rocm-smi"), "branch", "--show-current"); got != "slurm-26.11" {
		t.Errorf("rocm-smi worktree on %s", got)
	}
	if !strings.Contains(r.Stderr, "git push origin munge/slurm-26.11 ucx/slurm-26.11") ||
		!strings.Contains(r.Stderr, "git -C packages/rocm-smi push origin slurm-26.11") || !strings.Contains(r.Stderr, "repoctl pin") {
		t.Errorf("checkout --create Next:\n%s", r.Stderr)
	}
	// pin writes the new lines.
	r = s.RunIn(dir, "pin")
	t.Logf("pin of the new set:\n%s%s", r.Stdout, r.Stderr)
	if r.Code != 0 || !strings.Contains(r.Stdout, "new → "+lockCommit(t, s, "ucx")[:7]) {
		t.Errorf("pin of the new set: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}

	// A branch on the remote is not created again.
	s.Git(filepath.Join(dir, "packages", "ucx"), "push", "-q", "origin", "ucx/slurm-26.11")
	r = s.RunIn(dir, "checkout", "--create", "--from-set", testkit.SetBranch, "ucx", "--json")
	env := envelope(t, r)
	if r.Code != 1 || !contains(codes(env, "errors"), "branch.exists-remote") {
		t.Errorf("checkout --create of a branch on the remote: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
	// Without a start point --create is a usage error.
	if r = s.RunIn(dir, "checkout", "--create", "ucx"); r.Code != 2 {
		t.Errorf("checkout --create without a start point: exit %d", r.Code)
	}
	if r = s.RunIn(dir, "checkout", "--orphan", "ucx"); r.Code != 2 {
		t.Errorf("checkout --orphan without --create: exit %d", r.Code)
	}

	// A set directory forked in place: a clean worktree on the old branch
	// moves to the new branch, a dirty one stops.
	s.Git(s.SetDir, "switch", "-q", "-c", "set/slurm-27.05")
	s.MustRun("init", "--from", testkit.SetBranch, "--name", "slurm-27.05")
	if err := os.WriteFile(filepath.Join(s.SetDir, "packages", "pmix", "pmix.spec"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, r = s.RunJSON("checkout", "--create", "--from-set", testkit.SetBranch, "munge", "pmix")
	t.Logf("checkout --create in place:\n%s", r.Stderr)
	if r.Code != 1 || !contains(codes(env, "errors"), "worktree.dirty") {
		t.Errorf("checkout --create with a dirty worktree: exit %d\n%s", r.Code, r.Stdout)
	}
	if got := s.Git(filepath.Join(s.SetDir, "packages", "munge"), "branch", "--show-current"); got != "munge/slurm-27.05" {
		t.Errorf("munge worktree on %s", got)
	}
}

// envelope validates the --json output of a run and gives it.
func envelope(t *testing.T, r testkit.Result) map[string]any {
	t.Helper()
	if err := testkit.Validate("Envelope", []byte(r.Stdout)); err != nil {
		t.Fatalf("--json: %v\nstdout:\n%s\nstderr:\n%s", err, r.Stdout, r.Stderr)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &env); err != nil {
		t.Fatal(err)
	}
	return env
}

// codes gives the codes of the problems of an envelope list.
func codes(env map[string]any, key string) []string {
	var out []string
	list, _ := env[key].([]any)
	for _, p := range list {
		out = append(out, p.(map[string]any)["code"].(string))
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestCheckoutCreateOrphanAndFedora(t *testing.T) {
	s := testkit.NewStory(t, testkit.Modify("munge", func(p *testkit.Pkg) { p.Branch = "munge/new" }))
	// The story pushed munge/new; take it off the remote: a new package.
	s.Git(s.Origin, "branch", "-D", "munge/new")
	s.Git(s.Origin, "branch", "-q", "fedora-munge", s.BranchCommit("ucx"))
	r := s.MustRun("checkout", "--create", "--orphan", "munge")
	t.Logf("checkout --create --orphan:\n%s%s", r.Stdout, r.Stderr)
	wt := filepath.Join(s.SetDir, "packages", "munge")
	if got := s.Git(wt, "branch", "--show-current"); got != "munge/new" {
		t.Errorf("orphan worktree on %q", got)
	}
	if out := s.Git(wt, "ls-files"); out != "" {
		t.Errorf("orphan worktree has files: %s", out)
	}
	if !strings.Contains(r.Stdout, "orphan") {
		t.Errorf("orphan output:\n%s", r.Stdout)
	}

	// --from fedora fetches the dist-git branch of the package.
	fedora := filepath.Join(s.Root, "fedora")
	if err := os.MkdirAll(fedora, 0o755); err != nil {
		t.Fatal(err)
	}
	s.Git(fedora, "clone", "-q", "--bare", "--branch", "pmix/slurm-26.05", s.Origin, "pmix.git")
	s.Git(filepath.Join(fedora, "pmix.git"), "branch", "-q", "-m", "pmix/slurm-26.05", "rawhide")
	s.Setenv("REPOCTL_TEST_FEDORA_URL", "file://"+fedora+"/")
	s.Git(s.Origin, "branch", "-D", "pmix/slurm-26.05")
	r = s.MustRun("checkout", "--create", "--from", "fedora", "pmix")
	t.Logf("checkout --create --from fedora:\n%s%s", r.Stdout, r.Stderr)
	if !strings.Contains(r.Stdout, "forked from fedora pmix rawhide") {
		t.Errorf("fedora output:\n%s", r.Stdout)
	}
	if _, err := os.Stat(filepath.Join(s.SetDir, "packages", "pmix", "pmix.spec")); err != nil {
		t.Error(err)
	}
	env, _ := s.RunJSON("checkout", "--create", "--from", "fedora", "pmix")
	if env["kind"] != "Lock" {
		t.Errorf("checkout --create --json: %v", env)
	}
}

func TestInit(t *testing.T) {
	s := testkit.NewStory(t)
	dir := filepath.Join(s.Root, "new-set")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s.Git(dir, "init", "-q", "-b", "set/hpc-1")
	r := s.RunIn(dir, "init")
	t.Logf("init:\n%s%s", r.Stdout, r.Stderr)
	if r.Code != 0 || !strings.Contains(r.Stdout, "wrote repoctl.yaml") || !strings.Contains(r.Stderr, "Next:") {
		t.Fatalf("init: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "repoctl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: hpc-1") {
		t.Errorf("repoctl.yaml:\n%s", data)
	}
	if r = s.RunIn(dir, "init"); r.Code == 0 {
		t.Errorf("init over an existing repoctl.yaml: exit 0")
	}
	r = s.RunIn(dir, "init", "--force", "--name", "hpc-2", "--add-target", "el10=rocky+epel-10-x86_64", "--json")
	if err := testkit.Validate("Envelope", []byte(r.Stdout)); err != nil {
		t.Fatalf("init --json: %v\n%s", err, r.Stdout)
	}
	// The new set parses.
	data, err = os.ReadFile(filepath.Join(dir, "repoctl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := config.ParseSet(data, "repoctl.yaml")
	if err != nil {
		t.Fatalf("init writes an invalid repoctl.yaml: %v\n%s", err, data)
	}
	if set.Name != "hpc-2" || len(set.Targets) != 1 || set.Targets[0].ID != "el10" {
		t.Errorf("init --name hpc-2 --add-target el10: %+v", set)
	}
}

func TestSchemaOutputs(t *testing.T) {
	s := testkit.NewStory(t)
	for _, name := range []string{"config", "lock", "user-config"} {
		r := s.MustRun("schema", name)
		var v map[string]any
		if err := json.Unmarshal([]byte(r.Stdout), &v); err != nil {
			t.Fatalf("schema %s: %v\n%s", name, err, r.Stdout)
		}
		if v["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("schema %s: $schema %v", name, v["$schema"])
		}
	}
}

func TestMigrate(t *testing.T) {
	s := testkit.NewStory(t)
	dir := filepath.Join(s.Root, "bash")
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.Git(dir, "init", "-q", "-b", "set/slurm-26.05")
	lockText := "# dir   branch   sha\n"
	for _, p := range s.Pkgs {
		if p.Path == "" && !p.External {
			lockText += p.ID + "    " + p.Branch + " " + s.BranchCommit(p.ID) + "\n"
		}
	}
	files := map[string]string{
		"packages.lock":     lockText,
		"scripts/common.sh": "#!/bin/bash\nrepobasename=slurm\nreponame=${repobasename}\nos=rocky+epel\nreleasever=9\narch=$(rpm --eval \"%{_arch}\") # x86_64\nmockroot=${os}-${releasever}-${arch}\nreleasedir=el${releasever}\n",
		"scripts/macros":    "%_with_pmix 1\n",
		"scripts/build":     "#!/bin/bash\nbuild munge\nbuild ucx\nbuild pmix\nbuild slurm\n",
	}
	for n, text := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.Git(dir, "add", "-A")
	s.Git(dir, "commit", "-qm", "bash era")
	r := s.RunIn(dir, "migrate", "--dry-run")
	t.Logf("migrate --dry-run:\n%s%s", r.Stdout, r.Stderr)
	if r.Code != 0 {
		t.Fatalf("migrate --dry-run: exit %d\n%s", r.Code, r.Stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "repoctl.yaml")); err == nil {
		t.Fatal("migrate --dry-run wrote repoctl.yaml")
	}
	r = s.RunIn(dir, "migrate", "--json")
	if err := testkit.Validate("Envelope", []byte(r.Stdout)); err != nil {
		t.Fatalf("migrate --json: %v\n%s\n%s", err, r.Stdout, r.Stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "repoctl.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "slurm/slurm-26.05,   commit: "+s.BranchCommit("slurm")+"}") {
		t.Errorf("migrate repoctl.lock:\n%s", data)
	}
	data, err = os.ReadFile(filepath.Join(dir, "repoctl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := config.ParseSet(data, "repoctl.yaml")
	if err != nil {
		t.Fatalf("migrate writes an invalid repoctl.yaml: %v\n%s", err, data)
	}
	if set.Name != "slurm-26.05" || set.Repo.Name != "slurm" || len(set.Packages) != 9 || set.Targets[0].Mock != "rocky+epel-9-x86_64" {
		t.Errorf("migrate repoctl.yaml:\n%s", data)
	}
	// The set is ready for the workspace commands.
	s.Git(dir, "add", "-A")
	s.Git(dir, "commit", "-qm", "chore: migrate to repoctl")
	s.Git(dir, "remote", "add", "origin", s.Origin)
	r = s.RunIn(dir, "checkout")
	t.Logf("checkout after migrate:\n%s%s", r.Stdout, r.Stderr)
	if r.Code != 0 || !strings.Contains(r.Stdout, "9 packages: 9 worktrees created") {
		t.Errorf("checkout after migrate: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
	r = s.RunIn(dir, "migrate", "--api")
	t.Logf("migrate --api:\n%s%s", r.Stdout, r.Stderr)
	if r.Code != 0 {
		t.Errorf("migrate --api: exit %d\n%s", r.Code, r.Stderr)
	}
}

// TestPlanBeforeCheckout runs plan first, as the guide does: plan makes the
// bare clone of the external rocm-smi. checkout and pin --fetch then use
// that clone, and origin/BRANCH must exist in it for the lock checks of
// publish.
func TestPlanBeforeCheckout(t *testing.T) {
	if _, err := exec.LookPath("rpmbuild"); err != nil {
		t.Skip("rpmbuild is missing")
	}
	s := testkit.NewStory(t)
	r := s.Run("plan", "--target", "el9", "--progress", "none")
	t.Logf("plan:\n%s%s", r.Stdout, r.Stderr)
	s.MustRun("checkout", "--progress", "none")
	wt := filepath.Join(s.SetDir, "packages", "rocm-smi")
	common := s.Git(wt, "rev-parse", "--git-common-dir")
	if got := s.Git(common, "rev-parse", "refs/remotes/origin/slurm-26.05"); got != s.BranchCommit("rocm-smi") {
		t.Errorf("origin/slurm-26.05 of the clone is %q, want %s", got, s.BranchCommit("rocm-smi"))
	}
	r = s.Run("pin", "--fetch", "rocm-smi")
	if r.Code != 0 {
		t.Errorf("pin --fetch rocm-smi: exit %d\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
}
