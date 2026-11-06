package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/testkit"
)

func TestParseMode(t *testing.T) {
	c := MustLoad()
	all, err := ParseMode("all", c)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Names()) != len(c.Tools) || len(c.Tools) != 30 {
		t.Fatalf("all: %d tools of %d, want 30", len(all.Names()), len(c.Tools))
	}
	read, err := ParseMode("read", c)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(read.Names()); got != 25 {
		t.Fatalf("read: %d tools, want 25", got)
	}
	for _, tl := range c.Tools {
		if read.Allows(tl.Name) != (tl.Class == ClassRead) {
			t.Errorf("read mode and class of %s (%s) disagree", tl.Name, tl.Class)
		}
	}
	if read.Allows("repoctl_build") || !all.Allows("repoctl_build") {
		t.Error("repoctl_build: want only in mode all")
	}
	some, err := ParseMode("repoctl_status, repoctl_build", c)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(some.Names(), []string{"repoctl_build", "repoctl_status"}) {
		t.Errorf("names: %v", some.Names())
	}
	for _, bad := range []string{"repoctl_publish", "status", ","} {
		if _, err := ParseMode(bad, c); err == nil {
			t.Errorf("ParseMode(%q): want an error", bad)
		}
	}
}

func TestCatalogJSON(t *testing.T) {
	c := MustLoad()
	all, _ := ParseMode("all", c)
	if !bytes.Equal(c.JSON(all), api.SchemaFile("mcp-tools.json")) {
		t.Error("mode all: want mcp-tools.json as is")
	}
	read, _ := ParseMode("read", c)
	var doc struct {
		Tools []struct {
			Name  string `json:"name"`
			Class string `json:"class"`
		} `json:"tools"`
		Resources  []any    `json:"resources"`
		NotExposed []string `json:"notExposed"`
	}
	if err := json.Unmarshal(c.JSON(read), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Tools) != 25 || len(doc.Resources) == 0 || len(doc.NotExposed) == 0 {
		t.Fatalf("read: %d tools, %d resources, %d notExposed", len(doc.Tools), len(doc.Resources), len(doc.NotExposed))
	}
	for _, tl := range doc.Tools {
		if tl.Class != ClassRead {
			t.Errorf("read: tool %s has class %s", tl.Name, tl.Class)
		}
	}
}

func TestNotExposedAgreesWithCatalog(t *testing.T) {
	c := MustLoad()
	n := 0
	for _, e := range c.NotExposed {
		if strings.Contains(e, "foreign") {
			continue // repoctl_run_cancel checks origin.session
		}
		words := strings.Fields(strings.TrimSuffix(e, " *"))
		last := words[len(words)-1]
		for _, alt := range strings.Split(last, "|") {
			path := strings.Join(append(append([]string{}, words[:len(words)-1]...), alt), " ")
			n++
			if !IsNotExposed(path) {
				t.Errorf("notExposed %q of mcp-tools.json is not refused", path)
			}
		}
	}
	if n < 20 {
		t.Errorf("only %d paths in notExposed", n)
	}
}

func TestArgv(t *testing.T) {
	cases := []struct {
		tool string
		in   map[string]any
		want []string
	}{
		{"repoctl_status", nil, []string{"status"}},
		{"repoctl_build", map[string]any{"packages": []any{"ucx"}, "targets": []any{"el9", "el10"}, "failFast": true, "withDeps": false, "jobs": json.Number("3")},
			[]string{"build", "--fail-fast", "--jobs=3", "--target=el9", "--target=el10", "--lock-wait=0", "--", "ucx"}},
		{"repoctl_runs", nil, []string{"runs", "--limit=10"}},
		{"repoctl_runs", map[string]any{"limit": json.Number("3"), "state": "failed"}, []string{"runs", "--limit=3", "--state=failed"}},
		{"repoctl_run_get", map[string]any{"run": "last", "artifacts": true}, []string{"runs", "--", "latest"}},
		{"repoctl_logs", map[string]any{"node": "slurm@el10", "run": "r-20261102-1750-a3f", "tail": json.Number("5")},
			[]string{"logs", "-v", "--", "r-20261102-1750-a3f", "slurm@el10"}},
		{"repoctl_checkout", map[string]any{"packages": []any{"ucx"}, "fromSet": "slurm-25.11"}, []string{"checkout", "--from-set=slurm-25.11", "--lock-wait=0", "--", "ucx"}},
		{"repoctl_check", map[string]any{"codes": []any{"soname.removed"}}, []string{"check", "--code=soname.removed"}},
		{"repoctl_repo_ls", map[string]any{"kind": "source"}, []string{"repo", "ls", "--repo=source"}},
		{"repoctl_rebuild", map[string]any{"packages": []any{"slurm"}, "reason": "ucx soname -bump"}, []string{"rebuild", "--reason=ucx soname -bump", "--lock-wait=0", "--", "slurm"}},
		{"repoctl_config_show", map[string]any{"key": "sign"}, []string{"config", "show"}},
	}
	for _, c := range cases {
		got, err := Argv(c.tool, c.in)
		if err != nil {
			t.Errorf("%s: %v", c.tool, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: argv %q, want %q", c.tool, got, c.want)
		}
	}
}

func TestArgvRefusesFlagsInValues(t *testing.T) {
	bad := []struct {
		tool string
		in   map[string]any
	}{
		{"repoctl_build", map[string]any{"packages": []any{"--yes"}}},
		{"repoctl_build", map[string]any{"targets": []any{"-x"}}},
		{"repoctl_run_get", map[string]any{"run": "--force"}},
		{"repoctl_logs", map[string]any{"node": "slurm"}},
		{"repoctl_advisory", map[string]any{"id": "-rf"}},
		{"repoctl_plan", map[string]any{"from": "--orphan"}},
		{"repoctl_upstream", map[string]any{"packages": []any{"a\nb"}}},
		{"repoctl_repo_ls", map[string]any{"name": "-x*"}},
		{"repoctl_build", map[string]any{"packages": []any{1}}},
	}
	for _, b := range bad {
		if argv, err := Argv(b.tool, b.in); err == nil {
			t.Errorf("%s %v: argv %q, want an input error", b.tool, b.in, argv)
		}
	}
}

func TestCheckCommand(t *testing.T) {
	for _, argv := range [][]string{
		{"publish"}, {"repo", "push"}, {"repo", "restore", "--", "x"}, {"sign", "add"}, {"advisories", "dismiss"},
		{"mcp", "start"}, {"skills", "install"}, {"clean"}, {"release"},
		{"checkout", "--force", "--", "ucx"}, {"build", "--yes"}, {"pin", "--older"}, {"check", "--install-timer"},
		{"unknown"},
	} {
		if err := CheckCommand(nil, argv); err == nil {
			t.Errorf("CheckCommand(%q): want a refusal", argv)
		} else if e, ok := api.AsError(err); !ok || e.Code != api.PMCPToolNotAllowed {
			t.Errorf("CheckCommand(%q): %v, want mcp.tool-not-allowed", argv, err)
		}
	}
	for _, argv := range [][]string{{"status"}, {"repo", "ls", "--repo=source"}, {"build", "--force", "--lock-wait=0"}, {"cancel", "--force", "--", "r-20261102-1750-a3f"}} {
		if err := CheckCommand(nil, argv); err != nil {
			t.Errorf("CheckCommand(%q): %v", argv, err)
		}
	}
}

func TestToolResultNeedsHuman(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(testkit.ModuleRoot(), "docs/design/contract/examples/mcp-needs-human.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		Result struct {
			StructuredContent json.RawMessage `json:"structuredContent"`
			IsError           bool            `json:"isError"`
			Meta              map[string]any  `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &ex); err != nil {
		t.Fatal(err)
	}
	r := toolResult(ex.Result.StructuredContent, 5)
	if !r.IsError || r.Meta[MetaExitCode] != 5 {
		t.Errorf("isError %v, meta %v", r.IsError, r.Meta)
	}
	text := r.Content[0].(*sdk.TextContent).Text
	head, env, _ := strings.Cut(text, "\n")
	if !strings.HasPrefix(head, "NEEDS HUMAN: ") || !strings.Contains(head, "git -C packages/slurm status") {
		t.Errorf("summary %q", head)
	}
	if strings.Count(head, ". ") > 3 {
		t.Errorf("summary has more than 3 sentences: %q", head)
	}
	if !json.Valid([]byte(env)) {
		t.Error("the text has no envelope after the summary")
	}
	sc, _ := json.Marshal(r.StructuredContent)
	if err := testkit.Validate("Envelope", sc); err != nil {
		t.Error(err)
	}
}

func TestToolResultTooLarge(t *testing.T) {
	items := make([]string, 0, 3000)
	for range 3000 {
		items = append(items, strings.Repeat("x", 100))
	}
	env := map[string]any{"apiVersion": api.APIVersion, "kind": "NodeLog", "metadata": map[string]any{"command": "logs"}, "items": items, "warnings": []any{}, "errors": []any{}}
	b, _ := json.Marshal(env)
	r := toolResult(b, 0)
	sc, _ := json.Marshal(r.StructuredContent)
	if !r.IsError || !bytes.Contains(sc, []byte(PResultTooLarge)) {
		t.Errorf("want %s, got %s", PResultTooLarge, sc)
	}
}

func TestSummary(t *testing.T) {
	e := &envelope{Kind: "Package", Metadata: json.RawMessage(`{"command":"show","set":"slurm-26.05"}`), Items: []json.RawMessage{[]byte(`{}`), []byte(`{}`)},
		Warnings: []api.Problem{{Code: "lock.drift"}, {Code: "lock.drift"}}}
	if got, want := summarize(e, 0), "repoctl show in set slurm-26.05: 2 Package items. 2 warnings: lock.drift."; got != want {
		t.Errorf("summary %q, want %q", got, want)
	}
	e = &envelope{Kind: "Run", Errors: []api.Problem{{Code: "set.busy", Message: "Run r-1 holds the set lock", Hint: "Wait for the run"}}}
	if got, want := summarize(e, 7), "set.busy: Run r-1 holds the set lock. Wait for the run."; got != want {
		t.Errorf("summary %q, want %q", got, want)
	}
}

func TestRunLine(t *testing.T) {
	run := &api.Run{Kind: "build", Plan: &api.RunPlan{Build: 8}, Counts: api.StateCounts{Ok: 2, Failed: 1, Blocked: 1, Running: 2}}
	done, total, line := runLine(run)
	if done != 4 || total != 8 || line != "build: 4/8 done, 1 failed, 1 blocked" {
		t.Errorf("runLine: %d %d %q", done, total, line)
	}
	var sent []string
	rp := &runProgress{send: func(_ context.Context, p, tot float64, msg string) error {
		sent = append(sent, msg)
		return nil
	}}
	rp.update(context.Background(), run)
	rp.update(context.Background(), run)
	run.Counts.Ok = 3
	rp.update(context.Background(), run)
	if len(sent) != 2 {
		t.Errorf("sent %q, want 2 notifications", sent)
	}
}

func TestFilterLog(t *testing.T) {
	lines := []any{}
	for i := 1; i <= 20; i++ {
		l := "line"
		if i == 10 || i == 18 {
			l = "error: boom"
		}
		lines = append(lines, l)
	}
	item := map[string]any{"lines": append([]any{}, lines...), "firstLine": json.Number("1")}
	filterLog(item, 5, nil)
	if got := item["lines"].([]string); len(got) != 5 || item["firstLine"] != 16 || item["truncated"] != true {
		t.Errorf("tail: %v first %v truncated %v", got, item["firstLine"], item["truncated"])
	}
	item = map[string]any{"lines": append([]any{}, lines...), "firstLine": json.Number("1")}
	filterLog(item, 120, regexp.MustCompile(`^error`))
	got := strings.Join(item["lines"].([]string), "|")
	want := "7-line|8-line|9-line|10:error: boom|11-line|12-line|13-line|--|15-line|16-line|17-line|18:error: boom|19-line|20-line"
	if got != want {
		t.Errorf("grep:\n%s\nwant\n%s", got, want)
	}
	long := strings.Repeat("é", 1500)
	if c := cutLine(long); len(c) > maxLineLen || !strings.HasPrefix(long, c) {
		t.Errorf("cutLine: %d bytes", len(c))
	}
}

func TestPostConfigKey(t *testing.T) {
	c := MustLoad()
	tl, _ := c.Tool("repoctl_config_show")
	env := `{"apiVersion":"repoctl/v1alpha1","kind":"Config","metadata":{},"items":[{"key":"jobs","value":2,"source":"default"},{"key":"sign.key","value":"x","source":"user"}],"warnings":[],"errors":[]}`
	s := &Server{}
	out := s.post(tl, map[string]any{"key": "sign"}, []byte(env))
	var e struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(out, &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Items) != 1 || e.Items[0]["key"] != "sign.key" {
		t.Errorf("items %v", e.Items)
	}
}

func TestFollowerSendsProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	log := progress.NewLog(f, progress.LogOptions{FlushEvery: -1, AdvanceEvery: -1, Program: "repoctl"})
	bus := progress.NewBus(progress.BusOptions{Sinks: []progress.Sink{log}, Program: "repoctl"})
	ctx := progress.WithBus(context.Background(), bus)
	sctx, step := progress.Start(ctx, progress.KindStep, "build", progress.Flagged(progress.FlagFold), progress.Total(2))
	for _, n := range []string{"ucx@el9", "ucx@el10"} {
		_, sp := progress.Start(sctx, progress.KindTarget, n, progress.Node(n))
		sp.End(nil)
	}
	step.End(nil)
	bus.Close()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	var mu sync.Mutex
	var msgs []string
	fl := follow(path, func(_ context.Context, p, total float64, msg string) error {
		mu.Lock()
		defer mu.Unlock()
		msgs = append(msgs, msg)
		return nil
	})
	fl.close()
	fl.close()
	mu.Lock()
	defer mu.Unlock()
	if len(msgs) == 0 || msgs[len(msgs)-1] != "build: 2/2 done" {
		t.Fatalf("notifications %q, want the last build: 2/2 done", msgs)
	}
}

func TestChildEnviron(t *testing.T) {
	env := ChildEnviron([]string{"PATH=/bin", "REPOCTL_DIR=/x", "REPOCTL_JSON=1", "REPOCTL_SIGN_KEY=k", "REPOCTL_PROGRESS=tree", "TRACEPARENT=00-x"}, "TRACEPARENT=00-y")
	want := []string{"PATH=/bin", "REPOCTL_SIGN_KEY=k", "REPOCTL_PROGRESS=none", "TRACEPARENT=00-y"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env %q, want %q", env, want)
	}
	get := ChildGetenv(func(k string) string { return "v-" + k })
	if get("REPOCTL_TARGET") != "" || get("REPOCTL_PROGRESS") != "none" || get("REPOCTL_SIGN_KEY") != "v-REPOCTL_SIGN_KEY" || get("HOME") != "v-HOME" {
		t.Error("ChildGetenv does not follow the environment rule")
	}
	if id := NewSessionID(); len(id) != 16 {
		t.Errorf("session id %q", id)
	}
	if !reRun.MatchString(NewRunID(time.Date(2026, 11, 2, 17, 50, 0, 0, time.UTC))) {
		t.Error("NewRunID does not match the run id pattern")
	}
}

func TestHumanCommand(t *testing.T) {
	argv, err := Argv("repoctl_build", map[string]any{"packages": []any{"ucx"}, "targets": []any{"el10"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := HumanCommand(argv), "repoctl build --target el10 ucx"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
	if got := HumanCommand([]string{"rebuild", "--reason=a b", "--", "x"}); got != "repoctl rebuild --reason 'a b' x" {
		t.Errorf("%q", got)
	}
}
