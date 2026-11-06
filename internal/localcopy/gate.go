package localcopy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/snapshot"
)

// CheckTimeout bounds the freshness check of one target in plan, build
// and status: a registry that does not answer in time is unreachable.
const CheckTimeout = 20 * time.Second

// Gate runs the freshness check of plan, build and status over the
// targets of a set: one read of latest per target, at the same time
// (publish.html#freshness).
type Gate struct {
	Set   *config.Set
	Paths config.Paths
	// Getenv reads the environment of the registry client; nil is
	// os.Getenv.
	Getenv func(string) string
	// User is the user of this host.
	User string
	// Deployed reports a record of keys/deployed.yaml for a target; nil
	// reports none.
	Deployed func(target string) bool
	// Pushable gives the pushable trees of this user for a target; nil
	// gives none.
	Pushable func(target string) []Pushable
	// Open opens the OCI package of a target; nil opens
	// Set.OCIPackage(target) with snapshot.Open.
	Open func(target string) (*snapshot.Store, error)
	// Timeout bounds the check of one target; 0 is CheckTimeout.
	Timeout time.Duration
}

// Enabled reports whether the set names a registry. Without store.oci,
// plan and build have nothing to compare: the check does not run.
func (g Gate) Enabled() bool { return g.Set != nil && g.Set.Store.OCI != "" }

// Check runs the freshness check of each target, in the order of targets.
// It gives nil when the set has no store.oci. A store that cannot be opened
// gives the result unreachable with the error.
func (g Gate) Check(ctx context.Context, targets []string) []Freshness {
	if !g.Enabled() || len(targets) == 0 {
		return nil
	}
	out := make([]Freshness, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = g.one(ctx, t)
		}()
	}
	wg.Wait()
	return out
}

func (g Gate) one(ctx context.Context, target string) Freshness {
	local, err := ReadSnapshot(Dir(g.Paths, g.Set.Name, target))
	if err != nil {
		// A broken snapshot.json is no local copy: pull writes it again.
		local = nil
	}
	f := Freshness{Target: target, Local: local}
	open := g.Open
	if open == nil {
		open = func(t string) (*snapshot.Store, error) {
			return snapshot.Open(g.Set.OCIPackage(t), snapshot.Options{Getenv: g.Getenv, UserAgent: "repoctl/" + config.Version})
		}
	}
	store, err := open(target)
	if err != nil {
		f.Result, f.Err = FreshUnreachable, err
		return f
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = CheckTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	in := FreshInput{Store: store, Target: target, Local: local, User: g.User, HeadOnly: true}
	if g.Deployed != nil {
		in.Deployed = g.Deployed(target)
	}
	if g.Pushable != nil {
		in.Pushable = g.Pushable(target)
	}
	f = Check(cctx, in)
	if f.Result == FreshUnreachable && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		f.Err = fmt.Errorf("no answer in %s", timeout)
	}
	return f
}

// PlanOutcome gives what plan and build do with the results of the check
// (publish.html#freshness, the plan/build column): the warnings, and the
// error that stops them with exit 13, or nil when they continue with the
// local copy.
func PlanOutcome(fs []Freshness) ([]api.Problem, error) {
	var warns []api.Problem
	var stops []Freshness
	for _, f := range fs {
		if w := f.Warning(); w != nil {
			warns = append(warns, *w)
		}
		if w := f.OrphanedWarning(); w != nil {
			warns = append(warns, *w)
		}
		if f.PlanErr() != nil {
			stops = append(stops, f)
		}
	}
	switch len(stops) {
	case 0:
		return warns, nil
	case 1:
		return warns, stops[0].PlanErr()
	}
	// Several targets are behind: one error, one pull of each.
	var ids, cmds []string
	for _, f := range stops {
		ids = append(ids, f.Target)
		if f.Result == FreshNotCached {
			cmds = append(cmds, "repoctl repo push "+f.Publish+" --target "+f.Target)
		}
	}
	e := api.Errorf(api.PStateBehind, "latest of %s is newer than the local copy", strings.Join(ids, ", "))
	for _, c := range cmds {
		e = e.WithHint("the cache step of a publish did not run", c)
	}
	if len(cmds) < len(stops) {
		e = e.WithHint("", "repoctl repo pull")
	}
	return warns, e
}
