package advisory

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
)

// CodeCheck is one code of repoctl check: an advisory code that needs no
// build. Other parts of repoctl add theirs with RegisterCode, for example
// the signing part with sign.expiring.
type CodeCheck struct {
	Code api.AdvisoryCode
	// AllTargets marks a code with one result for all targets
	// (upstream.release); Run gets the target "".
	AllTargets bool
	// Run computes the advisories of the code on one target. A SkipError
	// narrows the check: the last result stays. Any other error fails the
	// result, which keeps the advisories of the last good result.
	Run func(ctx context.Context, in *Inputs, target string) (Outcome, error)
}

// Outcome is what a code found on one target.
type Outcome struct {
	Advisories []api.Advisory
	// Latest is the digest of latest of the target that the code used.
	Latest api.Digest
	// Partial is set when a part of the input failed, for example one
	// upstream: the result fails, the advisories of Advisories are new,
	// and the advisories of the subjects in Carry come from the last good
	// result.
	Partial *api.Problem
	Carry   []string
}

var (
	codesMu sync.Mutex
	codes   = map[api.AdvisoryCode]CodeCheck{}
)

// RegisterCode adds a code to repoctl check. A second registration of a
// code replaces the first.
func RegisterCode(c CodeCheck) {
	codesMu.Lock()
	defer codesMu.Unlock()
	codes[c.Code] = c
}

// Codes gives the registered codes in the order of the contract.
func Codes() []CodeCheck {
	codesMu.Lock()
	defer codesMu.Unlock()
	out := make([]CodeCheck, 0, len(codes))
	for _, c := range codes {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return codeRank(out[i].Code) < codeRank(out[j].Code) })
	return out
}

// Options select what a check runs.
type Options struct {
	// Codes are the codes of --code; empty runs all.
	Codes []api.AdvisoryCode
	// Targets are the targets of --target; empty runs all active targets.
	Targets []string
	// By is timer, human, ui or mcp.
	By string
	// Journal gets the events of the check run; nil writes none.
	Journal Journal
	// OnWait is called when another check holds the check lock, with the
	// start of that check.
	OnWait func(since time.Time)
	// Retain runs the retention of the check journals: the holder of the
	// check lock calls it at the start of the check (contract.html#writing).
	Retain func()
}

// Skip is a code and target that did not run.
type Skip struct {
	Code   api.AdvisoryCode
	Target string
	Reason string
}

// Summary is the end of a check run.
type Summary struct {
	// File is check.json after the run.
	File *CheckFile
	// Ran are the results of this run.
	Ran     []Result
	Skipped []Skip
	// Gone are the ids that this run did not find again.
	Gone []string
	// Waited reports that another check ran, and this call gives its
	// result.
	Waited   bool
	Duration time.Duration
}

// Failed gives the problems of the results of this run.
func (s *Summary) Failed() []api.Problem {
	var out []api.Problem
	for _, r := range s.Ran {
		if !r.Ok && r.Problem != nil {
			out = append(out, *r.Problem)
		}
	}
	return out
}

// Check runs the check under the check lock of the set: one check per set
// for one user on one host. A second check waits for the running one and
// gives its result. The holder writes the journal and check.json.
func Check(ctx context.Context, in *Inputs, o Options, holder lock.Holder) (*Summary, error) {
	path := in.Paths.CheckLock(in.Set.Name)
	what := lock.What{Code: api.PSetBusy, Name: "the check of " + in.Set.Name, Lock: "the check lock"}
	l, err := lock.Acquire(ctx, path, holder, 0, false, what)
	if err != nil {
		if _, ok := api.AsError(err); !ok {
			return nil, err
		}
		h, _ := lock.ReadHolder(path)
		if o.OnWait != nil && h != nil {
			o.OnWait(h.Started)
		}
		l, err = lock.Acquire(ctx, path, holder, time.Hour, true, what)
		if err != nil {
			return nil, err
		}
		l.Release()
		cf, err := ReadCheckFile(in.Paths, in.Set.Name)
		if err != nil {
			return nil, err
		}
		return &Summary{File: cf, Waited: true}, nil
	}
	defer l.Release()
	if o.Retain != nil {
		o.Retain()
	}
	run := NewRunID(in.now())
	_ = l.SetRun(run)
	return Run(ctx, in, o, run)
}

// NewRunID gives a run id r-YYYYMMDD-HHMM-xxx.
func NewRunID(t time.Time) api.RunId {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = digits[int(b[i])%len(digits)]
	}
	return fmt.Sprintf("r-%s-%s", t.UTC().Format("20060102-1504"), b)
}

type job struct {
	c      CodeCheck
	target string
}

// Run runs the selected codes on the selected targets and merges their
// results into check.json. The caller holds the check lock.
func Run(ctx context.Context, in *Inputs, o Options, run api.RunId) (*Summary, error) {
	start := in.now()
	prev, err := ReadCheckFile(in.Paths, in.Set.Name)
	if err != nil {
		prev = nil
	}
	want := map[api.AdvisoryCode]bool{}
	for _, c := range o.Codes {
		want[c] = true
	}
	targets := o.Targets
	if len(targets) == 0 {
		for _, t := range in.Set.ActiveTargets() {
			targets = append(targets, t.ID)
		}
	}
	var jobs []job
	for _, c := range Codes() {
		if len(want) > 0 && !want[c.Code] {
			continue
		}
		if c.AllTargets {
			jobs = append(jobs, job{c, ""})
			continue
		}
		for _, t := range targets {
			jobs = append(jobs, job{c, t})
		}
	}
	if o.Journal != nil {
		if err := o.Journal.Started(run, in.Set.Name, start); err != nil {
			return nil, err
		}
	}
	type answer struct {
		out Outcome
		err error
	}
	answers := make([]answer, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, err := j.c.Run(ctx, in, j.target)
			answers[i] = answer{out, err}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := api.FormatTime(in.now())
	results := map[string]Result{}
	var order []string
	if prev != nil {
		for _, r := range prev.Results {
			if keep(in.Set, r) {
				results[r.key()] = r
				order = append(order, r.key())
			}
		}
	}
	sum := &Summary{}
	for i, j := range jobs {
		a := answers[i]
		old, hadOld := results[resultKey(j.c.Code, j.target)]
		if s, ok := IsSkip(a.err); ok {
			sum.Skipped = append(sum.Skipped, Skip{Code: j.c.Code, Target: j.target, Reason: s.Reason})
			continue
		}
		r := Result{Code: j.c.Code, Target: j.target, At: now}
		if a.err != nil {
			p := problemOf(a.err)
			p.Target = j.target
			r.Problem = &p
			r.Latest = old.Latest
			r.GoodAt = goodAt(old, hadOld)
			for _, adv := range old.Advisories {
				adv.CarriedFrom = r.GoodAt
				r.Advisories = append(r.Advisories, adv)
			}
		} else {
			r.Ok = a.out.Partial == nil
			r.Latest = a.out.Latest
			r.Problem = a.out.Partial
			seen := map[string]api.Advisory{}
			for _, adv := range old.Advisories {
				seen[adv.ID] = adv
			}
			found := map[string]bool{}
			for _, adv := range a.out.Advisories {
				adv.Source = "check"
				adv.Seen = now
				adv.CarriedFrom = ""
				if o, ok := seen[adv.ID]; ok && o.FactsDigest == adv.FactsDigest && o.Seen != "" {
					adv.Seen = o.Seen
				}
				found[adv.ID] = true
				r.Advisories = append(r.Advisories, adv)
			}
			carry := map[string]bool{}
			for _, s := range a.out.Carry {
				carry[s] = true
			}
			if a.out.Partial != nil {
				r.GoodAt = goodAt(old, hadOld)
			}
			for _, adv := range old.Advisories {
				if found[adv.ID] {
					continue
				}
				if carry[adv.Subject] {
					adv.CarriedFrom = r.GoodAt
					r.Advisories = append(r.Advisories, adv)
					continue
				}
				sum.Gone = append(sum.Gone, adv.ID)
			}
		}
		Sort(r.Advisories)
		k := r.key()
		if _, ok := results[k]; !ok {
			order = append(order, k)
		}
		results[k] = r
		sum.Ran = append(sum.Ran, r)
	}
	cf := &CheckFile{Set: in.Set.Name, At: now, By: o.By, Run: run}
	if len(jobs) == 0 || len(sum.Ran) == 0 {
		// Nothing ran: keep the time of the last run.
		if prev != nil {
			cf.At, cf.By, cf.Run, cf.DurationMs = prev.At, prev.By, prev.Run, prev.DurationMs
		}
	}
	for _, k := range order {
		cf.Results = append(cf.Results, results[k])
	}
	sum.Duration = in.now().Sub(start)
	if len(sum.Ran) > 0 {
		cf.DurationMs = int(sum.Duration / time.Millisecond)
	}
	sort.Strings(sum.Gone)
	sum.File = cf
	if len(sum.Ran) > 0 {
		if err := cf.Write(in.Paths); err != nil {
			return nil, err
		}
	}
	if o.Journal != nil {
		var jerr error
		for _, r := range sum.Ran {
			for _, adv := range r.Advisories {
				if adv.CarriedFrom != "" {
					continue
				}
				jerr = errors.Join(jerr, o.Journal.Advisory(adv))
			}
		}
		for _, id := range sum.Gone {
			jerr = errors.Join(jerr, o.Journal.Gone(id))
		}
		state, code := api.RunStateOk, 0
		if len(sum.Failed()) > 0 {
			state, code = api.RunStateFailed, api.ExitFailed
		}
		jerr = errors.Join(jerr, o.Journal.Finished(state, sum.Duration, code))
		if jerr != nil {
			return sum, jerr
		}
	}
	return sum, nil
}

func resultKey(code api.AdvisoryCode, target string) string { return string(code) + "\x00" + target }

func goodAt(old Result, had bool) api.Time {
	switch {
	case !had:
		return ""
	case old.Ok:
		return old.At
	}
	return old.GoodAt
}

// keep reports whether a result of the last check still belongs to the
// set: its code is registered and its target is active.
func keep(set *config.Set, r Result) bool {
	known := false
	for _, c := range Codes() {
		if c.Code == r.Code {
			known = true
		}
	}
	if !known {
		return false
	}
	if r.Target == "" {
		return true
	}
	t, ok := set.Target(r.Target)
	return ok && !t.Retired()
}

func problemOf(err error) api.Problem {
	if e, ok := api.AsError(err); ok {
		return e.Problem
	}
	return api.Problem{Code: api.PCheckFailed, Message: err.Error()}
}
