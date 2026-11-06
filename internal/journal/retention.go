package journal

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
)

// Retention is the retention rule of the journals of a set
// (contract.html#writing): it deletes the journal, the progress log and the
// node logs of a run.
type Retention struct {
	// KeepRuns, KeepPublishes and KeepChecks are state.keep_runs,
	// state.keep_publishes and state.keep_checks of the user config.
	KeepRuns, KeepPublishes, KeepChecks int
	// Checks applies the rule of the check runs too: at the start of a
	// check, and in repoctl clean --logs.
	Checks bool
	// OnlyChecks applies only the rule of the check runs. The start of a
	// check uses it: a check holds no set lock, so it leaves the build and
	// publish runs alone.
	OnlyChecks bool
	// Keep reports a run that the rule keeps whatever its age: a build run
	// with a staged node, or a publish with a kept tree.
	Keep func(r *api.Run) bool
	// DryRun deletes nothing.
	DryRun bool
}

// Retain applies the retention rule to the runs of the set. It gives the
// runs whose files it deleted (or would delete). Active runs and runs of
// another host are always kept.
func Retain(o FoldOptions, set string, r Retention) ([]Entry, error) {
	es, err := List(o, set, true)
	if err != nil {
		return nil, err
	}
	var gone []Entry
	builds, publishes, checks := 0, 0, 0
	for _, e := range es {
		run := e.Fold.Run()
		if o.Host != "" && run.Origin.Host != "" && run.Origin.Host != o.Host {
			continue
		}
		if !e.Fold.Finished() && run.State != api.RunStateAbandoned {
			continue
		}
		if r.Keep != nil && r.Keep(run) {
			continue
		}
		var n *int
		var limit int
		if r.OnlyChecks && run.Kind != KindCheck {
			continue
		}
		switch run.Kind {
		case KindPublish:
			n, limit = &publishes, r.KeepPublishes
		case KindCheck:
			if !r.Checks && !r.OnlyChecks {
				continue
			}
			n, limit = &checks, r.KeepChecks
		default:
			n, limit = &builds, r.KeepRuns
		}
		*n++
		if *n <= limit {
			continue
		}
		if !r.DryRun {
			if err := Remove(o, set, e.ID); err != nil {
				return gone, err
			}
		}
		gone = append(gone, e)
	}
	return gone, nil
}

// Remove deletes the journal, the progress log, the cancel request and the
// logs of a run. It never deletes a staging.
func Remove(o FoldOptions, set, id string) error {
	if o.Paths == nil {
		return nil
	}
	p := o.Paths
	for _, f := range []string{p.Journal(set, id), p.ProgressLog(set, id), CancelPath(*p, set, id)} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	logs := p.Logs(set, id)
	if strings.HasPrefix(filepath.Clean(logs), filepath.Clean(p.SetState(set))) {
		return os.RemoveAll(logs)
	}
	return nil
}
