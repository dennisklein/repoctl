package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// RemovedVars are the REPOCTL_* variables that stand in for a flag. The
// server runs each command without them, so a call does what its command
// line says (config.html#env).
var RemovedVars = []string{
	"REPOCTL_DIR", "REPOCTL_CONFIG", "REPOCTL_TARGET", "REPOCTL_JSON", "REPOCTL_PROGRESS_LOG",
	"REPOCTL_QUIET", "REPOCTL_VERBOSE", "REPOCTL_JOBS", "REPOCTL_LOCK_WAIT", "REPOCTL_UI_LISTEN",
	"REPOCTL_ALLOW_UNATTENDED_PUBLISH",
}

func removed(name string) bool {
	for _, v := range RemovedVars {
		if v == name {
			return true
		}
	}
	return false
}

// ChildGetenv gives the environment of a command that the server runs in
// its process: without RemovedVars, with REPOCTL_PROGRESS=none and
// without the trace of the server.
func ChildGetenv(getenv func(string) string) func(string) string {
	return func(name string) string {
		switch {
		case removed(name), name == "TRACEPARENT", name == "TRACESTATE":
			return ""
		case name == "REPOCTL_PROGRESS":
			return "none"
		}
		return getenv(name)
	}
}

// ChildEnviron gives the environment of a child process: without
// RemovedVars and the trace of the server, with REPOCTL_PROGRESS=none and
// the extra variables.
func ChildEnviron(environ []string, extra ...string) []string {
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if removed(name) || name == "REPOCTL_PROGRESS" || name == "TRACEPARENT" || name == "TRACESTATE" {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "REPOCTL_PROGRESS=none")
	return append(out, extra...)
}

// randomHex gives n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewSessionID gives the session id of a server: 16 hex digits, one per
// server process (Run.origin.session).
func NewSessionID() string { return randomHex(8) }

// newTraceparent gives a W3C traceparent for a child that the server starts.
func newTraceparent() string {
	return "00-" + randomHex(16) + "-" + randomHex(8) + "-01"
}
