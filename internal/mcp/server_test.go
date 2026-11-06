package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/cli"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/mcp"
	"github.com/dennisklein/repoctl/internal/skills"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// knownGaps are input properties whose flag the command does not have yet.
// The server refuses a call that uses one, from the check of the command
// tree.
var knownGaps = map[string]string{
	"repoctl_repo_ls.snapshot": "repo ls has no --snapshot",
}

var toolClass = map[string]string{mcp.ClassRead: "read", mcp.ClassStaging: "staging", mcp.ClassLocalWrite: "local write"}

func lookupFlag(cmd *cobra.Command, name string) *pflag.Flag {
	if f := cmd.Flags().Lookup(name); f != nil {
		return f
	}
	if f := cmd.InheritedFlags().Lookup(name); f != nil {
		return f
	}
	return cmd.Root().PersistentFlags().Lookup(name)
}

// TestMappingsAgainstCommandTree checks each tool against the CLI: its
// command exists with the same class and kind, and each input property is
// a flag of the command, a positional argument or one that the server
// applies.
func TestMappingsAgainstCommandTree(t *testing.T) {
	tree := cli.CommandTree()
	cat := mcp.MustLoad()
	for name := range mcp.Mappings {
		if _, ok := cat.Tool(name); !ok {
			t.Errorf("mapping of unknown tool %s", name)
		}
	}
	for _, tl := range cat.Tools {
		m, ok := mcp.Mappings[tl.Name]
		if !ok {
			t.Errorf("%s: no mapping", tl.Name)
			continue
		}
		cmd, rest, err := tree.Find(m.Command)
		if err != nil || len(rest) > 0 || cmd.CommandPath() != "repoctl "+strings.Join(m.Command, " ") {
			t.Errorf("%s: command %q is not in the tree", tl.Name, m.Command)
			continue
		}
		if got := cmd.Annotations["class"]; got != toolClass[tl.Class] {
			t.Errorf("%s: class %s, command %s has class %s", tl.Name, tl.Class, cmd.CommandPath(), got)
		}
		if tl.OutputKind != "" && cmd.Annotations["kind"] != tl.OutputKind {
			t.Errorf("%s: outputKind %s, command kind %s", tl.Name, tl.OutputKind, cmd.Annotations["kind"])
		}
		var schema struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tl.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		for prop, ps := range schema.Properties {
			key := tl.Name + "." + prop
			switch {
			case m.Flags[prop] != "":
				f := lookupFlag(cmd, m.Flags[prop])
				if f == nil {
					if knownGaps[key] == "" {
						t.Errorf("%s: property %s maps to --%s, which %s does not have", tl.Name, prop, m.Flags[prop], cmd.CommandPath())
					}
					continue
				}
				if (ps.Type == "boolean") != (f.Value.Type() == "bool") {
					t.Errorf("%s: property %s is %s, flag --%s is %s", tl.Name, prop, ps.Type, f.Name, f.Value.Type())
				}
			case slices.Contains(m.Args, prop):
				if cmd.Args != nil && cmd.Args(cmd, []string{"x"}) != nil {
					t.Errorf("%s: property %s is an argument, but %s takes none", tl.Name, prop, cmd.CommandPath())
				}
			case m.Server[prop] != "":
			default:
				t.Errorf("%s: property %s has no flag, argument or server rule", tl.Name, prop)
			}
		}
		for _, n := range []string{"jobs", "lock-wait"} {
			if (n == "lock-wait" && m.LockWait) && lookupFlag(cmd, n) == nil {
				t.Errorf("%s: %s has no --%s", tl.Name, cmd.CommandPath(), n)
			}
		}
		if m.Verbose && lookupFlag(cmd, "verbose") == nil {
			t.Errorf("%s: no -v", tl.Name)
		}
		argv, err := mcp.Argv(tl.Name, nil)
		if err != nil {
			t.Errorf("%s: %v", tl.Name, err)
			continue
		}
		if err := mcp.CheckCommand(tree, argv); err != nil {
			t.Errorf("%s: %v", tl.Name, err)
		}
	}
}

// TestNoSharedStateTool checks the shared-state commands of the tree: no
// tool runs one, and CheckCommand refuses each.
func TestNoSharedStateTool(t *testing.T) {
	tree := cli.CommandTree()
	var walk func(c *cobra.Command)
	n := 0
	walk = func(c *cobra.Command) {
		if c.Annotations["class"] == "shared state" && c.Runnable() {
			n++
			path := strings.Fields(strings.TrimPrefix(c.CommandPath(), "repoctl "))
			if err := mcp.CheckCommand(tree, path); err == nil {
				t.Errorf("CheckCommand accepts the shared-state command %s", c.CommandPath())
			}
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(tree)
	if n == 0 {
		t.Fatal("the tree has no shared-state command")
	}
}

type harness struct {
	cs    *sdk.ClientSession
	srv   *mcp.Server
	home  string
	mu    sync.Mutex
	notes []string
}

func (h *harness) progress() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.notes...)
}

// progressN waits up to 2 s for n progress notifications: the client gets
// them asynchronously, so the last one can arrive after the call result.
func (h *harness) progressN(n int) []string {
	for range 200 {
		if notes := h.progress(); len(notes) >= n {
			return notes
		}
		time.Sleep(10 * time.Millisecond)
	}
	return h.progress()
}

// start runs a server with run on an in-memory transport and connects a
// client. run nil runs the CLI in the process.
func start(t *testing.T, mode string, run mcp.Runner, opts ...func(o *mcp.Options, home string)) *harness {
	t.Helper()
	home := t.TempDir()
	env := map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_STATE_HOME": filepath.Join(home, ".local", "state"),
		"XDG_DATA_HOME": filepath.Join(home, ".local", "share"), "XDG_CACHE_HOME": filepath.Join(home, ".cache"),
		"XDG_RUNTIME_DIR": filepath.Join(home, "run"), "PATH": os.Getenv("PATH"),
	}
	getenv := func(k string) string { return env[k] }
	if run == nil {
		run = cli.MCPRunner(getenv, time.Now)
	}
	cat := mcp.MustLoad()
	m, err := mcp.ParseMode(mode, cat)
	if err != nil {
		t.Fatal(err)
	}
	tree := cli.CommandTree()
	o := mcp.Options{
		Catalog: cat, Mode: m, SetDir: home, SetName: "slurm-26.05", Run: run,
		Tree:  func() *cobra.Command { return tree },
		Paths: config.NewPaths(getenv),
		Skill: func(name string) ([]byte, bool) {
			s, ok := skills.Lookup(name, "0.4.0")
			return s.Render(), ok
		},
		Version: "0.4.0", Poll: 10 * time.Millisecond,
	}
	for _, f := range opts {
		f(&o, home)
	}
	srv, err := mcp.New(o)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{srv: srv, home: home}
	ct, st := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, st) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "claude-code", Version: "1"}, &sdk.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *sdk.ProgressNotificationClientRequest) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.notes = append(h.notes, req.Params.Message)
		},
	})
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.cs = cs
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server: %v", err)
		}
	})
	return h
}

// call calls a tool and gives the result and its envelope, which it
// validates against the output schema.
func (h *harness) call(t *testing.T, name string, args map[string]any, token any) (*sdk.CallToolResult, envelope) {
	t.Helper()
	p := &sdk.CallToolParams{Name: name, Arguments: args}
	if token != nil {
		p.SetProgressToken(token)
	}
	r, err := h.cs.CallTool(context.Background(), p)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	b, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := testkit.Validate("Envelope", b); err != nil {
		t.Fatalf("%s: structuredContent: %v\n%s", name, err, b)
	}
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	text := r.Content[0].(*sdk.TextContent).Text
	head, rest, _ := strings.Cut(text, "\n")
	if head == "" || !json.Valid([]byte(rest)) {
		t.Errorf("%s: text is not a summary and the envelope: %q", name, text)
	}
	if r.IsError != (len(e.Errors) > 0) {
		t.Errorf("%s: isError %v with %d errors", name, r.IsError, len(e.Errors))
	}
	return r, e
}

type envelope struct {
	Kind     string            `json:"kind"`
	Metadata map[string]any    `json:"metadata"`
	Items    []json.RawMessage `json:"items"`
	Errors   []api.Problem     `json:"errors"`
}

func TestRoundTripReadMode(t *testing.T) {
	h := start(t, "read", nil)
	ctx := context.Background()

	tools, err := h.cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 25 {
		t.Errorf("tools/list: %d tools, want 25", len(tools.Tools))
	}
	for _, tl := range tools.Tools {
		if tl.Meta["repoctl/class"] != mcp.ClassRead || tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("tool %s: class %v", tl.Name, tl.Meta["repoctl/class"])
		}
		if tl.Name == "repoctl_build" {
			t.Error("repoctl_build in mode read")
		}
	}

	r, e := h.call(t, "repoctl_config_show", map[string]any{"key": "jobs"}, nil)
	if r.IsError || e.Kind != string(api.KindConfig) || len(e.Items) == 0 {
		t.Fatalf("config show: isError %v kind %s items %d errors %v", r.IsError, e.Kind, len(e.Items), e.Errors)
	}
	for _, it := range e.Items {
		var c api.Config
		_ = json.Unmarshal(it, &c)
		if !strings.HasPrefix(c.Key, "jobs") {
			t.Errorf("config show key jobs: item %s", c.Key)
		}
	}
	if r.Meta[mcp.MetaExitCode] != float64(0) {
		t.Errorf("exit code %v", r.Meta[mcp.MetaExitCode])
	}

	r, e = h.call(t, "repoctl_build", map[string]any{"packages": []any{"ucx"}, "targets": []any{"el10"}}, nil)
	if !r.IsError || len(e.Errors) != 1 || e.Errors[0].Code != api.PMCPToolNotAllowed {
		t.Fatalf("build in mode read: %v", e.Errors)
	}
	if ha := e.Errors[0].HumanAction; ha == nil || ha.Command != "repoctl build --target el10 ucx" {
		t.Errorf("humanAction %+v", e.Errors[0].HumanAction)
	}
	if text := r.Content[0].(*sdk.TextContent).Text; !strings.HasPrefix(text, "NEEDS HUMAN: ") {
		t.Errorf("text %q", text)
	}

	_, e = h.call(t, "repoctl_config_show", map[string]any{"key": 5}, nil)
	if len(e.Errors) != 1 || e.Errors[0].Code != mcp.PInvalidInput {
		t.Errorf("invalid input: %v", e.Errors)
	}
	_, e = h.call(t, "repoctl_runs", map[string]any{"state": "bogus"}, nil)
	if len(e.Errors) != 1 || e.Errors[0].Code != mcp.PInvalidInput {
		t.Errorf("invalid enum: %v", e.Errors)
	}

	// A command of the set gives its envelope, whatever its state.
	h.call(t, "repoctl_status", nil, nil)

	sr, err := h.cs.CallTool(ctx, &sdk.CallToolParams{Name: "repoctl_schema", Arguments: map[string]any{"name": "output"}})
	if err != nil || sr.IsError {
		t.Fatalf("schema: %v %v", err, sr)
	}
	if m, ok := sr.StructuredContent.(map[string]any); !ok || m["$id"] != api.OutputSchemaID {
		t.Errorf("schema output: no $id")
	}
}

func TestResources(t *testing.T) {
	h := start(t, "read", nil)
	ctx := context.Background()
	rl, err := h.cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tl, err := h.cs.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rl.Resources) != 4 || len(tl.ResourceTemplates) != 4 {
		t.Errorf("%d resources, %d templates", len(rl.Resources), len(tl.ResourceTemplates))
	}
	read := func(uri string) string {
		r, err := h.cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		return r.Contents[0].Text
	}
	if got := read("repoctl://schema/output"); !strings.Contains(got, api.OutputSchemaID) {
		t.Error("schema/output: no $id")
	}
	if got := read("repoctl://skills/triage-build"); !strings.Contains(got, "name: triage-build") || !strings.Contains(got, "repoctl-version: 0.4.0") {
		t.Errorf("skills/triage-build:\n%s", got)
	}
	if err := testkit.Validate("Envelope", []byte(read("repoctl://status"))); err != nil {
		t.Errorf("status: %v", err)
	}
	run := "r-20261102-1750-a3f"
	journal := filepath.Join(h.home, ".local", "state", "repoctl", "slurm-26.05", "runs", run+".jsonl")
	if err := os.MkdirAll(filepath.Dir(journal), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte(`{"type":"run.started"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read("repoctl://runs/" + run + "/journal"); !strings.Contains(got, "run.started") {
		t.Errorf("journal %q", got)
	}
	for _, bad := range []string{"repoctl://runs/../../etc/passwd/journal", "repoctl://skills/nope", "repoctl://runs/r-20261102-1750-zzz/logs/x@el9"} {
		if _, err := h.cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: bad}); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

// fakeRuns answers repoctl runs RUN with the runs of states, one per call,
// and records the other command lines.
type fakeRuns struct {
	mu     sync.Mutex
	states []api.RunState
	origin api.RunOriginItem
	calls  [][]string
}

func (f *fakeRuns) Run(_ context.Context, args []string) ([]byte, []byte, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	path := commandOf(args)
	md := api.Metadata{Command: path, GeneratedAt: api.FormatTime(time.Now()), Set: "slurm-26.05"}
	if path != "runs" {
		env := api.NewEnvelope(api.KindRun, md)
		b, _ := json.Marshal(env)
		return b, nil, 0
	}
	st := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	run := api.Run{
		ID: "r-20261102-1750-a3f", Kind: "build", Set: "slurm-26.05", State: st, Origin: f.origin,
		StartedAt: "2026-11-02T17:50:04.112Z", Plan: &api.RunPlan{Build: 4},
		Files: api.RunFiles{Journal: "/x/runs/r-20261102-1750-a3f.jsonl"},
		Nodes: []api.NodeResult{{
			Node: "ucx@el9", Package: "ucx", Target: "el9", State: "ok", Commit: strings.Repeat("a", 40),
			Artifacts: []api.Artifact{{File: "ucx-1.rpm", Kind: "rpm", Path: "/x/ucx-1.rpm", Sha256: strings.Repeat("b", 64), SizeBytes: 1}},
		}},
	}
	switch st {
	case api.RunStateRunning:
		run.Counts = api.StateCounts{Ok: 1, Running: 3}
	case api.RunStateOk:
		run.Counts = api.StateCounts{Ok: 4}
	case api.RunStateFailed:
		run.Counts, run.ExitCode = api.StateCounts{Ok: 2, Failed: 1, Blocked: 1}, 1
	default:
		run.Counts = api.StateCounts{Ok: 2, Running: 2}
	}
	env := api.NewEnvelope(api.KindRun, md)
	env.Items = []any{run}
	b, _ := json.Marshal(env)
	return b, nil, 0
}

func commandOf(args []string) string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C":
			i++
		case strings.HasPrefix(a, "-"):
			if len(out) > 0 {
				return strings.Join(out, " ")
			}
		default:
			out = append(out, a)
		}
	}
	return strings.Join(out, " ")
}

func TestRunWait(t *testing.T) {
	f := &fakeRuns{states: []api.RunState{api.RunStateRunning, api.RunStateRunning, api.RunStateFailed}, origin: api.RunOriginItem{Via: api.RunOriginCli, User: "dk", Host: "h"}}
	h := start(t, "read", f)
	r, e := h.call(t, "repoctl_run_wait", map[string]any{"run": "r-20261102-1750-a3f", "timeoutSeconds": 30}, "w-9")
	if r.Meta[mcp.MetaExitCode] != float64(1) || len(e.Items) != 1 {
		t.Fatalf("exit %v items %d", r.Meta[mcp.MetaExitCode], len(e.Items))
	}
	if strings.Contains(string(e.Items[0]), "artifacts") {
		t.Error("artifacts without artifacts: true")
	}
	notes := h.progressN(2)
	if len(notes) != 2 || notes[0] != "build: 1/4 done" || notes[1] != "build: 4/4 done, 1 failed, 1 blocked" {
		t.Errorf("progress %q", notes)
	}

	f2 := &fakeRuns{states: []api.RunState{api.RunStateAbandoned}, origin: api.RunOriginItem{Via: api.RunOriginCli, User: "dk", Host: "h"}}
	h2 := start(t, "read", f2)
	_, e = h2.call(t, "repoctl_run_get", map[string]any{"run": "last", "artifacts": true}, nil)
	if !strings.Contains(string(e.Items[0]), "artifacts") {
		t.Error("artifacts: true left out the artifacts")
	}
	_, e = h2.call(t, "repoctl_run_wait", map[string]any{"run": "last"}, nil)
	if len(e.Errors) != 1 || e.Errors[0].Code != api.PRunAbandoned {
		t.Errorf("abandoned: %v", e.Errors)
	}
}

func TestRunCancel(t *testing.T) {
	f := &fakeRuns{states: []api.RunState{api.RunStateRunning}, origin: api.RunOriginItem{Via: api.RunOriginCli, User: "dk", Host: "hpc-a1"}}
	h := start(t, "all", f)
	r, e := h.call(t, "repoctl_run_cancel", map[string]any{"run": "r-20261102-1750-a3f"}, nil)
	if !r.IsError || len(e.Errors) != 1 || e.Errors[0].Code != api.PCancelForeignRun || e.Errors[0].HumanAction == nil ||
		e.Errors[0].HumanAction.Command != "repoctl cancel r-20261102-1750-a3f" {
		t.Fatalf("foreign run: %+v", e.Errors)
	}
	for _, c := range f.calls {
		if commandOf(c) == "cancel" {
			t.Fatal("the server ran repoctl cancel for a foreign run")
		}
	}

	f.origin = api.RunOriginItem{Via: api.RunOriginMCP, User: "dk", Host: "hpc-a1", Session: h.srv.SessionID()}
	r, _ = h.call(t, "repoctl_run_cancel", map[string]any{"run": "r-20261102-1750-a3f", "force": true}, nil)
	if r.IsError {
		t.Fatal("own run: error")
	}
	last := f.calls[len(f.calls)-1]
	if commandOf(last) != "cancel" || !slices.Contains(last, "--force") || last[len(last)-1] != "r-20261102-1750-a3f" {
		t.Errorf("cancel argv %q", last)
	}
}

// fakeBuild is a repoctl binary for repoctl_build: it writes the journal of
// a run that names its pid, records its arguments and environment, and
// stays a moment.
const fakeBuild = `#!/bin/sh
dir="$XDG_STATE_HOME/repoctl/slurm-26.05/runs"
mkdir -p "$dir"
echo "$@" > "$dir/../argv"
env > "$dir/../env"
printf '{"type":"run.started","run":"r-20261102-1750-a3f","origin":{"pid":%d}}\n{"type":"plan.ready"}\n' $$ > "$dir/r-20261102-1750-a3f.jsonl"
sleep 1
`

// busyBuild is a repoctl binary whose build ends at once with set.busy.
const busyBuild = `#!/bin/sh
echo '{"apiVersion":"repoctl/v1alpha1","kind":"Run","metadata":{"command":"build","generatedAt":"2026-11-02T17:52:30.512Z","repoctl":{"version":"0.4.0"}},"items":[],"warnings":[],"errors":[{"code":"set.busy","message":"Run r-20261102-1750-a3f holds the set lock.","exitCode":7}]}'
exit 7
`

func withBinary(t *testing.T, script string) func(o *mcp.Options, home string) {
	return func(o *mcp.Options, home string) {
		exe := filepath.Join(t.TempDir(), "repoctl")
		if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		o.Executable = exe
		o.Environ = []string{"PATH=" + os.Getenv("PATH"), "XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"), "REPOCTL_JSON=1", "REPOCTL_TARGET=el9", "REPOCTL_SIGN_KEY=k"}
		o.Jobs = func() int { return 3 }
	}
}

func TestBuild(t *testing.T) {
	f := &fakeRuns{states: []api.RunState{api.RunStateRunning}, origin: api.RunOriginItem{Via: api.RunOriginMCP, User: "dk", Host: "h"}}
	h := start(t, "all", f, withBinary(t, fakeBuild))
	r, e := h.call(t, "repoctl_build", map[string]any{"packages": []any{"ucx"}, "targets": []any{"el10"}}, nil)
	if r.IsError || len(e.Items) != 1 || !strings.Contains(string(e.Items[0]), "r-20261102-1750-a3f") {
		t.Fatalf("build: %v %s", e.Errors, e.Items)
	}
	state := filepath.Join(h.home, ".local", "state", "repoctl", "slurm-26.05")
	argv, _ := os.ReadFile(filepath.Join(state, "argv"))
	for _, want := range []string{"-C " + h.home, "--json", "--progress=none", "build", "--jobs=3", "--target=el10", "--lock-wait=0", "-- ucx"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv %q has no %q", argv, want)
		}
	}
	env, _ := os.ReadFile(filepath.Join(state, "env"))
	for _, l := range []string{"REPOCTL_JSON=", "REPOCTL_TARGET="} {
		if strings.Contains(string(env), l) {
			t.Errorf("the build child has %s", l)
		}
	}
	for _, l := range []string{"REPOCTL_PROGRESS=none", "REPOCTL_SIGN_KEY=k", "TRACEPARENT=00-"} {
		if !strings.Contains(string(env), l) {
			t.Errorf("the build child has no %s", l)
		}
	}
}

func TestBuildBusy(t *testing.T) {
	f := &fakeRuns{states: []api.RunState{api.RunStateRunning}}
	h := start(t, "all", f, withBinary(t, busyBuild))
	r, e := h.call(t, "repoctl_build", nil, nil)
	if !r.IsError || r.Meta[mcp.MetaExitCode] != float64(7) || len(e.Errors) != 1 || e.Errors[0].Code != "set.busy" {
		t.Fatalf("busy: exit %v errors %v", r.Meta[mcp.MetaExitCode], e.Errors)
	}
	if text := r.Content[0].(*sdk.TextContent).Text; !strings.HasPrefix(text, "set.busy: ") {
		t.Errorf("text %q", text)
	}
}
