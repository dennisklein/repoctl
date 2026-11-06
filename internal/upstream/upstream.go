// Package upstream compares the version of each package at its lock commit
// with the newest upstream release: GitHub releases and tags with the tag
// filter and the version expression of packages[].upstream, and the
// version of Fedora rawhide (docs/design/cli.html#cmd-upstream). The
// answers stay in $XDG_CACHE_HOME/repoctl/upstream/ for one hour.
package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GSI-HPC/go-clikit/fanout"
	"github.com/GSI-HPC/go-clikit/fsutil"

	"github.com/dennisklein/repoctl/internal/advisory/rpmmd"
	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// Test hooks: the base URLs of the GitHub API and of the Fedora metadata
// API. The tests point them at httptest servers.
const (
	EnvTestGitHubAPI = "REPOCTL_TEST_GITHUB_API"
	EnvTestFedoraAPI = "REPOCTL_TEST_FEDORA_API"
)

// The default base URLs.
const (
	GitHubAPI = "https://api.github.com"
	FedoraAPI = "https://mdapi.fedoraproject.org"
)

// TTL is how long an answer stays in the cache.
const TTL = time.Hour

// Client asks the upstreams.
type Client struct {
	Paths     config.Paths
	HTTP      *http.Client
	GitHubAPI string
	FedoraAPI string
	// Token is the GitHub token: GH_TOKEN, GITHUB_TOKEN or gh auth token.
	Token string
	// Refresh ignores the cache; the answers still go into it.
	Refresh bool
	// Jobs is the number of queries at the same time.
	Jobs int
	Now  func() time.Time
	// Current gives the version of a package at its lock commit; nil reads
	// the spec with rpmspec.
	Current func(ctx context.Context, src workspace.Source) (string, error)
}

// NewClient gives a client from the environment.
func NewClient(p config.Paths, getenv func(string) string) *Client {
	c := &Client{Paths: p, HTTP: &http.Client{Timeout: 30 * time.Second}, GitHubAPI: GitHubAPI, FedoraAPI: FedoraAPI, Jobs: 4, Now: time.Now}
	test := false
	if v := getenv(EnvTestGitHubAPI); v != "" {
		c.GitHubAPI, test = strings.TrimSuffix(v, "/"), true
	}
	if v := getenv(EnvTestFedoraAPI); v != "" {
		c.FedoraAPI = strings.TrimSuffix(v, "/")
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := getenv(k); v != "" {
			c.Token = v
			break
		}
	}
	if c.Token == "" && !test {
		if out, err := exec.Command("gh", "auth", "token").Output(); err == nil {
			c.Token = strings.TrimSpace(string(out))
		}
	}
	return c
}

// Report is the answer of Query.
type Report struct {
	// Items are the packages in the order of repoctl.yaml.
	Items []api.Upstream
	// Failed holds the packages whose queries all failed.
	Failed map[string]error
	// Warnings are upstream.rate-limited and upstream.no-match.
	Warnings []api.Problem
	// Reasons gives a short reason for each package with status unknown,
	// for a table row; the notes of its item keep the detail.
	Reasons map[string]string
	// Queries counts the packages that asked an upstream.
	Queries int
}

// AllFailed reports that each package that asked an upstream failed.
func (r *Report) AllFailed() bool { return r.Queries > 0 && len(r.Failed) == r.Queries }

// Query asks the upstreams of the packages (nil: all packages of the set).
func (c *Client) Query(ctx context.Context, set *config.Set, setDir string, lf *lock.File, pkgs []string) (*Report, error) {
	var sources map[string]workspace.Source
	if lf != nil {
		var err error
		sources, err = workspace.Sources(ctx, c.Paths, set, setDir, lf, workspace.FromLock)
		if err != nil {
			return nil, err
		}
	}
	var list []config.Package
	want := map[string]bool{}
	for _, p := range pkgs {
		want[p] = true
	}
	for _, p := range set.Packages {
		if len(want) == 0 || want[p.ID] {
			list = append(list, p)
		}
	}
	rep := &Report{Items: make([]api.Upstream, len(list)), Failed: map[string]error{}, Reasons: map[string]string{}}
	var mu sync.Mutex
	fanout.Each(ctx, len(list), c.Jobs, func(i int) {
		p := list[i]
		src, ok := sources[p.ID]
		item, warns, err := c.one(ctx, p, src, ok)
		mu.Lock()
		defer mu.Unlock()
		rep.Items[i] = item
		rep.Warnings = append(rep.Warnings, warns...)
		if p.Upstream != nil {
			rep.Queries++
		}
		if err != nil {
			rep.Failed[p.ID] = err
		}
		if r := reason(item, err); r != "" {
			rep.Reasons[p.ID] = r
		}
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(rep.Warnings, func(i, j int) bool { return rep.Warnings[i].Package < rep.Warnings[j].Package })
	return rep, nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// one gives the state of one package. err is set when all its queries
// failed.
func (c *Client) one(ctx context.Context, p config.Package, src workspace.Source, haveSrc bool) (api.Upstream, []api.Problem, error) {
	item := api.Upstream{Package: p.ID, Notes: p.Notes, CheckedAt: api.FormatTime(c.now())}
	var warns []api.Problem
	curNote := ""
	if haveSrc {
		item.Current.Commit = src.Commit
		get := c.Current
		if get == nil {
			get = func(ctx context.Context, s workspace.Source) (string, error) {
				return SpecQuery(ctx, s, "%{VERSION}")
			}
		}
		v, err := get(ctx, src)
		if err != nil {
			curNote = "current version unknown: " + firstLine(err.Error())
		} else {
			item.Current.Version = v
		}
	} else {
		curNote = "no lock line"
	}
	up := p.Upstream
	if up == nil {
		item.Status = api.UpstreamStatusNone
		return item, nil, nil
	}
	fromCache := true
	failures, queries := 0, 0
	var firstErr error
	var gh *ghAnswer
	if up.GitHub != "" {
		queries++
		a, cached, err := c.github(ctx, up)
		fromCache = fromCache && cached
		if err != nil {
			failures++
			firstErr = err
			if isRateLimited(err) {
				warns = append(warns, api.Problem{Code: api.PUpstreamRateLimited, Message: "the GitHub API limit is reached; set GH_TOKEN", Package: p.ID, Hint: "set GH_TOKEN in the environment or in ~/.config/repoctl/check.env"})
			}
		} else {
			gh = a
		}
	}
	if up.Fedora != "" {
		queries++
		fv, cached, err := c.fedora(ctx, up.Fedora)
		fromCache = fromCache && cached
		if err != nil {
			failures++
			if firstErr == nil {
				firstErr = err
			}
		} else if fv != "" {
			item.Fedora = &api.UpstreamFedora{Package: up.Fedora, Version: fv, Branch: "rawhide", URL: "https://src.fedoraproject.org/rpms/" + up.Fedora}
		}
	}
	item.FromCache = queries > 0 && fromCache
	if queries > 0 && failures == queries {
		item.Status = api.UpstreamStatusUnknown
		item.Notes = joinNote("all queries failed: "+firstLine(firstErr.Error()), p.Notes)
		return item, warns, firstErr
	}
	latest := ""
	if gh != nil {
		g, warn := pick(gh, up, item.Current.Version)
		item.Github = g
		if warn != nil {
			warn.Package = p.ID
			warns = append(warns, *warn)
		}
		latest = g.Version
	}
	if latest == "" && item.Fedora != nil && (up.GitHub == "" || gh == nil) {
		latest = item.Fedora.Version
	}
	item.Latest = latest
	switch {
	case item.Current.Version == "":
		item.Status = api.UpstreamStatusUnknown
		item.Notes = joinNote(curNote, p.Notes)
	case up.Frozen != "" && rpmmd.Vercmp(item.Current.Version, up.Frozen) >= 0:
		item.Status = api.UpstreamStatusFrozen
		item.Latest = up.Frozen
	case up.Frozen != "":
		item.Status = api.UpstreamStatusUpdateAvailable
		item.Latest = up.Frozen
	case latest == "":
		item.Status = api.UpstreamStatusUnknown
	default:
		switch c := rpmmd.Vercmp(latest, item.Current.Version); {
		case c > 0:
			item.Status = api.UpstreamStatusUpdateAvailable
		case c < 0:
			item.Status = api.UpstreamStatusAhead
		default:
			item.Status = api.UpstreamStatusUpToDate
		}
	}
	return item, warns, nil
}

// Source gives where the latest version of an item comes from: github or
// fedora.
func Source(u api.Upstream) string {
	if u.Github != nil && u.Github.Version != "" && u.Github.Version == u.Latest {
		return "github"
	}
	if u.Fedora != nil && u.Fedora.Version == u.Latest {
		return "fedora"
	}
	return "github"
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// release is one tag of a GitHub repository, with the release data when
// the tag has a release.
type release struct {
	Tag        string
	Version    string
	Date       string
	URL        string
	Notes      string
	Prerelease bool
	SHA        string
}

type ghAnswer struct {
	repo string
	tags []release
	c    *Client
	ctx  context.Context
}

type ghRelease struct {
	TagName     string `json:"tag_name"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
	Prerelease  bool   `json:"prerelease"`
	Draft       bool   `json:"draft"`
}

type ghTag struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

func (c *Client) github(ctx context.Context, up *config.Upstream) (*ghAnswer, bool, error) {
	repo := strings.Trim(up.GitHub, "/")
	cached := true
	var rels []ghRelease
	for page := 1; page <= 3; page++ {
		var batch []ghRelease
		hit, more, err := c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", c.GitHubAPI, repo, page), true, &batch)
		cached = cached && hit
		if err != nil {
			return nil, false, err
		}
		rels = append(rels, batch...)
		if !more {
			break
		}
	}
	var tags []ghTag
	for page := 1; page <= 3; page++ {
		var batch []ghTag
		hit, more, err := c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/tags?per_page=100&page=%d", c.GitHubAPI, repo, page), true, &batch)
		cached = cached && hit
		if err != nil {
			return nil, false, err
		}
		tags = append(tags, batch...)
		if !more {
			break
		}
	}
	byTag := map[string]*release{}
	var order []string
	add := func(name string) *release {
		r, ok := byTag[name]
		if !ok {
			r = &release{Tag: name}
			byTag[name] = r
			order = append(order, name)
		}
		return r
	}
	for _, t := range tags {
		add(t.Name).SHA = t.Commit.SHA
	}
	for _, rl := range rels {
		if rl.Draft {
			continue
		}
		r := add(rl.TagName)
		r.URL, r.Notes, r.Date, r.Prerelease = rl.HTMLURL, rl.HTMLURL, rl.PublishedAt, rl.Prerelease
	}
	a := &ghAnswer{repo: repo, c: c, ctx: ctx}
	for _, n := range order {
		a.tags = append(a.tags, *byTag[n])
	}
	return a, cached, nil
}

var plainVersion = regexp.MustCompile(`^\d+(\.\d+)*$`)

// preRelease matches the rest of a tag after upstream.version that marks a
// pre-release: slurm-26-11-0-0rc1.
var preRelease = regexp.MustCompile(`(?i)(rc|alpha|beta|pre|dev|snapshot)`)

// TagVersion gives the version of a tag: the capture groups of expr joined
// with ".", or without expr the tag without a leading v, or the part after
// the last - or _ (therock-7.14.0 → 7.14.0). It gives "" for a tag that is
// a pre-release or a vendor tag: the version must be digits and dots.
func TagVersion(tag string, expr *regexp.Regexp) string {
	var v string
	if expr != nil {
		m := expr.FindStringSubmatch(tag)
		if m == nil {
			return ""
		}
		if end := strings.Index(tag, m[0]) + len(m[0]); preRelease.MatchString(tag[end:]) {
			return ""
		}
		if len(m) == 1 {
			v = m[0]
		} else {
			v = strings.Join(m[1:], ".")
		}
	} else {
		v = strings.TrimPrefix(tag, "v")
		if !plainVersion.MatchString(v) {
			if i := strings.LastIndexAny(tag, "-_"); i >= 0 {
				v = strings.TrimPrefix(tag[i+1:], "v")
			}
		}
	}
	if !plainVersion.MatchString(v) {
		return ""
	}
	return v
}

// pick chooses the newest release of a GitHub answer after the tag filter
// and the version expression, and lists the newer tags.
func pick(a *ghAnswer, up *config.Upstream, current string) (*api.UpstreamGithub, *api.Problem) {
	g := &api.UpstreamGithub{Repo: a.repo, Filter: up.Tags}
	var filter, expr *regexp.Regexp
	if up.Tags != "" {
		re, err := regexp.Compile(up.Tags)
		if err != nil {
			return g, &api.Problem{Code: api.PUpstreamNoMatch, Message: fmt.Sprintf("upstream.tags %q is not a regular expression: %v", up.Tags, err)}
		}
		filter = re
	}
	if up.Version != "" {
		re, err := regexp.Compile(up.Version)
		if err != nil {
			return g, &api.Problem{Code: api.PUpstreamNoMatch, Message: fmt.Sprintf("upstream.version %q is not a regular expression: %v", up.Version, err)}
		}
		expr = re
	}
	var cands []release
	matched := 0
	for _, r := range a.tags {
		if filter != nil && !filter.MatchString(r.Tag) {
			continue
		}
		matched++
		if r.Prerelease {
			continue
		}
		r.Version = TagVersion(r.Tag, expr)
		if r.Version == "" {
			continue
		}
		cands = append(cands, r)
	}
	if len(cands) == 0 {
		what := "the tag filter matches no tag"
		if matched > 0 {
			what = "no tag after the filter gives a release version"
		}
		return g, &api.Problem{Code: api.PUpstreamNoMatch, Message: fmt.Sprintf("%s: %s", a.repo, what), Hint: "correct upstream.tags or upstream.version in repoctl.yaml"}
	}
	sort.SliceStable(cands, func(i, j int) bool { return rpmmd.Vercmp(cands[i].Version, cands[j].Version) < 0 })
	best := cands[len(cands)-1]
	if up.Frozen != "" {
		for _, r := range cands {
			if r.Version == up.Frozen {
				best = r
			}
		}
	}
	if best.Date == "" && best.SHA != "" {
		best.Date = a.commitDate(best.SHA)
	}
	g.Tag, g.Version = best.Tag, best.Version
	if t, err := time.Parse(time.RFC3339, best.Date); err == nil {
		g.Date = api.FormatTime(t)
	}
	g.URL = best.URL
	if g.URL == "" {
		g.URL = "https://github.com/" + a.repo + "/tree/" + best.Tag
	}
	g.ReleaseNotesURL = best.Notes
	if current != "" {
		seen := map[string]bool{}
		for _, r := range cands {
			if rpmmd.Vercmp(r.Version, current) > 0 && rpmmd.Vercmp(r.Version, best.Version) <= 0 && !seen[r.Tag] {
				seen[r.Tag] = true
				g.Newer = append(g.Newer, r.Tag)
			}
		}
	}
	return g, nil
}

func (a *ghAnswer) commitDate(sha string) string {
	var c struct {
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if _, _, err := a.c.getJSON(a.ctx, fmt.Sprintf("%s/repos/%s/commits/%s", a.c.GitHubAPI, a.repo, sha), true, &c); err != nil {
		return ""
	}
	return c.Commit.Committer.Date
}

func (c *Client) fedora(ctx context.Context, name string) (string, bool, error) {
	var v struct {
		Version string `json:"version"`
	}
	hit, _, err := c.getJSON(ctx, c.FedoraAPI+"/rawhide/srcpkg/"+name, false, &v)
	var he *httpError
	if errors.As(err, &he) && he.status == http.StatusNotFound {
		return "", hit, nil
	}
	if err != nil {
		return "", hit, err
	}
	return v.Version, hit, nil
}

type httpError struct {
	url    string
	status int
	text   string
	rate   bool
}

// reason gives the short reason of a package with status unknown.
func reason(item api.Upstream, err error) string {
	switch {
	case err != nil:
		return ErrorReason(err)
	case item.Status != api.UpstreamStatusUnknown:
		return ""
	case strings.HasPrefix(item.Notes, "no lock line"):
		return "no lock line"
	case item.Current.Version == "":
		return "current version unknown"
	}
	return "no upstream version"
}

// ErrorReason gives a failed query in a few words: "GitHub API limit
// reached", "HTTP 502 from api.github.com", "api.github.com unreachable"
// or "api.github.com timed out".
func ErrorReason(err error) string {
	var he *httpError
	if errors.As(err, &he) {
		if he.rate {
			return "GitHub API limit reached"
		}
		return fmt.Sprintf("HTTP %d from %s", he.status, hostOf(he.url))
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return hostOf(ue.URL) + " timed out"
		}
		return hostOf(ue.URL) + " unreachable"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	return "query failed"
}

// hostOf gives the host of a URL, or the URL itself.
func hostOf(s string) string {
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Host
	}
	return s
}

func (e *httpError) Error() string {
	if e.rate {
		return e.url + ": the GitHub API limit is reached (upstream.rate-limited)"
	}
	return e.url + ": " + e.text
}

func isRateLimited(err error) bool {
	var he *httpError
	return errors.As(err, &he) && he.rate
}

type cacheEntry struct {
	URL  string          `json:"url"`
	At   time.Time       `json:"at"`
	Body json.RawMessage `json:"body"`
	More bool            `json:"more,omitempty"`
}

func (c *Client) cacheFile(url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(c.Paths.UpstreamCache(), hex.EncodeToString(sum[:])[:20]+".json")
}

// getJSON reads a JSON answer from the cache or the network. It reports a
// cache hit and whether a next page exists.
func (c *Client) getJSON(ctx context.Context, url string, github bool, into any) (bool, bool, error) {
	file := c.cacheFile(url)
	if !c.Refresh {
		if b, err := os.ReadFile(file); err == nil {
			var e cacheEntry
			if json.Unmarshal(b, &e) == nil && e.URL == url && c.now().Sub(e.At) < TTL && c.now().Sub(e.At) >= -time.Minute {
				if err := json.Unmarshal(e.Body, into); err == nil {
					return true, e.More, nil
				}
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "repoctl/"+config.Version)
	if github {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, false, fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return false, false, err
	}
	if resp.StatusCode != http.StatusOK {
		he := &httpError{url: url, status: resp.StatusCode, text: resp.Status}
		if github && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
			resp.Header.Get("X-RateLimit-Remaining") == "0" {
			he.rate = true
		}
		return false, false, he
	}
	if err := json.Unmarshal(body, into); err != nil {
		return false, false, fmt.Errorf("%s: the answer is not JSON: %w", url, err)
	}
	more := strings.Contains(resp.Header.Get("Link"), `rel="next"`)
	if b, err := json.Marshal(cacheEntry{URL: url, At: c.now().UTC(), Body: body, More: more}); err == nil {
		if os.MkdirAll(filepath.Dir(file), 0o700) == nil {
			_ = fsutil.WriteAtomic(file, b, 0o600)
		}
	}
	return false, more, nil
}
