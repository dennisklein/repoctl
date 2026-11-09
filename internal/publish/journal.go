package publish

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
)

// FileJournal is a Journal that appends JSON lines to a file
// (runs/RUN.jsonl).
type FileJournal struct {
	mu   sync.Mutex
	f    *os.File
	run  string
	seq  int
	now  func() time.Time
	Path string
}

// OpenJournal opens or creates the journal of a run for appending. now
// gives the time of each event.
func OpenJournal(path, run string, now func() time.Time) (*FileJournal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	seq := 0
	if b, err := os.ReadFile(path); err == nil {
		for _, c := range b {
			if c == '\n' {
				seq++
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &FileJournal{f: f, run: run, seq: seq, now: now, Path: path}, nil
}

// headerKeys are the keys of api.EventHeader, which the generated nested
// structs embed by mistake; Append drops their zero values below the top
// level.
var headerKeys = []string{"v", "run", "seq", "time", "type", "span", "parent", "node"}

// Append writes one event with its header.
func (j *FileJournal) Append(typ api.EventType, ev any) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for k, v := range m {
		m[k] = dropHeaders(v)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	m["v"] = 1
	m["run"] = j.run
	m["seq"] = j.seq
	m["time"] = api.FormatTime(j.now())
	m["type"] = string(typ)
	for _, k := range []string{"span", "parent", "node"} {
		if s, ok := m[k].(string); ok && s == "" {
			delete(m, k)
		}
	}
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = j.f.Write(append(line, '\n'))
	return err
}

func dropHeaders(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range headerKeys {
			switch y := x[k].(type) {
			case string:
				if y == "" {
					delete(x, k)
				}
			case float64:
				if y == 0 {
					delete(x, k)
				}
			}
		}
		for k, y := range x {
			x[k] = dropHeaders(y)
		}
	case []any:
		for i, y := range x {
			x[i] = dropHeaders(y)
		}
	}
	return v
}

// Close closes the file.
func (j *FileJournal) Close() error {
	if j == nil || j.f == nil {
		return nil
	}
	return j.f.Close()
}
