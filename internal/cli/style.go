package cli

import (
	"io"

	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/GSI-HPC/go-clikit/progress/display"

	"github.com/dennisklein/repoctl/internal/api"
)

// Style draws the marks and colours of one stream (terminal.html#marks).
// The colour is never the only sign of a state.
type Style struct {
	color bool
	ascii bool
}

// NewStyle gives the style of w: colour only on a terminal, without
// NO_COLOR, --no-color and TERM=dumb; ASCII marks without a UTF-8 locale.
func NewStyle(w io.Writer, getenv func(string) string, noColor bool) Style {
	color := cliprogress.IsTerminal(w) && !noColor && getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"
	return Style{color: color, ascii: !cliprogress.UTF8Locale(getenv)}
}

// PlainStyle has no colour and UTF-8 marks, for tests and files.
func PlainStyle() Style { return Style{} }

// Color reports whether the style draws colours.
func (s Style) Color() bool { return s.color }

// ASCII reports ASCII marks.
func (s Style) ASCII() bool { return s.ascii }

// Theme gives the go-clikit theme of the progress displays.
func (s Style) Theme() display.Theme {
	if !s.color {
		return display.Theme{}
	}
	t, err := display.ParseTheme("ember")
	if err != nil {
		return display.Theme{}
	}
	return t
}

func (s Style) sgr(code, text string) string {
	if !s.color || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// Red marks a failure or a blocker.
func (s Style) Red(t string) string { return s.sgr("31", t) }

// Amber marks a step to do.
func (s Style) Amber(t string) string { return s.sgr("33", t) }

// Copper marks work under way.
func (s Style) Copper(t string) string { return s.sgr("36", t) }

// Bold marks a block name, a command to copy or a question.
func (s Style) Bold(t string) string { return s.sgr("1", t) }

// Dim marks secondary text.
func (s Style) Dim(t string) string { return s.sgr("2", t) }

type mark struct{ utf, ascii, color string }

var marks = map[string]mark{
	"waiting":  {"◌", ".", ""},
	"queued":   {"○", "o", ""},
	"running":  {"▸", ">", "copper"},
	"ok":       {"✓", "+", ""},
	"failed":   {"✗", "x", "red"},
	"blocked":  {"⊘", "/", "amber"},
	"canceled": {"⊖", "~", ""},
	"build":    {"◆", "#", ""},
	"current":  {"≡", "=", ""},
	"staged":   {"●", "*", ""},
	"skip":     {"–", "-", ""},
	"warning":  {"!", "!", "amber"},
	"error":    {"✗", "x", "red"},
}

// Mark gives the mark of a node state, plan action or check result, with
// its colour.
func (s Style) Mark(state string) string {
	m, ok := marks[state]
	if !ok {
		return " "
	}
	t := m.utf
	if s.ascii {
		t = m.ascii
	}
	switch m.color {
	case "red":
		return s.Red(t)
	case "amber":
		return s.Amber(t)
	case "copper":
		return s.Copper(t)
	}
	return t
}

// State gives a state word with the colour of its state.
func (s Style) State(state string) string {
	switch state {
	case "failed", "required", "error":
		return s.Red(state)
	case "blocked", "behind", "retiring", "ahead", "kept", "warning", "dirty", "diverged":
		return s.Amber(state)
	case "running":
		return s.Copper(state)
	}
	return state
}

// Arrow gives → or ->.
func (s Style) Arrow() string {
	if s.ascii {
		return "->"
	}
	return "→"
}

// Ellipsis gives … or ...
func (s Style) Ellipsis() string {
	if s.ascii {
		return "..."
	}
	return "…"
}

// MarkOfCheck gives the doctor mark of a check status.
func (s Style) MarkOfCheck(st api.CheckStatus) string {
	switch st {
	case api.CheckStatusOk:
		return s.Mark("ok")
	case api.CheckStatusWarning:
		return s.Mark("warning")
	}
	return s.Mark("failed")
}
