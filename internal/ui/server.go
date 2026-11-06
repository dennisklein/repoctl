package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/GSI-HPC/go-clikit/localweb"
	"github.com/GSI-HPC/go-clikit/localweb/assets"
	"github.com/GSI-HPC/go-clikit/localweb/page"
	"github.com/GSI-HPC/go-clikit/localweb/sse"

	"github.com/dennisklein/repoctl/internal/config"
)

// Options configures a Server.
type Options struct {
	// Paths are the XDG paths of the user.
	Paths config.Paths
	// Source gives the data of the pages.
	Source Source
	// Version is the version of repoctl that serves.
	Version string
	// Home is the home directory, which pages show as ~.
	Home string
	// User and Host name who serves, for the top bar and the run origins.
	User, Host string
	// Executable is the repoctl binary the actions run; "" is
	// os.Executable.
	Executable string
	// Env is the environment of the children; nil is this process's.
	Env []string
	// Agent configures the agent buttons; a zero Command turns them off.
	Agent AgentConfig
	// Skills are the skills of the agent plugin (each NAME/SKILL.md), or nil.
	Skills fs.FS
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Log receives what the server logs; nil discards it.
	Log io.Writer
	// Poll is the interval of the watcher; 0 is 500 ms.
	Poll time.Duration
	// CacheTTL is how long a command result is kept; 0 is 2 s.
	CacheTTL time.Duration
	// StaleExecutable is why the binary of the server is no longer the one
	// on disk (it was replaced by an update), or "". The server then starts
	// no child and says so on every page.
	StaleExecutable string
}

// Server is the web UI of one user.
type Server struct {
	o       Options
	auth    *auth
	sets    *setList
	src     *cached
	runs    *runStore
	notify  sse.Notifier[string]
	assets  *assets.Set
	pages   *page.Set
	agents  *agents
	log     *log.Logger
	started time.Time
	session string

	stopOnce sync.Once
	stopping chan struct{}
}

// New makes the server of o. It reads the set list.
func New(o Options) (*Server, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.Poll == 0 {
		o.Poll = 500 * time.Millisecond
	}
	if o.CacheTTL == 0 {
		o.CacheTTL = 2 * time.Second
	}
	if o.Executable == "" {
		exe, err := os.Executable()
		if err == nil {
			o.Executable = exe
		}
	}
	if o.Source == nil {
		return nil, errors.New("ui: no Source")
	}
	sets, err := loadSetList(o.Paths.UISets())
	if err != nil {
		return nil, err
	}
	a, err := assets.New(staticFS(), "/-/static/", assets.Options{})
	if err != nil {
		return nil, err
	}
	s := &Server{
		o: o, auth: newAuth(o.Now), sets: sets, src: newCached(o.Source, o.CacheTTL), runs: newRunStore(),
		assets: a, log: log.New(o.Log, "repoctl ui: ", log.LstdFlags), started: o.Now(), stopping: make(chan struct{}),
		session: randomToken()[:16],
	}
	pages, err := page.Parse(templateFS(), page.Options{Funcs: s.funcs()}, "*.tmpl")
	if err != nil {
		return nil, err
	}
	s.pages = pages
	s.agents = newAgents(s)
	return s, nil
}

// SessionID is the id of this server, which the runs of its children
// record in origin.session.
func (s *Server) SessionID() string { return s.session }

// IssueToken makes a launch token, good once for TokenTTL.
func (s *Server) IssueToken() string { return s.auth.issue() }

// LaunchPath gives the path of the launch URL with a new token, which
// leads to next.
func (s *Server) LaunchPath(next string) string {
	return "/-/auth?t=" + s.IssueToken() + "&next=" + url.QueryEscape(safeNext(next))
}

// AddSet puts the set on the set list and reports whether it was new or
// moved.
func (s *Server) AddSet(name, dir string) (bool, error) {
	changed, err := s.sets.add(SetEntry{Name: name, Path: dir})
	if changed {
		s.notify.Notify("sets")
	}
	return changed, err
}

// RemoveSet takes the set off the set list.
func (s *Server) RemoveSet(name string) (bool, error) {
	ok, err := s.sets.remove(name)
	if ok {
		s.notify.Notify("sets")
	}
	return ok, err
}

// Sets gives the set list.
func (s *Server) Sets() []SetEntry { return s.sets.all() }

// Stop tells the open pages that the server stops (event bye).
func (s *Server) Stop() {
	s.stopOnce.Do(func() { close(s.stopping) })
}

// Handler gives the handler of the server with every check of localweb.
// answers says which Host headers the server answers to (Bound.Answers);
// nil lets every Host in, for a test.
func (s *Server) Handler(answers func(string) bool) http.Handler {
	var h http.Handler = s.routes()
	h = s.requireSession(h)
	h = localweb.NoStore(h)
	h = localweb.SameOrigin(localweb.OriginPolicy{Navigate: true})(h)
	h = methods(h)
	if answers != nil {
		h = localweb.Hosts(answers, localweb.HostsOptions{})(h)
	}
	return localweb.Headers("")(h)
}

// methods answers every method but GET, HEAD and POST with 405.
func methods(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodPost:
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Allow", "GET, HEAD, POST")
			http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		}
	})
}

func (s *Server) routes() *http.ServeMux {
	m := http.NewServeMux()
	m.Handle("GET /-/static/", s.assets)
	m.HandleFunc("GET /-/auth", s.handleAuth)
	m.HandleFunc("GET /-/session", s.handleSession)
	m.HandleFunc("GET /-/ping", s.handlePing)
	m.HandleFunc("GET /-/events", s.handleEvents)
	m.HandleFunc("GET /-/frag/set-menu", s.handleSetMenu)
	m.HandleFunc("GET /-/go", s.handleGo)
	m.HandleFunc("POST /-/sets/{set}/remove", s.handleSetRemove)
	m.HandleFunc("POST /-/agent/{id}/stop", s.handleAgentStop)
	m.HandleFunc("GET /{$}", s.page("sets", ""))
	m.HandleFunc("GET /doctor", s.page("doctor", ""))
	set := func(pattern, kind, nav string) {
		m.HandleFunc("GET /s/{set}"+pattern, s.page(kind, nav))
	}
	set("/{$}", "overview", "overview")
	set("/graph", "graph", "graph")
	set("/plan", "plan", "plan")
	set("/advisories", "advisories", "advisories")
	set("/upstream", "upstream", "upstream")
	set("/runs", "runs", "runs")
	set("/runs/{run}", "run", "runs")
	set("/runs/{run}/nodes/{node}", "node", "runs")
	set("/packages", "packages", "packages")
	set("/packages/{pkg}", "package", "packages")
	set("/repo/{target}", "repo", "repo")
	set("/repo/{target}/diff", "diff", "repo")
	set("/publish", "publish", "publish")
	set("/signing", "signing", "signing")
	set("/config", "config", "config")
	m.HandleFunc("GET /s/{set}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/s/"+url.PathEscape(r.PathValue("set"))+"/", http.StatusMovedPermanently)
	})
	m.HandleFunc("GET /s/{set}/-/events", s.handleEvents)
	m.HandleFunc("GET /s/{set}/-/frag/{region}", s.handleFrag)
	m.HandleFunc("GET /s/{set}/-/dialog/{name}", s.handleDialog)
	m.HandleFunc("POST /s/{set}/-/actions/{verb}", s.handleAction)
	m.HandleFunc("POST /s/{set}/-/agent/{task}", s.handleAgent)
	m.HandleFunc("GET /s/{set}/-/agent/{id}", s.handleAgentShow)
	return m
}

// setCtx is the set of a request: its entry in the set list and its
// repoctl.yaml as it loads now.
type setCtx struct {
	Name    string
	Dir     string
	Conf    *config.Set
	ConfErr error
}

// Targets gives the active targets of the set.
func (sc *setCtx) Targets() []string {
	if sc == nil || sc.Conf == nil {
		return nil
	}
	var out []string
	for _, t := range sc.Conf.ActiveTargets() {
		out = append(out, t.ID)
	}
	return out
}

func (s *Server) setOf(name string) (*setCtx, bool) {
	e, ok := s.sets.get(name)
	if !ok {
		return nil, false
	}
	sc := &setCtx{Name: e.Name, Dir: e.Path}
	sc.Conf, sc.ConfErr = config.LoadSet(e.Path)
	return sc, true
}

// run runs a command of the set through the cache.
func (s *Server) run(ctx context.Context, sc *setCtx, args ...string) *Result {
	return s.src.Run(ctx, sc.Dir, args...)
}

// sessionOf gives the session of a request that requireSession let in.
func sessionOf(ctx context.Context) *session {
	v, _ := ctx.Value(sessionKey{}).(*session)
	return v
}

func withSession(ctx context.Context, sess *session) context.Context {
	return context.WithValue(ctx, sessionKey{}, sess)
}

// fail answers with a short error page or fragment.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if page.IsHTMX(r) {
		s.toast(w, status, "err", msg)
		return
	}
	v := &pageView{Title: http.StatusText(status), Content: "page-error", Data: msg, Who: s.who(), Version: s.o.Version}
	if sess := sessionOf(r.Context()); sess != nil {
		v.CSRF = sess.csrf
	}
	s.respond(w, r, status, "layout", v)
}

// respond renders the block name; a failed render is logged and answered
// with 500.
func (s *Server) respond(w http.ResponseWriter, _ *http.Request, status int, name string, data any) {
	if err := s.pages.Respond(w, status, name, data); err != nil {
		s.log.Printf("render %s: %v", name, err)
		http.Error(w, "repoctl ui could not render this page. Its log says why.", http.StatusInternalServerError)
	}
}

// toast answers htmx with a toast (out of band into #toasts) and no other
// change.
func (s *Server) toast(w http.ResponseWriter, status int, kind, msg string) {
	html, err := s.pages.Render("toast-oob", toastView{Kind: kind, Text: msg})
	if err != nil {
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("HX-Reswap", "none")
	// htmx swaps no answer of an error status; the toast must show.
	if status >= 400 {
		w.Header().Set("HX-Retarget", "#toasts")
		w.Header().Set("HX-Reswap", "beforeend")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, html) //nolint:gosec // G705: a rendered template
}

func (s *Server) renderSession(w http.ResponseWriter, r *http.Request, status int, why string) {
	v := &pageView{Title: "Session · repoctl", Content: "page-session", Data: sessionPage{Why: why, Next: safeNext(r.URL.Query().Get("next"))}, Bare: true, Version: s.o.Version}
	s.respond(w, r, status, "layout", v)
}

type sessionPage struct {
	Why  string
	Next string
}

func (s *Server) who() string {
	if s.o.Host == "" {
		return s.o.User
	}
	return s.o.User + "@" + s.o.Host
}

// logf logs a line.
func (s *Server) logf(format string, args ...any) { s.log.Output(2, fmt.Sprintf(format, args...)) } //nolint:errcheck

// executable gives the binary for children, or an error when the server
// must start none.
func (s *Server) executable() (string, error) {
	if why := s.stale(); why != "" {
		return "", fmt.Errorf("the repoctl binary of this server was replaced (%s): it starts no child (host.stale-server). Restart it: repoctl ui --stop && repoctl ui", why)
	}
	if s.o.Executable == "" {
		return "", errors.New("the repoctl binary is unknown")
	}
	return s.o.Executable, nil
}

// stale says why the binary of the server is no longer the one on disk.
func (s *Server) stale() string {
	if s.o.StaleExecutable != "" {
		return s.o.StaleExecutable
	}
	return StaleExecutable()
}

// cliPrefix gives "repoctl -C DIR" with the home directory as ~.
func (s *Server) cliPrefix(dir string) string {
	return "repoctl -C " + tildePath(dir, s.o.Home)
}

func tildePath(p, home string) string {
	if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}
