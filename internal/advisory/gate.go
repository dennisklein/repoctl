package advisory

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/advisory/rpmmd"
	"github.com/dennisklein/repoctl/internal/api"
)

// The publish kinds of the gate (advisories.html#gate-kinds).
const (
	GatePublish = "publish"
	GateRestore = "restore"
	GatePrune   = "prune"
	GateResign  = "resign"
	GateImport  = "import"
)

// GateInput is what the gate of one target reads. The publish step check
// fills it before the first signature.
type GateInput struct {
	Target string
	// Kind is the publishKind: publish, restore, prune, resign or import.
	Kind string
	// Base are the binary RPMs of latest (the base of the publish), Tree
	// the binary RPMs of the result tree: the base plus the taken nodes.
	Base, Tree []*rpmmd.Package
	// Repos are the binary RPMs of the closure repos of the target.
	Repos []*rpmmd.Package
	// Owner maps an RPM to the set package that built it.
	Owner *Owner
	// Packages are the packages of this publish on the target: the
	// packages of the taken nodes (publish), the packages whose newest NVR
	// differs (restore), all packages (import).
	Packages []string
	// RunAdvisories are the soname.removed advisories of the taken nodes.
	RunAdvisories []api.Advisory
	// Accept are the ids of --accept-advisory, Reason its --reason.
	Accept []string
	Reason string
	// User and Host record an acceptance.
	User, Host string
	// Command is the publish command of the hints, for example
	// "repoctl publish --target el9".
	Command string
}

// Gate row states.
const (
	RowBlocks   = "blocks"
	RowAccepted = "accepted"
	RowOpen     = "open, not changed by this publish"
	RowOld      = "old NVR"
	RowGone     = "gone"
	RowInfo     = "does not block"
)

// GateRow is one row of the gate summary: one advisory, or one broken
// requirement of an old NVR.
type GateRow struct {
	Advisory api.Advisory
	Status   string
	Blocks   bool
	Accepted bool
	// Affected are the requirer NVRs of soname.removed: clients that have
	// them cannot update the provider.
	Affected []string
	// OldNVRA and Requires describe a broken requirement of an old NVR (a
	// row with status old NVR and no advisory id).
	OldNVRA, Requires string
}

// GateResult is the decision of the gate on one target.
type GateResult struct {
	Target string
	Rows   []GateRow
	// Accepted are the acceptances of this publish: the journal event
	// advisory.accepted and the snapshot annotation
	// io.repoctl.advisories.accepted.
	Accepted []api.SnapshotAcceptedItem
}

// Blocked reports a row that blocks and is not accepted.
func (g *GateResult) Blocked() bool {
	for _, r := range g.Rows {
		if r.Blocks && !r.Accepted {
			return true
		}
	}
	return false
}

// ValidateAccept checks --accept-advisory and --reason before the publish
// starts: the reason is required with at least MinNote characters, and an
// acceptance needs a human at a terminal (also with
// REPOCTL_ALLOW_UNATTENDED_PUBLISH=1). --yes never accepts an advisory.
func ValidateAccept(ids []string, reason string, terminal bool) error {
	if len(ids) == 0 {
		if reason != "" {
			return api.Errorf(api.PUsage, "--reason needs --accept-advisory").WithExit(api.ExitUsage)
		}
		return nil
	}
	for _, id := range ids {
		if !ValidID(id) {
			return api.Errorf(api.PUsage, "--accept-advisory %q is not an advisory id CODE/SUBJECT", id).WithExit(api.ExitUsage)
		}
	}
	if len(strings.TrimSpace(reason)) < MinNote {
		return api.Errorf(api.PUsage, "--accept-advisory needs --reason with at least %d characters", MinNote).WithExit(api.ExitUsage)
	}
	if !terminal {
		return api.Errorf(api.PPublishUnattendedRefused, "an acceptance of an advisory needs a human at a terminal").
			WithHuman("accept-advisory", "a required advisory blocks the publish", "repoctl publish --accept-advisory "+ids[0]+" --reason TEXT")
	}
	return nil
}

// Gate runs the publish gate of one target (advisories.html#gate):
// repoclosure --newest on the result tree plus the closure repos, one
// advisory for each broken requirement with its closure, and the decision.
// A required advisory blocks when its closure holds a package of this
// publish; an acceptance removes the block. The error is advisory.open or
// advisory.not-blocking (exit 12), or prune.breaks-closure for a prune.
func Gate(in GateInput) (*GateResult, error) {
	res := &GateResult{Target: in.Target}
	if in.Kind == GateResign {
		return res, notBlocking(in, res)
	}
	owner := in.Owner
	if owner == nil {
		owner = NewOwner(nil, nil, nil)
	}
	broken := ClosureNewest(in.Tree, in.Repos)
	soname, rest := splitSoname(in, broken, owner)
	advs, err := ClosureAdvisories(in.Target, rest, in.Tree, in.Repos, owner)
	if err != nil {
		return nil, err
	}
	advs = append(soname, advs...)
	for i := range advs {
		advs[i].Source = "publish"
	}
	Sort(advs)
	pkgs := map[string]bool{}
	for _, p := range in.Packages {
		pkgs[p] = true
	}
	var baseBroken map[string]bool
	if in.Kind == GatePrune {
		baseBroken = map[string]bool{}
		for _, b := range ClosureNewest(in.Base, in.Repos) {
			baseBroken[b.RPM.NVRA()+"\x00"+b.Req.String()] = true
		}
		for _, b := range broken {
			if !baseBroken[b.RPM.NVRA()+"\x00"+b.Req.String()] {
				return nil, api.Errorf(api.PPruneBreaksClosure, "%s: the prune breaks the closure: %s needs %s", in.Target, b.RPM.NVRA(), b.Req.String()).
					WithTarget(in.Target).WithHint("keep the provider, or prune the requirer too", "")
			}
		}
	}
	for _, a := range advs {
		row := GateRow{Advisory: a, Status: RowInfo}
		if a.Level == api.AdvisoryLevelRequired {
			row.Status = RowOpen
			if in.Kind == GatePublish || in.Kind == GateRestore || in.Kind == "" {
				for _, c := range a.Closure {
					if pkgs[c] {
						row.Blocks, row.Status = true, RowBlocks
					}
				}
			}
		}
		if a.Code == api.AdvisoryCodeSonameRemoved {
			var f api.FactsSonameRemoved
			_ = DecodeFacts(a, &f)
			for _, r := range f.RequiredBy {
				row.Affected = append(row.Affected, nvrOf(r.NVRA))
			}
			row.Affected = uniq(row.Affected)
		}
		res.Rows = append(res.Rows, row)
	}
	for _, b := range ClosureOld(in.Tree, in.Repos) {
		res.Rows = append(res.Rows, GateRow{Status: RowOld, OldNVRA: b.RPM.NVRA(), Requires: b.Req.String()})
	}
	found := map[string]bool{}
	for _, a := range advs {
		found[a.ID] = true
	}
	for _, a := range in.RunAdvisories {
		if a.Code == api.AdvisoryCodeSonameRemoved && !found[a.ID] {
			a.Source = "publish"
			res.Rows = append(res.Rows, GateRow{Advisory: a, Status: RowGone})
		}
	}
	if err := notBlocking(in, res); err != nil {
		return res, err
	}
	for i := range res.Rows {
		r := &res.Rows[i]
		for _, id := range in.Accept {
			if r.Blocks && r.Advisory.ID == id {
				r.Accepted, r.Status = true, RowAccepted
				res.Accepted = append(res.Accepted, api.SnapshotAcceptedItem{
					ID: id, FactsDigest: r.Advisory.FactsDigest, Reason: strings.TrimSpace(in.Reason), User: in.User, Host: in.Host,
				})
			}
		}
	}
	if res.Blocked() {
		return res, blockedError(in, res)
	}
	return res, nil
}

// notBlocking stops an acceptance of an id that does not block this
// publish: a human cannot accept an advisory in advance.
func notBlocking(in GateInput, res *GateResult) error {
	for _, id := range in.Accept {
		ok := false
		for _, r := range res.Rows {
			if r.Blocks && r.Advisory.ID == id {
				ok = true
			}
		}
		if !ok {
			return api.Errorf(api.PAdvisoryNotBlocking, "%s: --accept-advisory %s does not block this publish", in.Target, id).
				WithTarget(in.Target).WithHint("remove the flag for this id; the dry run shows the gate rows", "repoctl publish --dry-run --target "+in.Target)
		}
	}
	return nil
}

func blockedError(in GateInput, res *GateResult) error {
	var first *GateRow
	for i := range res.Rows {
		if res.Rows[i].Blocks && !res.Rows[i].Accepted {
			first = &res.Rows[i]
			break
		}
	}
	cmd := in.Command
	if cmd == "" {
		cmd = "repoctl publish --target " + in.Target
	}
	accept := cmd + " --accept-advisory " + first.Advisory.ID + " --reason TEXT"
	e := api.Errorf(api.PAdvisoryOpen, "%s: a required advisory has a package of this publish in its closure", in.Target).
		WithTarget(in.Target).WithHuman("accept-advisory", "the required advisory "+first.Advisory.ID+" blocks the publish of "+in.Target, accept)
	if dep := dependents(first.Advisory, in.Packages); len(dep) > 0 {
		reason := first.Advisory.Subject
		if first.Advisory.Code == api.AdvisoryCodeSonameRemoved {
			var f api.FactsSonameRemoved
			_ = DecodeFacts(first.Advisory, &f)
			pkg, _ := api.SplitNode(first.Advisory.Subject)
			reason = pkg + ": " + strings.Join(shortList(f.Removed), ", ") + " removed"
		}
		e = e.WithHint("rebuild the dependents into this publish", "repoctl rebuild "+strings.Join(dep, " ")+" --reason \""+reason+"\"")
	} else if first.Advisory.Suggestion != nil && first.Advisory.Suggestion.Command != "" {
		e = e.WithHint(strings.TrimSuffix(lowerFirst(first.Advisory.Suggestion.Text), "."), first.Advisory.Suggestion.Command)
	}
	return e.WithHint("or accept it for this publish", accept)
}

// dependents gives the packages of the closure that this publish does not
// change: the requirers to rebuild.
func dependents(a api.Advisory, pkgs []string) []string {
	in := map[string]bool{}
	for _, p := range pkgs {
		in[p] = true
	}
	var out []string
	for _, c := range a.Closure {
		if !in[c] {
			out = append(out, c)
		}
	}
	return out
}

func shortList(caps []string) []string {
	var out []string
	for _, c := range caps {
		out = append(out, shortCap(c))
	}
	return out
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// splitSoname names the broken requirements whose provider is a package of
// this publish: it provided the requirement in its newest build of the base,
// and its new build does not. They give soname.removed/PROVIDER@TARGET,
// whose closure is the provider plus each package that requires it.
func splitSoname(in GateInput, broken []Broken, owner *Owner) ([]api.Advisory, []Broken) {
	pub := map[string]bool{}
	for _, p := range in.Packages {
		pub[p] = true
	}
	provided := func(pkgs []*rpmmd.Package, pkg string, req rpmmd.Cap) bool {
		for _, p := range rpmmd.Newest(pkgs) {
			if o, _ := owner.Package(p); o != pkg {
				continue
			}
			for _, c := range p.Provides {
				if rpmmd.Satisfies(c, req) {
					return true
				}
			}
		}
		return false
	}
	type group struct {
		removed  map[string]bool
		required []api.FactsSonameRemovedRequiredByItem
		closure  map[string]bool
	}
	groups := map[string]*group{}
	var rest []Broken
	for _, b := range broken {
		provider := ""
		for _, p := range in.Packages {
			if provided(in.Base, p, b.Req) && !provided(in.Tree, p, b.Req) {
				provider = p
				break
			}
		}
		if provider == "" {
			rest = append(rest, b)
			continue
		}
		g := groups[provider]
		if g == nil {
			g = &group{removed: map[string]bool{}, closure: map[string]bool{provider: true}}
			groups[provider] = g
		}
		g.removed[b.Req.Name] = true
		g.required = append(g.required, api.FactsSonameRemovedRequiredByItem{NVRA: b.RPM.NVRA(), Requires: b.Req.String()})
		if o, _ := owner.Package(b.RPM); o != "" {
			g.closure[o] = true
		}
	}
	var names []string
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []api.Advisory
	for _, n := range names {
		g := groups[n]
		var removed []string
		for r := range g.removed {
			removed = append(removed, r)
		}
		sort.Strings(removed)
		sort.Slice(g.required, func(i, j int) bool {
			if g.required[i].NVRA != g.required[j].NVRA {
				return g.required[i].NVRA < g.required[j].NVRA
			}
			return g.required[i].Requires < g.required[j].Requires
		})
		a, err := New(api.AdvisoryCodeSonameRemoved, n+"@"+in.Target, api.AdvisoryLevelRequired, []api.TargetId{in.Target},
			api.FactsSonameRemoved{Removed: removed, RequiredBy: g.required})
		if err != nil {
			continue
		}
		for c := range g.closure {
			a.Closure = append(a.Closure, c)
		}
		sort.Strings(a.Closure)
		a.Title = fmt.Sprintf("%s removes %s, which kept set packages require", n, strings.Join(shortList(removed), ", "))
		out = append(out, a)
	}
	return out, rest
}
