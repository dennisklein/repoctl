package advisory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// StaleAfter is the age after which the last check is stale.
const StaleAfter = 36 * time.Hour

// Signal is one health signal of the check (advisories.html#health): doctor
// shows it as a check, status and the UI as the check line.
type Signal struct {
	Code api.ProblemCode
	// Note marks check.no-linger, which is a note, not a warning.
	Note bool
	// Message is the doctor detail; Line the words of the status line.
	Message string
	Line    string
	Hint    api.Hint
}

// HealthInput is what the health signals read.
type HealthInput struct {
	Paths config.Paths
	Set   *config.Set
	File  *CheckFile
	Timer *Timer
	Now   time.Time
}

// Health gives the health signals of the check of a set.
func Health(ctx context.Context, in HealthInput) []Signal {
	var out []Signal
	cf := in.File
	state := in.Timer.State(ctx)
	if state != "active" {
		out = append(out, Signal{Code: api.PCheckTimerOff, Message: "the timer " + in.Timer.Instance() + " is " + state,
			Line: "the timer is off", Hint: api.Hint{Do: "install and start the timer", Run: "repoctl check --install-timer"}})
	}
	if in.Timer.UnitsDiffer() {
		out = append(out, Signal{Code: api.PCheckTimerUnit, Message: "the timer units in " + in.Timer.UnitDir + " differ from the units of this repoctl",
			Hint: api.Hint{Do: "write them again", Run: "repoctl check --install-timer"}})
	}
	for _, o := range Orphans(in.Timer.UnitDir) {
		out = append(out, Signal{Code: api.PCheckTimerOrphan, Message: o + " points to a directory that is gone or holds no set",
			Hint: api.Hint{Do: "disable it", Run: "systemctl --user disable --now " + o}})
	}
	if cf == nil {
		out = append(out, Signal{Code: api.PCheckNever, Message: "no check ran for this set on this host",
			Line: "never checked: run repoctl check --install-timer", Hint: api.Hint{Do: "run the check daily", Run: "repoctl check --install-timer"}})
	} else {
		at := cf.Time()
		if failed := cf.Failed(); len(failed) > 0 {
			var codes []string
			for _, p := range failed {
				codes = append(codes, p.Code)
			}
			codes = uniq(codes)
			out = append(out, Signal{Code: api.PCheckFailed, Message: fmt.Sprintf("the last check of %s failed: %s", at.UTC().Format("2006-01-02 15:04"), strings.Join(codes, ", ")),
				Line: fmt.Sprintf("last check failed %s ago: %s", Ago(in.Now.Sub(at)), strings.Join(codes, ", ")), Hint: api.Hint{Do: "run it again", Run: "repoctl check"}})
		}
		if in.Now.Sub(at) > StaleAfter {
			out = append(out, Signal{Code: api.PCheckStale, Message: "the last check is " + Ago(in.Now.Sub(at)) + " old",
				Line: "last check " + Ago(in.Now.Sub(at)) + " ago", Hint: api.Hint{Do: "run the check", Run: "repoctl check"}})
		}
		for _, t := range in.Set.ActiveTargets() {
			snap := ReadSnapshotJSON(in.Paths, in.Set.Name, t.ID)
			if snap == nil {
				continue
			}
			if c := api.MustTime(snap.Created); !c.IsZero() && c.After(at) {
				out = append(out, Signal{Code: api.PCheckStale, Message: "the last check is older than latest of " + t.ID,
					Line: "check older than latest of " + t.ID + ": run repoctl check", Hint: api.Hint{Do: "run the check", Run: "repoctl check"}})
				continue
			}
			for _, r := range cf.Results {
				if r.Target == t.ID && r.Latest != "" && snap.Digest != "" && r.Latest != snap.Digest {
					out = append(out, Signal{Code: api.PCheckOutdated, Message: "latest of " + t.ID + " changed after the check",
						Line: "latest of " + t.ID + " changed after the check", Hint: api.Hint{Do: "run the check", Run: "repoctl check"}})
					break
				}
			}
		}
	}
	if !Linger(in.Timer.User) {
		out = append(out, Signal{Code: api.PCheckNoLinger, Note: true,
			Message: "lingering is off: the timer runs only while you are logged in",
			Hint:    api.Hint{Do: "to run it always", Run: "loginctl enable-linger " + in.Timer.User}})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Note != out[j].Note && !out[i].Note })
	return out
}

// Ago gives an age in the words of the terminal: 5m, 8h, 3 days.
func Ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return plural(int(d/(24*time.Hour)), "day")
}
