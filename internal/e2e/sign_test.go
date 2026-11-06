//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// signStory gives the story with the test key of d.klein in its GnuPG home
// and sign.key set to d.klein.
func signStory(t *testing.T) (*testkit.Story, *testkit.TestKey) {
	t.Helper()
	testkit.Binaries(t)
	s := testkit.NewStory(t)
	k := testkit.GPGKeys(t, s, "d.klein")["d.klein"]
	s.Setenv("REPOCTL_SIGN_KEY", "d.klein")
	return s, k
}

// signedCommit reports a commit line "%G? SUBJECT" with a good signature
// (G, or U for a key of unknown validity) and the subject.
func signedCommit(line, subject string) bool {
	return (strings.HasPrefix(line, "G ") || strings.HasPrefix(line, "U ")) && strings.HasPrefix(line[2:], subject)
}

// problem runs repoctl with --json and gives the exit code and the code of
// the first error.
func problem(s *testkit.Story, args ...string) (int, string) {
	s.T.Helper()
	env, r := s.RunJSON(args...)
	errs, _ := env["errors"].([]any)
	if len(errs) == 0 {
		return r.Code, ""
	}
	code, _ := errs[0].(map[string]any)["code"].(string)
	return r.Code, code
}

func TestSignAddReleaseWrite(t *testing.T) {
	s, k := signStory(t)

	r := s.MustRun("sign", "add", "d.klein", "--fingerprint", k.Fingerprint, "--from-gpg")
	for _, want := range []string{
		"wrote keys/d.klein.asc",
		"Dennis Klein <d.klein@example.org> · no expiry",
		"wrote packages/slurm-release/RPM-GPG-KEY-slurm",
		"1 key: d.klein",
		"committed ",
		"signed by d.klein",
		"git push origin set/slurm-26.05",
	} {
		if !strings.Contains(r.Stdout+r.Stderr, want) {
			t.Errorf("sign add: no %q in\n%s%s", want, r.Stdout, r.Stderr)
		}
	}
	// The commit is signed with the key and holds the key, the set file and
	// the release package.
	if got := s.Git(s.SetDir, "log", "-1", "--format=%G? %s"); !signedCommit(got, "Trust key d.klein "+k.Fingerprint) {
		t.Errorf("commit: %q", got)
	}
	files := s.Git(s.SetDir, "show", "--name-only", "--format=", "HEAD")
	for _, want := range []string{"keys/d.klein.asc", "repoctl.yaml", "packages/slurm-release/RPM-GPG-KEY-slurm",
		"packages/slurm-release/slurm-el9.repo", "packages/slurm-release/slurm-el10.repo", "packages/slurm-release/slurm-release.spec"} {
		if !strings.Contains(files, want) {
			t.Errorf("commit has no %s:\n%s", want, files)
		}
	}
	yml, _ := os.ReadFile(filepath.Join(s.SetDir, "repoctl.yaml"))
	if !strings.Contains(string(yml), "fingerprint: "+k.Fingerprint) || !strings.Contains(string(yml), "keyfile: RPM-GPG-KEY-slurm") {
		t.Errorf("repoctl.yaml:\n%s", yml)
	}
	if s.Git(s.SetDir, "status", "--porcelain") != "" {
		t.Errorf("the set is dirty after sign add")
	}

	// A second add of the key is refused.
	if code, p := problem(s, "sign", "add", "d.klein", "--fingerprint", k.Fingerprint); code != 1 || p != "sign.key-exists" {
		t.Errorf("second add: exit %d %s", code, p)
	}

	// release write: up to date after sign add, then repairs a hand edit.
	env, _ := s.RunJSON("release", "write")
	if env["kind"] != "File" {
		t.Errorf("release write kind %v", env["kind"])
	}
	repo := filepath.Join(s.SetDir, "packages/slurm-release/slurm-el9.repo")
	if err := os.WriteFile(repo, []byte("[slurm]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Only a .repo file is stale: the key file is still fine.
	s.MustRun("sign", "keyfile", "--tree")
	r = s.MustRun("release", "write")
	if !strings.Contains(r.Stdout, "wrote packages/slurm-release/slurm-el9.repo") {
		t.Errorf("release write:\n%s", r.Stdout)
	}
	data, _ := os.ReadFile(repo)
	if !strings.Contains(string(data), "baseurl=https://repo.example.org/slurm-26.05/el9/slurm/") {
		t.Errorf("slurm-el9.repo:\n%s", data)
	}

	// sign keyfile --tree --checksum gives the sum of the key file.
	r = s.MustRun("sign", "keyfile", "--tree", "--checksum")
	if !strings.Contains(r.Stdout, "packages/slurm-release/RPM-GPG-KEY-slurm") || len(strings.Fields(r.Stdout)[0]) != 64 {
		t.Errorf("keyfile --checksum:\n%s", r.Stdout)
	}
	s.RunJSON("sign", "keyfile", "--tree")
}

func TestSignLsLifecycle(t *testing.T) {
	s, k := signStory(t)
	a := testkit.GPGKeys(t, s, "a.admin")["a.admin"]
	s.MustRun("sign", "add", "d.klein", "--fingerprint", k.Fingerprint, "--from-gpg")
	s.MustRun("sign", "add", "a.admin", "--fingerprint", a.Fingerprint, "--from-gpg")

	env, _ := s.RunJSON("sign", "ls")
	if env["kind"] != "SignerList" {
		t.Fatalf("kind %v", env["kind"])
	}
	items := env["items"].([]any)
	l := items[0].(map[string]any)
	if l["current"] != "d.klein" || len(l["keys"].([]any)) != 2 {
		t.Errorf("sign ls: %v", l)
	}
	r := s.MustRun("sign", "ls")
	for _, want := range []string{"ID", "FINGERPRINT", "d.klein", "a.admin", "active", "no record", "never"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("sign ls: no %q in\n%s", want, r.Stdout)
		}
	}

	// Without a latest, retire needs no other deployed key.
	r = s.MustRun("sign", "retire", "a.admin")
	if !strings.Contains(r.Stdout, "a.admin") || !strings.Contains(r.Stdout, "retiring") {
		t.Errorf("sign retire:\n%s", r.Stdout)
	}
	if got := s.Git(s.SetDir, "log", "-1", "--format=%G? %s"); !signedCommit(got, "Retire key a.admin") {
		t.Errorf("commit: %q", got)
	}
	kf, _ := os.ReadFile(filepath.Join(s.SetDir, "packages/slurm-release/RPM-GPG-KEY-slurm"))
	if strings.Count(string(kf), "BEGIN PGP PUBLIC KEY BLOCK") != 2 {
		t.Errorf("a retiring key stays in the key file")
	}
	// A second retire is a wrong state.
	if code, p := problem(s, "sign", "retire", "a.admin"); code != 1 || p != "sign.wrong-state" {
		t.Errorf("second retire: exit %d %s", code, p)
	}

	// activate asks for the key id: refused without a terminal.
	if code, p := problem(s, "sign", "activate", "a.admin"); code != 6 || p != "publish.needs-confirmation" {
		t.Errorf("activate without a terminal: exit %d %s", code, p)
	}
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script is not installed")
	}
	cmd := exec.Command("script", "-qec", filepath.Join(s.Bin, "repoctl")+" sign activate a.admin", "/dev/null")
	cmd.Dir = s.SetDir
	cmd.Env = append(s.Env(), "REPOCTL_SIGN_KEY=d.klein")
	cmd.Stdin = strings.NewReader("a.admin\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sign activate in a terminal: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Type the key id") || !strings.Contains(string(out), "a.admin") {
		t.Errorf("sign activate:\n%s", out)
	}
	if got := s.Git(s.SetDir, "log", "-1", "--format=%s"); !strings.HasPrefix(got, "Activate key a.admin") {
		t.Errorf("commit: %q", got)
	}
	env, _ = s.RunJSON("sign", "ls")
	for _, x := range env["items"].([]any)[0].(map[string]any)["keys"].([]any) {
		m := x.(map[string]any)
		if m["keyId"] == "a.admin" && m["state"] != "active" {
			t.Errorf("a.admin after activate: %v", m["state"])
		}
	}

	// onboard has the steps of a key; JSON validates.
	env, _ = s.RunJSON("sign", "onboard", "a.admin")
	if env["kind"] != "OnboardReport" {
		t.Errorf("onboard kind %v", env["kind"])
	}
	// remove of an active key is a wrong state; revoke needs the key id.
	if code, p := problem(s, "sign", "remove", "a.admin"); code != 1 || p != "sign.wrong-state" {
		t.Errorf("remove of an active key: exit %d %s", code, p)
	}
	if r := s.Run("sign", "deployed", "a.admin", "--release", "slurm-release-26.05-2.el9"); r.Code != 2 {
		t.Errorf("deployed without --target: exit %d\n%s", r.Code, r.Stderr)
	}
	if code, p := problem(s, "sign", "deployed", "a.admin", "--target", "el9", "--release", "slurm-release-26.05-2.el9"); code != 12 || p != "sign.not-deployed" {
		t.Errorf("deployed without a latest: exit %d %s", code, p)
	}
}

// publishRelease puts the release nvr of the release package, with the
// in-tree key file, into the local copy of latest of el9, signed by signer.
// The RPMs of the earlier releases stay. It skips without rpmbuild.
func publishRelease(t *testing.T, s *testkit.Story, signer, nvr string) {
	t.Helper()
	if _, err := exec.LookPath("rpmbuild"); err != nil {
		t.Skip("rpmbuild is not installed")
	}
	dir := filepath.Join(s.Home, ".local", "share", "repoctl", testkit.SetName, "repo", "el9")
	repodata := filepath.Join(dir, "slurm", "repodata")
	pkgs := filepath.Join(dir, "slurm", "Packages", "s")
	for _, d := range []string{repodata, pkgs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	kf, err := os.ReadFile(filepath.Join(s.SetDir, "packages", "slurm-release", "RPM-GPG-KEY-slurm"))
	if err != nil {
		t.Fatal(err)
	}
	// The RPM with the key file.
	top := t.TempDir()
	for _, d := range []string{"SPECS", "SOURCES"} {
		if err := os.MkdirAll(filepath.Join(top, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(top, "SOURCES", "keyfile"), kf, 0o644); err != nil {
		t.Fatal(err)
	}
	rel := strings.TrimPrefix(nvr, "slurm-release-26.05-")
	spec := "Name: slurm-release\nVersion: 26.05\nRelease: " + rel + "\nSummary: test\nLicense: MIT\nBuildArch: noarch\n%description\ntest\n" +
		"%install\ninstall -D -m 0644 %{_sourcedir}/keyfile %{buildroot}/etc/pki/rpm-gpg/RPM-GPG-KEY-slurm\n%files\n/etc/pki/rpm-gpg/RPM-GPG-KEY-slurm\n"
	sp := filepath.Join(top, "SPECS", "slurm-release.spec")
	if err := os.WriteFile(sp, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("rpmbuild", "-bb", "--define", "_topdir "+top, sp).CombinedOutput(); err != nil {
		t.Fatalf("rpmbuild: %v\n%s", err, out)
	}
	rpm, err := os.ReadFile(filepath.Join(top, "RPMS", "noarch", nvr+".noarch.rpm"))
	if err != nil {
		t.Fatal(err)
	}
	// The manifest with the earlier releases.
	m := manifest.New(testkit.SetName, "el9")
	if data, err := os.ReadFile(filepath.Join(repodata, "repoctl.json")); err == nil {
		if err := json.Unmarshal(data, m); err != nil {
			t.Fatal(err)
		}
	}
	m.Publish = "p-20261103-1412-5e1"
	m.Builds = append(m.Builds, manifest.Build{Package: "slurm-release", Commit: strings.Repeat("a", 40), Reason: "commit-changed",
		SRPM: nvr + ".src.rpm", RPMs: []string{nvr + ".noarch.rpm"}, Signer: signer})
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	snap := `{"target":"el9","publish":"p-20261103-1412-5e1","digest":"sha256:` + strings.Repeat("9d07", 16) + `","kind":"publish","user":"d.klein","host":"hpc-a1"}`
	for name, text := range map[string]string{
		filepath.Join(repodata, "repoctl.json"): string(data),
		filepath.Join(repodata, "repomd.xml"):   `<repomd xmlns="http://linux.duke.edu/metadata/repo"><data type="repoctl"><location href="repodata/repoctl.json"/></data></repomd>` + "\n",
		filepath.Join(pkgs, nvr+".noarch.rpm"):  string(rpm),
		filepath.Join(dir, "RPM-GPG-KEY-slurm"): string(kf),
		filepath.Join(dir, "snapshot.json"):     snap,
	} {
		if err := os.WriteFile(name, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// inTerminal runs repoctl in a pseudo terminal (script) with stdin.
func inTerminal(t *testing.T, s *testkit.Story, stdin string, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script is not installed")
	}
	cmd := exec.Command("script", "-qec", filepath.Join(s.Bin, "repoctl")+" "+strings.Join(args, " "), "/dev/null")
	cmd.Dir = s.SetDir
	cmd.Env = s.Env()
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

type childResult struct {
	code           int
	stdout, stderr string
}

// uiChild runs repoctl as the web UI runs a child: no terminal, and the
// UI confirmation on file descriptor 3.
func uiChild(t *testing.T, s *testkit.Story, confirm string, args ...string) childResult {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	cmd := exec.Command(filepath.Join(s.Bin, "repoctl"), args...) //nolint:gosec // G204: the test binary
	cmd.Dir = s.SetDir
	cmd.Env = s.Env()
	cmd.ExtraFiles = []*os.File{pr}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_, _ = pw.WriteString(confirm + "\n")
	pw.Close()
	_ = cmd.Wait()
	return childResult{cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()}
}

// fakeUIServer makes the test process the web UI server of ui.json: the
// record names its pid, and it holds the flock. unlock lets it go.
func fakeUIServer(t *testing.T, s *testkit.Story) (unlock func()) {
	t.Helper()
	path := filepath.Join(s.Root, "run", "repoctl", "ui.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "listen": "127.0.0.1:0", "version": "test"})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) //nolint:gosec // G304: the test's runtime dir
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = f.Close()
		_ = os.Remove(path)
	}
}

func TestSignDeployed(t *testing.T) {
	s, k := signStory(t)
	a := testkit.GPGKeys(t, s, "a.admin")["a.admin"]
	s.MustRun("sign", "add", "d.klein", "--fingerprint", k.Fingerprint, "--from-gpg")
	// The first publish of el9 shipped slurm-release-26.05-2 with d.klein.
	publishRelease(t, s, k.Fingerprint, "slurm-release-26.05-2.el9")
	// The only active key confirms its own deployment.
	out, err := inTerminal(t, s, "d.klein\n", "sign", "deployed", "d.klein", "--release", "slurm-release-26.05-2", "--target", "el9")
	if err != nil {
		t.Fatalf("sign deployed d.klein: %v\n%s", err, out)
	}
	for _, want := range []string{"release  slurm-release-26.05-2.el9 in latest of el9 · key file holds d.klein",
		"Type the key id to confirm that all clients have this release:",
		`committed keys/deployed.yaml "Deployed d.klein on el9 with slurm-release-26.05-2", signed by d.klein`,
		"pin      el9/RPM-GPG-KEY-slurm "} {
		if !strings.Contains(out, want) {
			t.Errorf("sign deployed: no %q in\n%s", want, out)
		}
	}
	data, err := os.ReadFile(filepath.Join(s.SetDir, "keys", "deployed.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "release: slurm-release-26.05-2.el9") || !strings.Contains(string(data), "confirmedBy: d.klein") {
		t.Errorf("deployed.yaml:\n%s", data)
	}
	// d.klein is the last deployed signer of el9.
	if code, p := problem(s, "sign", "retire", "d.klein"); code != 12 || p != "sign.last-signer" {
		t.Errorf("retire of the last signer: exit %d %s", code, p)
	}

	// a.admin joins; the release with its key is not published yet.
	s.MustRun("sign", "add", "a.admin", "--fingerprint", a.Fingerprint, "--from-gpg")
	if code, p := problem(s, "sign", "deployed", "a.admin", "--release", "slurm-release-26.05-2", "--target", "el9", "--dry-run"); code != 12 || p != "sign.not-deployed" {
		t.Errorf("deployed with a key file without the key: exit %d %s", code, p)
	}
	publishRelease(t, s, k.Fingerprint, "slurm-release-26.05-3.el9")
	r := s.MustRun("sign", "deployed", "a.admin", "--release", "slurm-release-26.05-3", "--target", "el9", "--dry-run")
	if !strings.Contains(r.Stdout, "key file holds a.admin") || !strings.Contains(r.Stderr, "dry run") {
		t.Errorf("dry run:\n%s%s", r.Stdout, r.Stderr)
	}
	// The key of the record cannot confirm itself while d.klein is active.
	s.Setenv("REPOCTL_SIGN_KEY", "a.admin")
	if code, p := problem(s, "sign", "deployed", "a.admin", "--release", "slurm-release-26.05-3", "--target", "el9"); code != 12 || p != "sign.self-confirm" {
		t.Errorf("self-confirm: exit %d %s", code, p)
	}
	s.Setenv("REPOCTL_SIGN_KEY", "d.klein")
	// The web UI confirms it (cli.html#ui-confirmation): the confirmation
	// counts only from the server of ui.json.
	args := []string{"sign", "deployed", "a.admin", "--release", "slurm-release-26.05-3", "--target", "el9", "--note", "puppet", "--confirm-fd", "3"}
	confirm := `{"v":1,"command":"sign deployed","set":"slurm-26.05","targets":["el9"],"keys":["a.admin"]}`
	if r := uiChild(t, s, confirm, args...); r.code != 6 || !strings.Contains(r.stderr, "is not the web UI server") {
		t.Errorf("--confirm-fd without a UI server: exit %d\n%s", r.code, r.stderr)
	}
	unlock := fakeUIServer(t, s)
	if r := uiChild(t, s, strings.Replace(confirm, `"a.admin"]`, `"d.klein"]`, 1), args...); r.code != 6 || !strings.Contains(r.stderr, "typed key ids [d.klein] are not a.admin") {
		t.Errorf("--confirm-fd with another key: exit %d\n%s", r.code, r.stderr)
	}
	if r := uiChild(t, s, confirm, args...); r.code != 0 || !strings.Contains(r.stdout, `"Deployed a.admin on el9 with slurm-release-26.05-3"`) {
		t.Fatalf("sign deployed a.admin --confirm-fd: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	unlock()
	// The journal of a command without a run records the change.
	activity, err := os.ReadFile(filepath.Join(s.Home, ".local", "state", "repoctl", "activity.jsonl"))
	if err != nil || !strings.Contains(string(activity), "sign deployed") || !strings.Contains(string(activity), "Deployed a.admin on el9") {
		t.Errorf("activity.jsonl: %v\n%s", err, activity)
	}
	env, _ := s.RunJSON("sign", "ls", "--target", "el9")
	for _, x := range env["items"].([]any)[0].(map[string]any)["keys"].([]any) {
		m := x.(map[string]any)
		if m["deployed"] != true {
			t.Errorf("%v is not deployed", m["keyId"])
		}
	}
	r = s.MustRun("sign", "ls", "--target", "el9")
	if !strings.Contains(r.Stdout, "slurm-release-26.05-3, d.klein, ") || !strings.Contains(r.Stdout, "slurm-release-26.05-2, d.klein, ") {
		t.Errorf("sign ls:\n%s", r.Stdout)
	}
	// Now d.klein can retire, but not be removed: latest holds its RPMs.
	s.MustRun("sign", "retire", "d.klein")
	if code, p := problem(s, "sign", "remove", "d.klein"); code != 12 || p != "sign.key-in-use" {
		t.Errorf("remove of a key in use: exit %d %s", code, p)
	}
	// Withdraw the record of a.admin.
	s.Setenv("REPOCTL_SIGN_KEY", "a.admin")
	if out, err := inTerminal(t, s, "a.admin\n", "sign", "deployed", "a.admin", "--withdraw", "--target", "el9"); err != nil || !strings.Contains(out, "Withdraw deployment of a.admin on el9") {
		t.Fatalf("withdraw: %v\n%s", err, out)
	}
	if got := s.Git(s.SetDir, "log", "-1", "--format=%G? %s"); !signedCommit(got, "Withdraw deployment of a.admin on el9") {
		t.Errorf("commit: %q", got)
	}
	r = s.MustRun("sign", "keyfile", "--target", "el9", "--checksum")
	if !strings.Contains(r.Stdout, "el9/RPM-GPG-KEY-slurm") || !strings.Contains(r.Stdout, "slurm-release-26.05-3.el9 in p-20261103-1412-5e1") {
		t.Errorf("keyfile --checksum:\n%s", r.Stdout)
	}
	s.RunJSON("sign", "keyfile", "--target", "el9")
}
