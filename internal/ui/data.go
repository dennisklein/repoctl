package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
)

// CodeNotImplemented is the problem code of a command that this build of
// repoctl does not have yet. A page shows it as a calm empty state.
const CodeNotImplemented = "internal.not-implemented"

// Source runs a repoctl command with --json for the set in the directory
// dir and gives its envelope. The pages read all their data through it, so
// that a test injects envelopes.
type Source interface {
	Run(ctx context.Context, dir string, args ...string) *Result
}

// Result is the envelope of one command, its items still undecoded.
type Result struct {
	Args     []string
	Kind     api.Kind
	Items    []json.RawMessage
	Warnings []api.Problem
	Errors   []api.Problem
	Exit     int
	// Err is set when the command gave no envelope: it could not start,
	// it panicked, or its output was not JSON.
	Err error
}

// NotImplemented reports whether the command is a stub of this build.
func (r *Result) NotImplemented() bool {
	if r == nil {
		return false
	}
	for _, p := range r.Errors {
		if p.Code == CodeNotImplemented {
			return true
		}
	}
	return false
}

// Usable reports whether the command gave items to show.
func (r *Result) Usable() bool {
	return r != nil && r.Err == nil && len(r.Items) > 0
}

// Problem gives the first error of the command, or nil.
func (r *Result) Problem() *api.Problem {
	if r == nil {
		return nil
	}
	if len(r.Errors) > 0 {
		return &r.Errors[0]
	}
	if r.Err != nil {
		return &api.Problem{Code: api.PInternal, Message: r.Err.Error()}
	}
	return nil
}

// Items decodes the items of r as T; an item that does not decode is
// left out.
func Items[T any](r *Result) []T {
	if r == nil {
		return nil
	}
	out := make([]T, 0, len(r.Items))
	for _, raw := range r.Items {
		var v T
		if json.Unmarshal(raw, &v) == nil {
			out = append(out, v)
		}
	}
	return out
}

// First decodes the first item of r as T.
func First[T any](r *Result) (T, bool) {
	var v T
	if r == nil || len(r.Items) == 0 {
		return v, false
	}
	if json.Unmarshal(r.Items[0], &v) != nil {
		return v, false
	}
	return v, true
}

// envelope is api.Envelope with the items left raw.
type envelope struct {
	APIVersion string            `json:"apiVersion"`
	Kind       api.Kind          `json:"kind"`
	Items      []json.RawMessage `json:"items"`
	Warnings   []api.Problem     `json:"warnings"`
	Errors     []api.Problem     `json:"errors"`
}

// ParseEnvelope reads the --json output of a command that ended with
// exit.
func ParseEnvelope(out []byte, exit int, args []string) *Result {
	r := &Result{Args: args, Exit: exit}
	var env envelope
	if err := json.Unmarshal(bytes.TrimSpace(out), &env); err != nil || env.APIVersion == "" {
		if err == nil {
			err = errors.New("no envelope")
		}
		r.Err = fmt.Errorf("repoctl %s gave no JSON envelope (exit %d): %w", strings.Join(args, " "), exit, err)
		return r
	}
	r.Kind, r.Items, r.Warnings, r.Errors = env.Kind, env.Items, env.Warnings, env.Errors
	return r
}

// ExecFunc runs a repoctl command line in this process, as cli.App.Execute
// does, and gives its exit code.
type ExecFunc func(ctx context.Context, args []string, stdout, stderr io.Writer) int

// InProcess gives a Source that runs the commands in this process, one at
// a time: the command tree keeps state in package variables. A panic of a
// command is its Result.Err.
func InProcess(run ExecFunc) Source { return &inProcess{run: run} }

type inProcess struct {
	mu  sync.Mutex
	run ExecFunc
}

func (s *inProcess) Run(ctx context.Context, dir string, args ...string) (res *Result) {
	full := commandLine(dir, args)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out, errOut bytes.Buffer
	defer func() {
		if p := recover(); p != nil {
			res = &Result{Args: args, Exit: 1, Err: fmt.Errorf("repoctl %s panicked: %v", strings.Join(args, " "), p)}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	code := s.run(ctx, full, &out, &errOut)
	return ParseEnvelope(out.Bytes(), code, args)
}

// Child gives a Source that runs the program bin as a child process for
// each command, with the environment env (nil: this process's).
func Child(bin string, env []string) Source { return &childSource{bin: bin, env: env} }

type childSource struct {
	bin string
	env []string
}

func (s *childSource) Run(ctx context.Context, dir string, args ...string) *Result {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.bin, commandLine(dir, args)...) //nolint:gosec // G204: the repoctl binary itself
	cmd.Dir = dir
	cmd.Env = ChildEnv(s.env)
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return &Result{Args: args, Exit: 1, Err: err}
		}
		code = ee.ExitCode()
	}
	return ParseEnvelope(out.Bytes(), code, args)
}

func commandLine(dir string, args []string) []string {
	full := append([]string{}, args...)
	return append(full, "-C", dir, "--json")
}

// quietVars are the variables that would change what a --json command of
// the UI prints; the UI runs its commands without them.
var quietVars = []string{"REPOCTL_TARGET", "REPOCTL_PROGRESS", "REPOCTL_QUIET", "REPOCTL_VERBOSE", "REPOCTL_JSON", "REPOCTL_PROGRESS_LOG"}

// QuietEnv wraps getenv so that the variables which change the output of
// a command read as unset.
func QuietEnv(getenv func(string) string) func(string) string {
	return func(k string) string {
		for _, q := range quietVars {
			if k == q {
				return ""
			}
		}
		return getenv(k)
	}
}

// ChildEnv gives env (nil: this process's) without the variables that
// change the output of a command, and without the one that would let a
// publish run unattended: the UI always confirms a publish itself.
func ChildEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		drop := k == "REPOCTL_ALLOW_UNATTENDED_PUBLISH"
		for _, q := range quietVars {
			drop = drop || k == q
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// cached is a Source that keeps each result for ttl, until the watcher
// says the set changed.
type cached struct {
	src Source
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	at  time.Time
	res *Result
}

func newCached(src Source, ttl time.Duration) *cached {
	return &cached{src: src, ttl: ttl, now: time.Now, entries: map[string]cacheEntry{}}
}

func (c *cached) Run(ctx context.Context, dir string, args ...string) *Result {
	key := dir + "\x00" + strings.Join(args, "\x00")
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if ok && c.now().Sub(e.at) < c.ttl {
		return e.res
	}
	res := c.src.Run(ctx, dir, args...)
	if ctx.Err() == nil {
		c.mu.Lock()
		c.entries[key] = cacheEntry{at: c.now(), res: res}
		c.mu.Unlock()
	}
	return res
}

// Invalidate forgets the results of the set in dir ("" forgets all).
func (c *cached) Invalidate(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if dir == "" || strings.HasPrefix(k, dir+"\x00") {
			delete(c.entries, k)
		}
	}
}
