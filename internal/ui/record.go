package ui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Record is ui.json in the runtime directory: who serves, where, which
// version. The server holds an exclusive flock on the file while it runs.
type Record struct {
	PID     int    `json:"pid"`
	Listen  string `json:"listen"`
	Port    int    `json:"port"`
	URL     string `json:"url"`
	Version string `json:"version"`
	Started string `json:"started"`
	Session string `json:"session"`
}

// errHeld says that another process holds ui.json.
var errHeld = errors.New("another process holds ui.json")

// ReadRecord reads ui.json at path. ok is false when there is none, or
// when no server holds it (a server that was killed leaves it behind).
func ReadRecord(path string) (Record, bool) {
	var r Record
	b, err := os.ReadFile(path) //nolint:gosec // G304: the user's runtime file
	if err != nil || json.Unmarshal(b, &r) != nil || r.PID == 0 {
		return r, false
	}
	return r, recordHeld(path)
}

// recordHeld reports whether a process holds the flock on ui.json.
func recordHeld(path string) bool {
	f, err := os.Open(path) //nolint:gosec // G304: the user's runtime file
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// recordFile is ui.json held by this server.
type recordFile struct {
	path string
	f    *os.File
}

// lockRecord takes the flock on ui.json, or gives errHeld.
func lockRecord(path string) (*recordFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: the user's runtime file
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errHeld
		}
		return nil, err
	}
	return &recordFile{path: path, f: f}, nil
}

// write writes the record in place, in the file that holds the flock.
func (r *recordFile) write(rec Record) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := r.f.Truncate(0); err != nil {
		return err
	}
	if _, err := r.f.WriteAt(append(b, '\n'), 0); err != nil {
		return err
	}
	return r.f.Sync()
}

// release removes ui.json and lets the flock go.
func (r *recordFile) release() {
	_ = os.Remove(r.path)
	_ = r.f.Close()
}
