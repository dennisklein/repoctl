package localcopy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/snapshot"
)

// The probes of registry access for repoctl doctor (group registry) and the
// setup state of status (publish.html#registry, lifecycle.html#setup).

// EnvTestGitHubAPI is the base URL of the GitHub API in tests; with it,
// any registry counts as GHCR for the token scopes.
const EnvTestGitHubAPI = "REPOCTL_TEST_GITHUB_API"

// AccessTimeout bounds one probe of the registry.
const AccessTimeout = 15 * time.Second

// Login is the credential of the user for a registry host.
type Login struct {
	Host     string
	Username string
	// Secret is the password or the token.
	Secret string
}

// FindLogin looks up the credential of a registry host in the container
// credential files of the user (snapshot.AuthFiles). It gives nil when
// there is none.
func FindLogin(ctx context.Context, host string, getenv func(string) string) (*Login, error) {
	cs, err := snapshot.CredentialStore(getenv)
	if err != nil {
		return nil, err
	}
	c, err := cs.Get(ctx, host)
	if err != nil {
		return nil, err
	}
	secret := c.Password
	if secret == "" {
		secret = c.RefreshToken
	}
	if secret == "" && c.AccessToken == "" {
		return nil, nil
	}
	if secret == "" {
		secret = c.AccessToken
	}
	return &Login{Host: host, Username: c.Username, Secret: secret}, nil
}

// RegistryHost gives the registry host of store.oci, or "".
func RegistryHost(set *config.Set) string {
	if set == nil || set.Store.OCI == "" {
		return ""
	}
	ref, err := registry.ParseReference(set.Store.OCI)
	if err != nil {
		return ""
	}
	return ref.Registry
}

// repository gives an oras client of an OCI package with the credential of
// the user, or anonymous.
func repository(pkg string, getenv func(string) string, anonymous bool) (*remote.Repository, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	repo, err := remote.NewRepository(pkg)
	if err != nil {
		return nil, err
	}
	client := &auth.Client{Client: &http.Client{Timeout: AccessTimeout}, Cache: auth.NewCache(), Header: http.Header{}}
	client.SetUserAgent("repoctl/" + config.Version)
	if !anonymous {
		cs, err := snapshot.CredentialStore(getenv)
		if err != nil {
			return nil, err
		}
		client.Credential = credentials.Credential(cs)
	}
	repo.Client = client
	repo.PlainHTTP = snapshot.PlainHTTP(repo.Reference.Registry, getenv)
	return repo, nil
}

// CanWrite probes the write access to an OCI package: it starts a blob
// upload with the push scope and cancels it. Nothing is written.
func CanWrite(ctx context.Context, pkg string, getenv func(string) string) error {
	repo, err := repository(pkg, getenv, false)
	if err != nil {
		return err
	}
	ctx = auth.AppendRepositoryScope(ctx, repo.Reference, auth.ActionPull, auth.ActionPush)
	scheme := "https"
	if repo.PlainHTTP {
		scheme = "http"
	}
	u := fmt.Sprintf("%s://%s/v2/%s/blobs/uploads/", scheme, repo.Reference.Host(), repo.Reference.Repository)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := repo.Client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("the registry answers %s to the start of an upload", resp.Status)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if !strings.HasPrefix(loc, "http") {
			loc = scheme + "://" + repo.Reference.Host() + loc
		}
		if del, err := http.NewRequestWithContext(ctx, http.MethodDelete, loc, nil); err == nil {
			if r, err := repo.Client.Do(del); err == nil {
				r.Body.Close()
			}
		}
	}
	return nil
}

// Visibility is what an anonymous client sees of an OCI package.
type Visibility int

// The visibilities of an OCI package.
const (
	// NotPushed: the package has no tag latest, also for its owner.
	NotPushed Visibility = iota
	// Public: an anonymous client reads latest.
	Public
	// Private: the owner reads latest, an anonymous client does not.
	Private
)

// PackageVisibility reads latest of an OCI package anonymously and, when
// that fails, with the credential of the user.
func PackageVisibility(ctx context.Context, pkg string, getenv func(string) string) (Visibility, error) {
	anon, err := repository(pkg, getenv, true)
	if err != nil {
		return NotPushed, err
	}
	_, aerr := anon.Resolve(ctx, snapshot.TagLatest)
	if aerr == nil {
		return Public, nil
	}
	if !denied(aerr) && !snapshot.IsNotFound(aerr) {
		return NotPushed, aerr
	}
	owner, err := repository(pkg, getenv, false)
	if err != nil {
		return NotPushed, err
	}
	if _, err := owner.Resolve(ctx, snapshot.TagLatest); err != nil {
		if snapshot.IsNotFound(err) {
			return NotPushed, nil
		}
		return NotPushed, err
	}
	return Private, nil
}

func denied(err error) bool {
	if errors.Is(err, auth.ErrBasicCredentialNotFound) {
		return true
	}
	var er *errcode.ErrorResponse
	if errors.As(err, &er) {
		return er.StatusCode == http.StatusUnauthorized || er.StatusCode == http.StatusForbidden
	}
	return strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403")
}

// TokenInfo is what the GitHub API says about a classic token.
type TokenInfo struct {
	Scopes []string
	// Expires is the expiry of the token; zero without one.
	Expires time.Time
}

// GitHubToken reads the scopes and the expiry of a classic personal access
// token from the headers of GET /user (X-OAuth-Scopes and
// GitHub-Authentication-Token-Expiration).
func GitHubToken(ctx context.Context, getenv func(string) string, token string) (*TokenInfo, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	base := getenv(EnvTestGitHubAPI)
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "repoctl/"+config.Version)
	resp, err := (&http.Client{Timeout: AccessTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the GitHub API answers %s to the token", resp.Status)
	}
	info := &TokenInfo{}
	for _, s := range strings.Split(resp.Header.Get("X-OAuth-Scopes"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			info.Scopes = append(info.Scopes, s)
		}
	}
	sort.Strings(info.Scopes)
	if e := strings.TrimSpace(resp.Header.Get("GitHub-Authentication-Token-Expiration")); e != "" {
		for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", time.RFC3339} {
			if t, err := time.Parse(layout, e); err == nil {
				info.Expires = t
				break
			}
		}
	}
	return info, nil
}

// IsGHCR reports whether the scopes of the token can be read: the registry
// is ghcr.io, or a test sets EnvTestGitHubAPI.
func IsGHCR(host string, getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	return host == "ghcr.io" || getenv(EnvTestGitHubAPI) != ""
}

// SettingsURL gives the settings page of a GHCR package, for example
// https://github.com/orgs/GSI-HPC/packages/container/slurm-repo%2Fslurm-26.05-el10/settings,
// or "" for another registry.
func SettingsURL(pkg string) string {
	ref, err := registry.ParseReference(pkg)
	if err != nil || ref.Registry != "ghcr.io" {
		return ""
	}
	org, rest, ok := strings.Cut(ref.Repository, "/")
	if !ok {
		return ""
	}
	return "https://github.com/orgs/" + org + "/packages/container/" + strings.ReplaceAll(rest, "/", "%2F") + "/settings"
}
