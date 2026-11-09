package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/skills"
	"github.com/dennisklein/repoctl/internal/testkit"
)

const agentSetYAML = `apiVersion: repoctl/v1alpha1
name: slurm-26.05
repo:
  name: slurm
  url: https://repo.example.org/slurm-26.05
targets:
  - id: el9
    mock: rocky+epel-9-x86_64
    plugins: [rpmautospec]
    path: el9
skills: [agent]
packages:
  - id: ucx
    branch: ucx
`

type agentEnv struct {
	home, set string
	env       map[string]string
}

func newAgentEnv(t *testing.T) *agentEnv {
	root := t.TempDir()
	e := &agentEnv{home: filepath.Join(root, "home"), set: filepath.Join(root, "set")}
	e.env = map[string]string{
		"HOME": e.home, "XDG_CONFIG_HOME": filepath.Join(e.home, ".config"), "XDG_STATE_HOME": filepath.Join(e.home, ".local", "state"),
		"XDG_DATA_HOME": filepath.Join(e.home, ".local", "share"), "XDG_CACHE_HOME": filepath.Join(e.home, ".cache"),
		"XDG_RUNTIME_DIR": filepath.Join(root, "run"), "PATH": os.Getenv("PATH"),
	}
	for _, d := range []string{e.home, filepath.Join(e.set, "agent", "release-notes")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"repoctl.yaml":                   agentSetYAML,
		"agent/release-notes/SKILL.md":   "---\nname: release-notes\ndescription: Write the release notes.\nmetadata:\n  version: \"3\"\n---\nbody\n",
		"agent/triage-build/SKILL.md":    "---\nname: triage-build\ndescription: Shadow.\n---\nbody\n",
		"agent/triage-build/.keep":       "",
		"agent/release-notes/extra.txt":  "x",
		"agent/README.md":                "not a skill",
		"agent/release-notes/.gitignore": "",
	}
	if out, err := exec.Command("git", "init", "-q", e.set).CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	for name, text := range files {
		p := filepath.Join(e.set, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *agentEnv) run(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	a := &App{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Getenv: func(k string) string { return e.env[k] }, Now: time.Now}
	code := a.Execute(context.Background(), append([]string{"-C", e.set}, args...))
	return out.String(), errb.String(), code
}

func (e *agentEnv) runJSON(t *testing.T, args ...string) []api.Skill {
	t.Helper()
	out, errb, code := e.run(t, "", append(args, "--json")...)
	if code != 0 {
		t.Fatalf("%v: exit %d\n%s%s", args, code, out, errb)
	}
	if err := testkit.Validate("Envelope", []byte(out)); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	var env struct {
		Kind     string        `json:"kind"`
		Items    []api.Skill   `json:"items"`
		Warnings []api.Problem `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env.Kind != string(api.KindSkill) {
		t.Errorf("kind %s", env.Kind)
	}
	return env.Items
}

func TestSkillsCommands(t *testing.T) {
	e := newAgentEnv(t)
	items := e.runJSON(t, "skills", "ls")
	names := map[string]api.Skill{}
	for _, it := range items {
		names[it.Name+"/"+it.Source] = it
	}
	if len(items) != 7 || names["release-notes/set"].Version != "3" || !names["triage-build/set"].Conflict || len(names["repoctl/builtin"].Installed) != 0 {
		t.Fatalf("skills ls: %+v", items)
	}

	items = e.runJSON(t, "skills", "install", "--user", "triage-build", "repoctl")
	if len(items) != 2 || items[0].Installed[0].Scope != "user" {
		t.Fatalf("install: %+v", items)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".claude", "skills", "triage-build", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	items = e.runJSON(t, "skills", "ls")
	for _, it := range items {
		if it.Name == "triage-build" && it.Source == "builtin" && (len(it.Installed) != 1 || it.Installed[0].State != "same") {
			t.Errorf("ls after install: %+v", it)
		}
	}
	out, _, code := e.run(t, "", "skills", "show", "triage-build")
	if code != 0 || !strings.Contains(out, "name: triage-build") || !strings.Contains(out, "repoctl-version: ") {
		t.Errorf("show: %d\n%s", code, out)
	}
	if _, _, code := e.run(t, "", "skills", "install", "nope"); code != api.ExitUsage {
		t.Errorf("install nope: exit %d", code)
	}
	if _, _, code := e.run(t, "", "skills", "install", "--user", "--project"); code != api.ExitUsage {
		t.Errorf("--user --project: exit %d", code)
	}
	items = e.runJSON(t, "skills", "uninstall")
	if len(items) != 2 {
		t.Errorf("uninstall: %+v", items)
	}

	out, _, code = e.run(t, "", "skills", "path")
	if code != 0 || out != skills.SystemDir+"\n" {
		t.Errorf("path: %q", out)
	}
	in := `{"cwd":"` + e.set + `","tool_name":"Bash","tool_input":{"command":"repoctl publish"}}`
	out, _, code = e.run(t, in, "skills", "guard")
	if code != 0 || !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("guard: %d %q", code, out)
	}
	in = `{"cwd":"` + e.set + `","tool_name":"Edit","tool_input":{"file_path":"packages/ucx/ucx.spec"}}`
	if out, _, code = e.run(t, in, "skills", "guard"); code != 0 || out != "" {
		t.Errorf("guard allow: %d %q", code, out)
	}
	if _, _, code := e.run(t, "", "skills", "guard", "--json"); code != api.ExitUsage {
		t.Errorf("guard --json: exit %d", code)
	}
}

func TestMCPTools(t *testing.T) {
	e := newAgentEnv(t)
	out, _, code := e.run(t, "", "mcp", "tools")
	if code != 0 || out != string(api.SchemaFile("mcp-tools.json")) {
		t.Errorf("mcp tools: exit %d", code)
	}
	out, _, code = e.run(t, "", "mcp", "tools", "--tools", "read")
	var doc struct {
		Tools []struct {
			Class string `json:"class"`
		} `json:"tools"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || len(doc.Tools) != 25 {
		t.Errorf("mcp tools --tools read: exit %d, %d tools", code, len(doc.Tools))
	}
	if _, errb, code := e.run(t, "", "mcp", "start", "--tools", "repoctl_nope"); code != api.ExitUsage || !strings.Contains(errb, "repoctl_nope") {
		t.Errorf("mcp start --tools repoctl_nope: exit %d %s", code, errb)
	}
}

// TestMCPStartStdio runs repoctl mcp start on stdin and stdout: initialize,
// tools/list and two calls, one request after the answer of the one
// before; the end of stdin ends the server with exit 0.
func TestMCPStartStdio(t *testing.T) {
	e := newAgentEnv(t)
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"repoctl_build","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"repoctl_config_show","arguments":{"key":"jobs"}}}`,
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var errb bytes.Buffer
	a := &App{Stdin: inR, Stdout: outW, Stderr: &errb, Getenv: func(k string) string { return e.env[k] }, Now: time.Now}
	done := make(chan int, 1)
	go func() {
		done <- a.Execute(context.Background(), []string{"-C", e.set, "mcp", "start", "--tools", "read"})
		_ = outW.Close()
	}()
	results := map[float64]json.RawMessage{}
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for _, l := range lines {
		if _, err := io.WriteString(inW, l+"\n"); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(l, `"id"`) {
			if !sc.Scan() {
				t.Fatalf("no answer to %s: %s", l, errb.String())
			}
			var m struct {
				ID     float64         `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Fatalf("stdout line is no JSON: %q", sc.Text())
			}
			results[m.ID] = m.Result
		}
	}
	_ = inW.Close()
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	var list struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if err := json.Unmarshal(results[2], &list); err != nil || len(list.Tools) != 25 {
		t.Errorf("tools/list: %v %s", err, results[2])
	}
	for id, code := range map[float64]string{3: api.PMCPToolNotAllowed, 4: ""} {
		var r struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		}
		if err := json.Unmarshal(results[id], &r); err != nil {
			t.Fatalf("call %v: %v %s", id, err, results[id])
		}
		if err := testkit.Validate("Envelope", r.StructuredContent); err != nil {
			t.Errorf("call %v: %v", id, err)
		}
		if r.IsError != (code != "") || code != "" && !bytes.Contains(r.StructuredContent, []byte(code)) {
			t.Errorf("call %v: isError %v %s", id, r.IsError, r.StructuredContent)
		}
	}
}
