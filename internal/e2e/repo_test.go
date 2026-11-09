//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/testkit"
	"github.com/dennisklein/repoctl/internal/ui"
)

// stagedStory is the publish story with hello and slurm-release built and
// staged for el9 by the run publishRun.
func stagedStory(t *testing.T) *testkit.Story {
	t.Helper()
	s := publishStory(t)
	hello := fakeBuild(t, s, cloneBranch(t, s, "hello/slurm-26.05"), "hello.spec", filepath.Join(s.Root, "b", "hello"))
	rel := fakeBuild(t, s, filepath.Join(s.SetDir, "packages/slurm-release"), "slurm-release.spec", filepath.Join(s.Root, "b", "rel"))
	inTree := s.Git(s.SetDir, "log", "-1", "--format=%H", "--", "packages/slurm-release")
	stage(t, s, map[string]string{"hello": hello, "slurm-release": rel},
		map[string]string{"hello": s.BranchCommit("hello"), "slurm-release": inTree})
	return s
}

// TestRepoDiffRun compares the staging of a run with the local copy of
// latest, before and after its publish, and two snapshots.
func TestRepoDiffRun(t *testing.T) {
	s := stagedStory(t)

	r := s.MustRun("repo", "diff", "--run", publishRun, "--target", "el9")
	t.Logf("repo diff --run:\n%s%s", r.Stdout, r.Stderr)
	// hello has its SRPM and two RPMs, slurm-release its SRPM and one.
	mustContain(t, "repo diff --run", r.Stdout, "el9  latest none → staging "+publishRun, "+ hello ", "3 RPMs\n", "+ slurm-release ",
		"5 new RPMs · 0 packages unchanged · old versions stay")
	// The newest finished run is the default; --check finds the
	// difference.
	env, _ := s.RunJSON("repo", "diff", "--target", "el9")
	d := items(t, env)[0]
	if to := d["to"].(map[string]any); to["source"] != "staging" || to["run"] != publishRun {
		t.Errorf("repo diff: to %v", to)
	}
	if added := list(d, "added"); len(added) == 0 {
		t.Errorf("repo diff: nothing added: %v", d)
	}
	if r := s.Run("repo", "diff", "--target", "el9", "--check"); r.Code != 11 {
		t.Errorf("repo diff --check: exit %d, want 11\n%s", r.Code, r.Stderr)
	}
	r = s.MustRun("repo", "diff", "hello", "--target", "el9", "--rpms")
	if strings.Contains(r.Stdout, "slurm-release") || !strings.Contains(r.Stdout, "    + hello-devel-1.0-1") {
		t.Errorf("repo diff hello --rpms:\n%s", r.Stdout)
	}

	// A staging has no signatures.
	r = s.MustRun("repo", "verify", "--run", publishRun, "--target", "el9")
	if !strings.Contains(r.Stdout, "not signed (staging), checksums ok") || strings.Contains(r.Stdout, "signers") {
		t.Errorf("repo verify --run:\n%s", r.Stdout)
	}

	s.Setenv(publishUnattended, "1")
	s.MustRun("publish", "--target", "el9", "--yes")
	r = s.MustRun("repo", "diff", "--run", publishRun, "--target", "el9", "--check")
	mustContain(t, "repo diff after the publish", r.Stdout, "el9  latest p-", "0 new RPMs · 2 packages unchanged · old versions stay")

	// Between snapshots: latest has no parent, so each package is new.
	r = s.MustRun("repo", "diff", "--to", "latest", "--target", "el9")
	mustContain(t, "repo diff --to latest", r.Stdout, "el9  none → p-", "+ hello ", "0 removed RPMs")
}

// TestRepoRecords reads the provenance and the SBOM of a published build:
// by node, from the local copy and a snapshot, and with a changed subject.
func TestRepoRecords(t *testing.T) {
	s := stagedStory(t)
	s.Setenv(publishUnattended, "1")
	s.MustRun("publish", "--target", "el9", "--yes")

	// The node gives the target; no --target is needed.
	r := s.MustRun("repo", "provenance", "hello@el9")
	t.Logf("repo provenance hello@el9:\n%s", r.Stdout)
	mustContain(t, "repo provenance hello@el9", r.Stdout, "hello-1.0-1.el9  el9 of "+testkit.SetName+"  publish p-",
		"source    ", "run       "+publishRun, "builder   repoctl ", "subjects  ", "sha256 equal to the files")
	env, _ := s.RunJSON("repo", "provenance", "hello@el9")
	p := items(t, env)[0]
	if p["node"] != "hello@el9" || len(list(p, "subjects")) == 0 {
		t.Errorf("repo provenance --json: node %v, subjects %v", p["node"], p["subjects"])
	}
	for _, sub := range list(p, "subjects") {
		if sub.(map[string]any)["match"] != true {
			t.Errorf("repo provenance: subject %v does not match", sub)
		}
	}
	if r := s.Run("repo", "provenance", "hello@el9", "--target", "el10"); r.Code != 2 {
		t.Errorf("repo provenance hello@el9 --target el10: exit %d, want 2\n%s", r.Code, r.Stderr)
	}
	r = s.MustRun("repo", "provenance", "hello@el9", "--snapshot", "latest")
	mustContain(t, "repo provenance --snapshot latest", r.Stdout, "sha256 equal to the files")

	// repo verify --key takes a fingerprint: of sign.keys, or of the GnuPG
	// home.
	env, _ = s.RunJSON("repo", "snapshots", "--target", "el9")
	snaps := list(items(t, env)[0], "snapshots")
	if len(snaps) == 0 {
		t.Fatalf("repo snapshots: none")
	}
	signer, _ := snaps[0].(map[string]any)["signer"].(map[string]any)
	fpr, _ := signer["fingerprint"].(string)
	if fpr == "" {
		t.Fatalf("repo snapshots: no signer fingerprint: %v", items(t, env)[0])
	}
	r = s.MustRun("repo", "verify", "--target", "el9", "--key", fpr)
	mustContain(t, "repo verify --key", r.Stdout, "signers d.klein (")

	// One snapshot, one RPM count: the RPM files of its three
	// repositories, in repo snapshots, its --history, repo verify and sign
	// ls.
	snap := snaps[0].(map[string]any)
	rpms := snap["rpms"].(float64)
	if snap["added"] != rpms || rpms == 0 {
		t.Errorf("repo snapshots: rpms %v, added %v", snap["rpms"], snap["added"])
	}
	env, _ = s.RunJSON("repo", "snapshots", "--target", "el9", "--history")
	hist := list(items(t, env)[0], "snapshots")
	if len(hist) != 1 {
		t.Fatalf("repo snapshots --history: %v", hist)
	}
	h := hist[0].(map[string]any)
	if h["rpms"] != rpms || h["added"] != rpms || h["digest"] != snap["digest"] || h["signer"].(map[string]any)["keyId"] != "d.klein" {
		t.Errorf("repo snapshots --history: %v, want rpms %v of %v", h, rpms, snap["digest"])
	}
	n := fmt.Sprint(rpms)
	r = s.MustRun("repo", "snapshots", "--target", "el9")
	mustContain(t, "repo snapshots", r.Stdout, "  PUBLISH ", "* p-", "d.klein ", "* = latest")
	r = s.MustRun("repo", "verify", "--target", "el9")
	mustContain(t, "repo verify", r.Stdout, "el9: "+n+" RPMs in ", "signers d.klein ("+n+")")
	env, _ = s.RunJSON("sign", "ls", "--target", "el9")
	for _, k := range list(items(t, env)[0], "keys") {
		if k := k.(map[string]any); k["keyId"] == "d.klein" && k["rpms"].(map[string]any)["el9"] != rpms {
			t.Errorf("sign ls: d.klein has %v RPMs in el9, want %v", k["rpms"], rpms)
		}
	}
	r = s.MustRun("repo", "ls", "--target", "el9")
	mustContain(t, "repo ls", r.Stdout, "el9 / slurm  latest p-", "(local copy)", "NAME ", "SUMMARY", "\n3 RPMs\n")
	other := testkit.GenKey(t, "a.admin")
	writeFile(t, filepath.Join(s.Root, "other.asc"), string(other.Public))
	imp := exec.Command("gpg", "--batch", "--quiet", "--import", filepath.Join(s.Root, "other.asc"))
	imp.Env = s.Env()
	if out, err := imp.CombinedOutput(); err != nil {
		t.Fatalf("gpg --import: %v\n%s", err, out)
	}
	if r := s.Run("repo", "verify", "--target", "el9", "--key", other.Fingerprint); r.Code != 10 {
		t.Errorf("repo verify --key with another key: exit %d, want 10\n%s%s", r.Code, r.Stdout, r.Stderr)
	}
	if r := s.Run("repo", "verify", "--target", "el9", "--key", strings.Repeat("AB", 20)); r.Code != 2 {
		t.Errorf("repo verify --key of no key: exit %d, want 2\n%s", r.Code, r.Stderr)
	}

	// Of all targets, el10 has no local copy: a warning.
	env, r = s.RunJSON("repo", "sbom")
	if n := len(items(t, env)); n != 1 || r.Code != 0 {
		t.Errorf("repo sbom: %d items, exit %d\n%s", n, r.Code, r.Stderr)
	}
	r = s.MustRun("repo", "sbom", "--target", "el9", "--build", "hello")
	t.Logf("repo sbom --build hello:\n%s", r.Stdout)
	mustContain(t, "repo sbom --build hello", r.Stdout, "COMPONENT", "PURL", "hello-devel", "RPMs, ", "bundled librar")

	// A subject that is not its file: records.mismatch.
	dir := filepath.Join(s.Home, ".local", "share", "repoctl", testkit.SetName, "repo", "el9")
	var rpm string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), "hello-devel-") && strings.HasSuffix(p, ".rpm") {
			rpm = p
		}
		return nil
	})
	if rpm == "" {
		t.Fatalf("no hello-devel RPM in %s", dir)
	}
	f, err := os.OpenFile(rpm, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("x")
	f.Close()
	env, r = s.RunJSON("repo", "provenance", "hello@el9")
	if r.Code != 10 || firstError(env) != "records.mismatch" {
		t.Errorf("a changed subject: exit %d %s\n%s", r.Code, firstError(env), r.Stderr)
	}
}

// confirmPublish runs publish --confirm-fd 3 with the UI confirmation c on
// file descriptor 3, as the UI server starts it.
func confirmPublish(t *testing.T, s *testkit.Story, c ui.Confirmation) (int, map[string]any) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(s.Bin, "repoctl"), "publish", "--target", "el9", "--confirm-fd", "3", "--json")
	cmd.Dir, cmd.Env, cmd.ExtraFiles = s.SetDir, append(s.Env(), "REPOCTL_ORIGIN=ui"), []*os.File{pr}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pr.Close()
	b, _ := json.Marshal(c)
	_, _ = pw.Write(append(b, '\n'))
	pw.Close()
	err = cmd.Wait()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	t.Logf("publish --confirm-fd 3: exit %d\n%s", code, errb.String())
	testkit.ValidateEnvelope(t, out.Bytes())
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("publish --confirm-fd --json: %v\n%s", err, out.String())
	}
	return code, env
}

func firstError(env map[string]any) string {
	errs, _ := env["errors"].([]any)
	if len(errs) == 0 {
		return ""
	}
	code, _ := errs[0].(map[string]any)["code"].(string)
	return code
}

// TestPublishConfirmFD publishes el9 with a UI confirmation on file
// descriptor 3: only from the UI server, only with the set name and the
// plan digest of the review.
func TestPublishConfirmFD(t *testing.T) {
	s := stagedStory(t)
	// The readiness comes before the confirmation: el10 has no staged
	// node, so exit 12 and not 6.
	r := s.Run("publish", "--target", "el10")
	if r.Code != 12 || !strings.Contains(r.Stderr, "el10 is not publishable: hello@el10 is not built") ||
		!strings.Contains(r.Stderr, "hint: see what the target needs. run: repoctl plan --target el10") {
		t.Errorf("publish --target el10 without a terminal: exit %d\n%s", r.Code, r.Stderr)
	}
	// The summary of publish.html#summary.
	r = s.MustRun("publish", "--target", "el9", "--dry-run")
	t.Logf("publish --dry-run:\n%s", r.Stdout)
	mustContain(t, "publish --dry-run", r.Stdout, "publish  p-", "  el9 of "+testkit.SetName+"  from run "+publishRun,
		"\nstore    ", "\nsigner   d.klein  ", "\nPACKAGE ", "2 packages change: 5 new RPMs, 0 kept, 5 after",
		"\nsign     5 new RPMs with d.klein", "\nchecks   ok: set commit ", " and 2 package commits pushed · trust ")
	env, _ := s.RunJSON("publish", "--target", "el9", "--dry-run")
	digest, _ := items(t, env)[0]["planDigest"].(string)
	if digest == "" {
		t.Fatalf("publish --dry-run gives no plan digest: %v", env)
	}
	c := ui.Confirmation{V: 1, Command: "publish", Set: testkit.SetName, Targets: []string{"el9"}, PlanDigest: digest, User: "tester"}

	// No UI server: refused.
	if code, env := confirmPublish(t, s, c); code != 6 || firstError(env) != "publish.needs-confirmation" {
		t.Fatalf("without a UI server: exit %d %s", code, firstError(env))
	}

	// This test process is the UI server of ui.json.
	paths := config.NewPaths(os.Getenv)
	if err := os.MkdirAll(filepath.Dir(paths.UIJSON()), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(ui.Record{PID: os.Getpid(), Session: "0123456789abcdef"})
	writeFile(t, paths.UIJSON(), string(b))
	f, err := os.Open(paths.UIJSON())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	wrong := c
	wrong.Set = "slurm"
	if code, env := confirmPublish(t, s, wrong); code != 6 || firstError(env) != "publish.needs-confirmation" {
		t.Errorf("a wrong set name: exit %d %s", code, firstError(env))
	}
	changed := c
	changed.PlanDigest = "sha256:" + strings.Repeat("0", 64)
	if code, env := confirmPublish(t, s, changed); code != 6 || firstError(env) != "publish.plan-changed" {
		t.Errorf("another plan digest: exit %d %s", code, firstError(env))
	}
	code, env := confirmPublish(t, s, c)
	if code != 0 {
		t.Fatalf("publish with the UI confirmation: exit %d %v", code, env["errors"])
	}
	run := items(t, env)[0]
	origin, _ := run["origin"].(map[string]any)
	if run["state"] != "ok" || origin["via"] != "ui" || run["unattended"] == true {
		t.Errorf("the run of a UI publish: state %v, origin %v, unattended %v", run["state"], origin, run["unattended"])
	}
}

// TestRepoLockWait holds the set lock: repo verify, gc --apply, prune
// --apply and resign wait --lock-wait for it, then exit 7.
func TestRepoLockWait(t *testing.T) {
	s := stagedStory(t)
	s.Setenv(publishUnattended, "1")
	s.MustRun("publish", "--target", "el9", "--yes")
	p := config.NewPaths(os.Getenv).SetLock(testkit.SetName)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"repo", "verify", "--target", "el9"},
		{"repo", "gc", "--apply", "--yes"},
		{"repo", "prune", "--target", "el9", "--nvr", "hello-1.0-1.el9", "--apply", "--yes"},
		{"repo", "resign", "--target", "el9", "--signer", "d.klein", "--yes"},
	} {
		start := time.Now()
		r := s.Run(append(args, "--lock-wait", "1s")...)
		if r.Code != 7 || time.Since(start) < time.Second || !strings.Contains(r.Stderr, "--lock-wait") {
			t.Errorf("%v with the set lock held: exit %d after %v\n%s", args, r.Code, time.Since(start), r.Stderr)
		}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	s.MustRun("repo", "verify", "--target", "el9", "--lock-wait", "1s")
}
