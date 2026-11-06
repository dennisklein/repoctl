//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// TestMCPTools checks that repoctl mcp tools prints mcp-tools.json, and
// only the read tools with --tools read.
func TestMCPTools(t *testing.T) {
	s := testkit.NewStory(t)
	r := s.MustRun("mcp", "tools")
	if r.Stdout != string(api.SchemaFile("mcp-tools.json")) {
		t.Error("repoctl mcp tools differs from mcp-tools.json")
	}
	r = s.MustRun("mcp", "tools", "--tools", "read")
	var doc struct {
		Server struct {
			ToolModes struct {
				Read []string `json:"read"`
			} `json:"toolModes"`
		} `json:"server"`
		Tools []struct {
			Name  string `json:"name"`
			Class string `json:"class"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Tools) != len(doc.Server.ToolModes.Read) {
		t.Errorf("--tools read: %d tools, toolModes.read has %d", len(doc.Tools), len(doc.Server.ToolModes.Read))
	}
	for _, tl := range doc.Tools {
		if tl.Class != "read" {
			t.Errorf("--tools read lists %s (%s)", tl.Name, tl.Class)
		}
	}
	if r := s.Run("mcp", "tools", "--tools", "repoctl_publish"); r.Code != api.ExitUsage {
		t.Errorf("--tools repoctl_publish: exit %d", r.Code)
	}
}

// TestSkillsInstallProject installs the skills into the story set, checks
// the files, the settings, the exclude lines and the guard, and removes
// them again.
func TestSkillsInstallProject(t *testing.T) {
	s := testkit.NewStory(t)
	env, r := s.RunJSON("skills", "install", "--project")
	if r.Code != 0 {
		t.Fatalf("exit %d\n%s", r.Code, r.Stderr)
	}
	items, _ := env["items"].([]any)
	if env["kind"] != "Skill" || len(items) != 5 {
		t.Fatalf("install: kind %v, %d items", env["kind"], len(items))
	}
	root := filepath.Join(s.SetDir, ".claude", "skills")
	for _, name := range []string{"repoctl", "bump-package", "triage-build", "rebuild-check", "package-updates"} {
		b, err := os.ReadFile(filepath.Join(root, name, "SKILL.md"))
		if err != nil || !strings.Contains(string(b), "name: "+name) || !strings.Contains(string(b), "api-version: repoctl/v1alpha1") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".repoctl-skills.json")); err != nil {
		t.Error(err)
	}
	settings, err := os.ReadFile(filepath.Join(s.SetDir, ".claude", "settings.json"))
	if err != nil || !strings.Contains(string(settings), `"repoctl skills guard"`) || !strings.Contains(string(settings), `"Bash(repoctl publish:*)"`) {
		t.Fatalf("settings: %v\n%s", err, settings)
	}
	exclude, _ := os.ReadFile(filepath.Join(s.SetDir, ".git", "info", "exclude"))
	if !strings.Contains(string(exclude), "/.claude/skills/repoctl/") || !strings.Contains(string(exclude), "/.claude/settings.json") {
		t.Errorf("exclude:\n%s", exclude)
	}
	if st := s.Git(s.SetDir, "status", "--porcelain"); st != "" {
		t.Errorf("the install shows in git status:\n%s", st)
	}

	env, _ = s.RunJSON("skills", "ls")
	for _, it := range env["items"].([]any) {
		m := it.(map[string]any)
		inst, _ := m["installed"].([]any)
		if m["source"] == "builtin" && len(inst) != 1 {
			t.Errorf("ls: %v", m)
		}
	}

	// The guard of the install denies a publish and a commit on the set
	// branch, and allows a commit in a package worktree.
	guard := func(cmd string) string {
		in, _ := json.Marshal(map[string]any{"cwd": s.SetDir, "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]string{"command": cmd}})
		c := exec.Command(filepath.Join(s.Bin, "repoctl"), "skills", "guard")
		c.Dir, c.Env, c.Stdin = s.SetDir, s.Env(), strings.NewReader(string(in))
		out, err := c.Output()
		if err != nil {
			t.Fatalf("guard %q: %v", cmd, err)
		}
		return string(out)
	}
	if out := guard("repoctl publish --yes"); !strings.Contains(out, `"deny"`) {
		t.Errorf("guard publish: %q", out)
	}
	if out := guard("git -C packages/ucx/.. commit -a"); !strings.Contains(out, `"deny"`) {
		t.Errorf("guard set commit: %q", out)
	}
	if out := guard("git -C packages/ucx commit -m x"); out != "" {
		t.Errorf("guard package commit: %q", out)
	}

	env, r = s.RunJSON("skills", "uninstall", "--project")
	if r.Code != 0 || len(env["items"].([]any)) != 5 {
		t.Fatalf("uninstall: exit %d %v", r.Code, env["items"])
	}
	if _, err := os.Stat(filepath.Join(s.SetDir, ".claude")); !os.IsNotExist(err) {
		t.Errorf(".claude stays after uninstall: %v", err)
	}
	exclude, _ = os.ReadFile(filepath.Join(s.SetDir, ".git", "info", "exclude"))
	if strings.Contains(string(exclude), ".claude") {
		t.Errorf("exclude after uninstall:\n%s", exclude)
	}
}

// TestMCPStart drives repoctl mcp start over stdio in the story set.
func TestMCPStart(t *testing.T) {
	s := testkit.NewStory(t)
	c := exec.Command(filepath.Join(s.Bin, "repoctl"), "mcp", "start", "--tools", "read")
	c.Dir, c.Env = s.SetDir, s.Env()
	in, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	outp, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(outp)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	send := func(line string) json.RawMessage {
		if _, err := io.WriteString(in, line+"\n"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(line, `"id"`) {
			return nil
		}
		if !sc.Scan() {
			t.Fatalf("no answer to %s", line)
		}
		var m struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("%v: %s", err, sc.Bytes())
		}
		return m.Result
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}`)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	var list struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if err := json.Unmarshal(send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`), &list); err != nil || len(list.Tools) != 25 {
		t.Errorf("tools/list: %v, %d tools", err, len(list.Tools))
	}
	for _, call := range []struct{ name, args string }{
		{"repoctl_config_show", `{"key":"jobs"}`},
		{"repoctl_status", `{}`},
		{"repoctl_build", `{"packages":["ucx"]}`},
	} {
		res := send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"` + call.name + `","arguments":` + call.args + `}}`)
		var r struct {
			StructuredContent json.RawMessage `json:"structuredContent"`
		}
		if err := json.Unmarshal(res, &r); err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
		testkit.ValidateEnvelope(t, r.StructuredContent)
		if call.name == "repoctl_build" && !strings.Contains(string(r.StructuredContent), "mcp.tool-not-allowed") {
			t.Errorf("build in mode read: %s", r.StructuredContent)
		}
	}
	_ = in.Close()
	if err := c.Wait(); err != nil {
		t.Errorf("mcp start: %v", err)
	}
}
