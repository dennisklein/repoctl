package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/localweb/htmx"
	"github.com/GSI-HPC/go-clikit/localweb/page"

	"github.com/dennisklein/repoctl/internal/api"
)

// pageView is the data of the layout: the shell around the content block.
type pageView struct {
	Title    string
	Set      *setCtx
	Nav      string
	CSRF     string
	Who      string
	Version  string
	Events   string
	Chip     *chipView
	NavRuns  string
	NavAdv   string
	NavPlan  string
	Banner   string
	AgentOff string
	Content  string
	Data     any
	Bare     bool
	Theme    string
	CLI      []string
}

// CSRFHeaders gives the hx-headers value of the body.
func (v *pageView) CSRFHeaders() string {
	b, _ := json.Marshal(map[string]string{CSRFHeader: v.CSRF})
	return string(b)
}

// built is what a page builder gives: the title, the data of the content
// block, the CLI commands of the page and its live regions.
type built struct {
	Title  string
	Data   any
	CLI    []string
	Live   []liveRegion
	Status int
	// Log is the node log a node page tails, or nil.
	Log *logTail
}

// liveRegion is a fragment that the stream of a page pushes: the event
// name, the block that renders it and the data of the block.
type liveRegion struct {
	Event string
	Block string
	Data  any
}

// pageReq is one build of a page: for the page itself, a fragment or the
// stream.
type pageReq struct {
	s      *Server
	r      *http.Request
	ctx    context.Context
	set    *setCtx
	params url.Values
}

// p gives the parameter name: a path value of the page, or a query
// parameter (the stream carries the path values as parameters).
func (pr *pageReq) p(name string) string {
	if v := pr.r.PathValue(name); v != "" {
		return v
	}
	return pr.params.Get(name)
}

type builder func(pr *pageReq) *built

var builders map[string]builder

func init() {
	builders = map[string]builder{
		"sets": buildSets, "doctor": buildDoctor, "overview": buildOverview, "graph": buildGraph,
		"plan": buildPlan, "advisories": buildAdvisories, "upstream": buildUpstream, "runs": buildRuns,
		"run": buildRun, "node": buildNode, "packages": buildPackages, "package": buildPackage,
		"repo": buildRepo, "diff": buildDiff, "publish": buildPublish, "signing": buildSigning, "config": buildConfig,
	}
}

// pathParams are the path values a page carries into its stream.
var pathParams = []string{"run", "node", "pkg", "target"}

// page gives the handler of the page kind.
func (s *Server) page(kind, nav string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pr := &pageReq{s: s, r: r, ctx: r.Context(), params: r.URL.Query()}
		if name := r.PathValue("set"); name != "" {
			sc, ok := s.setOf(name)
			if !ok {
				s.fail(w, r, http.StatusNotFound, fmt.Sprintf("The set %s is not on the set list. Run repoctl ui in its directory to add it.", name))
				return
			}
			pr.set = sc
		}
		b := builders[kind](pr)
		v := s.shell(pr, kind, nav, b)
		status := b.Status
		if status == 0 {
			status = http.StatusOK
		}
		s.respond(w, r, status, "layout", v)
	}
}

// shell gives the layout data of a built page.
func (s *Server) shell(pr *pageReq, kind, nav string, b *built) *pageView {
	v := &pageView{
		Title: b.Title, Set: pr.set, Nav: nav, Who: s.who(), Version: s.o.Version, Content: "page-" + kind,
		Data: b.Data, CLI: b.CLI, Banner: s.stale(), AgentOff: s.agentOff(),
	}
	if c, err := pr.r.Cookie("repoctl_theme"); err == nil && (c.Value == "light" || c.Value == "dark") {
		v.Theme = c.Value
	}
	if sess := sessionOf(pr.ctx); sess != nil {
		v.CSRF = sess.csrf
	}
	q := url.Values{"page": {kind}}
	for k, vs := range pr.params {
		if k != "page" && len(vs) > 0 {
			q.Set(k, vs[0])
		}
	}
	for _, k := range pathParams {
		if val := pr.r.PathValue(k); val != "" {
			q.Set(k, val)
		}
	}
	if b.Log != nil {
		q.Set("from", fmt.Sprint(b.Log.End))
	}
	if pr.set != nil {
		v.Events = "/s/" + url.PathEscape(pr.set.Name) + "/-/events?" + q.Encode()
		v.Chip = s.chip(pr.set)
		v.NavRuns = s.navRuns(pr.set)
		v.NavAdv = s.navAdvisories(pr)
	} else {
		v.Events = "/-/events?" + q.Encode()
	}
	if b.Title == "" {
		v.Title = "repoctl"
	} else {
		v.Title = b.Title + " · repoctl"
	}
	return v
}

// chipView is the run chip of the top bar.
type chipView struct {
	Set, ID, Href, Title string
	Done, Total, Failed  int
	Since                string
	Publish              bool
}

func (s *Server) chip(sc *setCtx) *chipView {
	for _, f := range s.runs.list(s.o.Paths.Runs(sc.Name), 10) {
		if !f.Active() {
			continue
		}
		c := &chipView{Set: sc.Name, ID: f.Run.ID, Href: runHref(sc.Name, f.Run.ID), Done: f.Done(), Total: len(f.Builds()), Failed: len(f.Failed()), Since: f.Run.StartedAt, Publish: f.Publish()}
		c.Title = fmt.Sprintf("Run %s of %s: %d of %d builds done", f.Run.ID, sc.Name, c.Done, c.Total)
		if c.Failed > 0 {
			c.Title += fmt.Sprintf(", %d failed", c.Failed)
		}
		c.Title += ". Started " + clock(f.Run.StartedAt) + " by " + f.Run.Origin.User + "."
		return c
	}
	return &chipView{Set: sc.Name}
}

func (s *Server) navRuns(sc *setCtx) string {
	n := 0
	for _, f := range s.runs.list(s.o.Paths.Runs(sc.Name), 10) {
		if f.Active() {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d running", n)
}

func (s *Server) navAdvisories(pr *pageReq) string {
	st, ok := First[api.Status](s.run(pr.ctx, pr.set, "status"))
	if !ok || st.Check == nil || st.Check.Required == 0 {
		return ""
	}
	return fmt.Sprintf("%d required", st.Check.Required)
}

func runHref(set, run string) string {
	return "/s/" + url.PathEscape(set) + "/runs/" + url.PathEscape(run)
}

func nodeHref(set, run, node string) string {
	return runHref(set, run) + "/nodes/" + url.PathEscape(node)
}

// notAvailable is the data of the calm empty state of a command that this
// build does not have, or that failed.
type notAvailable struct {
	What    string
	Command string
	Problem *api.Problem
	NotImpl bool
}

// na gives the empty state of the result r of the command line cmd, or nil
// when r has items.
func na(what, cmd string, r *Result) *notAvailable {
	if r.Usable() {
		return nil
	}
	n := &notAvailable{What: what, Command: cmd, NotImpl: r.NotImplemented()}
	if !n.NotImpl {
		n.Problem = r.Problem()
	}
	return n
}

// funcs are the functions of the templates.
func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"static":     s.assets.URL,
		"setURL":     func(set, p string) string { return "/s/" + url.PathEscape(set) + p },
		"runURL":     runHref,
		"nodeURL":    nodeHref,
		"q":          url.QueryEscape,
		"pe":         url.PathEscape,
		"shortRun":   shortRun,
		"runLabel":   func(id string) string { return runLabel(id, s.o.Now()) },
		"commit":     api.ShortCommit,
		"digest":     api.ShortDigest,
		"fpr":        api.ShortFingerprint,
		"dur":        func(ms int) string { return api.FormatDuration(time.Duration(ms) * time.Millisecond) },
		"clock":      clock,
		"when":       func(t string) string { return when(t, s.o.Now()) },
		"elemID":     elemID,
		"pkgOf":      func(n string) string { p, _ := api.SplitNode(n); return p },
		"tgtOf":      func(n string) string { _, t := api.SplitNode(n); return t },
		"st":         stateClass,
		"dot":        dotClass,
		"qs":         qsClass,
		"pill":       pillState,
		"nvr":        nvrHTML,
		"plural":     plural,
		"join":       func(l []string, sep string) string { return strings.Join(l, sep) },
		"pct":        pct,
		"dict":       dict,
		"add":        func(a, b int) int { return a + b },
		"reason":     reasonText,
		"json":       func(v any) string { b, _ := json.Marshal(v); return string(b) },
		"tilde":      func(p string) string { return tildePath(p, s.o.Home) },
		"cli":        s.cliPrefix,
		"upper":      strings.ToUpper,
		"hasPrefix":  strings.HasPrefix,
		"lines":      func(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") },
		"isHTMX":     page.IsHTMX,
		"strs":       func(v ...string) []string { return v },
		"stepsOf":    stepsOf,
		"truncate":   truncate,
		"agentTitle": agentTitle,
		"htmxConfig": func() string { return htmx.Config },
		"css":        cssFor,
	}
}

// shortRun gives the short form of a run id: r-…a3f.
func shortRun(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:2] + "…" + id[len(id)-3:]
}

// runTime gives the start time in a run id r-YYYYMMDD-HHMM-xxx (UTC).
func runTime(id string) (time.Time, bool) {
	if len(id) < 16 {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102-1504", id[2:15])
	return t, err == nil
}

// runLabel names a run by its start: "Run today 17:50", "Run Sep 12 12:51".
func runLabel(id string, now time.Time) string {
	t, ok := runTime(id)
	if !ok {
		return id
	}
	return "Run " + dayClock(t, now)
}

func dayClock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	switch {
	case sameDay(t, now):
		return "today " + t.Format("15:04")
	case sameDay(t, now.AddDate(0, 0, -1)):
		return "yesterday " + t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("Jan 2 15:04")
	}
	return t.Format("2006-01-02 15:04")
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// clock gives the local time of day of an RFC 3339 time.
func clock(t string) string {
	tt, err := api.ParseTime(t)
	if err != nil {
		return t
	}
	return tt.Local().Format("15:04")
}

// when gives a time in words: today 17:50, Sep 12 12:51.
func when(t string, now time.Time) string {
	tt, err := api.ParseTime(t)
	if err != nil {
		return t
	}
	return dayClock(tt, now)
}

var notID = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// elemID gives an element id: prefix and the rest with every other
// character as "-": n-slurm-el9 for n and slurm@el9.
func elemID(prefix, s string) string {
	return prefix + "-" + notID.ReplaceAllString(s, "-")
}

// stateClass gives the class st-STATE of a state.
func stateClass(state any) string { return "st-" + fmt.Sprint(state) }

// dotClass gives the class of a state dot.
func dotClass(state any) string {
	switch fmt.Sprint(state) {
	case "running", "planning", "queued":
		return "d-running"
	case "ok", "staged":
		return "d-ok"
	case "failed", "abandoned":
		return "d-failed"
	case "blocked", "canceled":
		return "d-blocked"
	case "current":
		return "d-current"
	case "waiting":
		return "d-waiting"
	}
	return "d-none"
}

// qsClass gives the class of a quiet state word (R2: no pill).
func qsClass(state any) string {
	switch fmt.Sprint(state) {
	case "ok", "staged", "in-sync", "up-to-date":
		return "ok"
	case "running", "planning", "queued":
		return "run"
	case "current", "waiting", "in-tree":
		return "cur"
	case "failed", "abandoned":
		return "bad"
	}
	return "warn"
}

// pillState reports whether a state gets a pill (R2).
func pillState(state any) bool {
	switch fmt.Sprint(state) {
	case "failed", "blocked", "required", "running", "accepted", "retiring", "revoked", "behind", "not ready":
		return true
	}
	return false
}

// nvrHTML splits an NVR or NVRA into the spans n, v, r and d of the
// mockups: name, version, release and dist with arch.
func nvrHTML(nvr string) template.HTML {
	esc := template.HTMLEscapeString
	i := strings.LastIndexByte(nvr, '-')
	if i <= 0 {
		return template.HTML(`<span class="nvr">` + esc(nvr) + `</span>`) //nolint:gosec // G203: escaped
	}
	j := strings.LastIndexByte(nvr[:i], '-')
	if j <= 0 {
		return template.HTML(`<span class="nvr">` + esc(nvr) + `</span>`) //nolint:gosec // G203: escaped
	}
	name, ver, rel := nvr[:j], nvr[j+1:i], nvr[i+1:]
	dist := ""
	if k := strings.Index(rel, "."); k >= 0 {
		rel, dist = rel[:k], rel[k:]
	}
	h := `<span class="nvr"><span class="n">` + esc(name) + `</span>-<span class="v">` + esc(ver) + `</span>-<span class="r">` + esc(rel) + `</span>`
	if dist != "" {
		h += `<span class="d">` + esc(dist) + `</span>`
	}
	return template.HTML(h + `</span>`) //nolint:gosec // G203: every part is escaped
}

// plural gives "1 build", "2 builds".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if strings.HasSuffix(word, "y") && !strings.HasSuffix(word, "ey") {
		return fmt.Sprintf("%d %sies", n, word[:len(word)-1])
	}
	if strings.HasSuffix(word, "s") {
		return fmt.Sprintf("%d %ses", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// pct gives a of b in percent, 0 to 100, for the classes pw-N and fx-N.
func pct(a, b int) int {
	if b <= 0 || a <= 0 {
		return 0
	}
	p := a * 100 / b
	if p > 100 {
		p = 100
	}
	return p
}

// dict makes a map of key and value pairs for include.
func dict(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	return m
}

// reasonText gives the words of a plan reason code.
func reasonText(code any) string {
	switch fmt.Sprint(code) {
	case "commit-changed":
		return "new commit"
	case "rebuild":
		return "rebuild"
	case "new-package":
		return "new package"
	case "new-target":
		return "new target"
	case "forced":
		return "forced"
	case "staging-missing":
		return "staging missing"
	}
	return fmt.Sprint(code)
}

// stepView is one step of a node's step bar.
type stepView struct {
	Step  string
	State string
	Ms    int
}

// stepsOf gives the three steps of a node with their state.
func stepsOf(n api.NodeResult) []stepView {
	out := []stepView{{Step: "srpm", State: "waiting"}, {Step: "rpm", State: "waiting"}, {Step: "stage", State: "waiting"}}
	for _, st := range n.Steps {
		for i := range out {
			if out[i].Step == string(st.Step) {
				out[i].State, out[i].Ms = st.State, st.DurationMs
			}
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// toastView is a toast.
type toastView struct {
	Kind string
	Text string
}

// cssFor gives the style sheets of a page block, as the mockups load them.
func cssFor(content string) []string {
	out := []string{"repoctl.css", "shell.css"}
	switch strings.TrimPrefix(content, "page-") {
	case "graph", "plan":
		out = append(out, "graph.css")
	case "runs", "run", "node":
		out = append(out, "run.css")
	case "packages", "package", "repo", "diff":
		out = append(out, "pkg.css")
	case "publish", "signing":
		out = append(out, "pkg.css", "publish.css")
	default:
		out = append(out, "overview.css")
	}
	return append(out, "ui.css")
}
