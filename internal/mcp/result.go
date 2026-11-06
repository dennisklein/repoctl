package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dennisklein/repoctl/internal/api"
)

// MaxResult is the largest result of a tool call (agents.html#result).
const MaxResult = 256 << 10

// MetaExitCode is the _meta key of the exit code that the CLI would return.
const MetaExitCode = "repoctl/exitCode"

// Problem codes of the server that the registry of the contract does not
// list: input that the server refuses (the CLI gives exit 2 without an
// envelope) and a result above MaxResult.
const (
	PInvalidInput   = api.PMCPInvalidInput
	PResultTooLarge = api.PMCPResultTooLarge
)

// envelope is the part of an envelope that the server reads.
type envelope struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   json.RawMessage   `json:"metadata"`
	Items      []json.RawMessage `json:"items"`
	Warnings   []api.Problem     `json:"warnings"`
	Errors     []api.Problem     `json:"errors"`
}

// parseEnvelope reads the envelope of a command, or reports that stdout is
// none.
func parseEnvelope(b []byte) (*envelope, bool) {
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil || e.APIVersion == "" || e.Kind == "" {
		return nil, false
	}
	return &e, true
}

// toolResult makes the result of a tool call from an envelope and the exit
// code of the command (agents.html#result).
func toolResult(env []byte, exit int) *sdk.CallToolResult {
	compact := compactJSON(env)
	e, ok := parseEnvelope(compact)
	if !ok {
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: string(env)}},
			IsError: exit != 0,
			Meta:    sdk.Meta{MetaExitCode: exit},
		}
	}
	if len(compact) > MaxResult {
		return tooLarge(e, len(compact))
	}
	summary := summarize(e, exit)
	text := summary + "\n" + string(compact)
	if len(text)+len(compact) > MaxResult {
		text = summary + "\n(The envelope is in structuredContent; it is too large to repeat here.)"
	}
	return &sdk.CallToolResult{
		Content:           []sdk.Content{&sdk.TextContent{Text: text}},
		StructuredContent: json.RawMessage(compact),
		IsError:           len(e.Errors) > 0,
		Meta:              sdk.Meta{MetaExitCode: exit},
	}
}

func tooLarge(e *envelope, size int) *sdk.CallToolResult {
	p := api.Problem{
		Code:     PResultTooLarge,
		Message:  fmt.Sprintf("The result has %d bytes, more than %d KiB.", size, MaxResult>>10),
		Hint:     "Narrow the call, for example with packages, targets, name, tail or grep.",
		ExitCode: api.ExitFailed,
	}
	out := map[string]any{
		"apiVersion": api.APIVersion, "kind": e.Kind, "metadata": e.Metadata,
		"items": []any{}, "warnings": []any{}, "errors": []api.Problem{p},
	}
	b, _ := json.Marshal(out)
	return toolResult(b, api.ExitFailed)
}

// compactJSON gives JSON without the white space of the indent.
func compactJSON(b []byte) []byte {
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		return bytes.TrimSpace(b)
	}
	return out.Bytes()
}

// summarize gives the text before the envelope: at most 3 sentences. A
// problem with a humanAction, or exit 6, starts with NEEDS HUMAN:.
func summarize(e *envelope, exit int) string {
	for _, p := range e.Errors {
		if p.HumanAction != nil || p.ExitCode == api.ExitNeedsConfirmation {
			return needsHuman(p)
		}
	}
	if exit == api.ExitNeedsConfirmation && len(e.Errors) > 0 {
		return needsHuman(e.Errors[0])
	}
	if len(e.Errors) > 0 {
		p := e.Errors[0]
		s := p.Code + ": " + sentence(p.Message)
		if p.Hint != "" {
			s += " " + sentence(p.Hint)
		}
		if len(e.Errors) > 1 {
			s += fmt.Sprintf(" %d more errors in errors[].", len(e.Errors)-1)
		}
		return s
	}
	var md struct {
		Command string `json:"command"`
		Set     string `json:"set"`
	}
	_ = json.Unmarshal(e.Metadata, &md)
	n := len(e.Items)
	word := "items"
	if n == 1 {
		word = "item"
	}
	s := fmt.Sprintf("repoctl %s: %d %s %s.", md.Command, n, e.Kind, word)
	if md.Set != "" {
		s = fmt.Sprintf("repoctl %s in set %s: %d %s %s.", md.Command, md.Set, n, e.Kind, word)
	}
	if len(e.Warnings) > 0 {
		var codes []string
		seen := map[api.ProblemCode]bool{}
		for _, w := range e.Warnings {
			if !seen[w.Code] {
				seen[w.Code] = true
				codes = append(codes, w.Code)
			}
		}
		s += fmt.Sprintf(" %d warnings: %s.", len(e.Warnings), strings.Join(codes, ", "))
	}
	return s
}

// needsHuman gives the text of a step that only a human takes.
func needsHuman(p api.Problem) string {
	s := "NEEDS HUMAN: " + sentence(p.Message)
	if h := p.HumanAction; h != nil {
		if h.Reason != "" {
			s += " " + sentence(h.Reason)
		}
		switch {
		case h.Command != "":
			s += " Ask the human to run: " + h.Command
		case h.UI != "":
			s += " Ask the human to open " + h.UI + " in the web UI."
		default:
			s += " Ask the human to take the step " + h.Action + "."
		}
		return s
	}
	if p.Hint != "" {
		s += " Ask the human: " + sentence(p.Hint)
	}
	return s
}

// sentence ends a text with a period.
func sentence(s string) string {
	s = strings.TrimSpace(firstLine(s))
	if s == "" || strings.HasSuffix(s, ".") || strings.HasSuffix(s, "?") || strings.HasSuffix(s, "!") {
		return s
	}
	return s + "."
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// rawResult is the result of a tool whose output is no envelope, the
// schema of repoctl_schema.
func rawResult(b []byte, exit int) *sdk.CallToolResult {
	r := &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: string(b)}},
		IsError: exit != 0,
		Meta:    sdk.Meta{MetaExitCode: exit},
	}
	if c := compactJSON(b); json.Valid(c) && bytes.HasPrefix(c, []byte("{")) {
		r.StructuredContent = json.RawMessage(c)
	}
	return r
}
