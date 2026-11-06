package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// The hidden flags of repoctl build that the server passes when the build
// command has them: the run id that the server picks, and the origin of the
// run (origin.via mcp, origin.session, origin.client).
var buildHiddenFlags = []string{"run-id", "origin", "origin-session", "origin-client"}

// show runs repoctl show once per package and merges the envelopes: the
// command takes one package, the tool a list (absent: all packages).
func (s *Server) show(ctx context.Context, req *sdk.CallToolRequest, t Tool, in map[string]any) *sdk.CallToolResult {
	pkgs, err := stringList(in["packages"])
	if err != nil {
		return s.errorResult(t, err)
	}
	if len(pkgs) == 1 {
		argv, err := Argv(t.Name, in)
		if err != nil {
			return s.errorResult(t, err)
		}
		return s.runTool(ctx, req, t, argv, in)
	}
	if len(pkgs) == 0 {
		set, err := config.LoadSet(s.opts.SetDir)
		if err != nil {
			return s.errorResult(t, err)
		}
		for _, p := range set.Packages {
			pkgs = append(pkgs, p.ID)
		}
	}
	var merged map[string]any
	exit := 0
	for _, p := range pkgs {
		argv, err := Argv(t.Name, map[string]any{"packages": []any{p}})
		if err != nil {
			return s.errorResult(t, err)
		}
		if err := s.checkCommand(argv); err != nil {
			return s.errorResult(t, err)
		}
		out, errb, code := s.execute(ctx, nil, argv, true)
		env, err := decodeMap(s.envelopeOf(t, argv, out, errb, code))
		if err != nil {
			return s.errorResult(t, err)
		}
		if exit == 0 {
			exit = code
		}
		if merged == nil {
			merged = env
			continue
		}
		for _, k := range []string{"items", "warnings", "errors"} {
			a, _ := merged[k].([]any)
			b, _ := env[k].([]any)
			merged[k] = appendUnique(a, b, k != "items")
		}
	}
	if merged == nil {
		return s.errorResult(t, inputErrorf("the set has no package"))
	}
	b, _ := json.Marshal(merged)
	return toolResult(b, exit)
}

func appendUnique(a, b []any, unique bool) []any {
	if a == nil {
		a = []any{}
	}
	seen := map[string]bool{}
	if unique {
		for _, x := range a {
			k, _ := json.Marshal(x)
			seen[string(k)] = true
		}
	}
	for _, x := range b {
		if unique {
			k, _ := json.Marshal(x)
			if seen[string(k)] {
				continue
			}
			seen[string(k)] = true
		}
		a = append(a, x)
	}
	return a
}

func stringList(v any) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []any:
		var out []string
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, inputErrorf("each element must be a string")
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return x, nil
	}
	return nil, inputErrorf("expected a list of strings")
}

// decodeMap reads JSON into a map, with numbers as json.Number.
func decodeMap(b []byte) (map[string]any, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// post applies the input properties that the server handles itself
// (Mapping.Server) to an envelope.
func (s *Server) post(t Tool, in map[string]any, env []byte) []byte {
	switch t.Name {
	case "repoctl_run_get", "repoctl_run_wait":
		if b, _ := in["artifacts"].(bool); b {
			return env
		}
		return editItems(env, func(item map[string]any) bool {
			nodes, _ := item["nodes"].([]any)
			for _, n := range nodes {
				if m, ok := n.(map[string]any); ok {
					delete(m, "artifacts")
				}
			}
			return true
		})
	case "repoctl_config_show":
		key, _ := in["key"].(string)
		if key == "" {
			return env
		}
		return editItems(env, func(item map[string]any) bool {
			k, _ := item["key"].(string)
			return strings.HasPrefix(k, key)
		})
	case "repoctl_logs":
		tail := 120
		if n, ok := in["tail"].(json.Number); ok {
			if v, err := n.Int64(); err == nil {
				tail = int(v)
			}
		}
		var re *regexp.Regexp
		if g, _ := in["grep"].(string); g != "" {
			re, _ = regexp.Compile(g)
		}
		return editItems(env, func(item map[string]any) bool {
			filterLog(item, tail, re)
			return true
		})
	}
	return env
}

// editItems changes or drops the items of an envelope.
func editItems(env []byte, f func(item map[string]any) bool) []byte {
	m, err := decodeMap(env)
	if err != nil {
		return env
	}
	items, _ := m["items"].([]any)
	out := []any{}
	for _, it := range items {
		im, ok := it.(map[string]any)
		if !ok {
			out = append(out, it)
			continue
		}
		if f(im) {
			out = append(out, im)
		}
	}
	m["items"] = out
	b, err := json.Marshal(m)
	if err != nil {
		return env
	}
	return b
}

// The limits of repoctl_logs.
const (
	maxLogLines = 400
	maxLineLen  = 2000
	grepContext = 3
)

// filterLog keeps the last tail lines of a NodeLog, or with re the matching
// lines and 3 lines of context in the form of grep -n: "N:line" for a match,
// "N-line" for context and "--" between groups. Lines are cut at 2000
// bytes; at most 400 lines stay.
func filterLog(item map[string]any, tail int, re *regexp.Regexp) {
	raw, _ := item["lines"].([]any)
	lines := make([]string, len(raw))
	for i, l := range raw {
		lines[i], _ = l.(string)
	}
	first := 1
	if n, ok := item["firstLine"].(json.Number); ok {
		if v, err := n.Int64(); err == nil && v > 0 {
			first = int(v)
		}
	}
	truncated, _ := item["truncated"].(bool)
	var out []string
	if re != nil {
		keep := make([]bool, len(lines))
		match := make([]bool, len(lines))
		for i, l := range lines {
			if re.MatchString(l) {
				match[i] = true
				for j := max(0, i-grepContext); j <= min(len(lines)-1, i+grepContext); j++ {
					keep[j] = true
				}
			}
		}
		last := -2
		for i, l := range lines {
			if !keep[i] {
				continue
			}
			if last >= 0 && i != last+1 {
				out = append(out, "--")
			}
			sep := "-"
			if match[i] {
				sep = ":"
			}
			out = append(out, strconv.Itoa(first+i)+sep+cutLine(l))
			last = i
		}
		delete(item, "firstLine")
	} else {
		if tail > maxLogLines {
			tail = maxLogLines
		}
		if tail >= 0 && len(lines) > tail {
			first += len(lines) - tail
			lines = lines[len(lines)-tail:]
			truncated = true
		}
		for _, l := range lines {
			out = append(out, cutLine(l))
		}
		item["firstLine"] = first
	}
	if len(out) > maxLogLines {
		out = out[:maxLogLines]
		truncated = true
	}
	item["lines"] = out
	if truncated {
		item["truncated"] = true
	}
}

func cutLine(s string) string {
	if len(s) <= maxLineLen {
		return s
	}
	cut := maxLineLen
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

// runOf runs repoctl runs RUN and gives the envelope, its exit code and the
// Run, nil when the command failed.
func (s *Server) runOf(ctx context.Context, run string) ([]byte, int, *api.Run) {
	t, _ := s.cat.Tool("repoctl_run_get")
	argv, err := Argv("repoctl_run_get", map[string]any{"run": run})
	if err != nil {
		return s.problemEnvelope("Run", "runs", api.Problem{Code: PInvalidInput, Message: err.Error(), ExitCode: api.ExitUsage}), api.ExitUsage, nil
	}
	if err := s.checkCommand(argv); err != nil {
		e, _ := api.AsError(err)
		return s.problemEnvelope("Run", "runs", e.Problem), api.ExitFailed, nil
	}
	out, errb, exit := s.execute(ctx, nil, argv, true)
	env := s.envelopeOf(t, argv, out, errb, exit)
	e, ok := parseEnvelope(env)
	if !ok || len(e.Errors) > 0 || len(e.Items) == 0 {
		return env, exit, nil
	}
	var r api.Run
	if err := json.Unmarshal(e.Items[0], &r); err != nil {
		return env, exit, nil
	}
	return env, exit, &r
}

// finished reports a final state of a run.
func finished(st api.RunState) bool {
	switch st {
	case api.RunStateOk, api.RunStateFailed, api.RunStateCanceled, api.RunStateAbandoned:
		return true
	}
	return false
}

// runWait folds the journal of a run through repoctl runs RUN until the run
// has a final state or timeoutSeconds pass (at most 600). The server is free
// for other calls between the polls. Progress counts the build nodes that
// ended.
func (s *Server) runWait(ctx context.Context, req *sdk.CallToolRequest, t Tool, in map[string]any) *sdk.CallToolResult {
	timeout := 300
	if n, ok := in["timeoutSeconds"].(json.Number); ok {
		if v, err := n.Int64(); err == nil {
			timeout = int(min(max(v, 1), 600))
		}
	}
	run, _ := in["run"].(string)
	if _, err := Argv(t.Name, in); err != nil {
		return s.errorResult(t, err)
	}
	var rp *runProgress
	if tok := progressToken(req); tok != nil {
		rp = &runProgress{send: sender(req, tok)}
	}
	deadline := s.opts.Now().Add(time.Duration(timeout) * time.Second)
	for {
		env, exit, r := s.runOf(ctx, run)
		if r == nil {
			return toolResult(env, exit)
		}
		rp.update(ctx, r)
		if finished(r.State) {
			exit = r.ExitCode
			if r.State == api.RunStateAbandoned {
				env, exit = addError(env, api.Problem{
					Code: api.PRunAbandoned, Run: r.ID, ExitCode: api.ExitFailed,
					Message: fmt.Sprintf("Run %s is abandoned: its process ended without run.finished.", r.ID),
					Hint:    "Clean up the mock roots of the run. run: repoctl mock clean",
				})
			}
			return toolResult(s.post(t, in, env), exit)
		}
		if !s.opts.Now().Before(deadline) {
			return toolResult(s.post(t, in, env), api.ExitOK)
		}
		select {
		case <-ctx.Done():
			return toolResult(s.post(t, in, env), api.ExitInterrupted)
		case <-time.After(s.opts.Poll):
		}
	}
}

// addError adds an error to an envelope that has none of this code.
func addError(env []byte, p api.Problem) ([]byte, int) {
	m, err := decodeMap(env)
	if err != nil {
		return env, p.ExitCode
	}
	errs, _ := m["errors"].([]any)
	for _, e := range errs {
		if em, ok := e.(map[string]any); ok && em["code"] == p.Code {
			return env, p.ExitCode
		}
	}
	m["errors"] = append(errs, p)
	b, _ := json.Marshal(m)
	return b, p.ExitCode
}

// runCancel cancels a run that this server started; each other run gives
// cancel.foreign-run with a humanAction (agents.html#mcp-safety).
func (s *Server) runCancel(ctx context.Context, req *sdk.CallToolRequest, t Tool, in map[string]any) *sdk.CallToolResult {
	run, _ := in["run"].(string)
	if _, err := Argv(t.Name, in); err != nil {
		return s.errorResult(t, err)
	}
	env, exit, r := s.runOf(ctx, run)
	if r == nil {
		return toolResult(env, exit)
	}
	if r.Origin.Via != api.RunOriginMCP || r.Origin.Session != s.session {
		via := string(r.Origin.Via)
		if r.Origin.Via == api.RunOriginMCP {
			via = "another MCP session"
		}
		p := api.Problem{
			Code: api.PCancelForeignRun, Run: r.ID, ExitCode: api.ExitFailed,
			Message: fmt.Sprintf("Run %s was started by %s (%s@%s), not by this MCP server.", r.ID, via, r.Origin.User, r.Origin.Host),
			Hint:    "The human runs repoctl cancel " + r.ID + ".",
			HumanAction: &api.HumanAction{
				Action:  "cancel-foreign-run",
				Reason:  "This MCP server cancels only the runs that it started.",
				Command: "repoctl cancel " + r.ID,
			},
		}
		m, err := decodeMap(env)
		if err != nil {
			return s.errorResult(t, err)
		}
		m["errors"] = []any{p}
		b, _ := json.Marshal(m)
		return toolResult(b, api.ExitFailed)
	}
	in2 := map[string]any{"run": r.ID}
	if f, ok := in["force"]; ok {
		in2["force"] = f
	}
	argv, err := Argv(t.Name, in2)
	if err != nil {
		return s.errorResult(t, err)
	}
	return s.runTool(ctx, req, t, argv, in)
}

// build starts repoctl build as a detached child in its own session and
// returns the Run at plan.ready, at most after BuildStart. A child that
// ends before gives its own envelope, for example set.busy or lock.dirty.
func (s *Server) build(ctx context.Context, req *sdk.CallToolRequest, t Tool, in map[string]any) *sdk.CallToolResult {
	if s.opts.Executable == "" {
		return s.errorResult(t, api.Errorf(api.PInternal, "This MCP server cannot start a build: it does not know its executable."))
	}
	if _, ok := in["jobs"]; !ok && s.opts.Jobs != nil {
		if j := s.opts.Jobs(); j > 0 {
			in["jobs"] = json.Number(strconv.Itoa(j))
		}
	}
	argv, err := Argv(t.Name, in)
	if err != nil {
		return s.errorResult(t, err)
	}
	if err := s.checkCommand(argv); err != nil {
		return s.errorResult(t, err)
	}
	runID := ""
	if hidden := s.hiddenBuildFlags(); len(hidden) > 0 {
		var extra []string
		for _, f := range hidden {
			switch f {
			case "run-id":
				runID = NewRunID(s.opts.Now())
				extra = append(extra, "--run-id="+runID)
			case "origin":
				extra = append(extra, "--origin=mcp")
			case "origin-session":
				extra = append(extra, "--origin-session="+s.session)
			case "origin-client":
				if c := clientName(req); c != "" {
					extra = append(extra, "--origin-client="+c)
				}
			}
		}
		argv = insertFlags(argv, extra)
	}
	dir, err := os.MkdirTemp("", "repoctl-mcp-build-")
	if err != nil {
		return s.errorResult(t, err)
	}
	defer os.RemoveAll(dir)
	stdout, err := os.Create(filepath.Join(dir, "stdout.json"))
	if err != nil {
		return s.errorResult(t, err)
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(dir, "stderr.txt"))
	if err != nil {
		return s.errorResult(t, err)
	}
	defer stderr.Close()

	cmd := exec.Command(s.opts.Executable, append(s.globals(true), argv...)...)
	cmd.Dir = s.opts.SetDir
	cmd.Env = ChildEnviron(s.opts.Environ, "TRACEPARENT="+newTraceparent())
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	started := s.opts.Now()
	if err := cmd.Start(); err != nil {
		return s.errorResult(t, api.Errorf(api.PInternal, "cannot start %s build: %v", s.opts.Executable, err))
	}
	exited := make(chan int, 1)
	go func() {
		err := cmd.Wait()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			code = api.ExitFailed
		}
		exited <- code
	}()

	var f *follower
	defer func() {
		if f != nil {
			f.close()
		}
	}()
	tok := progressToken(req)
	deadline := started.Add(s.opts.BuildStart)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case code := <-exited:
			if f != nil {
				f.close()
			}
			out, _ := os.ReadFile(stdout.Name())
			errb, _ := os.ReadFile(stderr.Name())
			if _, ok := parseEnvelope(out); !ok && runID != "" {
				// The child ended after it wrote its journal: give the run.
				if env, exit, r := s.runOf(ctx, runID); r != nil {
					return toolResult(s.post(t, in, env), max(exit, code))
				}
			}
			return toolResult(s.envelopeOf(t, argv, out, errb, code), code)
		case <-ctx.Done():
			return s.startedResult(context.WithoutCancel(ctx), t, runID, cmd.Process.Pid, started)
		case <-tick.C:
		}
		if runID == "" || !fileExists(s.opts.Paths.Journal(s.opts.SetName, runID)) {
			if id := s.findRun(cmd.Process.Pid, started); id != "" {
				runID = id
			}
		}
		if runID != "" && tok != nil && f == nil {
			f = follow(s.opts.Paths.ProgressLog(s.opts.SetName, runID), sender(req, tok))
		}
		if runID != "" && journalHas(s.opts.Paths.Journal(s.opts.SetName, runID), "plan.ready", "run.finished") {
			break
		}
		if !s.opts.Now().Before(deadline) {
			break
		}
	}
	if f != nil {
		f.close()
	}
	return s.startedResult(ctx, t, runID, cmd.Process.Pid, started)
}

// startedResult gives the Run of a build child that runs on.
func (s *Server) startedResult(ctx context.Context, t Tool, runID string, pid int, started time.Time) *sdk.CallToolResult {
	if runID == "" {
		runID = s.findRun(pid, started)
	}
	if runID == "" {
		return s.errorResult(t, api.Errorf(api.PRunNotFound,
			"repoctl build (pid %d) started no run within %s; it may still be planning.", pid, s.opts.BuildStart).
			WithHint("List the runs of the set with repoctl_runs", ""))
	}
	env, exit, _ := s.runOf(ctx, runID)
	return toolResult(env, exit)
}

// hiddenBuildFlags gives the hidden flags that repoctl build of this binary
// has.
func (s *Server) hiddenBuildFlags() []string {
	s.treeMu.Lock()
	defer s.treeMu.Unlock()
	tree := s.tree()
	if tree == nil {
		return nil
	}
	cmd, _, err := tree.Find([]string{"build"})
	if err != nil || cmd == nil {
		return nil
	}
	var out []string
	for _, f := range buildHiddenFlags {
		if cmd.Flags().Lookup(f) != nil {
			out = append(out, f)
		}
	}
	return out
}

// insertFlags puts flags before the -- of argv.
func insertFlags(argv, flags []string) []string {
	for i, a := range argv {
		if a == "--" {
			out := append([]string{}, argv[:i]...)
			out = append(out, flags...)
			return append(out, argv[i:]...)
		}
	}
	return append(argv, flags...)
}

// NewRunID gives a new build run id r-YYYYMMDD-HHMM-xxx.
func NewRunID(now time.Time) string {
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := []byte(randomHex(3))
	suffix := make([]byte, 3)
	for i := range suffix {
		suffix[i] = chars[int(b[i])%len(chars)]
	}
	return "r-" + now.UTC().Format("20060102-1504") + "-" + string(suffix)
}

// findRun finds the run that the process pid started: a journal of the set,
// changed since the start, whose run.started names the pid.
func (s *Server) findRun(pid int, started time.Time) string {
	dir := s.opts.Paths.Runs(s.opts.SetName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".progress.jsonl") {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.ModTime().Before(started.Add(-2*time.Second)) {
			continue
		}
		var first struct {
			Run    string `json:"run"`
			Type   string `json:"type"`
			Origin struct {
				PID int `json:"pid"`
			} `json:"origin"`
		}
		line := firstFileLine(filepath.Join(dir, name))
		if json.Unmarshal(line, &first) == nil && first.Type == "run.started" && first.Origin.PID == pid {
			return first.Run
		}
	}
	return ""
}

func firstFileLine(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	r := bufio.NewReader(f)
	line, _ := r.ReadBytes('\n')
	return line
}

// journalHas reports whether a journal has a line of one of the types.
func journalHas(path string, types ...string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var l struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		for _, t := range types {
			if l.Type == t {
				return true
			}
		}
	}
	return false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
