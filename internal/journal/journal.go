// Package journal is the event journal of a run: the JSON Lines file
// runs/<run>.jsonl in the state directory of the set
// (docs/design/contract.html#journal).
//
// The process that holds the set lock writes the journal with a Writer;
// for a check run, the process that holds the check lock. Every reader
// that shows node state folds the journal with Fold: repoctl runs,
// repoctl logs, the status, the web UI and the MCP server. Thus all
// surfaces show the same state.
//
// The package is generic for the kinds of run: build, publish, check and
// gc. The build writes the node events; publish, check and gc write their
// own events with the same Writer.
package journal

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The kinds of run (run.started kind).
const (
	KindBuild   = "build"
	KindPublish = "publish"
	KindCheck   = "check"
	KindGC      = "gc"
)

// runIDRe is the pattern of a run id of the contract (output.json RunId).
var runIDRe = regexp.MustCompile(`^[rp]-[0-9]{8}-[0-9]{4}-[0-9a-z]{3}$`)

// IsRunID reports whether s has the form of a run id: r-YYYYMMDD-HHMM-xxx
// for a build, check or gc run, p-YYYYMMDD-HHMM-xxx for a publish run.
func IsRunID(s string) bool { return runIDRe.MatchString(s) }

// PrefixOf gives the prefix of the ids of a kind of run: "p" for a
// publish, else "r".
func PrefixOf(kind string) string {
	if kind == KindPublish {
		return "p"
	}
	return "r"
}

// NewID gives a new run id of the kind at now, for example
// r-20261102-1750-a3f. xxx is random. When dir is not empty, the id is one
// that has no journal in dir yet.
func NewID(kind string, now time.Time, dir string) string {
	for {
		var b [2]byte
		_, _ = rand.Read(b[:])
		id := PrefixOf(kind) + "-" + now.Format("20060102-1504") + "-" + hex.EncodeToString(b[:])[:3]
		if dir == "" {
			return id
		}
		if _, err := os.Stat(filepath.Join(dir, id+".jsonl")); os.IsNotExist(err) {
			return id
		}
	}
}

// IDTime gives the time in a run id, or the zero time.
func IDTime(id string) time.Time {
	if !IsRunID(id) {
		return time.Time{}
	}
	t, err := time.ParseInLocation("20060102-1504", id[2:15], time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// IsJournal reports whether a file name of the runs directory is a
// journal (RUN.jsonl), not a progress log (RUN.progress.jsonl).
func IsJournal(name string) bool {
	return strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".progress.jsonl")
}
