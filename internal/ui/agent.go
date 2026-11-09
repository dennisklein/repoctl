package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
)

// AgentConfig is the agent of the agent buttons: agent.command,
// agent.args, agent.timeout and state.keep_agent of the user config.
type AgentConfig struct {
	Command string
	Args    []string
	Timeout time.Duration
	Keep    int
}

// agentTask is what an agent button asks: a skill of the plugin
// repoctl-ui and its label.
type agentTask struct {
	Skill string
	Label string
}

var agentTasks = map[string]agentTask{
	"explain-failure": {Skill: "triage-build", Label: "Explain failure"},
	"rebuild-check":   {Skill: "rebuild-check", Label: "Is a rebuild necessary?"},
	"release-notes":   {Skill: "package-updates", Label: "Summarize upstream release"},
	"draft-bump":      {Skill: "bump-package", Label: "Draft a version bump"},
}

// agentTitle gives the title of an agent button (R5: say what it does).
func agentTitle(task string) string {
	return "Runs the agent with the skill " + agentTasks[task].Skill + ". Tools: read only. It changes nothing."
}

// agentResult is one agent run, stored as Paths.Agent(SET)/ID.json.
type agentResult struct {
	ID       string `json:"id"`
	Set      string `json:"set"`
	Task     string `json:"task"`
	Skill    string `json:"skill"`
	Input    string `json:"input"`
	State    string `json:"state"`
	Started  string `json:"started"`
	Finished string `json:"finished,omitempty"`
	Text     string `json:"text,omitempty"`
	Error    string `json:"error,omitempty"`
}

type agentRun struct {
	agentResult
	pgid    int
	stopped bool
}

// agents runs the agent buttons of all sets.
type agents struct {
	s    *Server
	mu   sync.Mutex
	runs map[string]*agentRun
}

func newAgents(s *Server) *agents {
	a := &agents{s: s, runs: map[string]*agentRun{}}
	// Results that were running when a server stopped did not finish.
	for _, e := range s.sets.all() {
		dir := s.o.Paths.Agent(e.Name)
		ents, _ := os.ReadDir(dir)
		for _, ent := range ents {
			p := filepath.Join(dir, ent.Name())
			var r agentResult
			if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &r) == nil && r.State == "running" { //nolint:gosec // G304: the user's state
				r.State, r.Error = "failed", "the server stopped"
				a.write(&r)
			}
		}
	}
	return a
}

// agentOff says why the agent buttons are off, or "".
func (s *Server) agentOff() string {
	if s.o.Agent.Command == "" {
		return "Agent buttons are off: agent.command is not set in the user config."
	}
	if _, err := exec.LookPath(s.o.Agent.Command); err != nil {
		return "Agent buttons are off: the agent command " + s.o.Agent.Command + " is not on PATH."
	}
	return ""
}

// agentView is the data of the agent drawer.
type agentView struct {
	agentResult
	Label   string
	Running bool
	Off     string
	SetName string
}

func (s *Server) agentView(r *agentRun) agentView {
	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	return agentView{agentResult: r.agentResult, Label: agentTasks[r.Task].Label, Running: r.State == "running", SetName: r.Set}
}

// ofSet gives the agent runs of a set that this server started.
func (a *agents) ofSet(set string) []*agentRun {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*agentRun
	for _, r := range a.runs {
		if r.Set == set {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(x, y *agentRun) int { return strings.Compare(x.ID, y.ID) })
	return out
}

func (a *agents) write(r *agentResult) {
	dir := a.s.o.Paths.Agent(r.Set)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	tmp := filepath.Join(dir, r.ID+".json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, r.ID+".json"))
	}
}

// prune keeps the newest Keep results of a set.
func (a *agents) prune(set string) {
	keep := a.s.o.Agent.Keep
	if keep <= 0 {
		keep = 50
	}
	dir := a.s.o.Paths.Agent(set)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	for len(names) > keep {
		_ = os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

// handleAgent starts the agent of a button and answers with its drawer.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.setOf(r.PathValue("set"))
	if !ok {
		s.fail(w, r, http.StatusNotFound, "No such set.")
		return
	}
	task, ok := agentTasks[r.PathValue("task")]
	if !ok {
		s.fail(w, r, http.StatusNotFound, "No such agent task.")
		return
	}
	if off := s.agentOff(); off != "" {
		s.respond(w, r, http.StatusOK, "agent-drawer", agentView{Off: off, Label: task.Label, SetName: sc.Name})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	var parts []string
	for _, k := range []string{"run", "node", "advisory", "package", "version"} {
		if v := r.PostForm.Get(k); v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	run, err := s.agents.start(sc, r.PathValue("task"), strings.Join(parts, " "))
	if err != nil {
		s.toast(w, http.StatusConflict, "err", err.Error())
		return
	}
	s.respond(w, r, http.StatusOK, "agent-drawer", s.agentView(run))
}

// handleAgentShow shows a stored agent result.
func (s *Server) handleAgentShow(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.setOf(r.PathValue("set"))
	if !ok {
		s.fail(w, r, http.StatusNotFound, "No such set.")
		return
	}
	id := filepath.Base(r.PathValue("id"))
	var res agentResult
	b, err := os.ReadFile(filepath.Join(s.o.Paths.Agent(sc.Name), id+".json")) //nolint:gosec // G304: base name in the user's state
	if err != nil || json.Unmarshal(b, &res) != nil {
		s.fail(w, r, http.StatusNotFound, "No such agent result.")
		return
	}
	s.respond(w, r, http.StatusOK, "agent-drawer", agentView{agentResult: res, Label: agentTasks[res.Task].Label, SetName: sc.Name})
}

// handleAgentStop stops an agent run: SIGINT to its process group, then
// SIGTERM after 10 s and SIGKILL after 10 s more.
func (s *Server) handleAgentStop(w http.ResponseWriter, r *http.Request) {
	s.agents.mu.Lock()
	run, ok := s.agents.runs[r.PathValue("id")]
	s.agents.mu.Unlock()
	if !ok {
		s.fail(w, r, http.StatusNotFound, "No such agent run.")
		return
	}
	s.agents.stop(run)
	s.respond(w, r, http.StatusOK, "agent-drawer", s.agentView(run))
}

func (a *agents) stop(run *agentRun) {
	a.mu.Lock()
	if run.State != "running" || run.pgid == 0 {
		a.mu.Unlock()
		return
	}
	run.stopped = true
	pgid := run.pgid
	a.mu.Unlock()
	_ = syscall.Kill(-pgid, syscall.SIGINT)
	go func() {
		for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
			time.Sleep(10 * time.Second)
			a.mu.Lock()
			done := run.State != "running"
			a.mu.Unlock()
			if done {
				return
			}
			_ = syscall.Kill(-pgid, sig)
		}
	}()
}

// pluginDir writes the plugin repoctl-ui with the skills of the server
// and gives its directory.
func (a *agents) pluginDir() (string, error) {
	dir := filepath.Join(a.s.o.Paths.UIChildren(), "plugin")
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o700); err != nil {
		return "", err
	}
	manifest := fmt.Sprintf(`{"name":"repoctl-ui","version":%q,"description":"The skills of the agent buttons of repoctl ui."}`+"\n", a.s.o.Version)
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(manifest), 0o600); err != nil {
		return "", err
	}
	if a.s.o.Skills == nil {
		return dir, nil
	}
	err := fs.WalkDir(a.s.o.Skills, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(a.s.o.Skills, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dir, "skills", filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o600)
	})
	return dir, err
}

// AgentArgs gives the command line of an agent run: agent.command and
// agent.args, the prompt of the skill, the plugin, and the read-only MCP
// server of repoctl as the only tools.
func AgentArgs(cfg AgentConfig, exe, pluginDir, skill, input string) []string {
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"repoctl": map[string]any{"command": exe, "args": []string{"mcp", "start", "--tools", "read"}}}})
	prompt := "/repoctl-ui:" + skill
	if input != "" {
		prompt += " " + input
	}
	args := slices.Clone(cfg.Args)
	return append(args, "-p", prompt, "--plugin-dir", pluginDir, "--output-format", "stream-json", "--verbose",
		"--mcp-config", string(mcp), "--strict-mcp-config", "--allowedTools", "mcp__repoctl",
		"--disallowedTools", "Bash", "Edit", "Write", "NotebookEdit")
}

func (a *agents) start(sc *setCtx, task, input string) (*agentRun, error) {
	exe, err := a.s.executable()
	if err != nil {
		return nil, err
	}
	plugin, err := a.pluginDir()
	if err != nil {
		return nil, err
	}
	t := agentTasks[task]
	now := a.s.o.Now()
	run := &agentRun{agentResult: agentResult{ID: "a-" + now.UTC().Format("20060102-1504") + "-" + randHex(3), Set: sc.Name, Task: task, Skill: t.Skill,
		Input: input, State: "running", Started: api.FormatTime(now)}}
	timeout := a.s.o.Agent.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	cmd := exec.Command(a.s.o.Agent.Command, AgentArgs(a.s.o.Agent, exe, plugin, t.Skill, input)...) //nolint:gosec // G204: the agent of the user config
	cmd.Dir = sc.Dir
	cmd.Env = ChildEnv(a.s.o.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	run.pgid = cmd.Process.Pid
	a.mu.Lock()
	a.runs[run.ID] = run
	a.mu.Unlock()
	a.write(&run.agentResult)
	a.prune(sc.Name)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	go func() {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			a.mu.Lock()
			run.Error = "the agent took longer than agent.timeout (" + timeout.String() + ")"
			a.mu.Unlock()
			a.stop(run)
		}
	}()
	go func() {
		defer cancel()
		sc2 := bufio.NewScanner(stdout)
		sc2.Buffer(make([]byte, 64<<10), 8<<20)
		for sc2.Scan() {
			if text, final := streamText(sc2.Bytes()); text != "" {
				a.mu.Lock()
				if final {
					run.Text = text
				} else {
					run.Text += text
				}
				a.mu.Unlock()
				a.s.notify.Notify("agent:" + run.ID)
			}
		}
		err := cmd.Wait()
		a.mu.Lock()
		switch {
		case run.stopped && run.Error == "":
			run.State = "stopped"
		case run.stopped:
			run.State = "failed"
		case err != nil:
			run.State = "failed"
			run.Error = "the agent ended with " + err.Error()
		default:
			run.State = "done"
		}
		run.Finished = api.FormatTime(a.s.o.Now())
		res := run.agentResult
		a.mu.Unlock()
		a.write(&res)
		a.s.notify.Notify("agent:" + run.ID)
	}()
	return run, nil
}

// streamText reads one line of --output-format stream-json: the text of
// an assistant message, or the final result.
func streamText(line []byte) (string, bool) {
	var ev struct {
		Type    string `json:"type"`
		Result  string `json:"result"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return "", false
	}
	switch ev.Type {
	case "result":
		return ev.Result, ev.Result != ""
	case "assistant":
		var b strings.Builder
		for _, c := range ev.Message.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		return b.String(), false
	}
	return "", false
}
