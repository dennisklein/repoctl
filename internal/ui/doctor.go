package ui

import (
	"fmt"
	"os/exec"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// DoctorChecks gives the checks of the UI for repoctl doctor, in the
// group host: host.stale-server (a UI server of the user runs another
// version of repoctl, or a binary that an upgrade replaced) and
// agent.command (the agent of the agent buttons is on PATH).
func DoctorChecks(p config.Paths, version string, agent AgentConfig) []api.DoctorReportCheck {
	return []api.DoctorReportCheck{StaleServerCheck(p, version), agentCheck(agent)}
}

// StaleServerCheck is the check host.stale-server. A server that runs
// another version is a warning: it starts no child of an older or newer
// repoctl than the one on PATH.
func StaleServerCheck(p config.Paths, version string) api.DoctorReportCheck {
	c := api.DoctorReportCheck{Name: api.PHostStaleServer, Group: "host", Status: api.CheckStatusOk}
	rec, running := ReadRecord(p.UIJSON())
	switch {
	case !running:
		c.Detail = "no UI server runs"
	case rec.Version != version:
		c.Status = api.CheckStatusWarning
		c.Detail = fmt.Sprintf("the UI server (pid %d) runs repoctl %s; this is %s", rec.PID, rec.Version, version)
		c.Remediation = "repoctl ui --stop && repoctl ui"
	default:
		c.Detail = fmt.Sprintf("the UI server (pid %d) runs repoctl %s", rec.PID, rec.Version)
	}
	return c
}

func agentCheck(a AgentConfig) api.DoctorReportCheck {
	c := api.DoctorReportCheck{Name: "agent.command", Group: "host", Status: api.CheckStatusOk}
	if a.Command == "" {
		c.Detail = "agent.command is not set: the agent buttons of the UI are off"
		return c
	}
	if _, err := exec.LookPath(a.Command); err != nil {
		c.Status = api.CheckStatusWarning
		c.Detail = "the agent command " + a.Command + " is not on PATH"
		c.Remediation = "install it, or set agent.command in the user config"
		return c
	}
	c.Detail = "the agent buttons run " + a.Command
	return c
}

// HostChecks gives the checks of the UI for its doctor page: the two of
// DoctorChecks, and whether this server's own binary was replaced.
func (s *Server) HostChecks() []api.DoctorReportCheck {
	out := DoctorChecks(s.o.Paths, s.o.Version, s.o.Agent)
	if why := s.stale(); why != "" {
		out[0] = api.DoctorReportCheck{Name: api.PHostStaleServer, Group: "host", Status: api.CheckStatusWarning,
			Detail: "this server runs a binary that an upgrade replaced: " + why + ". It starts no child.", Remediation: "repoctl ui --stop && repoctl ui"}
	}
	return out
}
