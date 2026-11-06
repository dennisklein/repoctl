package advisory

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// Journal gets the events of a check run: run.started with kind check,
// advisory and advisory.gone, and run.finished (contract.html#journal).
//
// JOURNAL HOOK: internal/journal (another part of repoctl) owns the run
// journal. Until the check writes through it, FileJournal writes the same
// lines of events.schema.json itself. Replace FileJournal with an adapter
// of internal/journal when it lands; the interface stays.
type Journal interface {
	Started(run api.RunId, set string, at time.Time) error
	Advisory(a api.Advisory) error
	Gone(id string) error
	Finished(state api.RunState, d time.Duration, exitCode int) error
}

// Origin is the origin of a run.started line.
type Origin struct {
	Via  string // cli, ui, mcp, ci
	User string
	Host string
	PID  int
}

// FileJournal writes the journal of a check run as JSON lines to
// runs/RUN.jsonl of the set.
type FileJournal struct {
	Paths   config.Paths
	Origin  Origin
	Repoctl string
	Now     func() time.Time

	mu  sync.Mutex
	f   *os.File
	run api.RunId
	seq int
}

type header struct {
	V    int           `json:"v"`
	Run  api.RunId     `json:"run"`
	Seq  int           `json:"seq"`
	Time api.Time      `json:"time"`
	Type api.EventType `json:"type"`
}

func (j *FileJournal) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *FileJournal) write(typ api.EventType, body map[string]any) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	j.seq++
	h := header{V: api.JournalVersion, Run: j.run, Seq: j.seq, Time: api.FormatTime(j.now()), Type: typ}
	line := map[string]any{"v": h.V, "run": h.Run, "seq": h.Seq, "time": h.Time, "type": h.Type}
	for k, v := range body {
		line[k] = v
	}
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if typ == api.EventTypeRunFinished {
		err := j.f.Close()
		j.f = nil
		return err
	}
	return nil
}

// Started opens runs/RUN.jsonl and writes run.started.
func (j *FileJournal) Started(run api.RunId, set string, _ time.Time) error {
	path := j.Paths.Journal(set, run)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.f, j.run, j.seq = f, run, 0
	j.mu.Unlock()
	trace := make([]byte, 16)
	_, _ = rand.Read(trace)
	origin := map[string]any{"via": j.Origin.Via, "user": j.Origin.User, "host": j.Origin.Host}
	if j.Origin.PID != 0 {
		origin["pid"] = j.Origin.PID
	}
	if origin["via"] == "" {
		origin["via"] = "cli"
	}
	body := map[string]any{"set": set, "kind": "check", "trace": hex.EncodeToString(trace), "origin": origin}
	if j.Repoctl != "" {
		body["repoctl"] = j.Repoctl
	}
	return j.write(api.EventTypeRunStarted, body)
}

// Advisory writes an advisory event.
func (j *FileJournal) Advisory(a api.Advisory) error {
	return j.write(api.EventTypeAdvisory, map[string]any{"id": a.ID, "state": a.State, "level": a.Level, "factsDigest": a.FactsDigest})
}

// Gone writes advisory.gone.
func (j *FileJournal) Gone(id string) error {
	return j.write(api.EventTypeAdvisoryGone, map[string]any{"id": id})
}

// Finished writes run.finished and closes the file.
func (j *FileJournal) Finished(state api.RunState, d time.Duration, exitCode int) error {
	return j.write(api.EventTypeRunFinished, map[string]any{
		"state": state, "counts": api.StateCounts{}, "durationMs": int(d / time.Millisecond), "exitCode": exitCode,
	})
}
