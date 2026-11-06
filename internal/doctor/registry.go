package doctor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/localcopy"
)

// The checks of the group registry (publish.html#registry): the
// credentials, the token scopes, the write access and the visibility of
// the OCI packages of the set, and the heartbeat of the served tree. A
// check that cannot run gives no line, never a pass.

// ServedClient is the HTTP client of served.stale; tests replace it.
var ServedClient = &http.Client{Timeout: localcopy.ServedTimeout}

// The token scopes that a push token may have (publish.html#registry).
var tokenScopes = map[string]bool{"write:packages": true, "read:packages": true, "delete:packages": true}

// checkRegistryAccess gives registry.login, registry.scope, registry.write
// and registry.public for the OCI packages of the selected targets.
func checkRegistryAccess(ctx context.Context, env *Env) []Check {
	if env.Set == nil || env.Set.Store.OCI == "" {
		return nil
	}
	host := localcopy.RegistryHost(env.Set)
	if host == "" {
		return nil
	}
	getenv := env.getenv
	var out []Check
	login, err := localcopy.FindLogin(ctx, host, getenv)
	loginFix := "log in with a token of the scope write:packages. run: oras login " + host + " -u USER --password-stdin"
	switch {
	case err != nil:
		out = append(out, Warn("registry.login", fmt.Sprintf("the container credentials cannot be read: %v", err), loginFix))
	case login == nil:
		out = append(out, Warn("registry.login", "no credentials for "+host+" in the container auth files; publish cannot push", loginFix))
	default:
		who := login.Username
		if who == "" {
			who = "a token"
		}
		out = append(out, Ok("registry.login", fmt.Sprintf("registry.login: %s as %s (container auth file)", host, who)))
	}
	if login != nil && localcopy.IsGHCR(host, getenv) {
		out = append(out, checkScope(ctx, env, login))
	}
	var pkgs, names []string
	for _, t := range env.TargetIDs() {
		pkg := env.Set.OCIPackage(t)
		pkgs = append(pkgs, pkg)
		names = append(names, env.Set.Name+"-"+t)
	}
	if len(pkgs) == 0 {
		return out
	}
	// registry.write needs a login; without one, registry.login warns.
	if login != nil {
		var denied []string
		var firstErr error
		for i, pkg := range pkgs {
			if err := localcopy.CanWrite(ctx, pkg, getenv); err != nil {
				denied = append(denied, names[i])
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		if len(denied) > 0 {
			out = append(out, Warn("registry.write", fmt.Sprintf("no write access to %s: %v", strings.Join(denied, ", "), firstErr),
				"log in with a token of the scope write:packages and ask for the team access to the packages. run: oras login "+host+" -u USER --password-stdin"))
		} else {
			out = append(out, Ok("registry.write", "registry.write: write access to "+strings.Join(names, ", ")))
		}
	}
	var public, private, notPushed []string
	var fixes []string
	for i, pkg := range pkgs {
		v, err := localcopy.PackageVisibility(ctx, pkg, getenv)
		if err != nil {
			out = append(out, Warn("registry.public", fmt.Sprintf("the registry %s does not answer for %s: %v", host, names[i], err), ""))
			return out
		}
		switch v {
		case localcopy.Public:
			public = append(public, names[i])
		case localcopy.Private:
			private = append(private, names[i])
			if u := localcopy.SettingsURL(pkg); u != "" {
				fixes = append(fixes, u)
			}
		default:
			notPushed = append(notPushed, names[i])
		}
	}
	switch {
	case len(private) > 0:
		fix := "an organization owner sets each package to public and turns on the inherited access"
		if len(fixes) > 0 {
			fix += ": " + strings.Join(fixes, " ")
		}
		out = append(out, Warn("registry.public", strings.Join(private, ", ")+" "+isAre(len(private))+" private: the mirror job and the clients cannot pull", fix))
	case len(public) > 0:
		line := "registry.public: " + strings.Join(public, ", ") + " " + isAre(len(public)) + " public"
		if len(notPushed) > 0 {
			line += "; " + strings.Join(notPushed, ", ") + " not pushed yet"
		}
		out = append(out, Ok("registry.public", line))
	}
	return out
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// checkScope is registry.scope: the token of GHCR has write:packages, no
// scope beyond the packages, and an expiry; without delete:packages, gc
// cannot run.
func checkScope(ctx context.Context, env *Env, login *localcopy.Login) Check {
	const name = "registry.scope"
	const page = "https://github.com/settings/tokens"
	info, err := localcopy.GitHubToken(ctx, env.getenv, login.Secret)
	if err != nil {
		return Warn(name, fmt.Sprintf("the scopes of the token cannot be read: %v", err), "make a classic token with the scope write:packages on "+page)
	}
	has := map[string]bool{}
	var extra []string
	for _, s := range info.Scopes {
		has[s] = true
		if !tokenScopes[s] {
			extra = append(extra, s)
		}
	}
	switch {
	case !has["write:packages"]:
		return Warn(name, "the token has no scope write:packages, so publish cannot push", "add the scope write:packages to the token on "+page)
	case len(extra) > 0:
		return Warn(name, "the token has the scopes "+strings.Join(extra, ", ")+" beyond the packages", "use a token for the registry only, with write:packages and delete:packages, on "+page)
	case info.Expires.IsZero():
		return Warn(name, "the token has no expiry", "make a token with an expiry of one year or less on "+page)
	case !has["delete:packages"]:
		return Warn(name, "the token has no scope delete:packages, so repo gc cannot run", "add the scope delete:packages to the token on "+page+" (only gc needs it)")
	}
	return Ok(name, fmt.Sprintf("%s: %s, expires %s", name, strings.Join(info.Scopes, ", "), info.Expires.UTC().Format("2006-01-02")))
}

// checkServedStale is served.stale: it reads the heartbeat of the mirror
// line of each target that this host has a local copy of, and warns as
// status does (publish.html#stale-served). A target without a local copy
// has nothing served to compare yet.
func checkServedStale(ctx context.Context, env *Env) []Check {
	const name = api.PServedStale
	if env.Set == nil || env.Set.Repo.URL == "" {
		return nil
	}
	var targets []string
	for _, t := range env.TargetIDs() {
		if s, _ := localcopy.ReadSnapshot(localcopy.Dir(env.Paths, env.Set.Name, t)); s != nil {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return []Check{Ok(name, name+": no local copy of latest, nothing served to compare")}
	}
	now := env.now()
	clock := func(t time.Time) string {
		if now.Sub(t) < 24*time.Hour {
			return t.Local().Format("15:04")
		}
		return t.Local().Format("2006-01-02 15:04")
	}
	fix := localcopy.StaleHint()
	var out []Check
	var oldest time.Time
	results := map[string]bool{}
	for _, s := range localcopy.ReadServed(ctx, ServedClient, env.Set, targets) {
		switch {
		case s.Err != nil:
			out = append(out, Warn(name, fmt.Sprintf("repo.url does not answer for %s: %v", s.Target, s.Err),
				"check the HTTP server of repo.url and the mirror job. run: journalctl -u repoctl-mirror.service"))
			continue
		case s.Mirror == nil:
			out = append(out, Warn(name, fmt.Sprintf("%s has no heartbeat %s: the mirror job does not run for this line", s.Target, s.URL+"/"+localcopy.MirrorFile), fix))
			continue
		}
		if w := s.Mirror.StaleWarning(s.Target, now, clock); w != nil {
			out = append(out, Warn(name, w.Message, fix))
			continue
		}
		if c := s.Mirror.CheckedTime(); oldest.IsZero() || c.Before(oldest) {
			oldest = c
		}
		results[s.Mirror.Result] = true
	}
	if len(out) > 0 {
		return out
	}
	words := "same"
	if !results["same"] || len(results) > 1 {
		var rs []string
		for _, r := range []string{"same", "swapped", "failed"} {
			if results[r] {
				rs = append(rs, r)
			}
		}
		words = strings.Join(rs, ", ")
	}
	both := "both"
	if len(targets) == 1 {
		both = "result"
	} else if len(targets) > 2 {
		both = "all"
	}
	return []Check{Ok(name, fmt.Sprintf("%s: the mirror job checked %s %s ago, %s %s", name, andList(targets), ago(now.Sub(oldest)), both, words))}
}

// andList gives "el9", "el9 and el10" or "el8, el9 and el10".
func andList(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// ago gives a short age: 40 s, 2 min, 3 h.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%d h", int(d.Hours()))
}
