package ui

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/localweb"
	"github.com/GSI-HPC/go-clikit/localweb/sse"
)

// The timing of the stream (design: ui.html#live).
const (
	streamRetry     = 5 * time.Second
	streamHeartbeat = 15 * time.Second
	streamCoalesce  = 300 * time.Millisecond
	logBatch        = 200 * time.Millisecond
	logBatchBytes   = 64 << 10
	logWindow       = 2000
)

// handleEvents is the one event stream of a tab: /-/events?page=sets on
// the host pages, /s/SET/-/events?page=KIND&… on the pages of a set. It
// sends the regions of the page that changed, each as the whole state of
// its region, a heartbeat every 15 s, the lines of a node log as they come,
// and bye when the server stops.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("page")
	if kind == "" {
		kind = "sets"
	}
	b, ok := builders[kind]
	if !ok {
		http.Error(w, "No such page.", http.StatusNotFound)
		return
	}
	pr := &pageReq{s: s, r: r, ctx: r.Context(), params: q}
	if name := r.PathValue("set"); name != "" {
		sc, ok := s.setOf(name)
		if !ok {
			http.Error(w, "No such set.", http.StatusNotFound)
			return
		}
		pr.set = sc
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sub := s.notify.Subscribe()
	defer sub.Close()

	st := &stream{w: w, rc: rc, hashes: map[string][32]byte{}}
	st.write(fmt.Sprintf("retry: %d\n\n", streamRetry.Milliseconds()))
	render := func(send bool) {
		pr.set = s.refreshSet(pr.set)
		bt := b(pr)
		regions := append(bt.Live, s.commonRegions(pr)...)
		for _, reg := range regions {
			html, err := s.pages.Render(reg.Block, reg.Data)
			if err != nil {
				s.logf("render %s: %v", reg.Block, err)
				continue
			}
			sum := sha256.Sum256([]byte(html))
			if old, ok := st.hashes[reg.Event]; ok && old == sum {
				continue
			}
			st.hashes[reg.Event] = sum
			if send {
				st.write(sse.Format(sse.Event{Name: reg.Event, Data: html}))
			}
		}
		st.flush()
	}
	render(false)

	var tail *logFollower
	if kind == "node" && pr.set != nil {
		tail = s.follow(pr, r)
	}
	hb := time.NewTicker(streamHeartbeat)
	defer hb.Stop()
	var logTick <-chan time.Time
	if tail != nil {
		t := time.NewTicker(logBatch)
		defer t.Stop()
		logTick = t.C
	}
	var coalesce <-chan time.Time
	stopping := localweb.Stopping(r.Context())
	for st.err == nil {
		select {
		case <-r.Context().Done():
			return
		case <-stopping:
			st.write(sse.Format(sse.Event{Name: "bye", Data: ""}))
			st.flush()
			return
		case <-s.stopping:
			st.write(sse.Format(sse.Event{Name: "bye", Data: ""}))
			st.flush()
			return
		case <-sub.Changed():
			sub.Take()
			if coalesce == nil {
				coalesce = time.After(streamCoalesce)
			}
		case <-coalesce:
			coalesce = nil
			render(true)
		case <-hb.C:
			st.write(sse.Format(sse.Event{Name: "hb", Data: ""}))
			st.flush()
		case <-logTick:
			if ev := tail.next(s); ev != "" {
				st.write(ev)
				st.flush()
			}
		}
	}
}

// stream writes the events of one connection and keeps the hash of each
// region it sent.
type stream struct {
	w      io.Writer
	rc     *http.ResponseController
	hashes map[string][32]byte
	err    error
}

func (st *stream) write(s string) {
	if st.err != nil {
		return
	}
	_, st.err = io.WriteString(st.w, s)
}

func (st *stream) flush() {
	if st.err == nil {
		st.err = st.rc.Flush()
	}
}

// refreshSet loads the set's repoctl.yaml again, as it may have changed.
func (s *Server) refreshSet(sc *setCtx) *setCtx {
	if sc == nil {
		return nil
	}
	if n, ok := s.setOf(sc.Name); ok {
		return n
	}
	return sc
}

// commonRegions are the regions of every page: the run chip, the run
// count of the side bar, the set menu and the agent drawers of the set.
func (s *Server) commonRegions(pr *pageReq) []liveRegion {
	out := []liveRegion{{Event: "sets", Block: "set-menu-items", Data: s.setMenu(pr.set)}}
	if pr.set == nil {
		return out
	}
	out = append(out,
		liveRegion{Event: "chip", Block: "chip", Data: s.chip(pr.set)},
		liveRegion{Event: "nav-runs", Block: "nav-runs", Data: s.navRuns(pr.set)},
	)
	for _, a := range s.agents.ofSet(pr.set.Name) {
		out = append(out, liveRegion{Event: "agent:" + a.ID, Block: "agent-body", Data: s.agentView(a)})
	}
	return out
}

// handleFrag renders one region of a page: /s/SET/-/frag/REGION?page=KIND.
func (s *Server) handleFrag(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.setOf(r.PathValue("set"))
	if !ok {
		http.Error(w, "No such set.", http.StatusNotFound)
		return
	}
	region := r.PathValue("region")
	pr := &pageReq{s: s, r: r, ctx: r.Context(), set: sc, params: r.URL.Query()}
	if region == "jump" {
		s.respond(w, r, http.StatusOK, "jump-results", s.jump(pr, r.URL.Query().Get("q")))
		return
	}
	kind := r.URL.Query().Get("page")
	if kind == "" {
		kind = "overview"
	}
	b, ok := builders[kind]
	if !ok {
		http.Error(w, "No such page.", http.StatusNotFound)
		return
	}
	bt := b(pr)
	for _, reg := range append(bt.Live, s.commonRegions(pr)...) {
		if reg.Event == region {
			s.respond(w, r, http.StatusOK, reg.Block, reg.Data)
			return
		}
	}
	http.Error(w, "No such region.", http.StatusNotFound)
}

// logTail is the end of a node log as a page shows it.
type logTail struct {
	Node      string
	Path      string
	Lines     []logLine
	End       int64
	NextLine  int
	Truncated bool
	Missing   bool
}

// logLine is one line of a log view.
type logLine struct {
	N     int
	Text  string
	Class string
}

// readLogTail reads the last logWindow lines of the log at path.
func readLogTail(node, path string) *logTail {
	t := &logTail{Node: node, Path: path, NextLine: 1}
	b, err := os.ReadFile(path) //nolint:gosec // G304: a log of the user's state directory
	if err != nil {
		t.Missing = true
		return t
	}
	// Only whole lines: the last one may be half written.
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		b = b[:i+1]
	} else {
		b = nil
	}
	t.End = int64(len(b))
	all := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(b) == 0 {
		all = nil
	}
	first := 0
	if len(all) > logWindow {
		first = len(all) - logWindow
		t.Truncated = true
	}
	for i := first; i < len(all); i++ {
		t.Lines = append(t.Lines, logLine{N: i + 1, Text: all[i], Class: lineClass(all[i])})
	}
	t.NextLine = len(all) + 1
	return t
}

// lineClass marks warnings and errors of a build log.
func lineClass(l string) string {
	low := strings.ToLower(l)
	switch {
	case strings.HasPrefix(low, "error") || strings.Contains(low, " error:") || strings.Contains(low, "failed build dependencies"):
		return "e"
	case strings.HasPrefix(low, "warning") || strings.Contains(low, " warning:"):
		return "w"
	}
	return ""
}

// logFollower sends the lines that a node log gets, from a byte offset.
type logFollower struct {
	node string
	path string
	off  int64
	line int
}

// follow starts at the Last-Event-ID of a stream that reconnects, else at
// the offset the page was rendered with.
func (s *Server) follow(pr *pageReq, r *http.Request) *logFollower {
	run, node := pr.p("run"), pr.p("node")
	if run == "" || node == "" {
		return nil
	}
	f := &logFollower{node: node, path: s.o.Paths.NodeLog(pr.set.Name, run, node)}
	from := r.Header.Get("Last-Event-ID")
	if from == "" {
		from = pr.params.Get("from")
	}
	f.off, _ = strconv.ParseInt(from, 10, 64)
	f.line = 1
	if f.off > 0 {
		b, err := os.ReadFile(f.path)
		if err != nil || int64(len(b)) < f.off {
			f.off = 0
		} else {
			f.line = bytes.Count(b[:f.off], []byte{'\n'}) + 1
		}
	}
	return f
}

// next gives the event of the lines added since the last call, or "".
func (f *logFollower) next(s *Server) string {
	fh, err := os.Open(f.path)
	if err != nil {
		return ""
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil || st.Size() <= f.off {
		if err == nil && st.Size() < f.off {
			// The log was replaced: the page reads it anew.
			f.off, f.line = 0, 1
			return sse.Format(sse.Event{Name: "reload", Data: ""})
		}
		return ""
	}
	buf := make([]byte, logBatchBytes)
	n, _ := fh.ReadAt(buf, f.off)
	buf = buf[:n]
	i := bytes.LastIndexByte(buf, '\n')
	if i < 0 {
		return ""
	}
	buf = buf[:i+1]
	var lines []logLine
	for _, l := range strings.Split(strings.TrimSuffix(string(buf), "\n"), "\n") {
		lines = append(lines, logLine{N: f.line, Text: l, Class: lineClass(l)})
		f.line++
	}
	f.off += int64(len(buf))
	html, err := s.pages.Render("log-lines", lines)
	if err != nil {
		return ""
	}
	return "id: " + strconv.FormatInt(f.off, 10) + "\n" + sse.Format(sse.Event{Name: "log:" + f.node, Data: html})
}
