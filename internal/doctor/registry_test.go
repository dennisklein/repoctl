package doctor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/localcopy"
)

// served.stale reads the heartbeat of each mirror line that this host has
// a local copy of (publish.html#stale-served).
func TestServedStale(t *testing.T) {
	files := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, text)
	}))
	defer srv.Close()
	dir := t.TempDir()
	set, err := config.ParseSet([]byte(`apiVersion: repoctl/v1alpha1
name: slurm-26.05
repo:
  name: slurm
  url: `+srv.URL+`/slurm-26.05
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
	paths := config.NewPaths(func(k string) string {
		if k == "HOME" {
			return dir
		}
		return ""
	})
	now := time.Date(2026, 11, 3, 14, 31, 0, 0, time.UTC)
	env := &Env{Paths: paths, Set: set, Now: func() time.Time { return now }}
	run := func() []Check { return checkServedStale(context.Background(), env) }

	// No local copy: nothing to compare.
	if cs := run(); len(cs) != 1 || cs[0].Status != api.CheckStatusOk || !strings.Contains(cs[0].Text, "no local copy") {
		t.Fatalf("no local copy: %+v", cs)
	}
	for _, tg := range []string{"el9", "el10"} {
		d := localcopy.Dir(paths, set.Name, tg)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := localcopy.WriteSnapshot(d, api.Snapshot{Target: tg, Publish: "p-20261103-1412-5e1", Digest: "sha256:" + strings.Repeat("5", 64), Kind: "publish"}); err != nil {
			t.Fatal(err)
		}
	}
	// No heartbeat: the job does not run for the lines.
	if cs := run(); len(cs) != 2 || cs[0].Status != api.CheckStatusWarning || !strings.Contains(cs[0].Detail, "no heartbeat") {
		t.Fatalf("no heartbeat: %+v", cs)
	}
	files["/slurm-26.05/el9/repoctl-mirror.json"] = `{"set":"slurm-26.05","target":"el9","checked":"2026-11-03T14:29:02Z","result":"same","behindSince":null,"error":null}`
	files["/slurm-26.05/el10/repoctl-mirror.json"] = `{"set":"slurm-26.05","target":"el10","checked":"2026-11-03T14:29:02Z","result":"same","behindSince":null,"error":null}`
	if cs := run(); len(cs) != 1 || cs[0].Status != api.CheckStatusOk || cs[0].Text != "served.stale: the mirror job checked el9 and el10 1 min ago, both same" {
		t.Fatalf("both same: %+v", cs)
	}
	// el9 is behind since 14:15 with a failed step.
	files["/slurm-26.05/el9/repoctl-mirror.json"] = `{"set":"slurm-26.05","target":"el9","checked":"2026-11-03T14:30:02Z","result":"failed","served":"sha256:` + strings.Repeat("c", 64) +
		`","latest":"sha256:` + strings.Repeat("5", 64) + `","behindSince":"2026-11-03T14:15:02Z","error":"gpgv: BAD signature"}`
	cs := run()
	if len(cs) != 1 || cs[0].Status != api.CheckStatusWarning || !strings.HasPrefix(cs[0].Detail, "el9 is behind since ") ||
		!strings.HasSuffix(cs[0].Detail, "Last error of the mirror job: gpgv: BAD signature") || !strings.Contains(cs[0].Fix, "journalctl -u repoctl-mirror.service") {
		t.Fatalf("behind: %+v", cs)
	}
	// Without repo.url, the check does not run.
	set.Repo.URL = ""
	if cs := run(); cs != nil {
		t.Fatalf("no repo.url: %+v", cs)
	}
}
