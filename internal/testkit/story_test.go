package testkit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStory(t *testing.T) {
	if _, err := exec.LookPath("rpmbuild"); err != nil {
		t.Skip("rpmbuild is not installed")
	}
	s := NewStory(t)
	if got := s.Git(s.SetDir, "rev-parse", "--abbrev-ref", "HEAD"); got != SetBranch {
		t.Fatalf("set dir on %s", got)
	}
	for _, f := range []string{"repoctl.yaml", "repoctl.lock", "packages/slurm-release/slurm-release.spec"} {
		if _, err := os.Stat(filepath.Join(s.SetDir, f)); err != nil {
			t.Fatal(err)
		}
	}
	r := s.MustRun("version")
	if !strings.HasPrefix(r.Stdout, "repoctl ") {
		t.Fatalf("version: %q", r.Stdout)
	}

	// The fake mock builds the SRPM of munge with %autorelease 1, and the
	// RPMs, from a source clone.
	clone := filepath.Join(s.Root, "clone-munge")
	s.Git(s.Root, "clone", "-q", "--local", "--no-checkout", s.Origin, clone)
	s.Git(clone, "checkout", "-q", "--detach", s.BranchCommit("munge"))
	wrapper := filepath.Join(s.Root, "el9.cfg")
	if err := os.WriteFile(wrapper, []byte("include('"+filepath.Join(s.MockConfigDir, "rocky+epel-9-x86_64.cfg")+"')\nconfig_opts['plugin_conf']['rpmautospec_enable'] = True\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := filepath.Join(s.Root, "result")
	mock := func(args ...string) string {
		cmd := exec.Command("mock", args...)
		cmd.Env = s.Env()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mock %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	mock("-r", wrapper, "--uniqueext", "repoctl-0-t-1", "--resultdir", res, "--buildsrpm", "--spec", filepath.Join(clone, "munge.spec"), "--sources", clone)
	srpm := filepath.Join(res, "munge-0.5.18-1.el9.src.rpm")
	if _, err := os.Stat(srpm); err != nil {
		t.Fatal(err)
	}
	mock("-r", wrapper, "--uniqueext", "repoctl-0-t-1", "--resultdir", res, "--rebuild", srpm)
	for _, f := range []string{"munge-0.5.18-1.el9.x86_64.rpm", "munge-devel-0.5.18-1.el9.x86_64.rpm", "buildroot_lock.json", "state.log", "build.log"} {
		if _, err := os.Stat(filepath.Join(res, f)); err != nil {
			t.Fatal(err)
		}
	}
	qa := mock("-r", wrapper, "--uniqueext", "repoctl-0-t-1", "--chroot", "--", "rpm", "-qa", "--qf", "x")
	if !strings.Contains(qa, "PKG bash-0:1.0-1.el9.x86_64") {
		t.Fatalf("rpm -qa: %s", qa)
	}

	// A rebuild commit raises %autorelease; pmix needs munge-devel.
	c := s.Commit("munge", "Rebuild for pmix", nil)
	s.Git(clone, "fetch", "-q", "origin")
	s.Git(clone, "checkout", "-q", "--detach", c)
	res2 := filepath.Join(s.Root, "result2")
	mock("-r", wrapper, "--resultdir", res2, "--buildsrpm", "--spec", filepath.Join(clone, "munge.spec"), "--sources", clone)
	if _, err := os.Stat(filepath.Join(res2, "munge-0.5.18-2.el9.src.rpm")); err != nil {
		t.Fatal(err)
	}
	pclone := filepath.Join(s.Root, "clone-pmix")
	s.Git(s.Root, "clone", "-q", "--local", "--no-checkout", s.Origin, pclone)
	s.Git(pclone, "checkout", "-q", "--detach", s.BranchCommit("pmix"))
	res3 := filepath.Join(s.Root, "result3")
	mock("-r", wrapper, "--resultdir", res3, "--buildsrpm", "--spec", filepath.Join(pclone, "pmix.spec"), "--sources", pclone)
	cmd := exec.Command("mock", "-r", wrapper, "--resultdir", res3, "--rebuild", filepath.Join(res3, "pmix-6.1.0-1.el9.src.rpm"))
	cmd.Env = s.Env()
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 30 {
		t.Fatalf("pmix without munge-devel: %v, want exit 30", err)
	}
	mock("-r", wrapper, "--resultdir", res3, "--addrepo", "file://"+res, "--rebuild", filepath.Join(res3, "pmix-6.1.0-1.el9.src.rpm"))
}
