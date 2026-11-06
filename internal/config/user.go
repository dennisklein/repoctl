package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/api"
)

// Setting is one key of the layers 1 to 3: how and where, never what to
// build (config.html#layers).
type Setting struct {
	Name    string
	Type    string // int, duration, string, bool, list
	Default string
	// Env is the REPOCTL_* variable that stands in for the key, if any.
	Env string
	// Alias is a second variable or key name, deprecated.
	AliasEnv string
	// Flag is the name of the command flag of the key, if any.
	Flag string
	// Enum lists the allowed values of a string key.
	Enum []string
	// Help is the text of config show -v and the docs.
	Help string
}

// Settings are the keys of the user config, in the order of config show.
var Settings = []Setting{
	{Name: "jobs", Type: "int", Default: "2", Env: "REPOCTL_JOBS", Flag: "jobs", Help: "parallel mock builds"},
	{Name: "lock_wait", Type: "duration", Default: "0", Env: "REPOCTL_LOCK_WAIT", Flag: "lock-wait", Help: "how long a command waits for a busy lock"},
	{Name: "progress", Type: "string", Default: "auto", Env: "REPOCTL_PROGRESS", Flag: "progress", Enum: []string{"auto", "tree", "counter", "plain", "none"}, Help: "the progress display"},
	{Name: "sign.key", Type: "string", Default: "", Env: "REPOCTL_SIGN_KEY", AliasEnv: "REPOCTL_GPG_NAME", Help: "the signing key of this human: an id of sign.keys or a fingerprint"},
	{Name: "sign.backend", Type: "string", Default: "gpg", Enum: []string{"gpg"}, Help: "the signing backend"},
	{Name: "sign.gpg.homedir", Type: "string", Default: "", Help: "the GnuPG home directory"},
	{Name: "ui.listen", Type: "string", Default: "127.0.0.1:0", Env: "REPOCTL_UI_LISTEN", Flag: "listen", Help: "the loopback address of the web UI"},
	{Name: "ui.open", Type: "bool", Default: "true", Help: "repoctl ui opens a browser"},
	{Name: "agent.command", Type: "string", Default: "", Help: "the agent of the agent buttons"},
	{Name: "agent.args", Type: "list", Default: "", Help: "the arguments of the agent"},
	{Name: "agent.timeout", Type: "duration", Default: "10m", Help: "the time limit of one agent run"},
	{Name: "build.timeout.srpm", Type: "duration", Default: "30m", Help: "the time limit of step srpm"},
	{Name: "build.timeout.rpm", Type: "duration", Default: "12h", Help: "the time limit of step rpm"},
	{Name: "build.timeout.stage", Type: "duration", Default: "30m", Help: "the time limit of step stage"},
	{Name: "build.timeout.idle", Type: "duration", Default: "1h", Help: "the time limit without a new log line"},
	{Name: "build.timeout.run", Type: "duration", Default: "0", Help: "the time limit of a whole run; 0 is no limit"},
	{Name: "mock.nosync", Type: "bool", Default: "false", Help: "dnf and rpm in the mock root skip fsync"},
	{Name: "mock.tmpfs", Type: "string", Default: "", Help: "the size of a tmpfs mock root, for example 32g"},
	{Name: "state.keep_runs", Type: "int", Default: "50", Help: "finished build runs whose journals stay"},
	{Name: "state.keep_publishes", Type: "int", Default: "100", Help: "publish runs whose journals stay"},
	{Name: "state.keep_checks", Type: "int", Default: "30", Help: "check runs whose journals stay"},
	{Name: "state.keep_agent", Type: "int", Default: "50", Help: "agent results per set that stay"},
}

// setKeys are the top-level keys of repoctl.yaml. In the user config they
// would change what repoctl builds.
var setKeys = map[string]bool{
	"apiVersion": true, "name": true, "minRepoctl": true, "repo": true, "store": true,
	"retention": true, "targets": true, "defaults": true, "packages": true, "skills": true,
	"sign.keys": true, "sign.release": true, "sign.checks": true,
}

// aliases are deprecated key names of the user config.
var aliases = map[string]string{"sign.gpg_name": "sign.key"}

// SettingByName gives the setting of a key.
func SettingByName(name string) (Setting, bool) {
	for _, s := range Settings {
		if s.Name == name {
			return s, true
		}
	}
	return Setting{}, false
}

// Value is the effective value of one key and the layer that gave it.
type Value struct {
	Name   string
	Value  string
	List   []string
	Source string // flag --jobs, env REPOCTL_JOBS, user config PATH, default
	Layer  int    // 1 flag, 2 variable, 3 user config, 5 default
}

// Effective holds the effective values of all settings for one set.
type Effective struct {
	values   map[string]Value
	Warnings []api.Problem
	// Path is the user config file that the values came from, "" for none.
	Path string
	// SetNames are the sets.NAME keys of the user config.
	SetNames []string
}

// UserFile is the parsed user config: flat keys, top level and per set.
type UserFile struct {
	Path string
	Top  map[string]any
	Sets map[string]map[string]any
}

// LoadUser reads the user config at path. A missing file at the default path
// is no error; an empty path reads nothing.
func LoadUser(path string, explicit bool) (*UserFile, error) {
	uf := &UserFile{Path: path, Top: map[string]any{}, Sets: map[string]map[string]any{}}
	if path == "" {
		return uf, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			uf.Path = ""
			return uf, nil
		}
		return nil, api.Errorf(api.PConfigInvalid, "user config %s: %v", path, err).WithPath(path)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "user config %s: %s", path, yamlMessage(err)).WithPath(path).
			WithHint("correct the file", "repoctl schema user-config")
	}
	for k, v := range raw {
		if k == "sets" {
			sets, ok := v.(map[string]any)
			if !ok {
				return nil, api.Errorf(api.PConfigInvalid, "user config %s: sets must be a map of set names", path).WithPath(path)
			}
			for name, sv := range sets {
				m, ok := sv.(map[string]any)
				if !ok {
					return nil, api.Errorf(api.PConfigInvalid, "user config %s: sets.%s must be a map", path, name).WithPath(path)
				}
				flat := map[string]any{}
				if err := flatten("sets."+name+".", "", m, flat, path); err != nil {
					return nil, err
				}
				uf.Sets[name] = flat
			}
			continue
		}
		if err := flatten("", k, v, uf.Top, path); err != nil {
			return nil, err
		}
	}
	return uf, nil
}

func flatten(prefix, key string, v any, out map[string]any, path string) error {
	if key != "" {
		if setKeys[key] {
			return api.Errorf(api.PConfigUnknownKey, "user config %s: %s%s changes what repoctl builds; it belongs in repoctl.yaml", path, prefix, key).WithPath(path).
				WithHint("remove the key from the user config", "repoctl config show")
		}
		if a, ok := aliases[key]; ok {
			out[a] = v
			return nil
		}
		if _, ok := SettingByName(key); ok {
			out[key] = v
			return nil
		}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return api.Errorf(api.PConfigUnknownKey, "user config %s: unknown key %s%s", path, prefix, key).WithPath(path).
			WithHint("remove the key, or upgrade repoctl when a newer release knows it", "repoctl config show")
	}
	for k, sub := range m {
		name := k
		if key != "" {
			name = key + "." + k
		}
		if err := flatten(prefix, name, sub, out, path); err != nil {
			return err
		}
	}
	return nil
}

// Resolve gives the effective values for the set (empty outside a set).
// flags holds the command line flags that the human gave, by key name.
func Resolve(uf *UserFile, set string, getenv func(string) string, flags map[string]string) (*Effective, error) {
	e := &Effective{values: map[string]Value{}, Path: uf.Path}
	for name := range uf.Sets {
		e.SetNames = append(e.SetNames, name)
	}
	sort.Strings(e.SetNames)
	for _, s := range Settings {
		v := Value{Name: s.Name, Value: s.Default, Source: "default", Layer: 5}
		if raw, ok := uf.Top[s.Name]; ok {
			v.Value, v.List = scalar(raw)
			v.Source = "user config " + uf.Path
			v.Layer = 3
		}
		if set != "" {
			if raw, ok := uf.Sets[set][s.Name]; ok {
				v.Value, v.List = scalar(raw)
				v.Source = "user config " + uf.Path + " (sets." + set + ")"
				v.Layer = 3
			}
		}
		for _, env := range []string{s.Env, s.AliasEnv} {
			if env == "" {
				continue
			}
			if x := getenv(env); x != "" {
				v.Value = x
				v.Source = "env " + env
				v.Layer = 2
				if env == s.AliasEnv {
					e.Warnings = append(e.Warnings, api.Warning(api.PDeprecated, "%s is deprecated; use %s", s.AliasEnv, s.Env))
				}
				break
			}
		}
		if f, ok := flags[s.Name]; ok {
			v.Value = f
			v.Source = "flag --" + s.Flag
			v.Layer = 1
		}
		if err := check(s, v); err != nil {
			// The progress variables never fail a command; the frame
			// writes the note.
			if v.Layer == 2 && s.Env == "REPOCTL_PROGRESS" {
				v = Value{Name: s.Name, Value: s.Default, Source: "default", Layer: 5}
			} else {
				return nil, err
			}
		}
		e.values[s.Name] = v
	}
	return e, nil
}

func scalar(raw any) (string, []string) {
	switch x := raw.(type) {
	case nil:
		return "", nil
	case []any:
		var l []string
		for _, i := range x {
			l = append(l, fmt.Sprint(i))
		}
		return strings.Join(l, " "), l
	default:
		return fmt.Sprint(x), nil
	}
}

func check(s Setting, v Value) error {
	bad := func(want string) error {
		where := v.Source
		if v.Layer == 2 {
			where = strings.TrimPrefix(v.Source, "env ")
			return api.Errorf(api.PConfigInvalid, "%s=%q: want %s", where, v.Value, want).
				WithHint("correct or unset the variable", "")
		}
		if v.Layer == 1 {
			return api.Errorf(api.PConfigInvalid, "invalid %s value %q: want %s", strings.TrimPrefix(v.Source, "flag "), v.Value, want).WithExit(api.ExitUsage)
		}
		return api.Errorf(api.PConfigInvalid, "%s: %s %q: want %s", where, s.Name, v.Value, want).
			WithHint("correct the file", "repoctl config show")
	}
	switch s.Type {
	case "int":
		n, err := strconv.Atoi(v.Value)
		if err != nil || n < 0 || (s.Name == "jobs" && n < 1) {
			return bad("a positive number")
		}
	case "duration":
		if _, err := ParseDuration(v.Value); err != nil {
			return bad("a duration, for example 10m")
		}
	case "bool":
		if _, err := strconv.ParseBool(v.Value); err != nil {
			return bad("true or false")
		}
	}
	if len(s.Enum) > 0 && v.Value != "" {
		ok := false
		for _, x := range s.Enum {
			ok = ok || x == v.Value
		}
		if !ok {
			return bad(strings.Join(s.Enum, ", "))
		}
	}
	return nil
}

// Get gives the effective value of a key.
func (e *Effective) Get(name string) Value {
	v, ok := e.values[name]
	if !ok {
		panic("config: unknown setting " + name)
	}
	return v
}

// String gives a string key.
func (e *Effective) String(name string) string { return e.Get(name).Value }

// Int gives an int key.
func (e *Effective) Int(name string) int {
	n, _ := strconv.Atoi(e.Get(name).Value)
	return n
}

// Bool gives a bool key.
func (e *Effective) Bool(name string) bool {
	b, _ := strconv.ParseBool(e.Get(name).Value)
	return b
}

// Duration gives a duration key.
func (e *Effective) Duration(name string) time.Duration {
	d, _ := ParseDuration(e.Get(name).Value)
	return d
}

// List gives a list key.
func (e *Effective) List(name string) []string { return e.Get(name).List }

// All gives the values in the order of Settings.
func (e *Effective) All() []Value {
	out := make([]Value, 0, len(Settings))
	for _, s := range Settings {
		out = append(out, e.values[s.Name])
	}
	return out
}

// DefaultSettings gives the effective values without a user config, a variable or
// a flag.
func DefaultSettings() *Effective {
	e, _ := Resolve(&UserFile{Top: map[string]any{}, Sets: map[string]map[string]any{}}, "", func(string) string { return "" }, nil)
	return e
}
