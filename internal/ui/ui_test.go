package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/testkit"
)

const (
	testSet = "slurm-26.05"
	testRun = "r-20261102-1750-a3f"
)

// fakeSource answers each command line with an envelope; a command it
// does not know is not implemented, as the stubs of this build are.
type fakeSource struct {
	mu    sync.Mutex
	res   map[string]*Result
	calls []string
}

func (f *fakeSource) Run(_ context.Context, _ string, args ...string) *Result {
	key := strings.Join(args, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)
	best := ""
	for k := range f.res {
		if (key == k || strings.HasPrefix(key, k+" ")) && len(k) > len(best) {
			best = k
		}
	}
	if best != "" {
		return f.res[best]
	}
	return notImplemented(args)
}

func notImplemented(args []string) *Result {
	return &Result{Args: args, Exit: 1, Errors: []api.Problem{{Code: CodeNotImplemented, Message: "repoctl " + strings.Join(args, " ") + " is not implemented in this release"}}}
}

// example reads an envelope of the contract examples.
func example(t *testing.T, name string) *Result {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testkit.ModuleRoot(), "docs", "design", "contract", "examples", name))
	if err != nil {
		t.Fatal(err)
	}
	r := ParseEnvelope(b, 0, nil)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	return r
}

// fakeEnvelope makes the envelope of items.
func fakeEnvelope(t *testing.T, kind api.Kind, items ...any) *Result {
	t.Helper()
	env := api.NewEnvelope(kind, api.Metadata{Command: string(kind)})
	env.Items = items
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	r := ParseEnvelope(b, 0, nil)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	return r
}

// fullSource gives envelopes for every command a page runs.
func fullSource(t *testing.T) *fakeSource {
	t.Helper()
	status := api.Status{
		Set:       api.StatusSet{Name: testSet, Config: "repoctl.yaml", Git: &api.StatusSetGit{Branch: "set/slurm-26.05", Dirty: true}},
		Targets:   []api.Target{{ID: "el9", Mock: "rocky+epel-9-x86_64"}},
		Packages:  []api.StatusPackage{{Package: "ucx", Lock: api.LockStateDirty}, {Package: "slurm", Lock: api.LockStateInSync}},
		Snapshots: []api.StatusSnapshot{{Target: "el9", Latest: &api.StatusSnapshotLatest{Publish: "p-20260912-1403-c21", Digest: "sha256:" + strings.Repeat("ab", 32), User: "dklein", At: "2026-09-12T14:03:00.000Z"}, Local: "same", Served: "same"}},
		Check:     &api.StatusCheck{At: "2026-11-02T06:00:00.000Z", Required: 1},
		Upstream:  &api.StatusUpstream{Updates: []api.StatusUpstreamUpdate{{Package: "slurm", Version: "26.05.5"}}},
	}
	graph := api.Graph{Target: "el9",
		Nodes: []api.GraphNode{{ID: "munge@el9", Package: "munge", EVR: "0.5.18-1"}, {ID: "ucx@el9", Package: "ucx", EVR: "1.22.1-1"}, {ID: "slurm@el9", Package: "slurm", EVR: "26.05.4-2", Depth: 1}, {ID: "slurm-singularity-exec@el9", Package: "slurm-singularity-exec", Depth: 2}},
		Edges: []api.GraphEdge{{From: "munge@el9", To: "slurm@el9", Via: []string{"munge-devel"}}, {From: "ucx@el9", To: "slurm@el9", Via: []string{"ucx-devel"}}, {From: "slurm@el9", To: "slurm-singularity-exec@el9", Via: []string{"slurm-devel"}}, {From: "munge@el9", To: "slurm-singularity-exec@el9"}},
	}
	pkg := api.Package{Package: "slurm", Role: "package", Source: api.PackageSource{Kind: "branch", Branch: "pkg/slurm"}, Lock: &api.PackageLock{Commit: strings.Repeat("d4", 20)},
		Targets: []api.PackageTarget{{ID: "el9", NVR: "slurm-26.05.4-2.el9", DependsOn: []string{"munge", "ucx"}, Action: api.PlanActionBuild}}}
	pub := api.Run{ID: "p-20261102-1900-b12", Kind: "publish", Set: testSet, State: "planning", Origin: api.RunOriginItem{Via: api.RunOriginUI, User: "tester", Host: "hpc-a1"},
		StartedAt: "2026-11-02T19:00:00.000Z", PlanDigest: "sha256:" + strings.Repeat("cd", 32), Files: api.RunFiles{Journal: "x"},
		Nodes:   []api.NodeResult{{Node: "ucx@el9", Package: "ucx", Target: "el9", State: api.NodeStateStaged, Commit: strings.Repeat("4b", 20), NVRs: []string{"ucx-1.22.1-1.el9.x86_64"}}},
		Publish: map[string]api.RunPublishValue{"el9": {Store: "ghcr.io/x/slurm-26.05-el9", Sign: &api.RunPublishValueSign{New: 9, Keep: 120}, Gate: []api.RunPublishValueGateItem{{ID: "closure.broken/pmix@el9", Blocks: true}}}}}
	doctor := api.DoctorReport{Worst: api.CheckStatusWarning, Counts: api.DoctorReportCounts{Ok: 1, Warning: 1},
		Checks: []api.DoctorReportCheck{{Name: "host.mock", Group: "host", Status: api.CheckStatusOk}, {Name: "sign.key", Group: "sign", Status: api.CheckStatusWarning, Detail: "the key expires soon", Remediation: "gpg --quick-set-expire"}}}
	return &fakeSource{res: map[string]*Result{
		"status":            fakeEnvelope(t, api.KindStatus, status),
		"advisories ls":     example(t, "advisories.json"),
		"plan":              example(t, "plan.json"),
		"sign ls":           example(t, "sign-ls.json"),
		"runs":              example(t, "build.json"),
		"graph":             fakeEnvelope(t, api.KindGraph, graph),
		"list":              fakeEnvelope(t, api.KindPackage, pkg),
		"show":              fakeEnvelope(t, api.KindPackage, pkg),
		"publish --dry-run": fakeEnvelope(t, api.KindRun, pub),
		"doctor":            fakeEnvelope(t, api.KindDoctorReport, doctor),
		"upstream":          fakeEnvelope(t, api.KindUpstream, api.Upstream{Package: "slurm", Current: api.UpstreamCurrent{Version: "26.05.4"}, Status: api.UpstreamStatusUpdateAvailable, Latest: "26.05.5"}),
		"config show":       fakeEnvelope(t, api.KindConfig, api.Config{Key: "jobs", Value: 2, Source: "default"}),
		"repo ls":           fakeEnvelope(t, api.KindRepoContents, api.RepoContents{Target: "el9", Repo: "slurm", Kind: api.RepoKindBinary, Source: "latest", Path: "/x", Packages: []api.RepoContentsPackage{{Name: "ucx", NVRA: "ucx-1.22.1-1.el9.x86_64", SizeBytes: 1}}}),
		"repo diff":         fakeEnvelope(t, api.KindRepoDiff, api.RepoDiff{Target: "el9", Repo: "slurm", Added: []string{"ucx-1.22.1-1.el9.x86_64"}, Removed: []string{}, Changed: []api.RepoDiffChangedItem{}, Rebuilt: []string{}}),
	}}
}

const setYAML = `apiVersion: repoctl/v1alpha1
name: slurm-26.05
repo:
  name: slurm
targets:
  - id: el9
    mock: rocky+epel-9-x86_64
    plugins: [rpmautospec]
  - id: el10
    mock: rocky+epel-10-x86_64
    plugins: [rpmautospec]
packages:
  - id: munge
    branch: pkg/munge
  - id: ucx
    branch: pkg/ucx
  - id: slurm
    branch: pkg/slurm
  - id: slurm-singularity-exec
    branch: pkg/slurm-singularity-exec
    targets: [el9]
`

// testEnv is a server with one set, a session and the journal of the
// contract example.
type testEnv struct {
	t      *testing.T
	root   string
	setDir string
	paths  config.Paths
	srv    *Server
	h      http.Handler
	now    time.Time
	cookie *http.Cookie
	csrf   string
}

func newTestEnv(t *testing.T, src Source) *testEnv {
	t.Helper()
	root := t.TempDir()
	vars := map[string]string{
		"HOME": filepath.Join(root, "home"), "XDG_STATE_HOME": filepath.Join(root, "state"), "XDG_RUNTIME_DIR": filepath.Join(root, "run"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"), "XDG_CACHE_HOME": filepath.Join(root, "cache"), "XDG_DATA_HOME": filepath.Join(root, "data"),
	}
	e := &testEnv{t: t, root: root, setDir: filepath.Join(root, "set"), now: time.Date(2026, 11, 2, 18, 0, 0, 0, time.UTC)}
	e.paths = config.NewPaths(func(k string) string { return vars[k] })
	mkfile(t, filepath.Join(e.setDir, "repoctl.yaml"), setYAML)
	b, err := os.ReadFile(filepath.Join(testkit.ModuleRoot(), "docs", "design", "contract", "examples", "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	mkfile(t, e.paths.Journal(testSet, testRun), string(b))
	mkfile(t, e.paths.NodeLog(testSet, testRun, "slurm@el10"), "Start: build setup\nerror: Failed build dependencies: rocm-smi-devel is needed\n")
	srv, err := New(Options{Paths: e.paths, Source: src, Version: "0.1.0-test", User: "tester", Host: "hpc-a1", Executable: "/bin/false",
		Now: func() time.Time { return e.now }, CacheTTL: time.Nanosecond, Poll: 20 * time.Millisecond, Home: vars["HOME"]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.AddSet(testSet, e.setDir); err != nil {
		t.Fatal(err)
	}
	e.srv, e.h = srv, srv.Handler(nil)
	return e
}

func mkfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// do sends a request through the whole handler.
func (e *testEnv) do(method, target string, hdr map[string]string, body url.Values) *httptest.ResponseRecorder {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		r = strings.NewReader(body.Encode())
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:8080"+target, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method == http.MethodPost {
		req.Header.Set("Origin", "http://127.0.0.1:8080")
	}
	if e.cookie != nil {
		req.AddCookie(e.cookie)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w
}

var csrfMeta = regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)">`)

// login trades a launch token for the session cookie and reads the CSRF
// token of the session from a page.
func (e *testEnv) login() {
	e.t.Helper()
	w := e.do(http.MethodGet, e.srv.LaunchPath("/"), nil, nil)
	if w.Code != http.StatusSeeOther {
		e.t.Fatalf("auth: %d %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			e.cookie = c
		}
	}
	if e.cookie == nil {
		e.t.Fatal("auth set no session cookie")
	}
	w = e.do(http.MethodGet, "/", nil, nil)
	m := csrfMeta.FindStringSubmatch(w.Body.String())
	if m == nil {
		e.t.Fatalf("no csrf token on the page: %s", w.Body)
	}
	e.csrf = m[1]
}

func TestAuth(t *testing.T) {
	e := newTestEnv(t, fullSource(t))

	w := e.do(http.MethodGet, "/s/"+testSet+"/runs", nil, nil)
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/-/session?next=") {
		t.Errorf("a page without a session: %d %q, want 303 to /-/session", w.Code, w.Header().Get("Location"))
	}
	if w := e.do(http.MethodGet, "/-/session?next=/", nil, nil); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "repoctl ui") {
		t.Errorf("/-/session: %d", w.Code)
	}
	w = e.do(http.MethodGet, "/s/"+testSet+"/", map[string]string{"HX-Request": "true"}, nil)
	if w.Code != http.StatusUnauthorized || w.Header().Get("HX-Redirect") == "" {
		t.Errorf("htmx without a session: %d %q, want 401 with HX-Redirect", w.Code, w.Header().Get("HX-Redirect"))
	}
	if w := e.do(http.MethodGet, "/s/"+testSet+"/-/events?page=overview", nil, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("events without a session: %d, want 401", w.Code)
	}
	if w := e.do(http.MethodGet, "/-/ping", nil, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("ping without a session: %d, want 401", w.Code)
	}
	if w := e.do(http.MethodGet, "/-/auth?t=nope&next=/", nil, nil); w.Code != http.StatusForbidden {
		t.Errorf("a bad token: %d, want 403", w.Code)
	}
	if w := e.do(http.MethodGet, "/-/auth?next=/", nil, nil); w.Code != http.StatusForbidden {
		t.Errorf("no token: %d, want 403", w.Code)
	}

	// A good token: a cookie and the page it leads to; once only.
	path := e.srv.LaunchPath("/s/" + testSet + "/runs")
	w = e.do(http.MethodGet, path, nil, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/s/"+testSet+"/runs" {
		t.Fatalf("a good token: %d %q", w.Code, w.Header().Get("Location"))
	}
	var c *http.Cookie
	for _, k := range w.Result().Cookies() {
		if k.Name == CookieName {
			c = k
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("session cookie: %+v", c)
	}
	if w := e.do(http.MethodGet, path, nil, nil); w.Code != http.StatusForbidden {
		t.Errorf("a token used twice: %d, want 403", w.Code)
	}
	old := e.srv.LaunchPath("/")
	e.now = e.now.Add(TokenTTL + time.Second)
	if w := e.do(http.MethodGet, old, nil, nil); w.Code != http.StatusForbidden {
		t.Errorf("a token older than 60 s: %d, want 403", w.Code)
	}
	if w := e.do(http.MethodGet, "/-/auth?t=x&next=//evil.example/", nil, nil); w.Code != http.StatusForbidden {
		t.Errorf("bad token with an open redirect: %d", w.Code)
	}
	if got := safeNext("//evil.example/"); got != "/" {
		t.Errorf("safeNext(//evil.example/) = %q", got)
	}

	e.login()
	if w := e.do(http.MethodGet, "/-/ping", nil, nil); w.Code != http.StatusNoContent {
		t.Errorf("ping with a session: %d, want 204", w.Code)
	}
	// A POST needs the CSRF token of the session.
	if w := e.do(http.MethodPost, "/s/"+testSet+"/-/actions/check", nil, url.Values{}); w.Code != http.StatusForbidden {
		t.Errorf("POST without CSRF: %d, want 403", w.Code)
	}
	if w := e.do(http.MethodPost, "/s/"+testSet+"/-/actions/check", map[string]string{CSRFHeader: "wrong"}, url.Values{}); w.Code != http.StatusForbidden {
		t.Errorf("POST with a wrong CSRF token: %d, want 403", w.Code)
	}
	// From another site: refused before the session is looked at.
	if w := e.do(http.MethodPost, "/s/"+testSet+"/-/actions/check", map[string]string{CSRFHeader: e.csrf, "Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}, url.Values{}); w.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: %d, want 403", w.Code)
	}
	if w := e.do(http.MethodPut, "/", nil, nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT: %d, want 405", w.Code)
	}
	w = e.do(http.MethodGet, "/", nil, nil)
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP: %q", csp)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control of a page: %q", w.Header().Get("Cache-Control"))
	}
}

// pages are every page of the sitemap, with a marker of its content.
var pages = []struct{ path, want string }{
	{"/", "slurm-26.05"},
	{"/doctor", "repoctl doctor"},
	{"/s/slurm-26.05/", "Needs you"},
	{"/s/slurm-26.05/graph", `class="graph"`},
	{"/s/slurm-26.05/graph?node=slurm@el9", "node-drawer"},
	{"/s/slurm-26.05/graph?view=matrix", "graph-matrix"},
	{"/s/slurm-26.05/graph?target=el10", "Graph"},
	{"/s/slurm-26.05/plan", "Plan"},
	{"/s/slurm-26.05/advisories", "Advisories"},
	{"/s/slurm-26.05/advisories?dismissed=1", "Advisories"},
	{"/s/slurm-26.05/upstream", "Upstream"},
	{"/s/slurm-26.05/runs", testRun},
	{"/s/slurm-26.05/runs/" + testRun, "fail-card"},
	{"/s/slurm-26.05/runs/" + testRun + "/nodes/slurm@el10", "rocm-smi-devel"},
	{"/s/slurm-26.05/packages", "Packages"},
	{"/s/slurm-26.05/packages/slurm", "slurm"},
	{"/s/slurm-26.05/repo/el9", "el9"},
	{"/s/slurm-26.05/repo/el9?run=" + testRun, "el9"},
	{"/s/slurm-26.05/repo/el9/diff?run=" + testRun, "Compare"},
	{"/s/slurm-26.05/publish", "Publish"},
	{"/s/slurm-26.05/publish?target=el9", "Confirm"},
	{"/s/slurm-26.05/signing", "Signing"},
	{"/s/slurm-26.05/config", "Settings"},
}

func TestPages(t *testing.T) {
	e := newTestEnv(t, fullSource(t))
	e.login()
	for _, p := range pages {
		w := e.do(http.MethodGet, p.path, nil, nil)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: %d\n%s", p.path, w.Code, w.Body)
			continue
		}
		body := w.Body.String()
		if !strings.Contains(body, p.want) {
			t.Errorf("GET %s: no %q in the page", p.path, p.want)
		}
		if strings.Contains(body, "not available in this build") && p.path != "/s/slurm-26.05/graph?target=el10" {
			t.Errorf("GET %s: an empty state with a full source", p.path)
		}
		if strings.Contains(body, " style=") || strings.Contains(body, " onclick=") {
			t.Errorf("GET %s: an inline style or handler, which the CSP refuses", p.path)
		}
	}
	for _, d := range []string{"build", "cancel?run=" + testRun, "rebuild?packages=slurm", "dismiss?advisory=buildroot.drift/munge@el9", "publish?target=el9", "sign-deployed?key=dklein-2026", "pin", "checkout"} {
		w := e.do(http.MethodGet, "/s/"+testSet+"/-/dialog/"+d, map[string]string{"HX-Request": "true"}, nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<dialog") {
			t.Errorf("dialog %s: %d\n%s", d, w.Code, w.Body)
		}
	}
	if w := e.do(http.MethodGet, "/-/frag/set-menu?set="+testSet, nil, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "All sets") {
		t.Errorf("set menu: %d", w.Code)
	}
	if w := e.do(http.MethodGet, "/s/"+testSet+"/-/frag/jump?q=slu", nil, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/packages/slurm") {
		t.Errorf("jump: %d %s", w.Code, w.Body)
	}
	if w := e.do(http.MethodGet, "/s/"+testSet+"/-/frag/attention?page=overview", nil, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "slurm") {
		t.Errorf("fragment attention: %d", w.Code)
	}
	if w := e.do(http.MethodGet, "/s/nope/", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("an unknown set: %d, want 404", w.Code)
	}
	if w := e.do(http.MethodGet, e.srv.assets.URL("shell.js"), nil, nil); w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("static file: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w := e.do(http.MethodGet, e.srv.assets.URL("htmx.min.js"), nil, nil); w.Code != http.StatusOK {
		t.Errorf("vendored htmx: %d", w.Code)
	}
}

func TestNotImplementedEmptyState(t *testing.T) {
	e := newTestEnv(t, &fakeSource{res: map[string]*Result{}})
	e.login()
	for _, p := range pages {
		w := e.do(http.MethodGet, p.path, nil, nil)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: %d, want 200 with the empty state\n%s", p.path, w.Code, w.Body)
		}
	}
	for _, p := range []string{"/s/slurm-26.05/graph", "/s/slurm-26.05/plan", "/s/slurm-26.05/advisories", "/s/slurm-26.05/publish", "/doctor", "/s/slurm-26.05/repo/el9"} {
		w := e.do(http.MethodGet, p, nil, nil)
		if !strings.Contains(w.Body.String(), "is not available in this build") {
			t.Errorf("GET %s: no calm empty state", p)
		}
	}
	if w := e.do(http.MethodGet, "/s/"+testSet+"/runs/r-20990101-0000-zzz", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("an unknown run: %d, want 404", w.Code)
	}
	// The pages that live on the journals and repoctl.yaml still show them.
	if w := e.do(http.MethodGet, "/s/slurm-26.05/packages", nil, nil); !strings.Contains(w.Body.String(), "slurm-singularity-exec") {
		t.Error("packages: no fallback to repoctl.yaml")
	}
	if w := e.do(http.MethodGet, "/s/slurm-26.05/", nil, nil); !strings.Contains(w.Body.String(), "slurm@el10") {
		t.Error("overview: the failed node of the journal is not in Needs you")
	}
}

func TestEventStream(t *testing.T) {
	e := newTestEnv(t, fullSource(t))
	e.login()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.srv.Watch(ctx)
	ts := httptest.NewServer(e.h)
	defer ts.Close()

	open := func(path string) (*bufio.Reader, func()) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
		req.AddCookie(e.cookie)
		resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // the returned func closes it
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			resp.Body.Close()
			t.Fatalf("events: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		return bufio.NewReader(resp.Body), func() { resp.Body.Close() }
	}
	// wait reads events until one named name comes, and gives its lines.
	wait := func(r *bufio.Reader, name string) string {
		t.Helper()
		done := make(chan string, 1)
		go func() {
			var ev strings.Builder
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					done <- ""
					return
				}
				ev.WriteString(line)
				if line == "\n" {
					if strings.Contains(ev.String(), "event: "+name+"\n") {
						done <- ev.String()
						return
					}
					ev.Reset()
				}
			}
		}()
		select {
		case ev := <-done:
			if ev == "" {
				t.Fatalf("the stream ended before the event %s", name)
			}
			return ev
		case <-time.After(10 * time.Second):
			t.Fatalf("no event %s within 10 s", name)
		}
		return ""
	}

	runs, closeRuns := open("/s/" + testSet + "/-/events?page=runs")
	defer closeRuns()
	time.Sleep(100 * time.Millisecond)
	// A new run: its journal gets its first line.
	j := e.paths.Journal(testSet, "r-20261102-1900-new")
	mkfile(t, j, `{"v":1,"run":"r-20261102-1900-new","seq":1,"time":"2026-11-02T19:00:00.000Z","type":"run.started","set":"slurm-26.05","kind":"build","trace":"x","origin":{"via":"ui","user":"tester","host":"elsewhere","pid":1}}`+"\n")
	ev := wait(runs, "runs-list")
	if !strings.Contains(ev, "r-20261102-1900-new") {
		t.Errorf("runs-list event without the new run:\n%s", ev)
	}
	// A line appended to the journal: the run page gets its region.
	run, closeRun := open("/s/" + testSet + "/-/events?page=run&run=r-20261102-1900-new")
	defer closeRun()
	time.Sleep(100 * time.Millisecond)
	f, err := os.OpenFile(j, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"v":1,"run":"r-20261102-1900-new","seq":2,"time":"2026-11-02T19:00:05.000Z","type":"plan.ready","summary":{"build":1,"current":0,"staged":0,"skip":0},"build":[{"node":"ucx@el9","reasons":["commit-changed"]}],"current":[],"commits":{}}` + "\n")
	f.Close()
	ev = wait(run, "run:r-20261102-1900-new")
	if !strings.Contains(ev, "ucx@el9") {
		t.Errorf("run event without the planned node:\n%s", ev)
	}

	// The node log streams its new lines with the byte offset as the id.
	logs, closeLogs := open("/s/" + testSet + "/-/events?page=node&run=" + testRun + "&node=slurm@el10&from=0")
	defer closeLogs()
	ev = wait(logs, "log:slurm@el10")
	if !strings.Contains(ev, "id: ") || !strings.Contains(ev, "rocm-smi-devel") {
		t.Errorf("log event:\n%s", ev)
	}
	lf, err := os.OpenFile(e.paths.NodeLog(testSet, testRun, "slurm@el10"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = lf.WriteString("one more line\n")
	lf.Close()
	ev = wait(logs, "log:slurm@el10")
	if !strings.Contains(ev, "one more line") || !strings.Contains(ev, `id="L3"`) {
		t.Errorf("appended log line:\n%s", ev)
	}

	// The server stops: every stream says bye.
	e.srv.Stop()
	wait(runs, "bye")
}

func TestRunFold(t *testing.T) {
	e := newTestEnv(t, fullSource(t))
	f, err := foldJournal(e.paths.Journal(testSet, testRun))
	if err != nil {
		t.Fatal(err)
	}
	r := f.Run
	if r.State != api.RunStateFailed || r.Counts.Ok != 6 || r.Counts.Failed != 1 || len(r.Publishable) != 1 {
		t.Errorf("run: state %s counts %+v publishable %v", r.State, r.Counts, r.Publishable)
	}
	n := f.Node("slurm@el10")
	if n == nil || n.State != api.NodeStateFailed || n.Error == nil || n.Error.Class != api.FailureClassBuilddep {
		t.Fatalf("slurm@el10: %+v", n)
	}
	if f.LastLine["slurm@el10"] == "" || len(f.Builds()) != 8 {
		t.Errorf("last line %q, builds %d", f.LastLine["slurm@el10"], len(f.Builds()))
	}
	// The fold is a Run of the contract.
	testkit.ValidateEnvelope(t, envelopeBytes(t, api.KindRun, r))
}

func envelopeBytes(t *testing.T, kind api.Kind, items ...any) []byte {
	t.Helper()
	env := api.NewEnvelope(kind, api.Metadata{Command: "runs", GeneratedAt: api.FormatTime(time.Now()), Repoctl: api.MetadataRepoctl{Version: "0.1.0"}})
	env.Items = items
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestActionsRefuseWithoutConfirmation(t *testing.T) {
	e := newTestEnv(t, fullSource(t))
	e.login()
	post := func(verb string, form url.Values) *httptest.ResponseRecorder {
		return e.do(http.MethodPost, "/s/"+testSet+"/-/actions/"+verb, map[string]string{CSRFHeader: e.csrf, "HX-Request": "true"}, form)
	}
	w := post("publish", url.Values{"typed": {"wrong"}})
	if !strings.Contains(w.Body.String(), "type the set name") {
		t.Errorf("publish with a wrong set name: %s", w.Body)
	}
	w = post("rebuild", url.Values{"packages": {"slurm"}, "reason": {"short"}})
	if !strings.Contains(w.Body.String(), "8 characters") {
		t.Errorf("rebuild with a short reason: %s", w.Body)
	}
	// /bin/false stands in for repoctl: the child fails, the toast says so.
	w = post("advisory-undismiss", url.Values{"advisory": {"x"}})
	if !strings.Contains(w.Body.String(), "exit 1") {
		t.Errorf("a failed child: %s", w.Body)
	}
	c := Confirmation{V: 1, Command: "publish", Set: testSet}
	if b, _ := json.Marshal(c); !strings.Contains(string(b), `"set":"slurm-26.05"`) {
		t.Errorf("confirmation: %s", b)
	}

	// sign deployed needs the targets and the release, and passes the
	// confirmation on file descriptor 3.
	w = post("sign-deployed", url.Values{"key": {"a.admin"}, "typed": {"a.admin"}, "release": {"slurm-release-26.05-3"}})
	if !strings.Contains(w.Body.String(), "choose the targets") {
		t.Errorf("sign-deployed without a target: %s", w.Body)
	}
	form := url.Values{"key": {"a.admin"}, "typed": {"a.admin"}, "release": {"slurm-release-26.05-3"}, "target": {"el9", "el10"}}
	act := actions["sign-deployed"]
	args, err := act.args(form)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "sign deployed a.admin --confirm-fd 3 --release slurm-release-26.05-3 --target el9 --target el10" {
		t.Errorf("sign-deployed args: %s", got)
	}
	b, err := act.confirm(form, testSet, "dklein")
	if err != nil {
		t.Fatal(err)
	}
	var got Confirmation
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.V != 1 || got.Command != "sign deployed" || got.Set != testSet || strings.Join(got.Keys, ",") != "a.admin" || strings.Join(got.Targets, ",") != "el9,el10" {
		t.Errorf("sign-deployed confirmation: %s", b)
	}
	if d := e.do(http.MethodGet, "/s/"+testSet+"/-/dialog/sign-deployed?key=a.admin", map[string]string{"HX-Request": "true"}, nil).Body.String(); !strings.Contains(d, `name="target" value="el9"`) || !strings.Contains(d, `name="release"`) {
		t.Errorf("sign-deployed dialog has no target or release field:\n%s", d)
	}
}

func TestAgentOff(t *testing.T) {
	e := newTestEnv(t, fullSource(t))
	e.login()
	w := e.do(http.MethodPost, "/s/"+testSet+"/-/agent/explain-failure", map[string]string{CSRFHeader: e.csrf, "HX-Request": "true"}, url.Values{"run": {testRun}, "node": {"slurm@el10"}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "No agent is configured") {
		t.Errorf("agent without agent.command: %d %s", w.Code, w.Body)
	}
	if w := e.do(http.MethodGet, "/s/"+testSet+"/", nil, nil); strings.Contains(w.Body.String(), "btn-agent") {
		t.Error("an agent button on a page without an agent")
	}
	args := AgentArgs(AgentConfig{Command: "claude", Args: []string{"--model", "x"}}, "/usr/bin/repoctl", "/run/plugin", "triage-build", "run=r node=n")
	got := strings.Join(args, " ")
	for _, want := range []string{"-p /repoctl-ui:triage-build run=r node=n", "--plugin-dir /run/plugin", "--strict-mcp-config", `"args":["mcp","start","--tools","read"]`, "--disallowedTools Bash Edit Write NotebookEdit"} {
		if !strings.Contains(got, want) {
			t.Errorf("agent args: no %q in %s", want, got)
		}
	}
}

func TestSetList(t *testing.T) {
	p := filepath.Join(t.TempDir(), "-ui", "sets.yaml")
	if err := WriteSetList(p, []SetEntry{{Name: "a", Path: "/a"}, {Name: "b", Path: "/b"}}); err != nil {
		t.Fatal(err)
	}
	ok, err := RemoveFromSetList(p, "a")
	if err != nil || !ok {
		t.Fatalf("remove a: %v %v", ok, err)
	}
	if ok, _ := RemoveFromSetList(p, "a"); ok {
		t.Error("removed a twice")
	}
	sets, _ := ReadSetList(p)
	if len(sets) != 1 || sets[0].Name != "b" {
		t.Errorf("sets: %+v", sets)
	}
}

func TestDoctorChecks(t *testing.T) {
	e := newTestEnv(t, fullSource(t))
	cs := DoctorChecks(e.paths, "0.1.0-test", AgentConfig{})
	if len(cs) != 2 || cs[0].Name != "host.stale-server" || cs[0].Status != api.CheckStatusOk {
		t.Fatalf("checks: %+v", cs)
	}
	rec, err := lockRecord(e.paths.UIJSON())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.release()
	if err := rec.write(Record{PID: os.Getpid(), Version: "0.0.9", Port: 1}); err != nil {
		t.Fatal(err)
	}
	cs = DoctorChecks(e.paths, "0.1.0-test", AgentConfig{})
	if cs[0].Status != api.CheckStatusWarning || !strings.Contains(cs[0].Detail, "0.0.9") {
		t.Errorf("a server of another version: %+v", cs[0])
	}
}
