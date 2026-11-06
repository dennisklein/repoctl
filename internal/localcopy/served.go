package localcopy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// The files that the mirror job writes into the served tree of a mirror
// line (publish.html#mirror).
const (
	// ServedFile names the snapshot that the HTTP server serves.
	ServedFile = "repoctl-snapshot.json"
	// MirrorFile is the heartbeat of the mirror line (job health).
	MirrorFile = "repoctl-mirror.json"
)

// StaleAfter is the age of the two rules of served.stale: three runs of
// the mirror job (publish.html#stale-served).
const StaleAfter = 15 * time.Minute

// ServedTimeout bounds one read of the served tree.
const ServedTimeout = 10 * time.Second

// ServedSnapshot is repoctl-snapshot.json of a served tree.
type ServedSnapshot struct {
	Set     string     `json:"set"`
	Target  string     `json:"target"`
	Digest  api.Digest `json:"digest"`
	Publish string     `json:"publish"`
	Pulled  string     `json:"pulled,omitempty"`
}

// Heartbeat is repoctl-mirror.json of a served tree: what the last run of
// the mirror job did for the line.
type Heartbeat struct {
	Set     string `json:"set"`
	Target  string `json:"target"`
	Checked string `json:"checked"`
	// Result is same, swapped or failed.
	Result string     `json:"result"`
	Served api.Digest `json:"served,omitempty"`
	Latest api.Digest `json:"latest,omitempty"`
	// BehindSince is the time of the first run that saw a latest other
	// than the served digest; nil when the two are the same.
	BehindSince *string `json:"behindSince"`
	// Error is the message of the failed step.
	Error *string `json:"error"`
}

// CheckedTime gives the time of the run, or the zero time.
func (h *Heartbeat) CheckedTime() time.Time { return parseTime(h.Checked) }

// BehindTime gives behindSince, or the zero time.
func (h *Heartbeat) BehindTime() time.Time {
	if h.BehindSince == nil {
		return time.Time{}
	}
	return parseTime(*h.BehindSince)
}

// LastError gives the message of the failed step, or "".
func (h *Heartbeat) LastError() string {
	if h.Error == nil {
		return ""
	}
	return strings.TrimSpace(*h.Error)
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Stale reports the warning served.stale at now: the served tree is behind
// latest since more than StaleAfter, or the job has not checked the line
// for more than StaleAfter (publish.html#stale-served).
func (h *Heartbeat) Stale(now time.Time) bool {
	if c := h.CheckedTime(); c.IsZero() || now.Sub(c) > StaleAfter {
		return true
	}
	b := h.BehindTime()
	return !b.IsZero() && now.Sub(b) > StaleAfter
}

// StaleWarning gives the warning served.stale of the line of target, or
// nil. clock formats a time for the message, for example 14:15.
func (h *Heartbeat) StaleWarning(target string, now time.Time, clock func(time.Time) string) *api.Problem {
	if !h.Stale(now) {
		return nil
	}
	var msg string
	if b := h.BehindTime(); !b.IsZero() && now.Sub(b) > StaleAfter {
		msg = fmt.Sprintf("%s is behind since %s", target, clock(b))
	} else if c := h.CheckedTime(); c.IsZero() {
		msg = fmt.Sprintf("the mirror job wrote no valid heartbeat for %s", target)
	} else {
		msg = fmt.Sprintf("the mirror job has not checked %s since %s", target, clock(c))
	}
	msg += "."
	if e := h.LastError(); e != "" {
		msg += " Last error of the mirror job: " + e
	}
	return &api.Problem{Code: api.PServedStale, Target: target, Message: msg, Hint: StaleHint()}
}

// StaleHint gives the hint of served.stale.
func StaleHint() string {
	return api.HintText("read the journal of the mirror job on the HTTP server", "journalctl -u repoctl-mirror.service")
}

// API gives the mirror object of StatusSnapshot, with the times in the
// form of the contract. A heartbeat without a valid checked time or result
// gives nil: status cannot read it.
func (h *Heartbeat) API(now time.Time) *api.StatusSnapshotMirror {
	c := h.CheckedTime()
	switch h.Result {
	case "same", "swapped", "failed":
	default:
		return nil
	}
	if c.IsZero() {
		return nil
	}
	m := &api.StatusSnapshotMirror{Checked: api.FormatTime(c), Result: h.Result, Error: h.Error, Stale: h.Stale(now)}
	if b := h.BehindTime(); !b.IsZero() {
		bt := api.FormatTime(b)
		m.BehindSince = &bt
	}
	return m
}

// Served is what the served tree of a target says: its snapshot and the
// heartbeat of its mirror line. A file that the server does not have is
// nil; Err is set when the server does not answer or a file is not valid.
type Served struct {
	Target   string
	URL      string
	Snapshot *ServedSnapshot
	Mirror   *Heartbeat
	Err      error
}

// ServedURL gives the served tree of a target: <repo.url>/<target.path>,
// or "" when repo.url is not set.
func ServedURL(set *config.Set, target string) string {
	if set == nil || set.Repo.URL == "" {
		return ""
	}
	path := target
	if t, ok := set.Target(target); ok {
		path = t.ClientPath()
	}
	return strings.TrimSuffix(set.Repo.URL, "/") + "/" + strings.Trim(path, "/")
}

// ReadServed reads repoctl-snapshot.json and repoctl-mirror.json of the
// served tree of each target, at the same time. A nil client uses one
// with ServedTimeout. Without repo.url, it gives nil.
func ReadServed(ctx context.Context, hc *http.Client, set *config.Set, targets []string) []Served {
	if set == nil || set.Repo.URL == "" || len(targets) == 0 {
		return nil
	}
	if hc == nil {
		hc = &http.Client{Timeout: ServedTimeout}
	}
	out := make([]Served, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = readServed(ctx, hc, set, t)
		}()
	}
	wg.Wait()
	return out
}

func readServed(ctx context.Context, hc *http.Client, set *config.Set, target string) Served {
	s := Served{Target: target, URL: ServedURL(set, target)}
	var snap ServedSnapshot
	ok, err := getJSON(ctx, hc, s.URL+"/"+ServedFile, &snap)
	if err != nil {
		s.Err = err
		return s
	}
	if ok {
		s.Snapshot = &snap
	}
	var hb Heartbeat
	ok, err = getJSON(ctx, hc, s.URL+"/"+MirrorFile, &hb)
	if err != nil {
		s.Err = err
		return s
	}
	if ok {
		s.Mirror = &hb
	}
	return s
}

// getJSON reads a JSON file over HTTP: false without an error for 404.
func getJSON(ctx context.Context, hc *http.Client, url string, v any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "repoctl/"+config.Version)
	resp, err := hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return false, nil
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("%s answers %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("%s is not valid JSON: %w", url, err)
	}
	return true, nil
}

// ServedState gives the word of the served tree in repoctl status: same
// when it serves latest, behind when it serves another snapshot, unknown
// when status cannot tell (contract.html#enums). latest is the digest of
// latest, or "" when it is not known.
func ServedState(s *Served, latest api.Digest) string {
	if s == nil || s.Snapshot == nil || latest == "" {
		return "unknown"
	}
	if s.Snapshot.Digest == latest {
		return "same"
	}
	return "behind"
}
