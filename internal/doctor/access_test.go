package doctor

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ggcr "github.com/google/go-containerregistry/pkg/registry"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/snapshot"
)

func checkByName(cs []Check, name string) *Check {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

// The registry checks login, scope, write and public on an in-process
// registry and a fake GitHub API (publish.html#registry).
func TestRegistryAccess(t *testing.T) {
	reg := ggcr.New(ggcr.Logger(log.New(io.Discard, "", 0)))
	var private atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if private.Load() && r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`, http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	defer srv.Close()
	scopes := "write:packages, read:packages"
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token ghp_test" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-OAuth-Scopes", scopes)
		w.Header().Set("GitHub-Authentication-Token-Expiration", "2027-10-01 12:00:00 UTC")
		_, _ = io.WriteString(w, `{"login":"dklein"}`)
	}))
	defer gh.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	dir := t.TempDir()
	set, err := config.ParseSet([]byte(`apiVersion: repoctl/v1alpha1
name: slurm-26.05
repo:
  name: slurm
store:
  oci: `+host+`/gsi-hpc
targets:
  - id: el9
    mock: rocky+epel-9-x86_64
    plugins: [rpmautospec]
  - id: el10
    mock: rocky+epel-10-x86_64
    plugins: [rpmautospec]
packages:
  - id: tiny
    branch: tiny/demo
`), filepath.Join(dir, "repoctl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"HOME": dir, "REGISTRY_AUTH_FILE": filepath.Join(dir, "auth.json"), "REPOCTL_TEST_GITHUB_API": gh.URL}
	getenv := func(k string) string { return vars[k] }
	env := &Env{Paths: config.NewPaths(getenv), Getenv: getenv, Set: set, Now: time.Now}
	run := func() []Check { return checkRegistryAccess(context.Background(), env) }

	// No login: a warning, no write check, no pass for a package that is
	// not pushed.
	cs := run()
	if c := checkByName(cs, "registry.login"); c == nil || c.Status != api.CheckStatusWarning {
		t.Fatalf("no login: %+v", cs)
	}
	if checkByName(cs, "registry.write") != nil || checkByName(cs, "registry.public") != nil || checkByName(cs, "registry.scope") != nil {
		t.Fatalf("checks without a login or a package: %+v", cs)
	}

	auth := base64.StdEncoding.EncodeToString([]byte("dklein:ghp_test"))
	if err := os.WriteFile(vars["REGISTRY_AUTH_FILE"], []byte(fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, auth)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Push latest of el9.
	s, err := snapshot.Open(set.OCIPackage("el9"), snapshot.Options{Getenv: getenv})
	if err != nil {
		t.Fatal(err)
	}
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, _ := snapshot.Scan(tree)
	d, _, err := s.Push(context.Background(), files, snapshot.Meta{Set: set.Name, Target: "el9", Publish: "p-20261103-1412-5e1", Kind: "publish"}, "p-20261103-1412-5e1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tag(context.Background(), d, snapshot.TagLatest); err != nil {
		t.Fatal(err)
	}

	cs = run()
	if c := checkByName(cs, "registry.login"); c == nil || c.Status != api.CheckStatusOk || !strings.Contains(c.Text, host+" as dklein") {
		t.Fatalf("login: %+v", cs)
	}
	if c := checkByName(cs, "registry.scope"); c == nil || c.Status != api.CheckStatusWarning || !strings.Contains(c.Detail, "no scope delete:packages") {
		t.Fatalf("scope: %+v", c)
	}
	if c := checkByName(cs, "registry.write"); c == nil || c.Status != api.CheckStatusOk || !strings.Contains(c.Text, "slurm-26.05-el9, slurm-26.05-el10") {
		t.Fatalf("write: %+v", c)
	}
	if c := checkByName(cs, "registry.public"); c == nil || c.Status != api.CheckStatusOk || c.Text != "registry.public: slurm-26.05-el9 is public; slurm-26.05-el10 not pushed yet" {
		t.Fatalf("public: %+v", c)
	}

	// A token with the repo scope.
	scopes = "repo, write:packages, delete:packages"
	if c := checkByName(run(), "registry.scope"); c == nil || c.Status != api.CheckStatusWarning || !strings.Contains(c.Detail, "repo beyond the packages") {
		t.Fatalf("scope repo: %+v", c)
	}
	scopes = "write:packages, read:packages, delete:packages"
	if c := checkByName(run(), "registry.scope"); c == nil || c.Status != api.CheckStatusOk {
		t.Fatalf("scope ok: %+v", c)
	}

	// A private package: the owner reads latest, an anonymous client does not.
	private.Store(true)
	if c := checkByName(run(), "registry.public"); c == nil || c.Status != api.CheckStatusWarning || !strings.Contains(c.Detail, "slurm-26.05-el9 is private") {
		t.Fatalf("private: %+v", c)
	}
}
