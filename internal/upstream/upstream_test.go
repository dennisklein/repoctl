package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/testkit"
	"github.com/dennisklein/repoctl/internal/workspace"
)

func TestTagVersion(t *testing.T) {
	slurm := regexp.MustCompile(`^slurm-(\d+)-(\d+)-(\d+)`)
	for _, c := range []struct {
		tag  string
		expr *regexp.Regexp
		want string
	}{
		{"slurm-26-05-4-1", slurm, "26.05.4"},
		{"slurm-26-05-5-1", slurm, "26.05.5"},
		{"slurm-26-11-0-0rc1", slurm, ""},
		{"v26.05.4", slurm, ""},
		{"v5.0.12", nil, "5.0.12"},
		{"5.0.12", nil, "5.0.12"},
		{"v6.0.0rc1", nil, ""},
		{"munge-0.5.18", nil, "0.5.18"},
		{"therock-7.14.0", nil, "7.14.0"},
		{"pmix_v4.2.9", nil, "4.2.9"},
		{"latest", nil, ""},
		{"v1.2.3-beta", nil, ""},
	} {
		if got := TagVersion(c.tag, c.expr); got != c.want {
			t.Errorf("TagVersion(%q) = %q, want %q", c.tag, got, c.want)
		}
	}
}

// fakeAPI serves GitHub under /gh and Fedora under /fedora.
type fakeAPI struct {
	srv      *httptest.Server
	calls    atomic.Int32
	limited  atomic.Bool
	releases map[string]string
	tags     map[string]string
	fedora   map[string]string
}

func newFake(t *testing.T) *fakeAPI {
	f := &fakeAPI{
		releases: map[string]string{
			"open-mpi/ompi": `[{"tag_name":"v5.0.12","html_url":"https://github.com/open-mpi/ompi/releases/tag/v5.0.12","published_at":"2026-10-21T10:00:00Z"},` +
				`{"tag_name":"v6.0.0rc1","html_url":"x","published_at":"2026-10-25T10:00:00Z","prerelease":true},` +
				`{"tag_name":"v5.0.13","html_url":"x","published_at":"2026-10-26T10:00:00Z","draft":true}]`,
			"openpmix/openpmix": `[{"tag_name":"v4.2.9","html_url":"https://github.com/openpmix/openpmix/releases/tag/v4.2.9","published_at":"2024-02-01T10:00:00Z"},` +
				`{"tag_name":"v4.2.8","html_url":"u","published_at":"2023-12-01T10:00:00Z"},{"tag_name":"v6.1.0","html_url":"u","published_at":"2026-06-01T10:00:00Z"}]`,
		},
		tags: map[string]string{
			"SchedMD/slurm": `[{"name":"slurm-26-05-5-1","commit":{"sha":"aaa"}},{"name":"slurm-26-05-4-1","commit":{"sha":"bbb"}},` +
				`{"name":"slurm-26-11-0-0rc1","commit":{"sha":"ccc"}},{"name":"slurm-25-11-9-1","commit":{"sha":"ddd"}}]`,
			"open-mpi/ompi": `[{"name":"v5.0.12","commit":{"sha":"d1"}},{"name":"v5.0.11","commit":{"sha":"d2"}},{"name":"v4.1.8","commit":{"sha":"d3"}}]`,
		},
		fedora: map[string]string{"slurm": "26.05.4", "openmpi": "5.0.11", "ucx": "1.22.0"},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		p := r.URL.Path
		if rest, ok := strings.CutPrefix(p, "/gh/repos/"); ok {
			if f.limited.Load() {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			parts := strings.SplitN(rest, "/", 4)
			repo := parts[0] + "/" + parts[1]
			body := ""
			switch parts[2] {
			case "releases":
				body = f.releases[repo]
			case "tags":
				body = f.tags[repo]
			case "commits":
				body = `{"commit":{"committer":{"date":"2026-10-29T12:00:00Z"}}}`
			}
			if body == "" {
				body = "[]"
			}
			fmt.Fprint(w, body)
			return
		}
		if name, ok := strings.CutPrefix(p, "/fedora/rawhide/srcpkg/"); ok {
			if v := f.fedora[name]; v != "" {
				fmt.Fprintf(w, `{"basename":%q,"version":%q,"release":"1.fc45"}`, name, v)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) client(t *testing.T, cache string) *Client {
	getenv := func(k string) string {
		switch k {
		case EnvTestGitHubAPI:
			return f.srv.URL + "/gh"
		case EnvTestFedoraAPI:
			return f.srv.URL + "/fedora/"
		}
		return ""
	}
	c := NewClient(config.Paths{CacheHome: cache}, getenv)
	c.HTTP = f.srv.Client()
	c.Now = func() time.Time { return time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC) }
	return c
}

func current(v string) func(context.Context, workspace.Source) (string, error) {
	return func(context.Context, workspace.Source) (string, error) { return v, nil }
}

func TestOne(t *testing.T) {
	f := newFake(t)
	c := f.client(t, t.TempDir())
	if c.GitHubAPI != f.srv.URL+"/gh" || c.FedoraAPI != f.srv.URL+"/fedora" {
		t.Fatalf("base URLs: %s %s", c.GitHubAPI, c.FedoraAPI)
	}
	src := workspace.Source{Package: "x", Commit: "60e1f6dd0d0f984b8ed068534b7995821e42af75"}
	for _, tc := range []struct {
		name    string
		up      *config.Upstream
		current string
		status  api.UpstreamStatus
		latest  string
		tag     string
		warn    api.ProblemCode
	}{
		{"slurm", &config.Upstream{GitHub: "SchedMD/slurm", Tags: "^slurm-", Version: `^slurm-(\d+)-(\d+)-(\d+)`, Fedora: "slurm"},
			"26.05.4", api.UpstreamStatusUpdateAvailable, "26.05.5", "slurm-26-05-5-1", ""},
		{"openmpi", &config.Upstream{GitHub: "open-mpi/ompi", Tags: `^v5\.`, Fedora: "openmpi"},
			"5.0.12", api.UpstreamStatusUpToDate, "5.0.12", "v5.0.12", ""},
		{"pmix_v4", &config.Upstream{GitHub: "openpmix/openpmix", Tags: "^v4", Frozen: "4.2.9"},
			"4.2.9", api.UpstreamStatusFrozen, "4.2.9", "v4.2.9", ""},
		{"pmix_v4-old", &config.Upstream{GitHub: "openpmix/openpmix", Tags: "^v4", Frozen: "4.2.9"},
			"4.2.8", api.UpstreamStatusUpdateAvailable, "4.2.9", "v4.2.9", ""},
		{"snapshot", &config.Upstream{GitHub: "openpmix/openpmix", Tags: "^v6"},
			"6.2.0^20261101git1234", api.UpstreamStatusAhead, "6.1.0", "v6.1.0", ""},
		{"nomatch", &config.Upstream{GitHub: "openpmix/openpmix", Tags: "^v9"},
			"9.0", api.UpstreamStatusUnknown, "", "", api.PUpstreamNoMatch},
		{"ucx", &config.Upstream{Fedora: "ucx"}, "1.21.0", api.UpstreamStatusUpdateAvailable, "1.22.0", "", ""},
	} {
		c.Current = current(tc.current)
		item, warns, err := c.one(t.Context(), config.Package{ID: tc.name, Upstream: tc.up}, src, true)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if item.Status != tc.status || item.Latest != tc.latest {
			t.Errorf("%s: %s %s, want %s %s", tc.name, item.Status, item.Latest, tc.status, tc.latest)
		}
		if tc.tag != "" && (item.Github == nil || item.Github.Tag != tc.tag) {
			t.Errorf("%s: github %+v, want the tag %s", tc.name, item.Github, tc.tag)
		}
		if tc.warn != "" && (len(warns) != 1 || warns[0].Code != tc.warn || warns[0].Package != tc.name) {
			t.Errorf("%s: warnings %+v, want %s", tc.name, warns, tc.warn)
		}
		if err := testkit.Validate("Upstream", testkit.MarshalItem(t, item)); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// The slurm item in detail: the commit date of a tag without a release,
	// the newer tags and Fedora.
	c.Current = current("26.05.3")
	item, _, _ := c.one(t.Context(), config.Package{ID: "slurm", Upstream: &config.Upstream{GitHub: "SchedMD/slurm", Tags: "^slurm-", Version: `^slurm-(\d+)-(\d+)-(\d+)`, Fedora: "slurm"}}, src, true)
	if g := item.Github; g.Date != "2026-10-29T12:00:00.000Z" || strings.Join(g.Newer, ",") != "slurm-26-05-4-1,slurm-26-05-5-1" ||
		g.URL != "https://github.com/SchedMD/slurm/tree/slurm-26-05-5-1" {
		t.Errorf("slurm github: %+v", g)
	}
	if item.Fedora == nil || item.Fedora.Version != "26.05.4" || !item.FromCache || Source(item) != "github" {
		t.Errorf("slurm: fedora %+v, from cache %v", item.Fedora, item.FromCache)
	}
	// No upstream key: none, without a query.
	n := f.calls.Load()
	if item, _, _ := c.one(t.Context(), config.Package{ID: "slurm-release"}, src, true); item.Status != api.UpstreamStatusNone || f.calls.Load() != n {
		t.Errorf("none: %+v", item)
	}
	// The current version comes from the lock commit; without it the
	// status is unknown.
	if item, _, _ := c.one(t.Context(), config.Package{ID: "x", Upstream: &config.Upstream{Fedora: "ucx"}}, src, false); item.Status != api.UpstreamStatusUnknown || !strings.Contains(item.Notes, "no lock line") {
		t.Errorf("no source: %+v", item)
	}
}

func TestCacheAndRateLimit(t *testing.T) {
	f := newFake(t)
	cache := t.TempDir()
	c := f.client(t, cache)
	c.Current = current("5.0.11")
	p := config.Package{ID: "openmpi", Upstream: &config.Upstream{GitHub: "open-mpi/ompi", Tags: `^v5\.`, Fedora: "openmpi"}}
	if _, _, err := c.one(t.Context(), p, workspace.Source{}, true); err != nil {
		t.Fatal(err)
	}
	n := f.calls.Load()
	item, _, err := c.one(t.Context(), p, workspace.Source{}, true)
	if err != nil || f.calls.Load() != n || !item.FromCache {
		t.Errorf("second query: %d new calls, from cache %v, %v", f.calls.Load()-n, item.FromCache, err)
	}
	files, _ := filepath.Glob(filepath.Join(cache, "upstream", "*.json"))
	if len(files) == 0 {
		t.Errorf("no cache files in %s", cache)
	}
	// After the TTL the client asks again.
	c.Now = func() time.Time { return time.Date(2026, 11, 3, 10, 1, 0, 0, time.UTC) }
	if _, _, _ = c.one(t.Context(), p, workspace.Source{}, true); f.calls.Load() == n {
		t.Error("the cache outlived its TTL")
	}
	// --refresh with the limit reached: GitHub fails with
	// upstream.rate-limited; Fedora still answers.
	f.limited.Store(true)
	c.Refresh = true
	item, warns, err := c.one(t.Context(), p, workspace.Source{}, true)
	if err != nil || len(warns) != 1 || warns[0].Code != api.PUpstreamRateLimited || item.Latest != "5.0.11" || item.Status != api.UpstreamStatusUpToDate {
		t.Errorf("rate limit: %+v %+v %v", item, warns, err)
	}
	// All queries failed: unknown and an error.
	p.Upstream.Fedora = ""
	item, _, err = c.one(t.Context(), p, workspace.Source{}, true)
	if err == nil || item.Status != api.UpstreamStatusUnknown || !isRateLimited(err) {
		t.Errorf("all failed: %+v %v", item, err)
	}
}

// TestReason: an unknown row gets a short reason; the notes keep the
// error.
func TestReason(t *testing.T) {
	unknown := api.Upstream{Status: api.UpstreamStatusUnknown}
	dial := &url.Error{Op: "Get", URL: "https://api.github.com/repos/SchedMD/slurm/tags?per_page=100", Err: errors.New("dial tcp: lookup api.github.com: no such host")}
	for _, tc := range []struct {
		item api.Upstream
		err  error
		want string
	}{
		{unknown, &httpError{url: "https://api.github.com/x", rate: true}, "GitHub API limit reached"},
		{unknown, &httpError{url: "https://src.fedoraproject.org/api/0/x", status: 502, text: "502 Bad Gateway"}, "HTTP 502 from src.fedoraproject.org"},
		{unknown, fmt.Errorf("github: %w", dial), "api.github.com unreachable"},
		{unknown, context.DeadlineExceeded, "timed out"},
		{api.Upstream{Status: api.UpstreamStatusUnknown, Notes: "no lock line"}, nil, "no lock line"},
		{api.Upstream{Status: api.UpstreamStatusUnknown, Notes: "current version unknown: rpmspec: error: line 12: bad"}, nil, "current version unknown"},
		{api.Upstream{Status: api.UpstreamStatusUnknown, Current: api.UpstreamCurrent{Version: "1.0"}}, nil, "no upstream version"},
		{api.Upstream{Status: api.UpstreamStatusUpToDate}, nil, ""},
	} {
		if got := reason(tc.item, tc.err); got != tc.want {
			t.Errorf("reason(%q, %v) = %q, want %q", tc.item.Notes, tc.err, got, tc.want)
		}
	}
}
