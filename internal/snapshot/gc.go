package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"oras.land/oras-go/v2/registry/remote/credentials"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// Retention are the rules of repo gc (publish.html#gc).
type Retention struct {
	// Keep is the count of the newest snapshots of the chain that stay.
	Keep int
	// KeepDays keeps the snapshots younger than it.
	KeepDays time.Duration
	// KeepRemoved keeps, for each entry of removed[], the newest snapshot
	// that still holds it, until this long after its removal.
	KeepRemoved time.Duration
}

// DefaultRetention is retention.snapshots without values in repoctl.yaml.
var DefaultRetention = Retention{Keep: 10, KeepDays: 90 * 24 * time.Hour, KeepRemoved: 365 * 24 * time.Hour}

// RemovedEntry is one entry of removed[] that gc keeps a snapshot for:
// the sha256 of each file of the NVR and the time of its removal.
type RemovedEntry struct {
	NVR     string
	SHA256  []string
	Removed time.Time
}

// GCDecision is what gc does with one snapshot.
type GCDecision struct {
	Snapshot *Snapshot
	Delete   bool
	// Why names the rule that keeps the snapshot.
	Why string
}

// GCPlan gives the decision for each snapshot of a target: chain is the
// chain of latest, newest first; others are the snapshots outside the
// chain (diverged publishes). It never deletes latest.
func GCPlan(chain, others []*Snapshot, removed []RemovedEntry, r Retention, now time.Time) []GCDecision {
	var out []GCDecision
	keepRemoved := map[string]string{} // digest -> nvr
	for _, e := range removed {
		if r.KeepRemoved > 0 && now.Sub(e.Removed) >= r.KeepRemoved {
			continue
		}
		// The newest snapshot of the chain that still holds a file of the
		// entry.
		want := map[string]bool{}
		for _, s := range e.SHA256 {
			want[s] = true
		}
	chain:
		for _, s := range chain {
			for _, f := range s.Files {
				if want[f.SHA256()] {
					keepRemoved[s.Digest()] = e.NVR
					break chain
				}
			}
		}
	}
	all := append(append([]*Snapshot{}, chain...), others...)
	for i, s := range all {
		d := GCDecision{Snapshot: s}
		onChain := i < len(chain)
		switch {
		case i == 0 && onChain:
			d.Why = "latest"
		case onChain && r.Keep > 0 && i < r.Keep:
			d.Why = fmt.Sprintf("one of the newest %d", r.Keep)
		case r.KeepDays > 0 && !s.Meta.Created.IsZero() && now.Sub(s.Meta.Created) < r.KeepDays:
			d.Why = "younger than " + config.FormatDuration(r.KeepDays)
		case keepRemoved[s.Digest()] != "":
			d.Why = "holds " + keepRemoved[s.Digest()] + " of removed[]"
		default:
			d.Delete = true
		}
		out = append(out, d)
	}
	// Oldest first, so the chain from latest has no gap.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Snapshot.Meta.Created.Before(out[j].Snapshot.Meta.Created) })
	return out
}

// Deleter deletes one snapshot (one package version) of a store.
type Deleter interface {
	Delete(ctx context.Context, s *Store, snap *Snapshot) error
}

// OCIDeleter deletes with the OCI distribution API.
type OCIDeleter struct{}

// Delete deletes the manifest of the snapshot.
func (OCIDeleter) Delete(ctx context.Context, s *Store, snap *Snapshot) error {
	return s.Delete(ctx, snap)
}

// GitHubDeleter deletes a package version of GHCR with the GitHub REST
// API: GHCR has no delete in its OCI API (publish.html#gc). It needs the
// token scope delete:packages and the role admin on the package.
type GitHubDeleter struct {
	// API is the base URL, default https://api.github.com.
	API    string
	Getenv func(string) string
	Client *http.Client
}

// DeleterFor gives the deleter of a store: the GitHub API for ghcr.io,
// else the OCI API.
func DeleterFor(s *Store, getenv func(string) string) Deleter {
	if s.Registry() == "ghcr.io" {
		return &GitHubDeleter{Getenv: getenv}
	}
	return OCIDeleter{}
}

type ghVersion struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Delete deletes the package version whose name is the digest of snap.
func (g *GitHubDeleter) Delete(ctx context.Context, s *Store, snap *Snapshot) error {
	cs, err := CredentialStore(g.Getenv)
	if err != nil {
		return err
	}
	cred, err := cs.Get(ctx, credentials.ServerAddressFromRegistry(s.Registry()))
	if err != nil || (cred.Password == "" && cred.AccessToken == "") {
		return api.Errorf(api.PRegistryDenied, "no credentials for %s: gc needs a token with the scope delete:packages", s.Registry()).
			WithHint("log in with the token", LoginCommand(s.Registry()))
	}
	token := cred.Password
	if token == "" {
		token = cred.AccessToken
	}
	base := g.API
	if base == "" {
		base = "https://api.github.com"
	}
	repo := s.repo.Reference.Repository // ORG/REPO/SET-TARGET
	owner, name, _ := strings.Cut(repo, "/")
	pkg := url.PathEscape(name)
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	do := func(method, u string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		return client.Do(req)
	}
	for _, scope := range []string{"orgs/" + owner, "users/" + owner} {
		for page := 1; page < 100; page++ {
			u := fmt.Sprintf("%s/%s/packages/container/%s/versions?per_page=100&page=%d", base, scope, pkg, page)
			resp, err := do(http.MethodGet, u)
			if err != nil {
				return api.Errorf(api.PGCFailed, "the GitHub API does not answer: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				break
			}
			if err := ghError(resp.StatusCode, body, s.Ref); err != nil {
				return err
			}
			var vs []ghVersion
			if err := json.Unmarshal(body, &vs); err != nil {
				return api.Errorf(api.PGCFailed, "the GitHub API gives no version list: %v", err)
			}
			for _, v := range vs {
				if v.Name != snap.Digest() {
					continue
				}
				resp, err := do(http.MethodDelete, fmt.Sprintf("%s/%s/packages/container/%s/versions/%d", base, scope, pkg, v.ID))
				if err != nil {
					return api.Errorf(api.PGCFailed, "the GitHub API does not answer: %v", err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				return ghError(resp.StatusCode, body, s.Ref)
			}
			if len(vs) < 100 {
				break
			}
		}
	}
	return api.Errorf(api.PGCFailed, "%s: GitHub has no package version %s (%s)", s.Ref, snap.Digest(), snap.Meta.Publish)
}

func ghError(code int, body []byte, ref string) error {
	switch {
	case code < 300:
		return nil
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return api.Errorf(api.PRegistryDenied, "%s: the GitHub API denies the delete (HTTP %d): the token needs the scope delete:packages and the role admin on the package", ref, code).
			WithHint("add the scope delete:packages to the token on https://github.com/settings/tokens", "")
	}
	return api.Errorf(api.PGCFailed, "%s: the GitHub API gives HTTP %d: %s", ref, code, strings.TrimSpace(string(body)))
}
