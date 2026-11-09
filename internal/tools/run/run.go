// Package run starts the child processes of the adapters: git, mock, rpm,
// createrepo_c and gpg. It logs each command line at -vvv, never with its
// environment, and gives a failed command an error with its stderr.
package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/GSI-HPC/go-clikit/termtext"
)

type traceKey struct{}

// WithTrace gives a context whose commands are written to w before they
// start (repoctl -vvv).
func WithTrace(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, traceKey{}, w)
}

// Cmd is one child process.
type Cmd struct {
	Name string
	Args []string
	Dir  string
	// Env is added to the environment of repoctl.
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Group starts the child in its own process group, so that a Ctrl-C of
	// the terminal reaches only repoctl.
	Group bool
}

// Error is the error of a child that failed.
type Error struct {
	Cmd      string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if i := strings.LastIndex(msg, "\n"); i >= 0 {
		msg = msg[i+1:]
	}
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("%s: %s", e.Cmd, termtext.Escape(msg))
}

func (e *Error) Unwrap() error { return e.Err }

// ExitCode gives the exit code of a failed child, or -1.
func ExitCode(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.ExitCode
	}
	return -1
}

// String gives the command line as text, quoted where needed.
func (c *Cmd) String() string {
	parts := []string{c.Name}
	for _, a := range c.Args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"$\\*?") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

func (c *Cmd) command(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	cmd.Stdin = c.Stdin
	if c.Group {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	return cmd
}

func (c *Cmd) trace(ctx context.Context) {
	if w, ok := ctx.Value(traceKey{}).(io.Writer); ok && w != nil {
		dir := ""
		if c.Dir != "" {
			dir = " (in " + c.Dir + ")"
		}
		fmt.Fprintf(w, "repoctl: run: %s%s\n", termtext.Escape(c.String()), dir)
	}
}

// Run runs the command. Stdout and Stderr go to c.Stdout and c.Stderr; the
// error holds the end of stderr.
func (c *Cmd) Run(ctx context.Context) error {
	cmd := c.command(ctx)
	var errBuf tailBuffer
	cmd.Stdout = c.Stdout
	if c.Stderr != nil {
		cmd.Stderr = io.MultiWriter(c.Stderr, &errBuf)
	} else {
		cmd.Stderr = &errBuf
	}
	c.trace(ctx)
	if err := cmd.Run(); err != nil {
		return c.wrap(err, errBuf.String())
	}
	return nil
}

// Output runs the command and gives its stdout.
func (c *Cmd) Output(ctx context.Context) ([]byte, error) {
	var out bytes.Buffer
	c.Stdout = &out
	err := c.Run(ctx)
	return out.Bytes(), err
}

// Start starts the command; Wait of the result waits for it.
func (c *Cmd) Start(ctx context.Context) (*exec.Cmd, error) {
	cmd := c.command(ctx)
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	c.trace(ctx)
	if err := cmd.Start(); err != nil {
		return nil, c.wrap(err, "")
	}
	return cmd, nil
}

func (c *Cmd) wrap(err error, stderr string) error {
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	return &Error{Cmd: c.Name + " " + firstArgs(c.Args), ExitCode: code, Stderr: stderr, Err: err}
}

func firstArgs(args []string) string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") && len(out) > 0 {
			break
		}
		out = append(out, a)
		if len(out) == 2 {
			break
		}
	}
	return strings.Join(out, " ")
}

// Wrap gives the error of a command that Start started and that failed.
func (c *Cmd) Wrap(err error, stderr string) error { return c.wrap(err, stderr) }

// tailBuffer keeps the last 8 KiB that it gets.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 8192 {
		t.b = t.b[len(t.b)-8192:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

// LookPath reports whether the program is in PATH.
func LookPath(name string) (string, bool) {
	p, err := exec.LookPath(name)
	return p, err == nil
}
