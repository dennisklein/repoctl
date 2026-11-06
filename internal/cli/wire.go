package cli

import (
	"context"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/doctor"
	"github.com/dennisklein/repoctl/internal/release"
	"github.com/dennisklein/repoctl/internal/sign"
	"github.com/dennisklein/repoctl/internal/skills"
	"github.com/dennisklein/repoctl/internal/ui"
)

// wire.go connects the parts that do not know each other: the doctor
// checks of signing, the publish side and the agents.

func init() {
	doctor.Register(doctor.Spec{Group: doctor.GroupHost, Run: func(_ context.Context, env *doctor.Env) []doctor.Check {
		return []doctor.Check{fromReportCheck(doctor.GroupHost, ui.StaleServerCheck(env.Paths, config.Version))}
	}})
	doctor.Register(doctor.Spec{Group: doctor.GroupHost, Run: agentDoctorChecks})
	doctor.Register(doctor.Spec{Group: doctor.GroupSign, Run: signDoctorChecks})
}

// agentDoctorChecks runs agent.command, skills.guard and
// skills.version-mismatch (agents.html).
func agentDoctorChecks(ctx context.Context, env *doctor.Env) []doctor.Check {
	in := skills.DoctorEnv{Home: env.Getenv("HOME"), SetDir: env.SetDir, Version: config.Version}
	if env.Settings != nil {
		in.AgentCommand = env.Settings.String("agent.command")
	}
	var out []doctor.Check
	for _, c := range skills.DoctorChecks(ctx, in) {
		out = append(out, fromReportCheck(doctor.GroupHost, c))
	}
	return out
}

// signDoctorChecks runs the sign checks of signing.html#doctor.
func signDoctorChecks(ctx context.Context, env *doctor.Env) []doctor.Check {
	in := sign.DoctorInput{}
	var trust *sign.Trust
	if env.Set != nil {
		targets := env.Targets
		if len(targets) == 0 {
			for _, t := range env.Set.ActiveTargets() {
				targets = append(targets, t.ID)
			}
		}
		v, err := sign.NewView(env.Set, env.SetDir, env.Paths, targets)
		if err == nil {
			if env.Now != nil {
				v.Now = env.Now()
			}
			in.View = v
			trust = v.Trust
			set, dir := env.Set, env.SetDir
			in.ReleaseCheck = func() (bool, []string, error) { return release.Diff(set, dir, v.Trust) }
		}
	}
	in.Signer, in.SignerErr = sign.NewSigner(ctx, trust, env.Settings)
	if in.View != nil {
		in.View.Current = in.Signer
	}
	var out []doctor.Check
	for _, c := range sign.DoctorChecks(ctx, in) {
		out = append(out, fromReportCheck(doctor.GroupSign, c))
	}
	return out
}

// fromReportCheck turns a check of the contract into a doctor check.
func fromReportCheck(group string, c api.DoctorReportCheck) doctor.Check {
	g := c.Group
	if g == "" {
		g = group
	}
	return doctor.Check{Name: c.Name, Group: g, Status: c.Status, Detail: c.Detail, Fix: c.Remediation}
}
