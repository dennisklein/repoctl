package api

import (
	"errors"
	"fmt"
)

// Hint is one hint line of the terminal: "hint: DO. run: RUN". Run is the
// command to copy; it can be empty.
type Hint struct {
	Do  string
	Run string
}

// String gives the text of the hint without the "hint: " prefix.
func (h Hint) String() string {
	if h.Run == "" {
		return h.Do
	}
	if h.Do == "" {
		return "run: " + h.Run
	}
	return h.Do + ". run: " + h.Run
}

// Error is an error with a problem of the contract. The core returns it
// where it knows the cause; the surfaces turn it into an error line and
// hints, an entry of errors[] and an exit code.
type Error struct {
	Problem
	// Hints are the hint lines of the terminal, at most 3. Problem.Hint
	// holds the first one.
	Hints []Hint
	// Err is the cause, if any.
	Err error
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error { return e.Err }

// ExitStatus gives the exit code of the error, for cobratree.Status.
func (e *Error) ExitStatus() int { return e.ExitCode }

// Errorf makes an Error with the code, the exit code of the registry and the
// message. A code that the registry gives no exit code gives ExitFailed.
func Errorf(code ProblemCode, format string, args ...any) *Error {
	e := &Error{Problem: Problem{Code: code, Message: fmt.Sprintf(format, args...)}}
	e.ExitCode = ExitFailed
	if info, ok := Lookup(code); ok && info.Exit != 0 {
		e.ExitCode = info.Exit
	}
	return e
}

// WithExit sets the exit code.
func (e *Error) WithExit(code int) *Error {
	e.ExitCode = code
	return e
}

// WithHint adds a hint line.
func (e *Error) WithHint(do, run string) *Error {
	e.Hints = append(e.Hints, Hint{Do: do, Run: run})
	if e.Hint == "" {
		e.Hint = Hint{Do: do, Run: run}.String()
	}
	return e
}

// WithCause sets the cause.
func (e *Error) WithCause(err error) *Error {
	e.Err = err
	return e
}

// WithNode sets the node, the package and the target of the problem.
func (e *Error) WithNode(n NodeId) *Error {
	e.Node = n
	e.Package, e.Target = SplitNode(n)
	return e
}

// WithPackage sets the package of the problem.
func (e *Error) WithPackage(p PackageId) *Error {
	e.Package = p
	return e
}

// WithTarget sets the target of the problem.
func (e *Error) WithTarget(t TargetId) *Error {
	e.Target = t
	return e
}

// WithRun sets the run of the problem.
func (e *Error) WithRun(r RunId) *Error {
	e.Run = r
	return e
}

// WithPath sets the path of the problem.
func (e *Error) WithPath(p string) *Error {
	e.Path = p
	return e
}

// WithHuman sets the step that only a human can take.
func (e *Error) WithHuman(action, reason, command string) *Error {
	e.HumanAction = &HumanAction{Action: action, Reason: reason, Command: command}
	return e
}

// AsError finds an *Error in the tree of err.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// Warning makes a warning Problem with the code and message.
func Warning(code ProblemCode, format string, args ...any) Problem {
	return Problem{Code: code, Message: fmt.Sprintf(format, args...)}
}

// HintText gives a hint text of the Problem form: "DO. run: RUN".
func HintText(do, run string) string { return Hint{Do: do, Run: run}.String() }
