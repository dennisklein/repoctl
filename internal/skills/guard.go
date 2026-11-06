package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/config"
)

// HookInput is the JSON that Claude Code gives a PreToolUse hook on stdin.
type HookInput struct {
	SessionID     string          `json:"session_id,omitempty"`
	Cwd           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name,omitempty"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
}

// GuardEnv is what the guard knows of the human: the home directory and
// the repoctl user config directory.
type GuardEnv struct {
	Home          string
	UserConfigDir string
}

// Decision is the answer of the guard. A call that the guard does not deny
// goes to the normal permission rules of Claude Code.
type Decision struct {
	Deny   bool
	Reason string
}

// guardOnly are rules that the guard applies in addition to DenyList: an
// agent must not change the guard itself.
var guardOnly = []string{"repoctl skills install", "repoctl skills uninstall"}

// The git subcommands that the guard allows in the set directory and in a
// package worktree.
var (
	gitSetAllowed     = map[string]bool{"status": true, "diff": true, "log": true, "show": true}
	gitPackageAllowed = map[string]bool{"status": true, "diff": true, "log": true, "show": true, "add": true, "commit": true}
)

// GuardHook reads the hook input of Claude Code and gives the hook output:
// a deny decision in the hook format of Claude Code, or nothing when the
// normal permission rules apply. Input that the guard cannot read is
// denied.
func GuardHook(stdin []byte, env GuardEnv) []byte {
	var in HookInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return HookOutput(Decision{Deny: true, Reason: "repoctl guard: cannot read the hook input: " + err.Error()})
	}
	return HookOutput(Guard(in, env))
}

// HookOutput gives the PreToolUse output of a decision.
func HookOutput(d Decision) []byte {
	if !d.Deny {
		return []byte{}
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": d.Reason,
	}})
	return append(b, '\n')
}

// Guard decides one tool call (agents.html#guard).
func Guard(in HookInput, env GuardEnv) Decision {
	g := &guard{env: env}
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	switch in.ToolName {
	case "Bash":
		var x struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(in.ToolInput, &x); err != nil {
			return deny("cannot read the Bash call: %v", err)
		}
		return g.bash(x.Command, cwd)
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		var x struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		}
		if err := json.Unmarshal(in.ToolInput, &x); err != nil {
			return deny("cannot read the %s call: %v", in.ToolName, err)
		}
		p := x.FilePath
		if p == "" {
			p = x.NotebookPath
		}
		if p == "" {
			return Decision{}
		}
		return g.path(p, cwd, in.ToolName)
	}
	return Decision{}
}

func deny(format string, args ...any) Decision {
	return Decision{Deny: true, Reason: "repoctl guard: " + fmt.Sprintf(format, args...) +
		". Only a human takes this step: show the command to the human (agents.html#guard)."}
}

type guard struct {
	env GuardEnv
}

// bash checks each simple command of a Bash line.
func (g *guard) bash(line, cwd string) Decision {
	sh := parseShell(line)
	if sh.unsafe != "" {
		return deny("the line has %s, which the guard cannot check", sh.unsafe)
	}
	lineEnv := map[string]bool{}
	for _, sc := range sh.cmds {
		names := assignNames(sc.assigns)
		for _, w := range sc.writes {
			if d := g.path(w, cwd, "a redirection to"); d.Deny {
				return d
			}
		}
		words, extra := stripWrappers(sc.words)
		names = append(names, extra...)
		if len(words) == 0 {
			for _, n := range names {
				lineEnv[n] = true
			}
			continue
		}
		name := filepath.Base(words[0])
		switch name {
		case "cd", "pushd":
			cwd = g.cd(cwd, words[1:])
			continue
		case "export", "declare", "typeset", "readonly", "local":
			for _, w := range words[1:] {
				if n, _, ok := strings.Cut(w, "="); ok {
					lineEnv[n] = true
				}
			}
			continue
		case "eval", "xargs", "source", ".":
			return deny("%s runs commands that the guard cannot check", name)
		case "sh", "bash", "zsh", "dash", "ksh", "fish":
			for _, w := range words[1:] {
				if strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "c") {
					return deny("%s -c runs a command that the guard cannot check", name)
				}
			}
		case "find":
			for _, w := range words[1:] {
				switch w {
				case "-exec", "-execdir", "-ok", "-okdir":
					return deny("find %s runs commands that the guard cannot check", w)
				}
			}
		}
		if rule := staticMatch(words); rule != "" {
			return deny("the deny list of repoctl has Bash(%s:*)", rule)
		}
		if name == "rpm" {
			for _, w := range words[1:] {
				if w == "--addsign" || w == "--resign" || w == "--delsign" {
					return deny("rpm %s signs RPMs", w)
				}
			}
		}
		if name == "git" {
			for n := range lineEnv {
				names = append(names, n)
			}
			if d := g.git(words, cwd, names); d.Deny {
				return d
			}
		}
		for _, p := range writtenPaths(name, words[1:]) {
			if d := g.path(p, cwd, name); d.Deny {
				return d
			}
		}
	}
	return Decision{}
}

// cd gives the working directory after cd ARGS.
func (g *guard) cd(cwd string, args []string) string {
	var dir string
	for _, a := range args {
		if a == "--" || strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		dir = a
		break
	}
	if dir == "" {
		return g.env.Home
	}
	return g.abs(dir, cwd)
}

// abs makes a path of a call absolute: ~ and $HOME are the home directory,
// and a relative path is below the working directory.
//
// The path keeps its .. parts: realPath resolves them after the symlinks
// before them, as the kernel does.
func (g *guard) abs(p, cwd string) string {
	switch {
	case p == "~" || p == "$HOME" || p == "${HOME}":
		p = g.env.Home
	case strings.HasPrefix(p, "~/"):
		p = g.env.Home + "/" + p[2:]
	case strings.HasPrefix(p, "$HOME/"):
		p = g.env.Home + "/" + p[6:]
	case strings.HasPrefix(p, "${HOME}/"):
		p = g.env.Home + "/" + p[8:]
	}
	if !filepath.IsAbs(p) {
		p = cwd + "/" + p
	}
	return p
}

// assignNames gives the names of NAME=VALUE words.
func assignNames(assigns []string) []string {
	var out []string
	for _, a := range assigns {
		n, _, _ := strings.Cut(a, "=")
		out = append(out, n)
	}
	return out
}

// stripWrappers removes the commands that run another command, such as env,
// sudo or nohup, and gives the variables that env sets.
func stripWrappers(words []string) ([]string, []string) {
	var names []string
	for len(words) > 0 {
		name := filepath.Base(words[0])
		valued := map[string]bool{}
		switch name {
		case "env":
			valued = map[string]bool{"-u": true, "--unset": true, "-C": true, "--chdir": true, "-S": true, "--split-string": true}
		case "sudo":
			valued = map[string]bool{"-u": true, "-g": true, "-C": true, "-D": true, "-h": true, "-p": true, "-r": true, "-t": true, "-U": true}
		case "nice":
			valued = map[string]bool{"-n": true, "--adjustment": true}
		case "timeout":
			valued = map[string]bool{"-s": true, "--signal": true, "-k": true, "--kill-after": true}
		case "stdbuf", "command", "builtin", "exec", "nohup", "time", "ionice", "setsid":
		default:
			return words, names
		}
		i := 1
		for i < len(words) {
			w := words[i]
			if w == "--" {
				i++
				break
			}
			if strings.HasPrefix(w, "-") {
				i++
				if valued[w] {
					i++
				}
				continue
			}
			if name == "env" && assignRE.MatchString(w) {
				n, _, _ := strings.Cut(w, "=")
				names = append(names, n)
				i++
				continue
			}
			break
		}
		if name == "timeout" && i < len(words) {
			i++ // the duration
		}
		if i > len(words) {
			i = len(words)
		}
		words = words[i:]
	}
	return words, names
}

// staticRules are the Bash rules of DenyList and the rules of the guard,
// as command prefixes.
func staticRules() []string {
	var out []string
	for _, r := range DenyList {
		if strings.HasPrefix(r, "Bash(") && strings.HasSuffix(r, ":*)") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(r, "Bash("), ":*)"))
		}
	}
	return append(out, guardOnly...)
}

// staticMatch gives the rule that the command matches: as a prefix of the
// command line, as Claude Code matches it, and for repoctl also with global
// flags between the words of the command.
func staticMatch(words []string) string {
	joined := strings.Join(append([]string{filepath.Base(words[0])}, words[1:]...), " ")
	pos := repoctlWords(words)
	for _, rule := range staticRules() {
		if strings.HasPrefix(joined, rule) {
			return rule
		}
		rw := strings.Fields(rule)
		if rw[0] != "repoctl" || filepath.Base(words[0]) != "repoctl" || len(pos) == 0 || pos[0] != rw[1] {
			continue
		}
		if subsequence(rw[2:], pos[1:]) {
			return rule
		}
	}
	return ""
}

// repoctlWords gives the words of a repoctl command line that are not flags
// or values of the global flags.
func repoctlWords(words []string) []string {
	if filepath.Base(words[0]) != "repoctl" {
		return nil
	}
	valued := map[string]bool{"-C": true, "--dir": true, "--config": true, "--target": true, "--progress": true, "--progress-log": true}
	var out []string
	for i := 1; i < len(words); i++ {
		w := words[i]
		if strings.HasPrefix(w, "-") {
			if valued[w] {
				i++
			}
			continue
		}
		out = append(out, w)
	}
	return out
}

func subsequence(want, have []string) bool {
	j := 0
	for _, h := range have {
		if j < len(want) && h == want[j] {
			j++
		}
	}
	return j == len(want)
}

// git checks one git command.
func (g *guard) git(words []string, cwd string, env []string) Decision {
	for _, n := range env {
		switch n {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE":
			return deny("git with %s set", n)
		}
	}
	var dirs []string
	i := 1
	for i < len(words) && strings.HasPrefix(words[i], "-") {
		w := words[i]
		opt, _, _ := strings.Cut(w, "=")
		switch opt {
		case "-C":
			if i+1 >= len(words) {
				return deny("git -C without a directory")
			}
			dirs = append(dirs, words[i+1])
			i += 2
			continue
		case "-c", "--config-env", "--git-dir", "--work-tree", "--namespace", "--exec-path", "--super-prefix":
			if opt == "--exec-path" && !strings.Contains(w, "=") {
				break
			}
			return deny("git with %s", opt)
		}
		i++
	}
	if i >= len(words) {
		return Decision{}
	}
	sub := words[i]
	if sub == "push" {
		return deny("git push publishes commits")
	}
	dir := cwd
	for _, d := range dirs {
		dir = g.abs(d, dir)
	}
	real := realPath(dir)
	set := findSet(real)
	if set == nil {
		return Decision{}
	}
	rel := relPath(set.dir, real)
	if pkg := set.worktreeOf(rel); pkg != "" {
		if !gitPackageAllowed[sub] {
			return deny("git %s in the package worktree packages/%s; the guard allows only status, diff, log, show, add and commit there", sub, pkg)
		}
		return Decision{}
	}
	if !gitSetAllowed[sub] {
		return deny("git %s in the set directory %s; the guard allows only status, diff, log and show there", sub, set.dir)
	}
	return Decision{}
}

// writtenPaths gives the files that a command writes, moves or removes:
// tee, cp, mv, rm, ln, sed -i and a few more.
func writtenPaths(name string, args []string) []string {
	var plain []string
	target := ""
	inPlace := false
	script := false
	ended := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !ended && a == "--" {
			ended = true
			continue
		}
		if !ended && strings.HasPrefix(a, "-") && a != "-" {
			switch {
			case (name == "cp" || name == "mv" || name == "ln" || name == "install") && (a == "-t" || a == "--target-directory"):
				if i+1 < len(args) {
					target = args[i+1]
					i++
				}
			case (name == "cp" || name == "mv" || name == "ln" || name == "install") && strings.HasPrefix(a, "--target-directory="):
				target = strings.TrimPrefix(a, "--target-directory=")
			case name == "sed" && (strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place") || len(a) > 1 && a[1] != '-' && strings.Contains(a[1:], "i") && !strings.ContainsAny(a[1:], "ef")):
				inPlace = true
			case name == "sed" && (a == "-e" || a == "-f" || a == "--expression" || a == "--file" || a == "-l"):
				script = script || a != "-l"
				i++
			case name == "sed" && (strings.HasPrefix(a, "--expression=") || strings.HasPrefix(a, "--file=")):
				script = true
			}
			continue
		}
		plain = append(plain, a)
	}
	switch name {
	case "tee", "mv", "rm", "rmdir", "unlink", "truncate", "touch", "shred", "chmod", "chown", "chgrp":
		return append(plain, target)
	case "cp", "install":
		if target != "" {
			return []string{target}
		}
		if len(plain) >= 2 {
			return plain[len(plain)-1:]
		}
	case "ln":
		if target != "" {
			return []string{target}
		}
		if len(plain) >= 2 {
			return plain[len(plain)-1:]
		}
		if len(plain) == 1 {
			return []string{filepath.Base(plain[0])}
		}
	case "sed":
		if !inPlace {
			return nil
		}
		if !script && len(plain) > 0 {
			plain = plain[1:]
		}
		return plain
	case "dd":
		for _, a := range plain {
			if strings.HasPrefix(a, "of=") {
				return []string{strings.TrimPrefix(a, "of=")}
			}
		}
	}
	return nil
}

// path checks a file that a call writes.
func (g *guard) path(p, cwd, what string) Decision {
	if p == "" {
		return Decision{}
	}
	real := realPath(g.abs(p, cwd))
	if g.env.Home != "" && within(realPath(filepath.Join(g.env.Home, ".claude")), real) {
		return deny("%s %s: ~/.claude/ holds the settings and the guard of the agent", what, p)
	}
	if g.env.UserConfigDir != "" && within(realPath(g.env.UserConfigDir), real) {
		return deny("%s %s: the repoctl user config is for the human", what, p)
	}
	set := findSet(filepath.Dir(real))
	if set == nil {
		return Decision{}
	}
	rel := relPath(set.dir, real)
	if why := set.protected(rel); why != "" {
		return deny("%s %s: %s", what, p, why)
	}
	return Decision{}
}

// setFacts are the facts of a set that the guard reads from repoctl.yaml at
// each call.
type setFacts struct {
	dir string
	// inputs are the set inputs besides repoctl.yaml, repoctl.lock and
	// keys/: the macros files and the in-tree package paths.
	inputs []string
	// worktrees are the packages with a worktree packages/PKG.
	worktrees map[string]bool
}

// findSet finds the set of a directory: the first directory with a
// repoctl.yaml from dir upwards.
func findSet(dir string) *setFacts {
	for d := dir; ; d = filepath.Dir(d) {
		file := filepath.Join(d, config.SetFile)
		if fi, err := os.Stat(file); err == nil && fi.Mode().IsRegular() {
			return readFacts(d, file)
		}
		if filepath.Dir(d) == d {
			return nil
		}
	}
}

func readFacts(dir, file string) *setFacts {
	f := &setFacts{dir: dir, worktrees: map[string]bool{}}
	b, err := os.ReadFile(file)
	if err != nil {
		return f
	}
	var raw struct {
		Targets []struct {
			Macros []string `yaml:"macros"`
		} `yaml:"targets"`
		Packages []struct {
			ID   string `yaml:"id"`
			Path string `yaml:"path"`
		} `yaml:"packages"`
	}
	// A file that does not parse still protects the fixed set inputs.
	_ = yaml.Unmarshal(b, &raw)
	for _, t := range raw.Targets {
		for _, m := range t.Macros {
			f.inputs = append(f.inputs, filepath.ToSlash(filepath.Clean(m)))
		}
	}
	for _, p := range raw.Packages {
		if p.Path != "" {
			f.inputs = append(f.inputs, filepath.ToSlash(filepath.Clean(p.Path)))
			continue
		}
		if p.ID != "" {
			f.worktrees[p.ID] = true
		}
	}
	return f
}

// worktreeOf gives the package whose worktree holds rel, or "".
func (f *setFacts) worktreeOf(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) >= 2 && parts[0] == "packages" && f.worktrees[parts[1]] {
		return parts[1]
	}
	return ""
}

// protected gives why a path of the set is protected, or "".
func (f *setFacts) protected(rel string) string {
	if rel == "." || rel == "" {
		return ""
	}
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		switch p {
		case ".git":
			return "the git data is for git and the human"
		case ".claude", ".repoctl":
			return p + "/ is a set file for the human"
		}
	}
	base := parts[len(parts)-1]
	switch {
	case base == "CLAUDE.md":
		return "CLAUDE.md holds the set notes for agents"
	case rel == ".mcp.json":
		return ".mcp.json configures the agent"
	case rel == config.SetFile || rel == "repoctl.lock" || parts[0] == "keys":
		return "it is a set input (security.html#set-inputs)"
	}
	for _, in := range f.inputs {
		if rel == in || strings.HasPrefix(rel, in+"/") {
			return "it is a set input (security.html#set-inputs)"
		}
	}
	return ""
}

// realPath resolves the symlinks and the .. parts of p from left to right,
// as the kernel does: a/link/.. is the parent of the target of link. The
// part of p that does not exist is cleaned without the file system.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	p = strings.TrimRight(p, "/")
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return filepath.Clean(p)
	}
	parent := p[:i]
	if parent == "" {
		parent = "/"
	}
	return filepath.Join(realPath(parent), p[i+1:])
}

func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func relPath(base, p string) string {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}
