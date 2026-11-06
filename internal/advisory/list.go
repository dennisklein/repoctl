package advisory

import (
	"github.com/dennisklein/repoctl/internal/api"
)

// Current gives the advisory list that all surfaces read: the last check
// result plus the advisories of the runs with staged nodes (run, in their
// own state, for example pending), with the states of the dismissal file.
// d can be nil (no file, or an invalid file: nothing is dismissed).
func Current(cf *CheckFile, d *Dismissals, run []api.Advisory) []api.Advisory {
	list := cf.Advisories()
	seen := map[string]bool{}
	for _, a := range list {
		seen[a.ID] = true
	}
	for _, a := range run {
		if !seen[a.ID] {
			seen[a.ID] = true
			list = append(list, a)
		}
	}
	ApplyDismissals(list, d)
	Sort(list)
	if list == nil {
		list = []api.Advisory{}
	}
	return list
}

// GateUnavailable is the stop of the gate when it cannot read the
// repodata of the result tree or of a closure repo: the gate fails closed
// (exit 13), and no acceptance overrides it.
func GateUnavailable(target, repo string, err error) *api.Error {
	return api.Errorf(api.PAdvisoryGateUnavailable, "%s: the advisory gate cannot read the repodata of %s: %v", target, repo, err).
		WithTarget(target).WithCause(err).WithHint("run the publish again when the repo answers", "")
}
