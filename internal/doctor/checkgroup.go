package doctor

import (
	"context"

	"github.com/dennisklein/repoctl/internal/advisory"
)

// checkCheck gives the health signals of the advisory check: the timer and
// the age and the result of the last check (advisories.html#health).
func checkCheck(ctx context.Context, env *Env) []Check {
	if env.Set == nil {
		return nil
	}
	cf, _ := advisory.ReadCheckFile(env.Paths, env.Set.Name)
	t := &advisory.Timer{UnitDir: advisory.UnitDir(env.Paths), SetDir: env.SetDir, TestHost: env.Paths.TestHost, User: env.User}
	signals := advisory.Health(ctx, advisory.HealthInput{Paths: env.Paths, Set: env.Set, File: cf, Timer: t, Now: env.now()})
	var out []Check
	warned := false
	for _, s := range signals {
		fix := s.Hint.String()
		if s.Note {
			c := Ok(s.Code, s.Code+": "+s.Message)
			c.Fix = fix
			out = append(out, c)
			continue
		}
		warned = true
		out = append(out, Warn(s.Code, s.Message, fix))
	}
	if !warned {
		line := "check: timer on"
		if cf != nil {
			line += ", last check " + cf.Time().UTC().Format("2006-01-02 15:04") + ", ok"
		}
		out = append([]Check{Ok("check", line)}, out...)
	}
	return out
}
