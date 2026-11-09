package ui

import (
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/GSI-HPC/go-clikit/localweb/page"

	"github.com/dennisklein/repoctl/internal/api"
)

// setCard is one set on the page of all sets and in the set menu.
type setCard struct {
	Name, Dir string
	// State is running, failed, ok or none (no run yet).
	State  string
	Word   string
	Last   *foldedRun
	Active *foldedRun
	Gone   bool
	Here   bool
	Latest []api.StatusSnapshot
}

func (s *Server) setCards(pr *pageReq, withStatus bool) []setCard {
	var out []setCard
	for _, e := range s.sets.all() {
		c := setCard{Name: e.Name, Dir: e.Path, State: "none", Word: "no run yet"}
		if _, err := os.Stat(e.Path); err != nil {
			c.Gone, c.Word = true, "directory missing"
		}
		for _, f := range s.runs.list(s.o.Paths.Runs(e.Name), 5) {
			if f.Active() && c.Active == nil {
				c.Active = f
			}
			if !f.Active() && c.Last == nil {
				c.Last = f
			}
		}
		switch {
		case c.Active != nil:
			c.State, c.Word = "running", "running"
		case c.Last != nil && (c.Last.Run.State == api.RunStateFailed || c.Last.Run.State == api.RunStateAbandoned):
			c.State, c.Word = "failed", string(c.Last.Run.State)
		case c.Last != nil:
			c.State, c.Word = "ok", "ok"
		}
		if pr != nil && pr.set != nil && pr.set.Name == e.Name {
			c.Here = true
		}
		if withStatus && !c.Gone {
			if st, ok := First[api.Status](s.src.Run(pr.ctx, e.Path, "status")); ok {
				c.Latest = st.Snapshots
			}
		}
		out = append(out, c)
	}
	return out
}

// setMenu gives the items of the set menu.
func (s *Server) setMenu(sc *setCtx) []setCard {
	pr := &pageReq{set: sc}
	return s.setCards(pr, false)
}

type setsPage struct {
	Sets []setCard
}

func buildSets(pr *pageReq) *built {
	sets := pr.s.setCards(pr, true)
	b := &built{Title: "Sets", Data: setsPage{Sets: sets}, CLI: []string{"repoctl ui            # in the directory of a set, to add it"}}
	b.Live = []liveRegion{{Event: "sets-list", Block: "sets-list", Data: setsPage{Sets: sets}}}
	return b
}

// doctorPage is the report of repoctl doctor and the checks of the UI.
type doctorPage struct {
	NA     *notAvailable
	Report *api.DoctorReport
	Groups []doctorGroup
	UI     []api.DoctorReportCheck
}

type doctorGroup struct {
	Name   string
	Checks []api.DoctorReportCheck
	Bad    int
}

func buildDoctor(pr *pageReq) *built {
	d := doctorPage{UI: pr.s.HostChecks()}
	var res *Result
	if sets := pr.s.sets.all(); len(sets) > 0 {
		res = pr.s.src.Run(pr.ctx, sets[0].Path, "doctor")
	} else {
		res = &Result{Errors: []api.Problem{{Code: api.PSetNone, Message: "No set is on the list."}}}
	}
	d.NA = na("The host checks", "repoctl doctor", res)
	if rep, ok := First[api.DoctorReport](res); ok {
		d.Report = &rep
		idx := map[string]int{}
		for _, c := range rep.Checks {
			g := c.Group
			if g == "" {
				g = "host"
			}
			i, ok := idx[g]
			if !ok {
				i = len(d.Groups)
				idx[g] = i
				d.Groups = append(d.Groups, doctorGroup{Name: g})
			}
			d.Groups[i].Checks = append(d.Groups[i].Checks, c)
			if c.Status != api.CheckStatusOk {
				d.Groups[i].Bad++
			}
		}
	}
	return &built{Title: "Doctor", Data: d, CLI: []string{"repoctl doctor"}}
}

// handleSetMenu renders the set menu.
func (s *Server) handleSetMenu(w http.ResponseWriter, r *http.Request) {
	var sc *setCtx
	if name := r.URL.Query().Get("set"); name != "" {
		sc, _ = s.setOf(name)
	}
	s.respond(w, r, http.StatusOK, "set-menu-items", s.setMenu(sc))
}

// handleGo goes to the page of another set: the same page if it has one.
func (s *Server) handleGo(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("set")
	if _, ok := s.sets.get(name); !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	to := "/s/" + url.PathEscape(name) + "/"
	from := r.URL.Query().Get("from")
	for _, p := range []string{"graph", "plan", "advisories", "upstream", "runs", "packages", "publish", "signing", "config"} {
		if strings.HasSuffix(from, "/"+p) {
			to += p
		}
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// handleSetRemove takes a set off the list. Its files stay.
func (s *Server) handleSetRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("set")
	ok, err := s.RemoveSet(name)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		s.fail(w, r, http.StatusNotFound, "The set "+name+" is not on the list.")
		return
	}
	page.Redirect(w, r, "/")
}

// jumpHit is one result of the jump field.
type jumpHit struct {
	Kind, Label, Href string
}

// jump finds packages, nodes, runs and pages of the set whose name holds q.
func (s *Server) jump(pr *pageReq, q string) []jumpHit {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	base := "/s/" + url.PathEscape(pr.set.Name)
	var out []jumpHit
	for _, p := range []struct{ name, path string }{
		{"Overview", "/"}, {"Graph", "/graph"}, {"Plan", "/plan"}, {"Runs", "/runs"}, {"Packages", "/packages"},
		{"Advisories", "/advisories"}, {"Upstream", "/upstream"}, {"Publish", "/publish"}, {"Signing", "/signing"}, {"Config and lock", "/config"},
	} {
		if strings.Contains(strings.ToLower(p.name), q) {
			out = append(out, jumpHit{Kind: "Page", Label: p.name, Href: base + p.path})
		}
	}
	if pr.set.Conf != nil {
		for _, p := range pr.set.Conf.Packages {
			if strings.Contains(strings.ToLower(p.ID), q) {
				out = append(out, jumpHit{Kind: "Package", Label: p.ID, Href: base + "/packages/" + url.PathEscape(p.ID)})
			}
		}
		for _, n := range pr.set.Conf.Nodes() {
			if strings.Contains(strings.ToLower(n), q) && strings.Contains(q, "@") {
				out = append(out, jumpHit{Kind: "Node", Label: n, Href: base + "/graph?node=" + url.QueryEscape(n)})
			}
		}
	}
	for _, f := range s.runs.list(s.o.Paths.Runs(pr.set.Name), 50) {
		if strings.Contains(f.Run.ID, q) {
			out = append(out, jumpHit{Kind: "Run", Label: f.Run.ID, Href: runHref(pr.set.Name, f.Run.ID)})
		}
	}
	return slices.Clip(out[:min(len(out), 12)])
}
