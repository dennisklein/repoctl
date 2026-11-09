package ui

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/GSI-HPC/go-clikit/localweb/page"

	"github.com/dennisklein/repoctl/internal/api"
)

// Confirmation is what the UI passes to `repoctl publish --confirm-fd 3`
// and `repoctl sign deployed --confirm-fd 3`
// on file descriptor 3 (design: cli.html#ui-confirmation): the set name the
// human typed, the advisories accepted with their reason and the digest of
// the plan the human reviewed. The UI never passes --yes.
type Confirmation struct {
	V          int      `json:"v"`
	Command    string   `json:"command"`
	Set        string   `json:"set"`
	Targets    []string `json:"targets,omitempty"`
	Run        string   `json:"run,omitempty"`
	Accept     []string `json:"accept,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Keys       []string `json:"keys,omitempty"`
	PlanDigest string   `json:"planDigest,omitempty"`
	User       string   `json:"user,omitempty"`
}

// Child environment variables that tell a run it was started by the UI.
const (
	EnvOrigin        = "REPOCTL_ORIGIN"
	EnvOriginSession = "REPOCTL_ORIGIN_SESSION"
)

// dialogView is the data of a dialog.
type dialogView struct {
	Set      *setCtx
	Name     string
	Q        url.Values
	Busy     string
	Publish  *publishPage
	Targets  []string
	Packages []string
}

// handleDialog renders a dialog into #modal.
func (s *Server) handleDialog(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.setOf(r.PathValue("set"))
	if !ok {
		s.fail(w, r, http.StatusNotFound, "No such set.")
		return
	}
	name := r.PathValue("name")
	if !s.pages.Has("dialog-" + name) {
		s.fail(w, r, http.StatusNotFound, "No such dialog.")
		return
	}
	d := dialogView{Set: sc, Name: name, Q: r.URL.Query(), Busy: s.busy(sc), Targets: sc.Targets()}
	if sc.Conf != nil {
		for _, p := range sc.Conf.Packages {
			d.Packages = append(d.Packages, p.ID)
		}
	}
	if name == "publish" {
		pr := &pageReq{s: s, r: r, ctx: r.Context(), set: sc, params: r.URL.Query()}
		b := buildPublish(pr)
		p := b.Data.(publishPage)
		d.Publish = &p
	}
	s.respond(w, r, http.StatusOK, "dialog-"+name, d)
}

// action is one verb of /s/SET/-/actions/VERB: the command line of the
// child, and whether the child starts a run that the page follows.
type action struct {
	args    func(f url.Values) ([]string, error)
	run     bool
	lock    bool
	publish bool
	// confirm gives the confirmation for file descriptor 3 of a verb that
	// is not publish.
	confirm func(f url.Values, set, user string) ([]byte, error)
}

var actions = map[string]action{
	"build": {run: true, lock: true, args: func(f url.Values) ([]string, error) {
		a := append([]string{"build"}, strings.Fields(f.Get("packages"))...)
		if t := f.Get("target"); t != "" {
			a = append(a, "--target", t)
		}
		if f.Get("force") != "" {
			a = append(a, "--force")
		}
		if f.Get("from") == "worktree" {
			a = append(a, "--from", "worktree")
		}
		return a, nil
	}},
	"cancel": {args: func(f url.Values) ([]string, error) {
		a := []string{"cancel"}
		if run := f.Get("run"); run != "" {
			a = append(a, run)
		}
		if f.Get("force") != "" {
			a = append(a, "--force")
		}
		return a, nil
	}},
	"check": {args: func(url.Values) ([]string, error) { return []string{"check"}, nil }},
	"checkout": {args: func(f url.Values) ([]string, error) {
		return append([]string{"checkout"}, strings.Fields(f.Get("packages"))...), nil
	}},
	"pin": {args: func(f url.Values) ([]string, error) {
		return append([]string{"pin"}, strings.Fields(f.Get("packages"))...), nil
	}},
	"rebuild": {lock: true, args: func(f url.Values) ([]string, error) {
		pkgs := strings.Fields(f.Get("packages"))
		if len(pkgs) == 0 {
			return nil, errors.New("name the packages to rebuild")
		}
		if len(strings.TrimSpace(f.Get("reason"))) < 8 {
			return nil, errors.New("the reason needs 8 characters or more")
		}
		return append(append([]string{"rebuild"}, pkgs...), "--reason", f.Get("reason")), nil
	}},
	"advisory-dismiss": {args: func(f url.Values) ([]string, error) {
		if len(strings.TrimSpace(f.Get("note"))) < 8 {
			return nil, errors.New("the note needs 8 characters or more")
		}
		return []string{"advisories", "dismiss", f.Get("advisory"), "--note", f.Get("note")}, nil
	}},
	"advisory-undismiss": {args: func(f url.Values) ([]string, error) {
		return []string{"advisories", "undismiss", f.Get("advisory")}, nil
	}},
	// sign deployed takes the key id that the human typed, the targets and
	// the release on file descriptor 3, as publish does
	// (signing.html#deployment-record, cli.html#ui-confirmation).
	"sign-deployed": {args: func(f url.Values) ([]string, error) {
		key := f.Get("key")
		if key == "" || f.Get("typed") != key {
			return nil, errors.New("type the key id to confirm")
		}
		targets := f["target"]
		if len(targets) == 0 {
			return nil, errors.New("choose the targets whose clients have the release")
		}
		rel := strings.TrimSpace(f.Get("release"))
		if rel == "" {
			return nil, errors.New("give the release package that the clients have, for example slurm-release-26.05-3")
		}
		a := []string{"sign", "deployed", key, "--confirm-fd", "3", "--release", rel}
		for _, t := range targets {
			a = append(a, "--target", t)
		}
		if note := f.Get("note"); note != "" {
			a = append(a, "--note", note)
		}
		return a, nil
	}, confirm: func(f url.Values, set, user string) ([]byte, error) {
		return json.Marshal(Confirmation{V: 1, Command: "sign deployed", Set: set, Keys: []string{f.Get("typed")}, Targets: f["target"], User: user})
	}},
	"publish": {run: true, lock: true, publish: true, args: func(f url.Values) ([]string, error) {
		a := []string{"publish", "--confirm-fd", "3"}
		if t := f.Get("target"); t != "" {
			a = append(a, "--target", t)
		}
		if run := f.Get("run"); run != "" {
			a = append(a, "--run", run)
		}
		for _, id := range f["accept"] {
			a = append(a, "--accept-advisory", id)
		}
		if len(f["accept"]) > 0 {
			a = append(a, "--reason", f.Get("reason"))
		}
		return a, nil
	}},
}

// handleAction starts the child of a verb. A verb that starts a run waits
// for its journal and sends the page there; the others wait for the
// child and show its outcome as a toast.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.setOf(r.PathValue("set"))
	if !ok {
		s.fail(w, r, http.StatusNotFound, "No such set.")
		return
	}
	verb := r.PathValue("verb")
	if verb == "advisory-accept" {
		page.Redirect(w, r, "/s/"+url.PathEscape(sc.Name)+"/publish?accept="+url.QueryEscape(r.PostFormValue("advisory")))
		return
	}
	act, ok := actions[verb]
	if !ok {
		s.fail(w, r, http.StatusNotFound, "No such action.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	args, err := act.args(r.PostForm)
	if err != nil {
		s.toast(w, http.StatusUnprocessableEntity, "err", err.Error())
		return
	}
	if act.lock {
		if b := s.busy(sc); b != "" {
			s.toast(w, http.StatusConflict, "err", b)
			return
		}
	}
	var confirm []byte
	switch {
	case act.publish:
		confirm, err = s.confirmation(r, sc)
	case act.confirm != nil:
		confirm, err = act.confirm(r.PostForm, sc.Name, s.o.User)
	}
	if err != nil {
		s.toast(w, http.StatusConflict, "err", err.Error())
		return
	}
	c, err := s.startChild(sc, verb, args, confirm)
	if err != nil {
		s.toast(w, http.StatusConflict, "err", err.Error())
		return
	}
	s.logf("%s: started repoctl %s (pid %d)", sc.Name, strings.Join(args, " "), c.pid)
	if act.run {
		if run, done := s.waitRun(r.Context(), sc, c, 15*time.Second); run != "" {
			page.Redirect(w, r, runHref(sc.Name, run))
			return
		} else if !done {
			s.toast(w, http.StatusOK, "info", "repoctl "+verb+" runs (pid "+fmt.Sprint(c.pid)+"). Its run shows on the Runs page when it starts.")
			return
		}
	} else {
		select {
		case <-c.done:
		case <-time.After(30 * time.Second):
			s.toast(w, http.StatusOK, "info", "repoctl "+strings.Join(args[:min(2, len(args))], " ")+" still runs.")
			return
		case <-r.Context().Done():
			return
		}
	}
	s.src.Invalidate(sc.Dir)
	s.notify.Notify("set:" + sc.Name)
	switch c.exit {
	case 0:
		w.Header().Set("HX-Refresh", "true")
		s.toast(w, http.StatusOK, "ok", "repoctl "+strings.Join(args[:min(2, len(args))], " ")+" is done.")
	case api.ExitBusy:
		s.toast(w, http.StatusConflict, "err", "The set is busy (set.busy): another command holds its lock.")
	default:
		s.toast(w, http.StatusUnprocessableEntity, "err", c.failure())
	}
}

// confirmation checks the typed set name and the plan digest of a publish
// and gives the JSON for file descriptor 3.
func (s *Server) confirmation(r *http.Request, sc *setCtx) ([]byte, error) {
	if r.PostFormValue("typed") != sc.Name {
		return nil, errors.New("type the set name " + sc.Name + " to confirm the publish")
	}
	if len(r.PostForm["accept"]) > 0 && len(strings.TrimSpace(r.PostFormValue("reason"))) < 8 {
		return nil, errors.New("the reason of an accepted advisory needs 8 characters or more")
	}
	digest := r.PostFormValue("planDigest")
	// The plan may have changed since the review: check it again, uncached.
	args := []string{"publish", "--dry-run"}
	if t := r.PostFormValue("target"); t != "" {
		args = append(args, "--target", t)
	}
	if run := r.PostFormValue("run"); run != "" {
		args = append(args, "--run", run)
	}
	res := s.o.Source.Run(r.Context(), sc.Dir, args...)
	if plan, ok := First[api.Run](res); ok && digest != "" && plan.PlanDigest != "" && plan.PlanDigest != digest {
		return nil, errors.New("the plan changed since the review (publish.plan-changed). Review it again")
	}
	c := Confirmation{V: 1, Command: "publish", Set: r.PostFormValue("typed"), Run: r.PostFormValue("run"), Accept: r.PostForm["accept"],
		Reason: r.PostFormValue("reason"), Keys: r.PostForm["key"], PlanDigest: digest, User: s.o.User}
	if t := r.PostFormValue("target"); t != "" {
		c.Targets = []string{t}
	}
	return json.Marshal(c)
}

// child is a repoctl process that the UI started. It runs in its own
// session and outlives the server.
type child struct {
	pid     int
	id      string
	errPath string
	done    chan struct{}
	exit    int
}

func (c *child) failure() string {
	b, _ := os.ReadFile(c.errPath)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	msg := strings.Join(lines[max(0, len(lines)-3):], "\n")
	if strings.TrimSpace(msg) == "" {
		msg = "no message"
	}
	return fmt.Sprintf("repoctl ended with exit %d: %s", c.exit, msg)
}

// startChild runs repoctl ARGS -C DIR with stdin /dev/null and its output
// in files, in a session of its own. confirm, when not nil, goes to file
// descriptor 3.
func (s *Server) startChild(sc *setCtx, verb string, args []string, confirm []byte) (*child, error) {
	exe, err := s.executable()
	if err != nil {
		return nil, err
	}
	dir := s.o.Paths.UIChildren()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id := verb + "-" + s.o.Now().UTC().Format("20060102-150405") + "-" + randHex(3)
	outPath, errPath := filepath.Join(dir, id+".out"), filepath.Join(dir, id+".err")
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: the runtime dir of the user
	if err != nil {
		return nil, err
	}
	defer out.Close()
	errf, err := os.OpenFile(errPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: the runtime dir of the user
	if err != nil {
		return nil, err
	}
	defer errf.Close()
	cmd := exec.Command(exe, append(slices.Clone(args), "-C", sc.Dir)...) //nolint:gosec // G204: the repoctl binary with the human's choice
	cmd.Dir = sc.Dir
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = out, errf
	cmd.Env = append(ChildEnv(s.o.Env), EnvOrigin+"=ui", EnvOriginSession+"="+s.SessionID(), "TRACEPARENT="+traceparent())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var pw *os.File
	if confirm != nil {
		pr, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer pr.Close()
		pw = w
		cmd.ExtraFiles = []*os.File{pr}
	}
	if err := cmd.Start(); err != nil {
		if pw != nil {
			pw.Close()
		}
		return nil, err
	}
	if pw != nil {
		_, _ = pw.Write(append(confirm, '\n'))
		pw.Close()
	}
	c := &child{pid: cmd.Process.Pid, id: id, errPath: errPath, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		c.exit = cmd.ProcessState.ExitCode()
		close(c.done)
		s.notify.Notify("set:" + sc.Name)
	}()
	return c, nil
}

// waitRun waits for the journal of the run that the child started, by
// the pid in run.started. It gives the run id, or "" with done set when
// the child ended first.
func (s *Server) waitRun(ctx context.Context, sc *setCtx, c *child, limit time.Duration) (string, bool) {
	dir := s.o.Paths.Runs(sc.Name)
	deadline := time.After(limit)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if run := runOfPID(dir, c.pid); run != "" {
			return run, false
		}
		select {
		case <-c.done:
			if run := runOfPID(dir, c.pid); run != "" {
				return run, true
			}
			return "", true
		case <-deadline:
			return "", false
		case <-ctx.Done():
			return "", false
		case <-tick.C:
		}
	}
}

// runOfPID finds the journal whose run.started has the pid.
func runOfPID(dir string, pid int) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for i := len(ents) - 1; i >= 0; i-- {
		n := ents[i].Name()
		if !strings.HasSuffix(n, ".jsonl") || strings.HasSuffix(n, ".progress.jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, n)) //nolint:gosec // G304: the user's state directory
		if err != nil {
			continue
		}
		buf := make([]byte, 4096)
		k, _ := f.Read(buf)
		f.Close()
		line, _, _ := bytes.Cut(buf[:k], []byte{'\n'})
		var ev api.RunStarted
		if json.Unmarshal(line, &ev) == nil && ev.Type == api.EventTypeRunStarted && ev.Origin.PID == pid {
			return ev.Run
		}
	}
	return ""
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// traceparent gives a new W3C trace context for a child.
func traceparent() string {
	t := make([]byte, 16)
	sp := make([]byte, 8)
	_, _ = rand.Read(t)
	_, _ = rand.Read(sp)
	return "00-" + hex.EncodeToString(t) + "-" + hex.EncodeToString(sp) + "-01"
}
