package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
)

// The control socket ui.sock takes one JSON request per connection and
// gives one JSON answer, both with "v":1.
const (
	OpHello     = "hello"
	OpAddSet    = "add-set"
	OpRemoveSet = "remove-set"
	OpToken     = "token"
	OpStop      = "stop"
)

// ControlRequest is a request on the control socket.
type ControlRequest struct {
	V    int    `json:"v"`
	Op   string `json:"op"`
	Set  string `json:"set,omitempty"`
	Path string `json:"path,omitempty"`
	Next string `json:"next,omitempty"`
}

// ControlAnswer is the answer of the server.
type ControlAnswer struct {
	V       int      `json:"v"`
	OK      bool     `json:"ok"`
	Version string   `json:"version,omitempty"`
	PID     int      `json:"pid,omitempty"`
	Started string   `json:"started,omitempty"`
	URL     string   `json:"url,omitempty"`
	Added   bool     `json:"added,omitempty"`
	Removed bool     `json:"removed,omitempty"`
	Runs    []string `json:"runs,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// serveControl answers the control socket until ctx ends. base is the
// address of the web server, http://HOST:PORT; stop stops the server.
func (s *Server) serveControl(ctx context.Context, l net.Listener, base string, stop func()) {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go s.control(c, base, stop)
	}
}

func (s *Server) control(c net.Conn, base string, stop func()) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	var req ControlRequest
	ans := ControlAnswer{V: 1, OK: true, Version: s.o.Version, PID: osPID(), Started: api.FormatTime(s.started)}
	if err := json.NewDecoder(c).Decode(&req); err != nil || req.V != 1 {
		ans.OK, ans.Error = false, "a request needs v 1 and an op"
		_ = json.NewEncoder(c).Encode(ans)
		return
	}
	switch req.Op {
	case OpHello:
	case OpAddSet:
		added, err := s.AddSet(req.Set, req.Path)
		if err != nil {
			ans.OK, ans.Error = false, err.Error()
			break
		}
		ans.Added = added
		ans.URL = base + s.LaunchPath(setPath(req.Set))
	case OpToken:
		ans.URL = base + s.LaunchPath(req.Next)
	case OpRemoveSet:
		removed, err := s.RemoveSet(req.Set)
		if err != nil {
			ans.OK, ans.Error = false, err.Error()
		}
		ans.Removed = removed
	case OpStop:
		ans.Runs = s.uiRuns()
		defer stop()
	default:
		ans.OK, ans.Error = false, "unknown op "+req.Op
	}
	_ = json.NewEncoder(c).Encode(ans)
}

// uiRuns gives the active runs that a child of this server started.
func (s *Server) uiRuns() []string {
	var out []string
	for _, e := range s.sets.all() {
		for _, f := range s.runs.list(s.o.Paths.Runs(e.Name), 10) {
			if f.Active() && (f.Run.Origin.Via == api.RunOriginUI || f.Run.Origin.Session == s.SessionID()) {
				out = append(out, e.Name+" "+f.Run.ID)
			}
		}
	}
	return out
}

// Control sends one request to the server at the socket path.
func Control(socket string, req ControlRequest) (ControlAnswer, error) {
	var ans ControlAnswer
	req.V = 1
	c, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return ans, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return ans, err
	}
	if err := json.NewDecoder(c).Decode(&ans); err != nil {
		return ans, err
	}
	if !ans.OK {
		return ans, errors.New(ans.Error)
	}
	return ans, nil
}
