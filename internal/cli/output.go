package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/GSI-HPC/go-clikit/cliout"
	"github.com/GSI-HPC/go-clikit/cobratree"
	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// Result is what a command gives the frame: the items of its kind for
// --json, and the human output for the terminal.
type Result struct {
	Kind  api.Kind
	Items []any
	// Next are the Next: steps. Lock and File put them into metadata.next;
	// other kinds hold them in their item.
	Next []api.NextStep
	// Run is the run that the command made or read (metadata.run).
	Run api.RunId
	// Targets are the targets of the command (metadata.targets).
	Targets []api.TargetId
	// Human writes the human output: the result to o.Stdout, notes and
	// warnings to o.Stderr. It runs also when the command fails, before the
	// error line.
	Human func(o *Out) error
	// Raw is written to stdout as is, without an envelope (schema,
	// completion, repo sbom --raw).
	Raw []byte
	// ShownWarnings says that Human wrote the warnings itself.
	ShownWarnings bool
	// NextShown says that Human wrote the Next: line itself.
	NextShown bool
}

// Out is the human output of a command.
type Out struct {
	Stdout io.Writer
	Stderr io.Writer
	Style  Style // of stderr
	// StdoutStyle is the style of stdout.
	StdoutStyle Style
	Quiet       bool
	Verbose     int
}

// Note writes "note: TEXT" to stderr.
func (o *Out) Note(format string, args ...any) {
	if o.Quiet {
		return
	}
	fmt.Fprintln(o.Stderr, o.Style.Dim("note: "+fmt.Sprintf(format, args...)))
}

// Warning writes "warning: TEXT" to stderr.
func (o *Out) Warning(p api.Problem) {
	if o.Quiet {
		return
	}
	fmt.Fprintln(o.Stderr, o.Style.Amber("warning:")+" "+termtext.Escape(p.Message))
	if p.Hint != "" {
		o.hintLine(p.Hint)
	}
}

// Hint writes "hint: DO. run: CMD" to stderr.
func (o *Out) Hint(do, run string) {
	o.hintLine(api.HintText(do, run))
}

func (o *Out) hintLine(text string) {
	do, run, ok := strings.Cut(text, "run: ")
	if ok {
		fmt.Fprintln(o.Stderr, o.Style.Dim("hint: "+do+"run: ")+o.Style.Bold(run))
		return
	}
	fmt.Fprintln(o.Stderr, o.Style.Dim("hint: "+text))
}

// Next writes the Next: line, with more commands on indented lines.
func (o *Out) Next(steps []api.NextStep) {
	if o.Quiet || len(steps) == 0 {
		return
	}
	for i, s := range steps {
		text := s.Command
		if text == "" {
			text = s.Reason
		}
		if i == 0 {
			fmt.Fprintln(o.Stderr, "Next: "+o.Style.Bold(text))
			continue
		}
		fmt.Fprintln(o.Stderr, "      "+o.Style.Bold(text))
	}
}

// Table writes a table to stdout.
func (o *Out) Table(t *Table) error {
	return t.Write(o.Stdout, o.Verbose > 0)
}

// Printf writes to stdout.
func (o *Out) Printf(format string, args ...any) { fmt.Fprintf(o.Stdout, format, args...) }

// Errorf writes to stderr.
func (o *Out) Errorf(format string, args ...any) { fmt.Fprintf(o.Stderr, format, args...) }

// KV writes a key-value block line: the key in dim, padded to width.
func (o *Out) KV(width int, key, value string) {
	fmt.Fprintf(o.Stdout, "%s%s  %s\n", o.StdoutStyle.Dim(key), strings.Repeat(" ", max(0, width-len(key))), value)
}

// Handler is the function of a command.
type Handler func(ctx context.Context, a *App, cmd *cobra.Command, args []string) (*Result, error)

// run wraps a Handler for cobra: it writes the envelope or the human
// output and turns the error into the exit code.
func (a *App) run(h Handler) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx := a.Context(cmd.Context())
		res, err := h(ctx, a, cmd, args)
		if err != nil && (isUsage(err) || isCobraUsage(err) || cobratree.IsUsage(err)) {
			return err
		}
		if res != nil && res.Raw != nil && err == nil {
			_, werr := a.Stdout.Write(res.Raw)
			return werr
		}
		if a.jsonOut {
			a.writeEnvelope(cmd, res, err)
		} else {
			a.writeHuman(res, err)
		}
		if err != nil {
			return &silent{err: err}
		}
		return nil
	}
}

// silent is an error that the frame already wrote.
type silent struct{ err error }

func (s *silent) Error() string { return s.err.Error() }
func (s *silent) Unwrap() error { return s.err }

func isCobraUsage(err error) bool {
	e, ok := api.AsError(err)
	return ok && e.ExitCode == api.ExitUsage
}

func (a *App) metadata(cmd *cobra.Command, res *Result) api.Metadata {
	md := api.Metadata{
		Command:     strings.TrimPrefix(cmd.CommandPath(), "repoctl "),
		GeneratedAt: api.FormatTime(a.Now()),
		Repoctl:     api.MetadataRepoctl{Version: config.Version, Commit: config.Commit},
	}
	if a.set != nil {
		md.Set = a.set.Name
		md.SetDir = a.setDir
	}
	if res != nil {
		md.Run = res.Run
		md.Targets = res.Targets
		if res.Kind == api.KindLock || res.Kind == api.KindFile {
			md.Next = res.Next
		}
	}
	return md
}

func (a *App) writeEnvelope(cmd *cobra.Command, res *Result, err error) {
	kind := api.Kind("")
	if res != nil {
		kind = res.Kind
	}
	if kind == "" {
		kind = kindOf(cmd)
	}
	env := api.NewEnvelope(kind, a.metadata(cmd, res))
	if res != nil && res.Items != nil {
		env.Items = res.Items
	}
	env.Warnings = append(env.Warnings, a.warnings...)
	var eo *exitOnlyError
	if err != nil && !errors.As(err, &eo) {
		env.Errors = append(env.Errors, problemsOf(err)...)
	}
	_ = cliout.WriteJSON(a.Stdout, env)
}

// problemsOf gives the errors[] of an error: each problem of an error that
// holds several, such as a failed build with one per failed node, else
// the one problem.
func problemsOf(err error) []api.Problem {
	var m interface{ Problems() []api.Problem }
	if errors.As(err, &m) {
		if ps := m.Problems(); len(ps) > 0 {
			return ps
		}
	}
	return []api.Problem{problemOf(err)}
}

// kindOf gives the kind that a command declares in its annotation.
func kindOf(cmd *cobra.Command) api.Kind {
	if k, ok := cmd.Annotations["kind"]; ok {
		return api.Kind(k)
	}
	return api.KindVersion
}

func problemOf(err error) api.Problem {
	if e, ok := api.AsError(err); ok {
		p := e.Problem
		if p.ExitCode == 0 {
			p.ExitCode = api.ExitFailed
		}
		return p
	}
	return api.Problem{Code: api.PInternal, Message: err.Error(), ExitCode: exitCode(err)}
}

func (a *App) out() *Out {
	return &Out{
		Stdout:      a.Stdout,
		Stderr:      a.Stderr,
		Style:       a.style,
		StdoutStyle: NewStyle(a.Stdout, a.Getenv, a.noColor),
		Quiet:       a.quiet,
		Verbose:     a.verbose,
	}
}

func (a *App) writeHuman(res *Result, err error) {
	o := a.out()
	if res != nil && res.Human != nil {
		if herr := res.Human(o); herr != nil && err == nil {
			err = herr
		}
	}
	if res == nil || !res.ShownWarnings {
		for _, w := range a.warnings {
			o.Warning(w)
		}
	}
	if err != nil {
		a.printError(err)
		return
	}
	if res != nil && !res.NextShown {
		o.Next(res.Next)
	}
}

// printError writes the error line and its hints.
func (a *App) printError(err error) {
	var s *silent
	if ok := asSilent(err, &s); ok {
		return
	}
	o := a.out()
	msg := err.Error()
	e, isAPI := api.AsError(err)
	if isAPI {
		msg = e.Message
	}
	fmt.Fprintln(a.Stderr, a.style.Red("repoctl: error:")+" "+termtext.Escape(firstLine(msg)))
	if isAPI {
		n := 0
		for _, h := range e.Hints {
			if n == 3 {
				break
			}
			o.Hint(h.Do, h.Run)
			n++
		}
		if len(e.Hints) == 0 && e.Hint != "" {
			o.hintLine(e.Hint)
		}
	}
}

func asSilent(err error, s **silent) bool { return errors.As(err, s) }
