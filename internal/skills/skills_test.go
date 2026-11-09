package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/testkit"
)

var designNames = []string{"bump-package", "package-updates", "rebuild-check", "repoctl", "triage-build"}

func TestBuiltinFrontmatter(t *testing.T) {
	if got := Names(); !slices.Equal(got, designNames) {
		t.Fatalf("names %v, want %v", got, designNames)
	}
	for _, s := range Builtin("0.4.0") {
		fm, body, err := ParseFrontmatter(s.Text)
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		if fm.Name != s.Name || fm.Description == "" || len(fm.AllowedTools) == 0 || len(bytes.TrimSpace(body)) == 0 {
			t.Errorf("%s: frontmatter %+v", s.Name, fm)
		}
		if s.Version != "0.4.0" || s.APIVersion != api.APIVersion || s.Source != SourceBuiltin {
			t.Errorf("%s: version %s api %s source %s", s.Name, s.Version, s.APIVersion, s.Source)
		}
		for _, tool := range fm.AllowedTools {
			for _, bad := range []string{"publish", "repoctl sign", "git push", "rpmsign", "gpg"} {
				if strings.Contains(tool, bad) {
					t.Errorf("%s: allowed tool %q", s.Name, tool)
				}
			}
		}
		r := s.Render()
		rf, _, err := ParseFrontmatter(r)
		if err != nil {
			t.Fatalf("%s: rendered: %v", s.Name, err)
		}
		if rf.Metadata["repoctl-version"] != "0.4.0" || rf.Metadata["api-version"] != api.APIVersion || rf.Name != s.Name {
			t.Errorf("%s: rendered metadata %v", s.Name, rf.Metadata)
		}
	}
	tb, ok := Lookup("triage-build", "1")
	if !ok {
		t.Fatal("no triage-build")
	}
	want := []string{"Read", "Grep", "Bash(repoctl runs:*)", "Bash(repoctl logs:*)", "Bash(repoctl graph:*)", "Bash(repoctl show:*)",
		"Bash(repoctl doctor:*)", "mcp__repoctl__repoctl_runs", "mcp__repoctl__repoctl_run_get", "mcp__repoctl__repoctl_logs",
		"mcp__repoctl__repoctl_graph", "mcp__repoctl__repoctl_show", "mcp__repoctl__repoctl_doctor"}
	if !slices.Equal(tb.AllowedTools, want) {
		t.Errorf("triage-build allowed-tools %q", tb.AllowedTools)
	}
}

func TestParseFrontmatter(t *testing.T) {
	fm, body, err := ParseFrontmatter([]byte("\xef\xbb\xbf---\nname: x\ndescription: d\nallowed-tools: Read, Grep\n---\nbody\n"))
	if err != nil || fm.Name != "x" || !slices.Equal(fm.AllowedTools, []string{"Read", "Grep"}) || string(body) != "body\n" {
		t.Errorf("%+v %q %v", fm, body, err)
	}
	for _, bad := range []string{"no frontmatter", "---\nname: x\n---\n", "---\ndescription: d\n---\n", "---\nname: [\n---\n"} {
		if _, _, err := ParseFrontmatter([]byte(bad)); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestSetSkills(t *testing.T) {
	set := t.TempDir()
	write(t, filepath.Join(set, "agent", "release-notes", "SKILL.md"), "---\nname: release-notes\ndescription: Notes.\nmetadata:\n  version: \"2\"\n---\nx\n")
	write(t, filepath.Join(set, "agent", "triage-build", "SKILL.md"), "---\nname: triage-build\ndescription: Mine.\n---\nx\n")
	list, errs := SetSkills(set, []string{"agent", "missing"})
	if len(errs) != 0 || len(list) != 2 {
		t.Fatalf("%v %v", list, errs)
	}
	if list[0].Name != "release-notes" || list[0].Version != "2" || list[0].Conflict || list[0].Source != SourceSet {
		t.Errorf("release-notes: %+v", list[0])
	}
	if list[1].Name != "triage-build" || !list[1].Conflict {
		t.Errorf("triage-build: %+v", list[1])
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// guardSet writes a set with the packages ucx and slurm (worktrees), the
// in-tree package site and a macros file.
func guardSet(t *testing.T, extra string) (set, home string) {
	root := t.TempDir()
	set, home = filepath.Join(root, "slurm-repo"), filepath.Join(root, "home")
	write(t, filepath.Join(set, "repoctl.yaml"), `apiVersion: repoctl/v1alpha1
name: slurm-26.05
targets:
  - id: el9
    macros: [macros/el9.macros]
packages:
  - id: ucx
  - id: slurm
  - id: site
    path: site
`+extra)
	for _, d := range []string{"packages/ucx", "packages/slurm", "site", "keys", ".claude"} {
		if err := os.MkdirAll(filepath.Join(set, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	return set, home
}

func bash(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func file(path string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"file_path": path})
	return b
}

func TestGuard(t *testing.T) {
	set, home := guardSet(t, "")
	env := GuardEnv{Home: home, UserConfigDir: filepath.Join(home, ".config", "repoctl")}
	other := t.TempDir()
	cases := []struct {
		tool, cwd string
		input     json.RawMessage
		deny      bool
	}{
		{"Bash", set, bash("git -C packages/ucx commit -m 'ucx: 1.19'"), false},
		{"Bash", set, bash("git -C packages/ucx add ucx.spec && git -C packages/ucx diff --staged"), false},
		{"Bash", set, bash("git -C packages/ucx push"), true},
		{"Bash", set, bash("git -C packages/ucx/.. commit -a"), true},
		{"Bash", set, bash("git -C packages/.. push"), true},
		{"Bash", set, bash("git -C packages/ucx/../.. commit -a -m x"), true},
		{"Bash", filepath.Join(set, "packages", "ucx"), bash("git commit -m x"), false},
		{"Bash", filepath.Join(set, "packages", "ucx"), bash("cd .. && git commit -a"), true},
		{"Bash", filepath.Join(set, "packages", "ucx"), bash("git rebase -i HEAD~2"), true},
		{"Bash", set, bash("git status"), false},
		{"Bash", set, bash("git log --oneline -3"), false},
		{"Bash", set, bash("git commit -m x"), true},
		{"Bash", set, bash("git -c core.hooksPath=/x -C packages/ucx commit"), true},
		{"Bash", set, bash("GIT_DIR=/x git -C packages/ucx commit"), true},
		{"Bash", set, bash("export GIT_WORK_TREE=/x; git -C packages/ucx commit"), true},
		{"Bash", set, bash("env GIT_INDEX_FILE=/x git -C packages/ucx add ."), true},
		{"Bash", other, bash("git commit -m x"), false},
		{"Bash", other, bash("git push"), true},
		{"Bash", set, bash("repoctl status --json"), false},
		{"Bash", set, bash("repoctl build ucx"), false},
		{"Bash", set, bash("repoctl publish"), true},
		{"Bash", set, bash("repoctl -C /x --json publish --yes"), true},
		{"Bash", set, bash("/usr/bin/repoctl repo push"), true},
		{"Bash", set, bash("repoctl sign ls"), true},
		{"Bash", set, bash("repoctl skills install --project"), true},
		{"Bash", set, bash("repoctl status && repoctl clean"), true},
		{"Bash", set, bash("echo $(repoctl publish)"), true},
		{"Bash", set, bash("echo `id`"), true},
		{"Bash", set, bash("eval repoctl status"), true},
		{"Bash", set, bash("bash -c 'repoctl status'"), true},
		{"Bash", set, bash("ls | xargs rm"), true},
		{"Bash", set, bash("rpm --addsign x.rpm"), true},
		{"Bash", set, bash("rpm -qa"), false},
		{"Bash", set, bash("gpg --list-keys"), true},
		{"Bash", set, bash("sudo mock -r el9 --shell"), true},
		{"Bash", set, bash("echo x > repoctl.lock"), true},
		{"Bash", set, bash("echo x >> packages/ucx/ucx.spec"), false},
		{"Bash", set, bash("tee keys/deployed.yaml < /dev/null"), true},
		{"Bash", set, bash("sed -i s/a/b/ repoctl.yaml"), true},
		{"Bash", set, bash("sed -n 1p repoctl.yaml"), false},
		{"Bash", set, bash("cp /tmp/x .git/config"), true},
		{"Bash", set, bash("rm -rf .claude"), true},
		{"Bash", set, bash("mv x macros/el9.macros"), true},
		{"Bash", set, bash("echo x > site/site.spec"), true},
		{"Bash", set, bash("cat ~/.claude/settings.json"), false},
		{"Bash", set, bash("echo x > ~/.claude/settings.json"), true},
		{"Edit", set, file("keys/deployed.yaml"), true},
		{"Edit", set, file(filepath.Join(set, "repoctl.yaml")), true},
		{"Write", set, file("repoctl.lock"), true},
		{"Write", set, file("packages/ucx/ucx.spec"), false},
		{"Edit", set, file("CLAUDE.md"), true},
		{"Edit", set, file("packages/ucx/CLAUDE.md"), true},
		{"Edit", set, file(".mcp.json"), true},
		{"Edit", set, file(".claude/settings.json"), true},
		{"Write", set, file(".repoctl/x"), true},
		{"Write", set, file("site/site.spec"), true},
		{"Write", set, file("macros/el9.macros"), true},
		{"Edit", other, file(filepath.Join(home, ".claude", "settings.json")), true},
		{"Edit", other, file(filepath.Join(home, ".config", "repoctl", "config.yaml")), true},
		{"Edit", other, file(filepath.Join(other, "notes.md")), false},
		{"NotebookEdit", set, json.RawMessage(`{"notebook_path":"keys/x.ipynb"}`), true},
		{"Read", set, file("keys/deployed.yaml"), false},
	}
	for _, c := range cases {
		d := Guard(HookInput{Cwd: c.cwd, ToolName: c.tool, ToolInput: c.input}, env)
		if d.Deny != c.deny {
			t.Errorf("%s %s in %s: deny %v, want %v (%s)", c.tool, c.input, filepath.Base(c.cwd), d.Deny, c.deny, d.Reason)
		}
		if d.Deny && !strings.HasPrefix(d.Reason, "repoctl guard: ") {
			t.Errorf("reason %q", d.Reason)
		}
	}
}

func TestGuardSymlink(t *testing.T) {
	set, home := guardSet(t, "")
	link := filepath.Join(t.TempDir(), "keys-link")
	if err := os.Symlink(filepath.Join(set, "keys"), link); err != nil {
		t.Fatal(err)
	}
	d := Guard(HookInput{Cwd: home, ToolName: "Write", ToolInput: file(filepath.Join(link, "x"))}, GuardEnv{Home: home})
	if !d.Deny {
		t.Error("a write through a symlink into keys/ is allowed")
	}
	pkg := filepath.Join(t.TempDir(), "ucx")
	if err := os.Symlink(filepath.Join(set, "packages", "ucx"), pkg); err != nil {
		t.Fatal(err)
	}
	if d := Guard(HookInput{Cwd: home, ToolName: "Bash", ToolInput: bash("git -C " + pkg + "/.. commit")}, GuardEnv{Home: home}); !d.Deny {
		t.Error("a commit through a symlink to packages/ucx/.. is allowed")
	}
}

// TestGuardNewPackage checks that a package that repoctl.yaml adds after
// the install gets its worktree rules without a new rule.
func TestGuardNewPackage(t *testing.T) {
	set, home := guardSet(t, "  - id: pmix\n")
	if err := os.MkdirAll(filepath.Join(set, "packages", "pmix"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := GuardEnv{Home: home}
	if d := Guard(HookInput{Cwd: set, ToolName: "Bash", ToolInput: bash("git -C packages/pmix commit -m x")}, env); d.Deny {
		t.Errorf("commit in a new package: %s", d.Reason)
	}
	if d := Guard(HookInput{Cwd: set, ToolName: "Bash", ToolInput: bash("git -C packages/pmix reset --hard")}, env); !d.Deny {
		t.Error("reset in a new package is allowed")
	}
}

func TestGuardHook(t *testing.T) {
	set, home := guardSet(t, "")
	env := GuardEnv{Home: home}
	in, _ := json.Marshal(HookInput{SessionID: "s", Cwd: set, HookEventName: "PreToolUse", ToolName: "Bash", ToolInput: bash("repoctl publish")})
	out := GuardHook(in, env)
	var o struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &o); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if o.HookSpecificOutput.HookEventName != "PreToolUse" || o.HookSpecificOutput.PermissionDecision != "deny" || o.HookSpecificOutput.PermissionDecisionReason == "" {
		t.Errorf("deny output %s", out)
	}
	in, _ = json.Marshal(HookInput{Cwd: set, ToolName: "Bash", ToolInput: bash("repoctl status")})
	if out := GuardHook(in, env); len(out) != 0 {
		t.Errorf("allow: %s", out)
	}
	if out := GuardHook([]byte("{"), env); !bytes.Contains(out, []byte(`"deny"`)) {
		t.Errorf("bad input: %s", out)
	}
}

func TestParseShell(t *testing.T) {
	sh := parseShell(`A=1 git -C "packages/u x" commit -m 'a; b' && cat <<EOF > out.txt
$(not a command)
EOF
echo "x" 2>&1 | tee -a log`)
	if sh.unsafe != "" {
		t.Fatalf("unsafe %q", sh.unsafe)
	}
	if len(sh.cmds) != 4 {
		t.Fatalf("%d commands: %+v", len(sh.cmds), sh.cmds)
	}
	if c := sh.cmds[0]; !slices.Equal(c.assigns, []string{"A=1"}) || !slices.Equal(c.words, []string{"git", "-C", "packages/u x", "commit", "-m", "a; b"}) {
		t.Errorf("first: %+v", c)
	}
	if c := sh.cmds[1]; !slices.Equal(c.writes, []string{"out.txt"}) {
		t.Errorf("here-doc: %+v", c)
	}
	for _, line := range []string{"echo $(id)", "echo `id`", "cat <(ls)", `echo "$(id)"`} {
		if parseShell(line).unsafe == "" {
			t.Errorf("%q: want unsafe", line)
		}
	}
}

func TestSettingsMerge(t *testing.T) {
	in := []byte(`{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls:*)"], "deny": ["Bash(rm:*)", "Bash(gpg:*)"]},
  "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "my-hook"}]}]}
}`)
	st, err := ParseSettings(in)
	if err != nil {
		t.Fatal(err)
	}
	added, err := st.AddDeny(DenyList)
	if err != nil || len(added) != len(DenyList)-1 {
		t.Fatalf("added %d, %v", len(added), err)
	}
	if ok, err := st.AddGuard(); !ok || err != nil {
		t.Fatalf("guard %v %v", ok, err)
	}
	if ok, _ := st.AddGuard(); ok {
		t.Error("the guard twice")
	}
	if !st.HasGuard() || len(st.MissingDeny()) != 0 {
		t.Error("merge incomplete")
	}
	b, _ := st.Bytes()
	if !bytes.Contains(b, []byte(`"model": "opus"`)) || !bytes.Contains(b, []byte("my-hook")) || !bytes.Contains(b, []byte("Bash(ls:*)")) {
		t.Errorf("merge lost keys:\n%s", b)
	}
	if bytes.Index(b, []byte(`"model"`)) > bytes.Index(b, []byte(`"permissions"`)) {
		t.Error("merge changed the key order")
	}
	if err := st.RemoveDeny(added); err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveGuard(); err != nil {
		t.Fatal(err)
	}
	b, _ = st.Bytes()
	if bytes.Contains(b, []byte(GuardCommand)) || !bytes.Contains(b, []byte("Bash(gpg:*)")) || !bytes.Contains(b, []byte("my-hook")) || !bytes.Contains(b, []byte("Bash(rm:*)")) {
		t.Errorf("remove took too much or too little:\n%s", b)
	}
	if _, err := ParseSettings([]byte(`{"permissions": []}`)); err != nil {
		if _, err2 := ParseSettings([]byte(`[`)); err2 == nil {
			t.Error("bad JSON accepted")
		}
	}
}

func TestInstallUninstallUser(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	write(t, settings, `{"theme": "dark", "permissions": {"deny": ["Bash(gpg:*)"]}}`)
	p := UserPlace(home)
	res, err := Install(InstallOptions{Place: p, Skills: Builtin("0.4.0"), Version: "0.4.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Installed) != 5 || !res.HookAdded || len(res.DenyAdded) != len(DenyList)-1 {
		t.Fatalf("result %+v", res)
	}
	for _, it := range res.Installed {
		if err := testkit.Validate("Skill", testkit.MarshalItem(t, it)); err != nil {
			t.Errorf("%s: %v", it.Name, err)
		}
	}
	text, err := os.ReadFile(filepath.Join(home, ".claude", "skills", "triage-build", FileName))
	if err != nil || !bytes.Contains(text, []byte("repoctl-version: 0.4.0")) {
		t.Fatalf("SKILL.md: %v\n%s", err, text)
	}
	m, err := ReadManifest(p.Root)
	if err != nil || len(m.Skills) != 5 || !m.Hook || m.Repoctl != "0.4.0" || m.SettingsCreated {
		t.Fatalf("manifest %+v %v", m, err)
	}
	if c := Copies(home, "", "", "0.4.0"); len(c) != 5 || c[0].State != "same" {
		t.Errorf("copies %+v", c)
	}
	if c := Copies(home, "", "", "0.5.0"); c[0].State != "older" {
		t.Errorf("copies against a newer binary: %+v", c[0])
	}
	if ch := GuardCheck(DoctorEnv{Home: home}); ch.Status != api.CheckStatusOk {
		t.Errorf("guard check: %+v", ch)
	}
	if ch := VersionMismatchCheck(DoctorEnv{Home: home, Version: "0.5.0"}); ch.Status != api.CheckStatusWarning || !strings.Contains(ch.Remediation, "--user") {
		t.Errorf("version check: %+v", ch)
	}

	// Again: the same files, no new settings.
	res, err = Install(InstallOptions{Place: p, Skills: Builtin("0.5.0"), Version: "0.5.0"})
	if err != nil || res.HookAdded || len(res.DenyAdded) != 0 {
		t.Fatalf("again: %+v %v", res, err)
	}

	// A human changes a file: install and uninstall refuse.
	write(t, filepath.Join(p.Root, "repoctl", FileName), "mine")
	if _, err := Install(InstallOptions{Place: p, Skills: Builtin("0.5.0"), Version: "0.5.0"}); !isChanged(err) {
		t.Errorf("install over a changed file: %v", err)
	}
	if _, err := Uninstall(p, nil); !isChanged(err) {
		t.Errorf("uninstall of a changed file: %v", err)
	}
	if c := Copies(home, "", "", "0.5.0"); c[slices.IndexFunc(c, func(c Copy) bool { return c.Name == "repoctl" })].State != "changed" {
		t.Error("copy state of a changed file")
	}
	if err := os.Remove(filepath.Join(p.Root, "repoctl", FileName)); err != nil {
		t.Fatal(err)
	}

	// A directory that install did not write.
	write(t, filepath.Join(home, "other", "skills", "repoctl", FileName), "x")
	if _, err := Install(InstallOptions{Place: Place{Scope: ScopeUser, Root: filepath.Join(home, "other", "skills"), Settings: filepath.Join(home, "other", "settings.json")}, Skills: Builtin("1"), Version: "1"}); !isChanged(err) {
		t.Errorf("install over a foreign directory: %v", err)
	}

	ur, err := Uninstall(p, []string{"triage-build"})
	if err != nil || len(ur.Removed) != 1 || ur.Settings {
		t.Fatalf("uninstall one: %+v %v", ur, err)
	}
	ur, err = Uninstall(p, nil)
	if err != nil || len(ur.Removed) != 4 || !ur.Settings {
		t.Fatalf("uninstall all: %+v %v", ur, err)
	}
	if _, err := os.Stat(filepath.Join(p.Root, ManifestFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the manifest stays")
	}
	b, _ := os.ReadFile(settings)
	st, _ := ParseSettings(b)
	if st.HasGuard() || !slices.Equal(st.Deny(), []string{"Bash(gpg:*)"}) || !bytes.Contains(b, []byte(`"theme"`)) {
		t.Errorf("settings after uninstall:\n%s", b)
	}
	if ch := GuardCheck(DoctorEnv{Home: home}); ch.Status != api.CheckStatusOk {
		t.Errorf("guard check without skills: %+v", ch)
	}
}

func isChanged(err error) bool {
	e, ok := api.AsError(err)
	return ok && e.Code == PChanged
}

func TestInstallProject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	set := t.TempDir()
	cmd := exec.Command("git", "init", "-q", set)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	p, err := ProjectPlace(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Install(InstallOptions{Place: p, Skills: Builtin("0.4.0")[:2], Version: "0.4.0"})
	if err != nil {
		t.Fatal(err)
	}
	ex, _ := os.ReadFile(filepath.Join(set, ".git", "info", "exclude"))
	for _, l := range []string{"/.claude/skills/" + ManifestFile, "/.claude/skills/bump-package/", "/.claude/settings.json"} {
		if !bytes.Contains(ex, []byte(l+"\n")) {
			t.Errorf("exclude has no %s:\n%s", l, ex)
		}
	}
	if len(res.Exclude) != 4 {
		t.Errorf("exclude added %q", res.Exclude)
	}
	if ch := GuardCheck(DoctorEnv{Home: t.TempDir(), SetDir: set}); ch.Status != api.CheckStatusOk {
		t.Errorf("guard check: %+v", ch)
	}
	if _, err := Uninstall(p, nil); err != nil {
		t.Fatal(err)
	}
	ex, _ = os.ReadFile(filepath.Join(set, ".git", "info", "exclude"))
	if bytes.Contains(ex, []byte(".claude")) {
		t.Errorf("exclude after uninstall:\n%s", ex)
	}
	if _, err := os.Stat(p.Settings); !errors.Is(err, os.ErrNotExist) {
		t.Error("the settings file that install made stays")
	}
}

func TestDoctorChecks(t *testing.T) {
	home := t.TempDir()
	env := DoctorEnv{Home: home, Version: "0.4.0", AgentCommand: "claude",
		LookPath:   func(string) (string, error) { return "/usr/bin/claude", nil },
		RunVersion: func(context.Context, string) (string, error) { return "2.1.0 (Claude Code)\n", nil },
	}
	checks := DoctorChecks(context.Background(), env)
	if len(checks) != 3 {
		t.Fatalf("%d checks", len(checks))
	}
	for _, c := range checks {
		if c.Status != api.CheckStatusOk || c.Group != "host" {
			t.Errorf("%+v", c)
		}
		if err := testkit.Validate("DoctorReport/properties/checks/items", testkit.MarshalItem(t, c)); err != nil {
			t.Errorf("%s: %v", c.Name, err)
		}
	}
	env.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if c := AgentCommandCheck(context.Background(), env); c.Status != api.CheckStatusWarning {
		t.Errorf("missing agent: %+v", c)
	}
	// Skills without a guard hook.
	if _, err := Install(InstallOptions{Place: UserPlace(home), Skills: Builtin("0.4.0"), Version: "0.4.0"}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, ".claude", "settings.json"), `{"permissions": {"deny": []}}`)
	if c := GuardCheck(env); c.Status != api.CheckStatusWarning || !strings.Contains(c.Remediation, "repoctl skills install --user") {
		t.Errorf("no guard: %+v", c)
	}
	write(t, filepath.Join(home, ".claude", "settings.json"), `{"hooks": {"PreToolUse": [{"matcher": "Bash|Edit|Write|NotebookEdit", "hooks": [{"type": "command", "command": "repoctl skills guard"}]}]}}`)
	if c := GuardCheck(env); c.Status != api.CheckStatusWarning || !strings.Contains(c.Detail, "deny") {
		t.Errorf("deny list differs: %+v", c)
	}
}
