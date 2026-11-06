package skills

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// DenyList is the static deny list that skills install merges into the
// Claude Code settings (agents.html#deny-list). It has no rule for one set,
// so it stays correct when a package or a set is added.
var DenyList = []string{
	"Bash(repoctl publish:*)", "Bash(repoctl clean:*)",
	"Bash(repoctl repo push:*)", "Bash(repoctl repo restore:*)", "Bash(repoctl repo prune:*)",
	"Bash(repoctl repo gc:*)", "Bash(repoctl repo resign:*)",
	"Bash(repoctl sign:*)", "Bash(repoctl release:*)",
	"Bash(repoctl advisories dismiss:*)", "Bash(repoctl advisories undismiss:*)",
	"Bash(rpmsign:*)", "Bash(rpm --addsign:*)", "Bash(rpm --resign:*)", "Bash(gpg:*)",
	"Bash(mock:*)", "Bash(createrepo_c:*)", "Bash(git push:*)",
	"Bash(git -C .:*)", "Bash(git -C packages/..:*)",
	"Edit(repoctl.yaml)", "Write(repoctl.yaml)", "Edit(repoctl.lock)", "Write(repoctl.lock)",
	"Edit(keys/**)", "Write(keys/**)", "Edit(.repoctl/**)", "Write(.repoctl/**)",
	"Edit(.claude/**)", "Write(.claude/**)",
}

// GuardCommand is the command of the guard hook, and GuardMatcher the tools
// that it sees.
const (
	GuardCommand = "repoctl skills guard"
	GuardMatcher = "Bash|Edit|Write|NotebookEdit"
)

// object is a JSON object that keeps the order of its keys, so that a
// merge into a settings file of a human changes only what it must.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func newObject() *object { return &object{vals: map[string]json.RawMessage{}} }

// parseObject reads a JSON object. Empty input is an empty object.
func parseObject(b []byte) (*object, error) {
	o := newObject()
	if len(bytes.TrimSpace(b)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("not a JSON object")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(key, v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("text after the JSON object")
	}
	return o, nil
}

func (o *object) get(k string) (json.RawMessage, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *object) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *object) empty() bool { return len(o.keys) == 0 }

// child gives the object under key k, or a new one.
func (o *object) child(k string) (*object, error) {
	v, ok := o.get(k)
	if !ok {
		return newObject(), nil
	}
	c, err := parseObject(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", k, err)
	}
	return c, nil
}

// setChild stores c under k, or removes k when c is empty.
func (o *object) setChild(k string, c *object) {
	if c.empty() {
		o.del(k)
		return
	}
	b, _ := c.MarshalJSON()
	o.set(k, b)
}

// MarshalJSON writes the object in the order of its keys.
func (o *object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// indent writes the object with two spaces and a final newline.
func (o *object) indent() ([]byte, error) {
	raw, _ := o.MarshalJSON()
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// Settings is a Claude Code settings file: permissions.deny and
// hooks.PreToolUse are the parts that repoctl changes.
type Settings struct {
	root *object
}

// ParseSettings reads a settings file. Empty input is an empty file.
func ParseSettings(b []byte) (*Settings, error) {
	o, err := parseObject(b)
	if err != nil {
		return nil, err
	}
	return &Settings{root: o}, nil
}

// Bytes gives the file.
func (s *Settings) Bytes() ([]byte, error) { return s.root.indent() }

// Empty reports a file with no key.
func (s *Settings) Empty() bool { return s.root.empty() }

// Deny gives permissions.deny.
func (s *Settings) Deny() []string {
	perm, err := s.root.child("permissions")
	if err != nil {
		return nil
	}
	v, ok := perm.get("deny")
	if !ok {
		return nil
	}
	var list []any
	if json.Unmarshal(v, &list) != nil {
		return nil
	}
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// AddDeny adds the rules that permissions.deny does not hold yet and gives
// them.
func (s *Settings) AddDeny(rules []string) ([]string, error) {
	return s.editDeny(func(list []json.RawMessage, have map[string]bool) ([]json.RawMessage, []string) {
		var added []string
		for _, r := range rules {
			if have[r] {
				continue
			}
			b, _ := json.Marshal(r)
			list = append(list, b)
			have[r] = true
			added = append(added, r)
		}
		return list, added
	})
}

// RemoveDeny removes these rules from permissions.deny.
func (s *Settings) RemoveDeny(rules []string) error {
	drop := map[string]bool{}
	for _, r := range rules {
		drop[r] = true
	}
	_, err := s.editDeny(func(list []json.RawMessage, _ map[string]bool) ([]json.RawMessage, []string) {
		var out []json.RawMessage
		for _, raw := range list {
			var str string
			if json.Unmarshal(raw, &str) == nil && drop[str] {
				continue
			}
			out = append(out, raw)
		}
		return out, nil
	})
	return err
}

func (s *Settings) editDeny(f func([]json.RawMessage, map[string]bool) ([]json.RawMessage, []string)) ([]string, error) {
	perm, err := s.root.child("permissions")
	if err != nil {
		return nil, err
	}
	var list []json.RawMessage
	if v, ok := perm.get("deny"); ok {
		if err := json.Unmarshal(v, &list); err != nil {
			return nil, fmt.Errorf("permissions.deny: %w", err)
		}
	}
	have := map[string]bool{}
	for _, raw := range list {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			have[str] = true
		}
	}
	list, added := f(list, have)
	if len(list) == 0 {
		perm.del("deny")
	} else {
		b, _ := json.Marshal(list)
		perm.set("deny", b)
	}
	s.root.setChild("permissions", perm)
	return added, nil
}

// hookEntry is one entry of hooks.PreToolUse.
type hookEntry struct {
	Matcher string `json:"matcher,omitempty"`
	Hooks   []struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	} `json:"hooks"`
}

// IsGuardCommand reports a hook command that runs repoctl skills guard,
// also with a path to the binary.
func IsGuardCommand(cmd string) bool {
	f := strings.Fields(cmd)
	return len(f) == 3 && filepath.Base(f[0]) == "repoctl" && f[1] == "skills" && f[2] == "guard"
}

// GuardMatches reports whether a hook matcher covers each tool that the
// guard checks: Bash, Edit, Write and NotebookEdit.
func GuardMatches(matcher string) bool {
	if matcher == "" || matcher == "*" {
		return true
	}
	have := map[string]bool{}
	for _, m := range strings.Split(matcher, "|") {
		have[strings.TrimSpace(m)] = true
	}
	for _, t := range []string{"Bash", "Edit", "Write", "NotebookEdit"} {
		if !have[t] {
			return false
		}
	}
	return true
}

// HasGuard reports a PreToolUse hook that runs the guard for each tool that
// it checks.
func (s *Settings) HasGuard() bool {
	for _, e := range s.preToolUse() {
		var h hookEntry
		if json.Unmarshal(e, &h) != nil || !GuardMatches(h.Matcher) {
			continue
		}
		for _, x := range h.Hooks {
			if x.Type == "command" && IsGuardCommand(x.Command) {
				return true
			}
		}
	}
	return false
}

func (s *Settings) preToolUse() []json.RawMessage {
	hooks, err := s.root.child("hooks")
	if err != nil {
		return nil
	}
	v, ok := hooks.get("PreToolUse")
	if !ok {
		return nil
	}
	var list []json.RawMessage
	_ = json.Unmarshal(v, &list)
	return list
}

func (s *Settings) setPreToolUse(list []json.RawMessage) error {
	hooks, err := s.root.child("hooks")
	if err != nil {
		return err
	}
	if len(list) == 0 {
		hooks.del("PreToolUse")
	} else {
		b, _ := json.Marshal(list)
		hooks.set("PreToolUse", b)
	}
	s.root.setChild("hooks", hooks)
	return nil
}

// guardEntry is the PreToolUse entry that install adds.
func guardEntry() json.RawMessage {
	return json.RawMessage(`{"matcher":"` + GuardMatcher + `","hooks":[{"type":"command","command":"` + GuardCommand + `"}]}`)
}

// AddGuard adds the guard hook when the file has none and reports whether
// it added it.
func (s *Settings) AddGuard() (bool, error) {
	if s.HasGuard() {
		return false, nil
	}
	if _, err := s.root.child("hooks"); err != nil {
		return false, err
	}
	return true, s.setPreToolUse(append(s.preToolUse(), guardEntry()))
}

// RemoveGuard removes the entry that install added: the matcher of the
// guard with the guard command as its only hook.
func (s *Settings) RemoveGuard() error {
	var out []json.RawMessage
	for _, e := range s.preToolUse() {
		var h hookEntry
		if json.Unmarshal(e, &h) == nil && h.Matcher == GuardMatcher && len(h.Hooks) == 1 && IsGuardCommand(h.Hooks[0].Command) {
			continue
		}
		out = append(out, e)
	}
	return s.setPreToolUse(out)
}

// MissingDeny gives the rules of DenyList that the file does not hold.
func (s *Settings) MissingDeny() []string {
	have := map[string]bool{}
	for _, r := range s.Deny() {
		have[r] = true
	}
	var out []string
	for _, r := range DenyList {
		if !have[r] {
			out = append(out, r)
		}
	}
	return out
}
