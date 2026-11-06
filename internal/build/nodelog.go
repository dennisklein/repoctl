package build

import (
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"

	"github.com/dennisklein/repoctl/internal/api"
)

// Marker starts each line that repoctl writes into a node log.
const Marker = "── repoctl ── "

// ErrorLine matches a line of mock output that names an error: the
// journal sends each such line, and repoctl logs marks it.
var ErrorLine = regexp.MustCompile(`(?i)(^|[\s:])(error|fatal)(:|\s)|No matching package to install|Failed build dependencies|Bad exit status|Command failed`)

// nodeLog is the log of a node, logs/RUN/NODE.log: the markers of repoctl
// and a tee of the mock output. It samples the lines into node.log events:
// at most one line every SampleEvery, plus each error line
// (contract.html#events).
type nodeLog struct {
	mu       sync.Mutex
	r        *runner
	n        *node
	f        *os.File
	off      int64
	step     api.Step
	span     *progress.Span
	part     []byte
	partOff  int64
	lastSent time.Time
	dropped  int
	last     time.Time
}

func (r *runner) openNodeLog(n *node) *nodeLog {
	l := &nodeLog{r: r, n: n, last: r.now()}
	path := r.o.Paths.NodeLog(r.o.Set.Name, r.o.RunID, n.id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			l.f = f
			if fi, err := f.Stat(); err == nil {
				l.off = fi.Size()
			}
		}
	}
	return l
}

// setStep names the step of the lines that follow.
func (l *nodeLog) setStep(s api.Step, span *progress.Span) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.step, l.span = s, span
}

func (l *nodeLog) curSpan() *progress.Span {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.span
}

// Last gives the time of the last line.
func (l *nodeLog) Last() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// Write takes output of mock.
func (l *nodeLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last = l.r.now()
	if l.f != nil {
		_, _ = l.f.Write(p)
	}
	if len(l.part) == 0 {
		l.partOff = l.off
	}
	l.off += int64(len(p))
	l.part = append(l.part, p...)
	for {
		i := indexByte(l.part, '\n')
		if i < 0 {
			break
		}
		line := string(l.part[:i])
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		l.sample(line, l.partOff)
		l.partOff += int64(i + 1)
		l.part = l.part[i+1:]
	}
	if len(l.part) > 64<<10 {
		l.part = l.part[:0]
	}
	return len(p), nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func (l *nodeLog) sample(line string, off int64) {
	isErr := ErrorLine.MatchString(line)
	now := l.r.now()
	if !isErr && now.Sub(l.lastSent) < l.r.o.SampleEvery {
		l.dropped++
		return
	}
	ev := &api.NodeLogEvent{Step: l.step, Line: line, Offset: int(off), Dropped: l.dropped}
	if isErr {
		ev.Level = "error"
	}
	l.r.emit(l.n, l.span, nil, ev)
	l.lastSent, l.dropped = now, 0
}

// Marker writes a line of repoctl into the log.
func (l *nodeLog) Marker(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	s := Marker + text + "\n"
	if len(l.part) > 0 {
		s = "\n" + s
		l.part = l.part[:0]
	}
	_, _ = l.f.WriteString(s)
	l.off += int64(len(s))
}

// errorLine writes a line of repoctl that the journal sends as an error,
// for example a time limit.
func (l *nodeLog) errorLine(text string) {
	l.mu.Lock()
	step, span := l.step, l.span
	off := l.off
	l.mu.Unlock()
	l.Marker(text)
	l.r.emit(l.n, span, nil, &api.NodeLogEvent{Step: step, Line: Marker + text, Offset: int(off), Level: "error"})
}

// Close closes the log file.
func (l *nodeLog) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}
