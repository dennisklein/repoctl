// Package plan makes the plan of a build: the selection of the nodes, the
// plan action and the reasons of each node (docs/design/build.html#plan).
package plan

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// Selector is one parsed selector of the command line (cli.html#selectors).
type Selector struct {
	Raw    string
	Not    bool
	Here   bool   // "."
	Pkg    string // glob; "" for a target selector
	Target string // glob; "" for all targets
}

// ParseSelectors splits the arguments at commas and white space and parses
// each selector. A bad selector is a usage error.
func ParseSelectors(args []string) ([]Selector, error) {
	var out []Selector
	for _, arg := range args {
		for _, raw := range strings.FieldsFunc(arg, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
			s := Selector{Raw: raw}
			body := raw
			if strings.HasPrefix(body, "!") {
				s.Not = true
				body = body[1:]
			}
			if body == "." {
				s.Here = true
				out = append(out, s)
				continue
			}
			pkg, target, hasAt := strings.Cut(body, "@")
			if hasAt && target == "" {
				return nil, usagef("selector %q: a target pattern must follow @", raw)
			}
			if !hasAt && pkg == "" {
				return nil, usagef("selector %q is empty", raw)
			}
			for _, g := range []string{pkg, target} {
				if g == "" {
					continue
				}
				if err := checkGlob(g); err != nil {
					return nil, usagef("selector %q: %v", raw, err)
				}
			}
			s.Pkg, s.Target = pkg, target
			out = append(out, s)
		}
	}
	return out, nil
}

func checkGlob(g string) error {
	for _, r := range g {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_+.*?[]!^", r):
		default:
			return fmt.Errorf("character %q is not allowed", r)
		}
	}
	if _, err := path.Match(g, ""); err != nil {
		return fmt.Errorf("bad pattern %q", g)
	}
	return nil
}

func match(glob, s string) bool {
	if glob == "" {
		return true
	}
	// "*" and "?" never match "@": ids have no "@", so path.Match is enough.
	ok, _ := path.Match(strings.ReplaceAll(glob, "[!", "[^"), s)
	return ok
}

// UsageError is an error of the command line (exit 2).
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &UsageError{msg: fmt.Sprintf(format, args...)}
}

// Selection is the result of the selectors: the selected nodes, in the
// order of Set.Nodes.
type Selection struct {
	Nodes []api.NodeId
	// Targets are the targets after --target, in file order.
	Targets []string
}

// Contains reports whether the selection holds the node.
func (s *Selection) Contains(n api.NodeId) bool {
	for _, x := range s.Nodes {
		if x == n {
			return true
		}
	}
	return false
}

// Packages gives the selected packages, in file order, each once.
func (s *Selection) Packages() []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range s.Nodes {
		p, _ := api.SplitNode(n)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// FilterTargets gives the active targets that --target names (ids or
// globs), in file order. An id that matches no target, or a retired target
// when rejectRetired, is a usage error.
func FilterTargets(set *config.Set, targetArgs []string, rejectRetired bool) ([]string, error) {
	var globs []string
	for _, a := range targetArgs {
		for _, g := range strings.Split(a, ",") {
			if g = strings.TrimSpace(g); g != "" {
				globs = append(globs, g)
			}
		}
	}
	if len(globs) == 0 {
		var out []string
		for _, t := range set.ActiveTargets() {
			out = append(out, t.ID)
		}
		return out, nil
	}
	picked := map[string]bool{}
	for _, g := range globs {
		n := 0
		for _, t := range set.Targets {
			if match(g, t.ID) {
				if t.Retired() {
					if rejectRetired {
						return nil, usagef("--target %s: target %s is retired", g, t.ID)
					}
					continue
				}
				picked[t.ID] = true
				n++
			}
		}
		if n == 0 {
			return nil, usagef("--target %s matches no target (targets: %s)", g, strings.Join(targetIDs(set), ", "))
		}
	}
	var out []string
	for _, t := range set.Targets {
		if picked[t.ID] {
			out = append(out, t.ID)
		}
	}
	return out, nil
}

func targetIDs(set *config.Set) []string {
	var out []string
	for _, t := range set.Targets {
		out = append(out, t.ID)
	}
	return out
}

// Select evaluates the selectors against the nodes of the set
// (cli.html#evaluation). targets is the result of FilterTargets. here is
// the package of the current directory, "" outside a worktree.
func Select(set *config.Set, sels []Selector, targets []string, here string) (*Selection, error) {
	inTargets := map[string]bool{}
	for _, t := range targets {
		inTargets[t] = true
	}
	all := set.Nodes()
	var avail []api.NodeId
	for _, n := range all {
		if _, t := api.SplitNode(n); inTargets[t] {
			avail = append(avail, n)
		}
	}
	chosen := map[api.NodeId]bool{}
	positive := false
	for _, s := range sels {
		if s.Not {
			continue
		}
		positive = true
		got, err := selectOne(set, s, avail, all, inTargets, here)
		if err != nil {
			return nil, err
		}
		for _, n := range got {
			chosen[n] = true
		}
	}
	if !positive {
		for _, n := range avail {
			chosen[n] = true
		}
	}
	for _, s := range sels {
		if !s.Not {
			continue
		}
		got, _ := selectOne(set, Selector{Raw: s.Raw, Here: s.Here, Pkg: s.Pkg, Target: s.Target}, avail, all, inTargets, here)
		for _, n := range got {
			delete(chosen, n)
		}
	}
	out := &Selection{Targets: targets}
	for _, n := range avail {
		if chosen[n] {
			out.Nodes = append(out.Nodes, n)
		}
	}
	return out, nil
}

func selectOne(set *config.Set, s Selector, avail, all []api.NodeId, inTargets map[string]bool, here string) ([]api.NodeId, error) {
	pkgGlob, targetGlob := s.Pkg, s.Target
	if s.Here {
		if here == "" {
			return nil, usagef("selector \".\": the current directory is not in a package worktree")
		}
		pkgGlob = here
	}
	var got []api.NodeId
	for _, n := range avail {
		p, t := api.SplitNode(n)
		if match(pkgGlob, p) && match(targetGlob, t) {
			got = append(got, n)
		}
	}
	if len(got) > 0 || s.Not {
		return got, nil
	}
	// Explain why the selector selects no node.
	pkgs := 0
	var known []string
	for _, p := range set.Packages {
		known = append(known, p.ID)
		if match(pkgGlob, p.ID) {
			pkgs++
		}
	}
	targetsKnown := 0
	for _, t := range set.Targets {
		if match(targetGlob, t.ID) {
			targetsKnown++
		}
	}
	switch {
	case pkgGlob != "" && pkgs == 0:
		msg := fmt.Sprintf("selector %q selects no node: unknown package %s", s.Raw, pkgGlob)
		if m := closest(pkgGlob, known); m != "" {
			msg += fmt.Sprintf(" (did you mean %s?)", m)
		}
		return nil, usagef("%s", msg)
	case targetGlob != "" && targetsKnown == 0:
		return nil, usagef("selector %q selects no node: unknown target %s", s.Raw, targetGlob)
	}
	// Known package and target: not built for this target, or removed by --target.
	for _, n := range all {
		p, t := api.SplitNode(n)
		if match(pkgGlob, p) && match(targetGlob, t) && !inTargets[t] {
			return nil, usagef("selector %q selects no node: %s is removed by --target", s.Raw, n)
		}
	}
	if pk, ok := set.Package(pkgGlob); ok {
		return nil, &NotForTarget{Raw: s.Raw, Pkg: pk.ID, Target: targetGlob, Targets: set.PackageTargets(*pk)}
	}
	return nil, usagef("selector %q selects no node", s.Raw)
}

// NotForTarget is the usage error of a selector that names a package on a
// target that it is not built for.
type NotForTarget struct {
	Raw     string
	Pkg     string
	Target  string
	Targets []string
}

func (e *NotForTarget) Error() string {
	return fmt.Sprintf("selector %q selects no node: %s is not built for %s (targets: [%s])", e.Raw, e.Pkg, e.Target, strings.Join(e.Targets, " "))
}

// closest gives the known id with the smallest edit distance up to 2.
func closest(s string, known []string) string {
	best, bestD := "", 3
	sort.Strings(known)
	for _, k := range known {
		if d := distance(s, k); d < bestD {
			best, bestD = k, d
		}
	}
	return best
}

func distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// PackageSelectors checks that the selectors name only packages (checkout,
// pin, rebuild, upstream) and gives the package ids, in file order. A
// selector with @ is a usage error.
func PackageSelectors(set *config.Set, args []string, here string) ([]string, error) {
	sels, err := ParseSelectors(args)
	if err != nil {
		return nil, err
	}
	for _, s := range sels {
		if s.Target != "" || strings.Contains(s.Raw, "@") {
			return nil, usagef("selector %q: this command takes packages, not nodes", s.Raw)
		}
	}
	var ids []string
	for _, p := range set.Packages {
		ids = append(ids, p.ID)
	}
	if len(sels) == 0 {
		return ids, nil
	}
	chosen := map[string]bool{}
	positive := false
	for _, s := range sels {
		if s.Not {
			continue
		}
		positive = true
		g := s.Pkg
		if s.Here {
			if here == "" {
				return nil, usagef("selector \".\": the current directory is not in a package worktree")
			}
			g = here
		}
		n := 0
		for _, id := range ids {
			if match(g, id) {
				chosen[id] = true
				n++
			}
		}
		if n == 0 {
			msg := fmt.Sprintf("selector %q selects no package: unknown package %s", s.Raw, g)
			if m := closest(g, ids); m != "" {
				msg += fmt.Sprintf(" (did you mean %s?)", m)
			}
			return nil, usagef("%s", msg)
		}
	}
	if !positive {
		for _, id := range ids {
			chosen[id] = true
		}
	}
	for _, s := range sels {
		if s.Not {
			for _, id := range ids {
				if match(s.Pkg, id) {
					delete(chosen, id)
				}
			}
		}
	}
	var out []string
	for _, id := range ids {
		if chosen[id] {
			out = append(out, id)
		}
	}
	return out, nil
}
