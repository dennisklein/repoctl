package ui

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/GSI-HPC/go-clikit/localweb/page"
)

// The session of a browser (design: ui.html#auth).
const (
	// CookieName is the session cookie: HttpOnly, SameSite=Strict.
	CookieName = "repoctl_session"
	// TokenTTL is how long a launch token is good, for one use.
	TokenTTL = 60 * time.Second
	// SessionTTL is the longest life of a session.
	SessionTTL = 7 * 24 * time.Hour
	// CSRFHeader carries the CSRF token of the session on each POST.
	CSRFHeader = "X-CSRF-Token"
)

type session struct {
	csrf    string
	expires time.Time
}

// auth holds the launch tokens and the sessions, in memory only: a new
// server starts with none.
type auth struct {
	now      func() time.Time
	mu       sync.Mutex
	tokens   map[[32]byte]time.Time
	sessions map[[32]byte]*session
}

func newAuth(now func() time.Time) *auth {
	return &auth{now: now, tokens: map[[32]byte]time.Time{}, sessions: map[[32]byte]*session{}}
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// issue makes a launch token.
func (a *auth) issue() string {
	t := randomToken()
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for k, exp := range a.tokens {
		if now.After(exp) {
			delete(a.tokens, k)
		}
	}
	a.tokens[sha256.Sum256([]byte(t))] = now.Add(TokenTTL)
	return t
}

// redeem trades a launch token for a new session and gives its cookie
// value. A token is good once, for TokenTTL.
func (a *auth) redeem(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	k := sha256.Sum256([]byte(token))
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.tokens[k]
	delete(a.tokens, k)
	if !ok || a.now().After(exp) {
		return "", false
	}
	id := randomToken()
	a.sessions[sha256.Sum256([]byte(id))] = &session{csrf: randomToken(), expires: a.now().Add(SessionTTL)}
	return id, true
}

// lookup gives the session of the request, or nil.
func (a *auth) lookup(r *http.Request) *session {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	k := sha256.Sum256([]byte(c.Value))
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[k]
	if !ok {
		return nil
	}
	if a.now().After(s.expires) {
		delete(a.sessions, k)
		return nil
	}
	return s
}

// safeNext gives next when it is a path of this server, else "/".
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") || strings.HasPrefix(next, "/-/auth") {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	return next
}

// openPath reports whether a path needs no session.
func openPath(p string) bool {
	return p == "/-/auth" || p == "/-/session" || p == "/-/ping" || strings.HasPrefix(p, "/-/static/")
}

type sessionKey struct{}

// requireSession lets in the requests of a session. Without one, a page
// GET goes to /-/session (303), htmx gets 401 with HX-Redirect, and the
// event stream and the rest get 401. A POST needs the CSRF token of the
// session too (403 without).
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if openPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		sess := s.auth.lookup(r)
		if sess == nil {
			to := "/-/session?next=" + url.QueryEscape(r.URL.RequestURI())
			switch {
			case page.IsHTMX(r):
				w.Header().Set("HX-Redirect", to)
				http.Error(w, "The session ended.", http.StatusUnauthorized)
			case r.Method == http.MethodGet && !strings.Contains(r.URL.Path, "/-/"):
				http.Redirect(w, r, to, http.StatusSeeOther)
			default:
				http.Error(w, "The session ended. Run repoctl ui for a new link.", http.StatusUnauthorized)
			}
			return
		}
		if r.Method == http.MethodPost {
			got := r.Header.Get(CSRFHeader)
			if got == "" {
				got = r.PostFormValue("csrf")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) != 1 {
				http.Error(w, "The request has no valid CSRF token. Reload the page.", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
	})
}

// handleAuth trades the launch token for the session cookie.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	id, ok := s.auth.redeem(r.URL.Query().Get("t"))
	if !ok {
		s.renderSession(w, r, http.StatusForbidden, "This link was used before or is older than 60 s.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(SessionTTL / time.Second),
	})
	http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
}

// handleSession is the page of a browser without a session.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	s.renderSession(w, r, http.StatusUnauthorized, "")
}

// handlePing tells the page whether its session is good: 204 or 401.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	if s.auth.lookup(r) == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
