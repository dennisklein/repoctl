package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/testkit"
)

func TestSPDX(t *testing.T) {
	for expr, want := range map[string]bool{
		"GPL-2.0-or-later":                         true,
		"BSD-3-Clause":                             true,
		"MIT AND (BSD-2-Clause OR Apache-2.0)":     true,
		"GPL-2.0-or-later WITH Linux-syscall-note": true,
		"LicenseRef-Fedora-Public-Domain":          true,
		"GPLv2+":                                   false,
		"BSD":                                      false,
		"GPLv2+ and BSD":                           false,
		"MIT and BSD-3-Clause":                     false,
		"MIT AND":                                  false,
		"":                                         false,
		"ASL 2.0":                                  false,
	} {
		if got := SPDX(expr); got != want {
			t.Errorf("SPDX(%q) = %v, want %v", expr, got, want)
		}
	}
}

type plain struct{}

func (plain) MarkOfCheck(s api.CheckStatus) string {
	return map[api.CheckStatus]string{api.CheckStatusOk: "✓", api.CheckStatusWarning: "!", api.CheckStatusFailed: "✗"}[s]
}
func (plain) Dim(s string) string  { return s }
func (plain) Bold(s string) string { return s }

func TestReportAndWrite(t *testing.T) {
	checks := []Check{
		Ok("git", "git 2.43.0"),
		Warn("build.linger", "lingering is off", "enable it. run: loginctl enable-linger dk"),
		Fail("mock.configs", "missing on this host: el10 rocky+epel-10-x86_64", "install the config"),
	}
	checks[0].Group, checks[1].Group, checks[2].Group = GroupHost, GroupHost, GroupHost
	r := Report(checks)
	if r.Worst != api.CheckStatusFailed || r.Counts.Ok != 1 || r.Counts.Warning != 1 || r.Counts.Failed != 1 {
		t.Errorf("Report: %+v", r)
	}
	if r.Checks[0].Detail != "git 2.43.0" {
		t.Errorf("the detail of an ok check: %q", r.Checks[0].Detail)
	}
	if err := testkit.Validate("DoctorReport", testkit.MarshalItem(t, r)); err != nil {
		t.Error(err)
	}
	if got := Totals(r); got != "3 checks: 1 ok, 1 warning, 1 failed" {
		t.Errorf("Totals: %q", got)
	}
	var b strings.Builder
	if err := Write(&b, plain{}, checks); err != nil {
		t.Fatal(err)
	}
	want := "✓ git 2.43.0\n" +
		"! build.linger: lingering is off\n" +
		"    fix: enable it. run: loginctl enable-linger dk\n" +
		"✗ mock.configs: missing on this host: el10 rocky+epel-10-x86_64\n" +
		"    fix: install the config\n"
	if b.String() != want {
		t.Errorf("Write:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestRegistryAndPreflight(t *testing.T) {
	regMu.Lock()
	saved := registry
	registry = nil
	regMu.Unlock()
	t.Cleanup(func() {
		regMu.Lock()
		registry = saved
		regMu.Unlock()
	})
	Register(Spec{Group: GroupSign, Placeholder: true, Run: func(context.Context, *Env) []Check {
		return []Check{Ok("sign", "sign: not checked by this release")}
	}})
	Register(Spec{Group: GroupHost, Needs: []string{NeedBuild, NeedCheck}, Run: func(_ context.Context, env *Env) []Check {
		if env.Need == NeedCheck {
			return []Check{Ok("tools", "tools: all there")}
		}
		c := Fail("tools", "rpmspec is missing", "install rpm-build. run: dnf install rpm-build")
		c.Code = api.PHostNotReady
		return []Check{c}
	}})
	Register(Spec{Group: GroupHost, Needs: []string{NeedBuild}, Run: func(context.Context, *Env) []Check {
		c := Fail("mock.configs", "missing on this host: el10 rocky+epel-10-x86_64", "install the mock config. run: repoctl help mock")
		c.Code = api.PMockConfigMissing
		return []Check{c}
	}})
	env := &Env{}
	all := Run(t.Context(), env, "")
	if len(all) != 3 || all[0].Group != GroupSign {
		t.Fatalf("Run: %+v", all)
	}
	if only := Run(t.Context(), env, GroupSign); len(only) != 1 {
		t.Errorf("--only sign: %+v", only)
	}
	// A real spec replaces the placeholder of its group.
	Register(Spec{Group: GroupSign, Run: func(context.Context, *Env) []Check {
		return []Check{Warn("sign.expiring", "the key expires in 20 days", "")}
	}})
	if only := Run(t.Context(), env, GroupSign); len(only) != 1 || only[0].Name != "sign.expiring" {
		t.Errorf("placeholder: %+v", only)
	}
	if err := Preflight(t.Context(), env, NeedCheck); err != nil {
		t.Errorf("check preflight: %v", err)
	}
	if err := Preflight(t.Context(), env, NeedPublish); err != nil {
		t.Errorf("publish preflight runs no spec: %v", err)
	}
	err := Preflight(t.Context(), env, NeedBuild)
	e, ok := api.AsError(err)
	if !ok || e.Code != api.PHostNotReady || e.ExitCode != api.ExitHostNotReady {
		t.Fatalf("build preflight: %v", err)
	}
	if !strings.Contains(e.Message, "rpmspec is missing") || !strings.Contains(e.Message, "missing on this host: el10") {
		t.Errorf("message: %s", e.Message)
	}
	if len(e.Hints) != 3 || e.Hints[0].Run != "dnf install rpm-build" || e.Hints[2].Run != "repoctl doctor" || e.HumanAction == nil {
		t.Errorf("hints: %+v %+v", e.Hints, e.HumanAction)
	}
}
