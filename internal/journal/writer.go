package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"

	"github.com/dennisklein/repoctl/internal/api"
)

// The rules of the writer (contract.html#writing).
const (
	// FlushEvery is how often the writer writes its buffered lines.
	FlushEvery = 250 * time.Millisecond
	// MaxLogBytes is the size of the journal after which the writer drops
	// node.log lines and writes the warning journal.sampling-stopped once.
	MaxLogBytes = 32 << 20
)

// Options configure a Writer.
type Options struct {
	// Now is the clock of the time key; nil is time.Now.
	Now func() time.Time
	// FlushEvery overrides FlushEvery; a negative value writes each line
	// at once.
	FlushEvery time.Duration
	// MaxLogBytes overrides MaxLogBytes.
	MaxLogBytes int64
}

// Writer appends the events of one run to its journal. It numbers the
// lines (seq from 1, without gaps), stamps them and writes whole lines
// only: it buffers them and writes them every 250 ms, and at once after a
// terminal node.state and after run.finished, with an fsync. It is safe for
// concurrent use.
//
// A write that fails is sticky: Err gives it, and the caller ends the run
// (build.html#failures).
type Writer struct {
	mu      sync.Mutex
	f       *os.File
	path    string
	run     api.RunId
	seq     int
	buf     bytes.Buffer
	size    int64
	now     func() time.Time
	maxLog  int64
	stopped bool
	err     error
	closed  bool
	quit    chan struct{}
	done    chan struct{}
}

// Create makes the journal of a run at path, with file mode 0600 in a
// directory of mode 0700, and gives its writer. The journal must not exist.
func Create(path string, run api.RunId, o Options) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := trusted(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	w := &Writer{f: f, path: path, run: run, now: o.Now, maxLog: o.MaxLogBytes, quit: make(chan struct{}), done: make(chan struct{})}
	if w.now == nil {
		w.now = time.Now
	}
	if w.maxLog == 0 {
		w.maxLog = MaxLogBytes
	}
	every := o.FlushEvery
	if every == 0 {
		every = FlushEvery
	}
	if every > 0 {
		go w.loop(every)
	} else {
		close(w.done)
	}
	return w, nil
}

// trusted checks that dir belongs to this user and that no other user can
// write it.
func trusted(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("the state directory %s belongs to another user", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("the state directory %s can be written by other users", dir)
	}
	return nil
}

func (w *Writer) loop(every time.Duration) {
	defer close(w.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-w.quit:
			return
		case <-t.C:
			w.mu.Lock()
			_ = w.flushLocked(false)
			w.mu.Unlock()
		}
	}
}

// Path gives the path of the journal.
func (w *Writer) Path() string { return w.path }

// Run gives the run id of the journal.
func (w *Writer) Run() api.RunId { return w.run }

// Seq gives the seq of the last line.
func (w *Writer) Seq() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seq
}

// Err gives the first error of a write, or nil.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Write appends one event: a pointer to an event struct of internal/api.
// It sets the keys v, run, seq and type, and time when the event has none.
// A node.log line is sanitized and cut to 512 bytes; once the journal
// holds 32 MiB, node.log lines are dropped and the warning
// journal.sampling-stopped is written once.
func (w *Writer) Write(ev api.Event) error {
	typ := api.TypeOfEvent(ev)
	if typ == "" {
		return fmt.Errorf("journal: %T is no event", ev)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if w.closed {
		return errors.New("journal: the writer is closed")
	}
	if typ == api.EventTypeNodeLog {
		nl := ev.(*api.NodeLogEvent)
		nl.Line = progress.Sanitize(nl.Line, progress.MaxText)
		if w.size+int64(w.buf.Len()) >= w.maxLog {
			if w.stopped {
				return nil
			}
			w.stopped = true
			warn := &api.WarningEvent{Code: api.PJournalSamplingStopped,
				Message: "the journal holds 32 MiB: repoctl stops the node.log lines of this run; the node logs keep the full text"}
			return w.appendLocked(warn, api.EventTypeWarning)
		}
	}
	if err := w.appendLocked(ev, typ); err != nil {
		return err
	}
	if terminalEvent(ev, typ) {
		return w.flushLocked(true)
	}
	return nil
}

func (w *Writer) appendLocked(ev api.Event, typ api.EventType) error {
	h := ev.Head()
	h.V = api.JournalVersion
	h.Run = w.run
	h.Seq = w.seq + 1
	h.Type = typ
	if h.Time == "" {
		h.Time = api.FormatTime(w.now())
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	w.seq++
	w.buf.Write(b)
	w.buf.WriteByte('\n')
	return nil
}

// terminalEvent reports a line after which the writer flushes at once: a
// terminal node.state, run.finished and publish.finished.
func terminalEvent(ev api.Event, typ api.EventType) bool {
	switch typ {
	case api.EventTypeRunFinished, api.EventTypePublishFinished:
		return true
	case api.EventTypeNodeState:
		return IsTerminal(ev.(*api.NodeStateEvent).State)
	}
	return false
}

// IsTerminal reports a terminal node state of a run: ok, failed, blocked
// or canceled.
func IsTerminal(s api.NodeState) bool {
	switch s {
	case api.NodeStateOk, api.NodeStateFailed, api.NodeStateBlocked, api.NodeStateCanceled:
		return true
	}
	return false
}

func (w *Writer) flushLocked(sync bool) error {
	if w.err != nil {
		return w.err
	}
	if w.buf.Len() > 0 {
		n, err := w.f.Write(w.buf.Bytes())
		w.size += int64(n)
		w.buf.Reset()
		if err != nil {
			w.err = fmt.Errorf("journal %s: %w", w.path, err)
			return w.err
		}
	}
	if sync {
		if err := w.f.Sync(); err != nil {
			w.err = fmt.Errorf("journal %s: %w", w.path, err)
			return w.err
		}
	}
	return nil
}

// Flush writes the buffered lines.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushLocked(false)
}

// Sync writes the buffered lines and syncs the file.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushLocked(true)
}

// Close writes the buffered lines, syncs and closes the journal.
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return w.err
	}
	w.closed = true
	close(w.quit)
	err := w.flushLocked(true)
	w.mu.Unlock()
	<-w.done
	if cerr := w.f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	return err
}
