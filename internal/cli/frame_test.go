package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/testkit"
)

// exec runs the command line in-process without a set.
func execCLI(t *testing.T, env map[string]string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	home := t.TempDir()
	a := &App{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string {
			if v, ok := env[k]; ok {
				return v
			}
			switch k {
			case "HOME":
				return home
			case "LC_ALL":
				return "C.UTF-8"
			}
			return ""
		},
		Now: func() time.Time { return time.Date(2026, 11, 2, 17, 50, 0, 0, time.UTC) },
	}
	a.dir = t.TempDir()
	code := a.Execute(context.Background(), append([]string{"-C", a.dir}, args...))
	return out.String(), errb.String(), code
}

func TestHelpTopics(t *testing.T) {
	for _, name := range topicNames {
		out, _, code := execCLI(t, nil, "help", name)
		if code != 0 || strings.TrimSpace(out) == "" {
			t.Errorf("help %s: exit %d, %q", name, code, out)
		}
	}
	out, _, _ := execCLI(t, nil, "help", "graph")
	if !strings.HasPrefix(out, "GRAPH") {
		t.Errorf("help graph prints the command help, not the topic:\n%s", out)
	}
	_, errOut, code := execCLI(t, nil, "help", "nope")
	if code != 2 || !strings.Contains(errOut, "repoctl: error:") {
		t.Errorf("unknown topic: exit %d, %q", code, errOut)
	}
}

func TestRootHelp(t *testing.T) {
	out, _, code := execCLI(t, nil, "--help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	workspace := strings.Index(out, "Workspace commands:")
	admin := strings.Index(out, "Admin commands:")
	topics := strings.Index(out, "Additional help topics:")
	flags := strings.Index(out, "Flags:")
	if workspace < 0 || admin < workspace || topics < admin || flags < topics {
		t.Fatalf("root help order: workspace %d, admin %d, topics %d, flags %d\n%s", workspace, admin, topics, flags, out)
	}
	if i, j := strings.Index(out, "  init "), strings.Index(out, "  checkout "); i < 0 || j < i {
		t.Fatalf("commands are sorted, not in the order of a work day")
	}
}

// TestSubcommandHelp checks that the help of a command with subcommands
// lists them, and that only the root help lists the help topics.
func TestSubcommandHelp(t *testing.T) {
	for cmd, want := range map[string][]string{
		"sign":   {"Available Commands:", "  deployed ", "  keyfile "},
		"repo":   {"Available Commands:", "  verify ", "  snapshots "},
		"config": {"Available Commands:", "  show ", "  paths "},
	} {
		out, _, code := execCLI(t, nil, cmd, "--help")
		if code != 0 {
			t.Fatalf("%s --help: exit %d", cmd, code)
		}
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("%s --help has no %q:\n%s", cmd, w, out)
			}
		}
		if strings.Contains(out, "Additional help topics:") {
			t.Errorf("%s --help lists the help topics", cmd)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"nosuchcommand"},
		{"build", "--nosuchflag"},
		{"version", "--target", "el9"},
		{"ui", "--json"},
		{"plan", "--progress", "fancy"},
		{"schema", "nosuchschema"},
	} {
		out, errOut, code := execCLI(t, nil, args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2\n%s", args, code, errOut)
		}
		if out != "" {
			t.Errorf("%v: a usage error writes no envelope: %q", args, out)
		}
		if !strings.HasPrefix(errOut, "repoctl: error: ") || strings.Count(errOut, "repoctl: error:") != 1 {
			t.Errorf("%v: stderr %q", args, errOut)
		}
	}
}

func TestVersionJSON(t *testing.T) {
	out, _, code := execCLI(t, nil, "version", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if err := testkit.Validate("Envelope", []byte(out)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, _, _ = execCLI(t, map[string]string{"REPOCTL_JSON": "1"}, "version")
	if !strings.HasPrefix(out, "{") {
		t.Fatalf("REPOCTL_JSON=1 gives %q", out)
	}
}

func TestErrorEnvelope(t *testing.T) {
	// No set: an error envelope with errors[0].code and exitCode.
	out, errOut, code := execCLI(t, nil, "config", "paths", "--json")
	if code != 0 {
		t.Fatalf("config paths outside a set: exit %d %s", code, errOut)
	}
	if err := testkit.Validate("Envelope", []byte(out)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, _, code = execCLI(t, nil, "sign", "ls", "--json")
	if code == 0 {
		t.Skip("sign ls is implemented")
	}
	var env struct {
		Errors []struct {
			Code     string `json:"code"`
			ExitCode int    `json:"exitCode"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if err := testkit.Validate("Envelope", []byte(out)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(env.Errors) != 1 || env.Errors[0].ExitCode != code {
		t.Fatalf("errors %+v, exit %d", env.Errors, code)
	}
}

func TestSchemaCommand(t *testing.T) {
	out, _, code := execCLI(t, nil, "schema", "--list")
	if code != 0 || !strings.Contains(out, "output\n") || !strings.Contains(out, "Plan\n") {
		t.Fatalf("schema --list: %d %q", code, out)
	}
	for _, name := range []string{"output", "events", "deployments", "Plan"} {
		out, _, code := execCLI(t, nil, "schema", name)
		var v map[string]any
		if code != 0 || json.Unmarshal([]byte(out), &v) != nil {
			t.Errorf("schema %s: exit %d, not JSON", name, code)
		}
	}
}

func TestConfigShow(t *testing.T) {
	out, _, code := execCLI(t, map[string]string{"REPOCTL_JOBS": "5"}, "config", "show", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if err := testkit.Validate("Envelope", []byte(out)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, `"key": "jobs"`) || !strings.Contains(out, `"env": "REPOCTL_JOBS"`) {
		t.Fatalf("jobs from REPOCTL_JOBS:\n%s", out)
	}
}

func TestCompletion(t *testing.T) {
	out, _, code := execCLI(t, nil, "completion", "bash")
	if code != 0 || !strings.Contains(out, "repoctl") {
		t.Fatalf("completion bash: %d", code)
	}
}
