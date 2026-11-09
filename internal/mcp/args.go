package mcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
)

// Mapping says how a tool call becomes a repoctl command line.
type Mapping struct {
	// Command is the command path, for example {"repo", "ls"}.
	Command []string
	// Args are the input properties that become positional arguments, in
	// this order. An array gives one argument per element.
	Args []string
	// Flags maps an input property to the flag of the command, without --.
	// A boolean gives the flag when true, an array one flag per element.
	Flags map[string]string
	// Server are the input properties that the server applies itself, with
	// what it does.
	Server map[string]string
	// Defaults are tool defaults that differ from the defaults of the CLI.
	Defaults map[string]any
	// LockWait passes --lock-wait 0: a busy set answers at once.
	LockWait bool
	// Verbose passes -v, for the whole log of repoctl logs.
	Verbose bool
}

// targetsFlag maps the input property targets to the global flag --target.
var targetsFlag = map[string]string{"targets": "target"}

func flags(extra map[string]string, withTargets bool) map[string]string {
	out := map[string]string{}
	if withTargets {
		for k, v := range targetsFlag {
			out[k] = v
		}
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// Mappings are the command lines of the tools of mcp-tools.json.
var Mappings = map[string]Mapping{
	"repoctl_status": {Command: []string{"status"}},
	"repoctl_doctor": {Command: []string{"doctor"}},
	// show takes one package; the server runs it once per package and
	// merges the envelopes.
	"repoctl_show":     {Command: []string{"show"}, Args: []string{"packages"}},
	"repoctl_graph":    {Command: []string{"graph"}, Flags: flags(nil, true)},
	"repoctl_plan":     {Command: []string{"plan"}, Args: []string{"packages"}, Flags: flags(map[string]string{"force": "force", "from": "from", "withDeps": "with-deps", "rdeps": "rdeps", "check": "check"}, true)},
	"repoctl_upstream": {Command: []string{"upstream"}, Args: []string{"packages"}, Flags: flags(map[string]string{"refresh": "refresh"}, false)},
	"repoctl_runs":     {Command: []string{"runs"}, Flags: flags(map[string]string{"limit": "limit", "state": "state", "kind": "kind"}, false), Defaults: map[string]any{"limit": 10}},
	"repoctl_run_get": {Command: []string{"runs"}, Args: []string{"run"},
		Server: map[string]string{"artifacts": "leaves out nodes[].artifacts unless true"}},
	"repoctl_run_wait": {Command: []string{"runs"}, Args: []string{"run"},
		Server: map[string]string{"timeoutSeconds": "polls the run until it ends or the time is up", "artifacts": "leaves out nodes[].artifacts unless true"}},
	"repoctl_logs": {Command: []string{"logs"}, Args: []string{"run", "node"}, Flags: flags(map[string]string{"step": "step", "file": "file"}, false),
		Server: map[string]string{"tail": "keeps the last lines of NodeLog.lines", "grep": "keeps the matching lines with 3 lines of context"}, Verbose: true},
	"repoctl_repo_ls":         {Command: []string{"repo", "ls"}, Flags: flags(map[string]string{"run": "run", "kind": "repo", "name": "name", "snapshot": "snapshot", "removed": "removed"}, true)},
	"repoctl_repo_diff":       {Command: []string{"repo", "diff"}, Flags: flags(map[string]string{"run": "run"}, true)},
	"repoctl_repo_verify":     {Command: []string{"repo", "verify"}, Flags: flags(map[string]string{"snapshot": "snapshot", "served": "served", "run": "run", "el": "el"}, true)},
	"repoctl_repo_provenance": {Command: []string{"repo", "provenance"}, Args: []string{"build"}, Flags: flags(map[string]string{"snapshot": "snapshot"}, false)},
	"repoctl_repo_sbom":       {Command: []string{"repo", "sbom"}, Flags: flags(map[string]string{"build": "build", "snapshot": "snapshot"}, true)},
	"repoctl_snapshots":       {Command: []string{"repo", "snapshots"}, Flags: flags(map[string]string{"limit": "limit", "all": "all", "history": "history"}, true), Defaults: map[string]any{"limit": 20}},
	"repoctl_advisories":      {Command: []string{"advisories", "ls"}, Flags: flags(map[string]string{"level": "level", "dismissed": "dismissed", "refresh": "refresh"}, true)},
	"repoctl_advisory":        {Command: []string{"advisories", "show"}, Args: []string{"id"}},
	"repoctl_check":           {Command: []string{"check"}, Flags: flags(map[string]string{"codes": "code"}, true)},
	"repoctl_sign_ls":         {Command: []string{"sign", "ls"}, Flags: flags(nil, true)},
	"repoctl_sign_onboard":    {Command: []string{"sign", "onboard"}, Args: []string{"id"}},
	"repoctl_sign_offboard":   {Command: []string{"sign", "offboard"}, Args: []string{"id"}, Flags: flags(nil, true)},
	"repoctl_sign_keyfile":    {Command: []string{"sign", "keyfile"}, Flags: flags(map[string]string{"tree": "tree", "checksum": "checksum"}, true)},
	"repoctl_config_show":     {Command: []string{"config", "show"}, Server: map[string]string{"key": "keeps the items whose key has this prefix"}},
	"repoctl_schema":          {Command: []string{"schema"}, Args: []string{"name"}},
	"repoctl_build": {Command: []string{"build"}, Args: []string{"packages"}, LockWait: true,
		Flags: flags(map[string]string{"force": "force", "jobs": "jobs", "from": "from", "failFast": "fail-fast", "withDeps": "with-deps", "rdeps": "rdeps"}, true)},
	"repoctl_run_cancel": {Command: []string{"cancel"}, Args: []string{"run"}, Flags: flags(map[string]string{"force": "force"}, false)},
	"repoctl_checkout": {Command: []string{"checkout"}, Args: []string{"packages"}, LockWait: true,
		Flags: flags(map[string]string{"create": "create", "from": "from", "fromSet": "from-set", "orphan": "orphan"}, false)},
	"repoctl_pin":     {Command: []string{"pin"}, Args: []string{"packages"}, LockWait: true},
	"repoctl_rebuild": {Command: []string{"rebuild"}, Args: []string{"packages"}, Flags: flags(map[string]string{"reason": "reason"}, false), LockWait: true},
}

// Patterns of the input values. A value that becomes an argument never
// starts with a dash, and the server never runs a shell.
var (
	rePackage  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]*$`)
	reTarget   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	reNode     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]*@[a-z0-9][a-z0-9._-]*$`)
	reRun      = regexp.MustCompile(`^[rp]-[0-9]{8}-[0-9]{4}-[0-9a-z]{3}$`)
	reAdvisory = regexp.MustCompile(`^[a-z][a-z0-9.-]*/[A-Za-z0-9][A-Za-z0-9_.+@-]*$`)
	reKeyID    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@+-]*$`)
	reBuild    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+@:~^-]*$`)
	reSnapshot = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)
	reRef      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./:+-]*$`)
	reSetName  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
	reCode     = regexp.MustCompile(`^[a-z][a-z0-9.-]*$`)
	reKey      = regexp.MustCompile(`^[a-z0-9_.]*$`)
	reGlob     = regexp.MustCompile(`^[^-\s][^\s]*$`)
)

// patternOf gives the pattern of a property of a tool, or nil for a value
// that the schema limits (enum, boolean, integer) or free text.
func patternOf(tool, prop string) (*regexp.Regexp, string) {
	switch prop {
	case "packages":
		return rePackage, "a package id"
	case "targets":
		return reTarget, "a target id"
	case "node":
		return reNode, "a node id PKG@TARGET"
	case "run":
		return reRun, "a run id such as r-20261102-1750-a3f, or last"
	case "id":
		if tool == "repoctl_advisory" {
			return reAdvisory, "an advisory id CODE/SUBJECT"
		}
		return reKeyID, "a key id"
	case "build":
		return reBuild, "an NVR or PKG@TARGET"
	case "snapshot":
		return reSnapshot, "a publish id or a digest"
	case "from":
		return reRef, "a branch or fedora[:BRANCH]"
	case "fromSet":
		return reSetName, "a set name"
	case "codes":
		return reCode, "an advisory code"
	case "key":
		return reKey, "a key prefix such as sign"
	case "name":
		if tool == "repoctl_repo_ls" {
			return reGlob, "a glob on the package name"
		}
	}
	return nil, ""
}

// InputError is an input value that the server refuses.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func inputErrorf(format string, args ...any) error {
	return &InputError{Msg: fmt.Sprintf(format, args...)}
}

// Argv gives the command line of a tool call, without the global flags of
// the server: the command path, the flags, then -- and the positional
// arguments. Input properties of Mapping.Server are not in it.
func Argv(tool string, input map[string]any) ([]string, error) {
	m, ok := Mappings[tool]
	if !ok {
		return nil, inputErrorf("tool %s has no command", tool)
	}
	in := map[string]any{}
	for k, v := range m.Defaults {
		in[k] = v
	}
	for k, v := range input {
		in[k] = v
	}
	argv := append([]string{}, m.Command...)
	keys := make([]string, 0, len(m.Flags))
	for k := range m.Flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, prop := range keys {
		v, ok := in[prop]
		if !ok || v == nil {
			continue
		}
		vals, isBool, err := values(tool, prop, v)
		if err != nil {
			return nil, err
		}
		flag := m.Flags[prop]
		if isBool {
			if vals[0] == "true" {
				argv = append(argv, "--"+flag)
			}
			continue
		}
		for _, x := range vals {
			argv = append(argv, "--"+flag+"="+x)
		}
	}
	if m.LockWait {
		argv = append(argv, "--lock-wait=0")
	}
	if m.Verbose {
		argv = append(argv, "-v")
	}
	var pos []string
	for _, prop := range m.Args {
		v, ok := in[prop]
		if !ok || v == nil {
			continue
		}
		vals, _, err := values(tool, prop, v)
		if err != nil {
			return nil, err
		}
		pos = append(pos, vals...)
	}
	if len(pos) > 0 {
		argv = append(argv, "--")
		argv = append(argv, pos...)
	}
	return argv, nil
}

// values gives the strings of an input value, checked against the pattern
// of its property.
func values(tool, prop string, v any) ([]string, bool, error) {
	var out []string
	switch x := v.(type) {
	case bool:
		return []string{strconv.FormatBool(x)}, true, nil
	case json.Number:
		out = []string{x.String()}
	case float64:
		out = []string{strconv.FormatFloat(x, 'f', -1, 64)}
	case int:
		out = []string{strconv.Itoa(x)}
	case string:
		out = []string{x}
	case []any:
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, false, inputErrorf("%s: each element must be a string", prop)
			}
			out = append(out, s)
		}
	case []string:
		out = x
	default:
		return nil, false, inputErrorf("%s: unexpected value %v", prop, v)
	}
	re, what := patternOf(tool, prop)
	for i, s := range out {
		if prop == "run" && (s == "last" || s == "latest") {
			out[i] = "latest"
			continue
		}
		if re != nil && !re.MatchString(s) {
			return nil, false, inputErrorf("%s: %q is not %s", prop, s, what)
		}
		if strings.ContainsAny(s, "\x00\n\r") {
			return nil, false, inputErrorf("%s: control characters are not allowed", prop)
		}
	}
	return out, false, nil
}

// exposed are the command paths that the tools run.
var exposed = func() map[string]bool {
	out := map[string]bool{}
	for _, m := range Mappings {
		out[strings.Join(m.Command, " ")] = true
	}
	return out
}()

// notExposed are the command paths that never run in the server, as
// notExposed of mcp-tools.json lists them (agents.html#not-exposed).
var notExposed = []string{
	"publish", "repo push", "repo pull", "repo restore", "repo prune", "repo gc", "repo resign",
	"sign add", "sign retire", "sign activate", "sign remove", "sign revoke", "sign deployed", "sign removed",
	"release", "advisories dismiss", "advisories undismiss", "clean", "mock", "init", "migrate",
	"ui", "skills", "mcp", "completion",
}

// neverFlags are flags that the server never passes: they confirm, accept,
// discard or write what only a human may.
var neverFlags = map[string]bool{
	"yes": true, "accept-advisory": true, "hold": true, "confirm-fd": true, "apply": true,
	"install-timer": true, "remove-timer": true, "older": true, "orphans": true,
}

// neverFlagsOf are flags that one command never gets from the server.
var neverFlagsOf = map[string]map[string]bool{
	"checkout": {"force": true}, // checkout never discards changes
}

// IsNotExposed reports a command path of notExposed.
func IsNotExposed(path string) bool {
	for _, n := range notExposed {
		if path == n || strings.HasPrefix(path, n+" ") {
			return true
		}
	}
	return false
}

// CheckCommand refuses a command line that no tool may run: a command path
// that no tool maps to, one of notExposed, one of the safety class shared
// state, or a flag of neverFlags. tree is the command tree of repoctl; nil
// skips the check of the class.
func CheckCommand(tree *cobra.Command, argv []string) error {
	var path []string
	for _, a := range argv {
		if strings.HasPrefix(a, "-") {
			break
		}
		path = append(path, a)
	}
	p := strings.Join(path, " ")
	if IsNotExposed(p) || !exposed[p] {
		return api.Errorf(api.PMCPToolNotAllowed, "repoctl %s has no MCP tool; only a human runs it", p)
	}
	for _, a := range argv {
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if neverFlags[name] || neverFlagsOf[p][name] {
			return api.Errorf(api.PMCPToolNotAllowed, "repoctl %s --%s is for a human only", p, name)
		}
	}
	if tree != nil {
		cmd, rest, err := tree.Find(path)
		if err != nil || cmd == nil || len(rest) > 0 || strings.TrimPrefix(cmd.CommandPath(), tree.Name()+" ") != p {
			return api.Errorf(api.PMCPToolNotAllowed, "repoctl %s is not a command of this binary", p)
		}
		if cmd.Annotations["class"] == "shared state" {
			return api.Errorf(api.PMCPToolNotAllowed, "repoctl %s changes shared state; only a human runs it", p)
		}
	}
	return nil
}
