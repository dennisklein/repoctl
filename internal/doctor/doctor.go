// Package doctor holds the checks of this host and of the set: repoctl
// doctor runs all of them, and the preflight of build, publish, check and
// mock runs the ones that the command needs (docs/design/cli.html#cmd-doctor,
// build.html#run). The checks live in a registry: each part of repoctl adds
// the checks of its group with Register (host, set, sign, registry, check).
package doctor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	clikitdoctor "github.com/GSI-HPC/go-clikit/doctor"
	"github.com/GSI-HPC/go-clikit/termtext"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/mock"
)

// The groups of --only, in the order of the output.
const (
	GroupHost     = "host"
	GroupSet      = "set"
	GroupSign     = "sign"
	GroupRegistry = "registry"
	GroupCheck    = "check"
)

// Groups lists the groups of --only.
var Groups = []string{GroupHost, GroupSet, GroupSign, GroupRegistry, GroupCheck}

// The needs of the preflight: the commands that stop with exit 9 when a
// required check fails.
const (
	NeedBuild   = "build"
	NeedPublish = "publish"
	NeedCheck   = "check"
	NeedMock    = "mock"
)

// Env is what the checks read: this host, the set of the command and the
// need of a preflight.
type Env struct {
	Paths  config.Paths
	Getenv func(string) string
	Now    func() time.Time
	// Set is the set of the command, nil outside a set. SetErr is the
	// error of repoctl.yaml (not the error "no set").
	Set    *config.Set
	SetDir string
	SetErr error
	// Targets are the selected targets; empty: all active targets.
	Targets []string
	// Need is "" for repoctl doctor, else the preflight: build, publish,
	// check or mock. A check can be a warning in doctor and fail a
	// preflight (the secret key of a publish).
	Need string
	// PublishBytes is the size of the RPMs that a publish signs.
	PublishBytes int64
	// Version and Commit are the version of repoctl.
	Version, Commit string
	// User is the login name.
	User string
	// Settings are the effective user settings; nil without a valid user
	// config.
	Settings *config.Effective
	// Mock runs mock; nil uses mock(1) on PATH.
	Mock *mock.Mock
}

// TargetIDs gives the selected targets, or all active targets of the set.
func (e *Env) TargetIDs() []string {
	if len(e.Targets) > 0 || e.Set == nil {
		return e.Targets
	}
	var out []string
	for _, t := range e.Set.ActiveTargets() {
		out = append(out, t.ID)
	}
	return out
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Env) getenv(k string) string {
	if e.Getenv != nil {
		return e.Getenv(k)
	}
	return os.Getenv(k)
}

func (e *Env) mock() *mock.Mock {
	if e.Mock != nil {
		return e.Mock
	}
	return mock.New(e.Paths, mock.Options{})
}

// Check is one check: ok, warning or failed, with the fix of a warning or
// a failure.
type Check struct {
	// Name is the check id, for example sign.retiring or build.linger.
	Name   string
	Group  string
	Status api.CheckStatus
	Detail string
	// Fix is the remediation: "WHAT TO DO. run: CMD".
	Fix string
	// Text is the line of the terminal, default "NAME: DETAIL".
	Text string
	// Code is the problem of a failed check in a preflight, default
	// host.not-ready.
	Code api.ProblemCode
}

// Line gives the text of the terminal line of the check.
func (c Check) Line() string {
	if c.Text != "" {
		return c.Text
	}
	if c.Detail == "" {
		return c.Name
	}
	return c.Name + ": " + c.Detail
}

// Ok gives a passed check.
func Ok(name, text string) Check { return Check{Name: name, Status: api.CheckStatusOk, Text: text} }

// Warn gives a warning.
func Warn(name, detail, fix string) Check {
	return Check{Name: name, Status: api.CheckStatusWarning, Detail: detail, Fix: fix}
}

// Fail gives a failed check.
func Fail(name, detail, fix string) Check {
	return Check{Name: name, Status: api.CheckStatusFailed, Detail: detail, Fix: fix}
}

// Spec is one entry of the registry.
type Spec struct {
	Group string
	// Needs are the preflights that run the spec.
	Needs []string
	// Run gives the checks of the spec.
	Run func(ctx context.Context, env *Env) []Check
	// Placeholder marks a spec that stands in for a group until a real
	// spec of that group registers.
	Placeholder bool
}

var (
	regMu    sync.Mutex
	registry []Spec
)

// Register adds a spec of checks to a group. The checks of repoctl doctor
// come out in the order of registration.
func Register(s Spec) {
	regMu.Lock()
	defer regMu.Unlock()
	registry = append(registry, s)
}

// specs gives the registry without the placeholders of a group that has a
// real spec.
func specs() []Spec {
	regMu.Lock()
	defer regMu.Unlock()
	real := map[string]bool{}
	for _, s := range registry {
		if !s.Placeholder {
			real[s.Group] = true
		}
	}
	var out []Spec
	for _, s := range registry {
		if s.Placeholder && real[s.Group] {
			continue
		}
		out = append(out, s)
	}
	return out
}

func hasNeed(s Spec, need string) bool {
	for _, n := range s.Needs {
		if n == need {
			return true
		}
	}
	return false
}

// Run runs the checks of repoctl doctor, of one group with only.
func Run(ctx context.Context, env *Env, only string) []Check {
	var out []Check
	for _, s := range specs() {
		if only != "" && s.Group != only {
			continue
		}
		for _, c := range s.Run(ctx, env) {
			if c.Group == "" {
				c.Group = s.Group
			}
			out = append(out, c)
		}
	}
	return out
}

// Preflight runs the checks that a command needs and stops with exit 9
// when one fails (host.not-ready, or the code of the check, such as
// mock.config-missing). need is build, publish, check or mock.
func Preflight(ctx context.Context, env *Env, need string) error {
	e := *env
	e.Need = need
	var failed []Check
	for _, s := range specs() {
		if !hasNeed(s, need) {
			continue
		}
		for _, c := range s.Run(ctx, &e) {
			if c.Status == api.CheckStatusFailed {
				failed = append(failed, c)
			}
		}
	}
	if len(failed) == 0 {
		return nil
	}
	code := failed[0].Code
	if code == "" {
		code = api.PHostNotReady
	}
	var what []string
	for _, c := range failed {
		what = append(what, c.Line())
	}
	err := api.Errorf(code, "this host is not ready for %s: %s", need, strings.Join(what, "; ")).WithExit(api.ExitHostNotReady)
	for i, c := range failed {
		if i == 2 {
			break
		}
		if c.Fix != "" {
			do, run, _ := strings.Cut(c.Fix, "run: ")
			err = err.WithHint(strings.TrimSuffix(strings.TrimSpace(do), "."), run)
		}
	}
	return err.WithHint("check this host", "repoctl doctor").WithHuman("install-tool", "a required host check failed", "repoctl doctor")
}

// Report gives the kind DoctorReport of the checks.
func Report(checks []Check) api.DoctorReport {
	r := api.DoctorReport{Checks: []api.DoctorReportCheck{}}
	var kit []clikitdoctor.Check
	for _, c := range checks {
		r.Checks = append(r.Checks, api.DoctorReportCheck{Name: c.Name, Group: c.Group, Status: c.Status, Detail: detailOf(c), Remediation: c.Fix})
		kit = append(kit, clikitdoctor.Check{Name: c.Name, Status: clikitdoctor.Status(c.Status), Detail: c.Detail, Remediation: c.Fix})
		switch c.Status {
		case api.CheckStatusOk:
			r.Counts.Ok++
		case api.CheckStatusWarning:
			r.Counts.Warning++
		default:
			r.Counts.Failed++
		}
	}
	r.Worst = api.CheckStatus(clikitdoctor.Worst(kit))
	return r
}

// detailOf gives the detail of the JSON form: the detail, else the line
// without the name.
func detailOf(c Check) string {
	if c.Detail != "" {
		return c.Detail
	}
	return strings.TrimPrefix(c.Text, c.Name+": ")
}

// Style draws the marks and colours of the doctor text.
type Style interface {
	MarkOfCheck(api.CheckStatus) string
	Dim(string) string
	Bold(string) string
}

// Write writes the checks for a person: passed checks dim, and the fix of
// each warning or failure directly under it (terminal.html#doctor).
func Write(w io.Writer, st Style, checks []Check) error {
	var b strings.Builder
	for _, c := range checks {
		line := termtext.Escape(c.Line())
		if c.Status == api.CheckStatusOk {
			line = st.Dim(line)
		}
		b.WriteString(st.MarkOfCheck(c.Status) + " " + line + "\n")
		if c.Status != api.CheckStatusOk && c.Fix != "" {
			do, run, ok := strings.Cut(c.Fix, "run: ")
			if ok {
				b.WriteString("    " + st.Dim("fix: "+termtext.Escape(do)+"run: ") + st.Bold(termtext.Escape(run)) + "\n")
			} else {
				b.WriteString("    " + st.Dim("fix: ") + termtext.Escape(c.Fix) + "\n")
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Totals gives the totals line: "17 checks: 14 ok, 3 warnings".
func Totals(r api.DoctorReport) string {
	n := r.Counts.Ok + r.Counts.Warning + r.Counts.Failed
	parts := []string{fmt.Sprintf("%d ok", r.Counts.Ok)}
	if r.Counts.Warning > 0 {
		parts = append(parts, plural(r.Counts.Warning, "warning"))
	}
	if r.Counts.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", r.Counts.Failed))
	}
	return fmt.Sprintf("%s: %s", plural(n, "check"), strings.Join(parts, ", "))
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// tilde writes a path below HOME as ~/….
func (e *Env) tilde(p string) string {
	home := e.getenv("HOME")
	if home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return p
}
