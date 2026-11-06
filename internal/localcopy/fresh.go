package localcopy

import (
	"context"
	"fmt"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/snapshot"
)

// The results of the freshness check (publish.html#freshness).
const (
	FreshSame        = "same"
	FreshFirst       = "first"
	FreshMissing     = "missing"
	FreshBehind      = "behind"
	FreshUnpushed    = "unpushed"
	FreshNotCached   = "not-cached"
	FreshOrphaned    = "orphaned"
	FreshUnreachable = "unreachable"
)

// Pushable is a kept tree of this user that a state file records as
// pushable for a target.
type Pushable struct {
	Publish string
	Base    api.Digest
	New     api.Digest
}

// FreshInput is what the freshness check of one target reads.
type FreshInput struct {
	Store  *snapshot.Store
	Target string
	// Local is snapshot.json of the local copy, or nil.
	Local *api.Snapshot
	// Deployed is true when keys/deployed.yaml has a record for the target.
	Deployed bool
	// Pushable are the pushable trees of this user for the target.
	Pushable []Pushable
	// User is the user of this host.
	User string
	// HeadOnly reads only the digest of latest when it is the digest of
	// the local copy: the one request of plan, build and status. Latest
	// is then nil; the manifest is read only when the two differ.
	HeadOnly bool
}

// Freshness is the result of the freshness check of one target.
type Freshness struct {
	Target string
	Result string
	// Latest is the snapshot of latest, when the registry has one and the
	// check read its manifest (not with HeadOnly and result same).
	Latest *snapshot.Snapshot
	// Digest is the digest of latest, when the registry has one.
	Digest api.Digest
	Local  *api.Snapshot
	// Publish is the pushable publish id of unpushed and not-cached.
	Publish string
	// Orphaned is set with behind when the local copy holds a publish of
	// this user that is not on the chain of latest.
	Orphaned bool
	// Err is the registry error of unreachable.
	Err error
}

// Check runs the freshness check of a target: one read of latest, compared
// with snapshot.json of the local copy and the pushable trees.
func Check(ctx context.Context, in FreshInput) Freshness {
	f := Freshness{Target: in.Target, Local: in.Local}
	d, ok, err := in.Store.Latest(ctx)
	if err != nil {
		f.Result, f.Err = FreshUnreachable, err
		return f
	}
	if !ok {
		if in.Local != nil || in.Deployed {
			f.Result = FreshMissing
		} else {
			f.Result = FreshFirst
		}
		return f
	}
	f.Digest = d.Digest.String()
	if in.HeadOnly && in.Local != nil && in.Local.Digest == f.Digest {
		f.Result = FreshSame
		for _, p := range in.Pushable {
			if p.Base == f.Digest {
				f.Result, f.Publish = FreshUnpushed, p.Publish
				break
			}
		}
		return f
	}
	latest, err := in.Store.Fetch(ctx, d)
	if err != nil {
		f.Result, f.Err = FreshUnreachable, err
		return f
	}
	f.Latest = latest
	for _, p := range in.Pushable {
		if p.New == latest.Digest() {
			f.Result, f.Publish = FreshNotCached, p.Publish
			return f
		}
	}
	if in.Local != nil && in.Local.Digest == latest.Digest() {
		for _, p := range in.Pushable {
			if p.Base == latest.Digest() {
				f.Result, f.Publish = FreshUnpushed, p.Publish
				return f
			}
		}
		f.Result = FreshSame
		return f
	}
	f.Result = FreshBehind
	if in.Local != nil && in.Local.User == in.User && in.Local.Kind != snapshot.KindImport {
		if on, err := in.Store.OnChain(ctx, d, in.Local.Digest); err == nil && !on {
			f.Orphaned = true
		}
	}
	return f
}

// LatestID gives the publish id of latest, or "". With HeadOnly and the
// result same, it is the publish id of the local copy.
func (f Freshness) LatestID() string {
	if f.Latest != nil {
		return f.Latest.Meta.Publish
	}
	if f.Local != nil && f.Digest != "" && f.Local.Digest == f.Digest {
		return f.Local.Publish
	}
	return ""
}

// OrphanedWarning gives the warning snapshot.orphaned, or nil.
func (f Freshness) OrphanedWarning() *api.Problem {
	if !f.Orphaned || f.Local == nil {
		return nil
	}
	return &api.Problem{Code: api.PSnapshotOrphaned, Target: f.Target,
		Message: fmt.Sprintf("the local copy holds %s of %s, which is not on the chain of latest %s", f.Local.Publish, f.Local.User, f.LatestID()),
		Hint:    api.HintText("pull, then plan and publish again", "repoctl repo pull --target "+f.Target)}
}

// PublishErr gives the error of the freshness check for publish, or nil
// for same and first.
func (f Freshness) PublishErr() error {
	t := f.Target
	switch f.Result {
	case FreshSame, FreshFirst:
		return nil
	case FreshMissing:
		return api.Errorf(api.PSnapshotMissing, "the registry has no snapshot of %s, but the local copy or keys/deployed.yaml has one", f.Target).WithTarget(t).
			WithHint("re-seed the OCI package from the newest local copy", "repoctl repo push --reseed --target "+f.Target)
	case FreshUnpushed:
		return api.Errorf(api.PStateUnpushed, "the tree of %s is not pushed: latest of %s is still its base", f.Publish, f.Target).WithTarget(t).
			WithHint("complete the publish", "repoctl repo push "+f.Publish+" --target "+f.Target).
			WithHint("or discard the tree", "repoctl clean --publish "+f.Publish)
	case FreshNotCached:
		return api.Errorf(api.PStateBehind, "publish %s is in latest of %s, the cache step did not run", f.Publish, f.Target).WithTarget(t).
			WithHint("", "repoctl repo push "+f.Publish+" --target "+f.Target)
	case FreshUnreachable:
		e := api.Errorf(api.PRegistryUnreachable, "the registry does not answer for %s: %v", f.Target, f.Err).WithTarget(t).WithCause(f.Err)
		if ae, ok := api.AsError(f.Err); ok && ae.Code == api.PRegistryDenied {
			return ae
		}
		return e.WithHint("publish when the registry answers", "")
	}
	local := "no local copy"
	if f.Local != nil {
		local = "the local copy is " + f.Local.Publish
	}
	return api.Errorf(api.PStateBehind, "latest of %s is %s, %s", f.Target, f.LatestID(), local).WithTarget(t).
		WithHint("", "repoctl repo pull --target "+f.Target)
}

// Warning gives the warning of the freshness check for plan and build, or
// nil.
func (f Freshness) Warning() *api.Problem {
	t := f.Target
	switch f.Result {
	case FreshMissing:
		return &api.Problem{Code: api.PSnapshotMissing, Target: t, Message: fmt.Sprintf("the registry has no snapshot of %s; repoctl uses the local copy", f.Target)}
	case FreshUnpushed:
		return &api.Problem{Code: api.PStateUnpushed, Target: t, Message: fmt.Sprintf("the tree of %s is not pushed: latest of %s is still its base", f.Publish, f.Target),
			Hint: api.HintText("complete the publish, or discard the tree with repoctl clean --publish "+f.Publish, "repoctl repo push "+f.Publish+" --target "+f.Target)}
	case FreshUnreachable:
		msg := fmt.Sprintf("the registry does not answer for %s; repoctl uses the local copy", f.Target)
		if f.Err != nil {
			msg = fmt.Sprintf("the registry does not answer for %s (%s); repoctl uses the local copy", f.Target, causeText(f.Err))
		}
		return &api.Problem{Code: api.PRegistryUnreachable, Target: t, Message: msg}
	}
	return nil
}

// causeText gives the short cause of a registry error: the network error
// of registry.unreachable, else the message.
func causeText(err error) string {
	if ae, ok := api.AsError(err); ok {
		if ae.Code == api.PRegistryUnreachable && ae.Err != nil {
			return ae.Err.Error()
		}
		return ae.Message
	}
	return err.Error()
}

// PlanErr gives the error of the freshness check for plan and build, or
// nil when they continue (publish.html#freshness, the plan/build column):
// behind, not-cached and orphaned stop with state.behind (exit 13).
func (f Freshness) PlanErr() error {
	switch f.Result {
	case FreshBehind, FreshNotCached:
		return f.PublishErr()
	}
	return nil
}

// StatusLocal gives the word of the local copy in repoctl status: same,
// behind, unpushed or none (contract.html#enums). A target that the
// registry cannot tell about (missing, unreachable) shows the local copy
// as same, or none without one.
func (f Freshness) StatusLocal() string {
	switch f.Result {
	case FreshBehind, FreshNotCached:
		return "behind"
	case FreshUnpushed:
		return "unpushed"
	case FreshFirst:
		return "none"
	}
	if f.Local == nil {
		if f.Result == FreshSame {
			return "same"
		}
		return "none"
	}
	return "same"
}
