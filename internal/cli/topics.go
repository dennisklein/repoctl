package cli

import (
	"embed"
	"strings"
)

// The texts of "repoctl help TOPIC" come from the chapters that own the
// facts (cli.html#topics); the files are extracted from docs/design.
//
//go:embed topics/*.txt
var topicFiles embed.FS

// topicNames are the topics in the order of the help.
var topicNames = []string{
	"config", "selectors", "graph", "build-state", "advisories", "signing",
	"key", "emergency", "team", "upgrade", "new-set", "end-set", "packages",
	"xdg", "environment", "urgent", "exit-codes", "safety",
}

// topicSummary gives the one-line summary of each topic.
var topicSummary = map[string]string{
	"config":      "Configuration files, layers and precedence",
	"selectors":   "How to name packages, targets and nodes",
	"graph":       "How to read the graph output",
	"build-state": "How repoctl decides what to build, and when to rebuild",
	"advisories":  "Advisories, dismissals, the publish gate and the recovery path",
	"signing":     "Keys, gates, deployment record and re-sign",
	"key":         "Make a signing key on a YubiKey or in a file",
	"emergency":   "A compromised or a lost key",
	"team":        "A new admin or a new host",
	"upgrade":     "Upgrade repoctl on a host and in a team",
	"new-set":     "Start a new set from the previous set",
	"end-set":     "End a set after its clients move to a newer set",
	"packages":    "Bump, add, remove, rename, move and retire a package",
	"xdg":         "Where repoctl keeps its files",
	"environment": "The REPOCTL_* variables and others",
	"urgent":      "Publish a fix while another node of the target is not ready",
	"exit-codes":  "The exit status of each kind of failure",
	"safety":      "Rules for shared state, signing and agents",
}

// topicText gives the text of a topic.
func topicText(name string) (string, bool) {
	if _, ok := topicSummary[name]; !ok {
		return "", false
	}
	b, err := topicFiles.ReadFile("topics/" + name + ".txt")
	if err != nil {
		return "", false
	}
	return strings.TrimRight(string(b), "\n") + "\n", true
}

func topicList() []string { return topicNames }

// topicsHelp gives the "Additional help topics:" block of the root help.
func topicsHelp() string {
	var b strings.Builder
	b.WriteString("Additional help topics:\n")
	for _, n := range topicNames {
		b.WriteString("  repoctl help " + n + strings.Repeat(" ", 13-len(n)) + topicSummary[n] + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
