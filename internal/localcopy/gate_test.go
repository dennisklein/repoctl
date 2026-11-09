package localcopy

import (
	"context"
	"encoding/json"
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

	"github.com/google/go-containerregistry/pkg/registry"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/snapshot"
	"github.com/dennisklein/repoctl/internal/testkit"
)

const gateSet = `apiVersion: repoctl/v1alpha1
name: slurm-26.05
repo:
  name: slurm
  url: %URL%
store:
  oci: %OCI%
targets:
  - id: el9
    mock: rocky+epel-9-x86_64
    plugins: [rpmautospec]
  - id: el10
    mock: rocky+epel-10-x86_64
    plugins: [rpmautospec]
    path: el10-x86_64
packages:
  - id: tiny
    branch: tiny/demo
`

// gateEnv is a set on an in-process registry, a counting proxy in front of
// it, and the paths of a host.
type gateEnv struct {
	set      *config.Set
	paths    config.Paths
	requests atomic.Int64
	ociHost  string
}

func newGateEnv(t *testing.T, repoURL string) *gateEnv {
	t.Helper()
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	e := &gateEnv{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			e.requests.Add(1)
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e.ociHost = strings.TrimPrefix(srv.URL, "http://")
	text := strings.NewReplacer("%URL%", repoURL, "%OCI%", e.ociHost+"/gsi-hpc").Replace(gateSet)
	dir := t.TempDir()
	set, err := config.ParseSet([]byte(text), filepath.Join(dir, "repoctl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	e.set = set
	e.paths = config.NewPaths(func(k string) string {
		switch k {
		case "HOME":
			return dir
		case "XDG_DATA_HOME":
			return filepath.Join(dir, "data")
		}
		return ""
	})
	return e
}

func (e *gateEnv) gate() Gate {
	return Gate{Set: e.set, Paths: e.paths, Getenv: func(string) string { return "" }, User: "d.klein"}
}

// publish pushes a snapshot of target and moves latest to it.
func (e *gateEnv) publish(t *testing.T, target, id, user string) *snapshot.Snapshot {
	t.Helper()
	ctx := context.Background()
	s, err := snapshot.Open(e.set.OCIPackage(target), snapshot.Options{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "README"), []byte(id), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := snapshot.Scan(tree)
	if err != nil {
		t.Fatal(err)
	}
	meta := snapshot.Meta{Set: e.set.Name, Target: target, Publish: id, Kind: snapshot.KindPublish, User: user, Host: "hpc-a1", Created: time.Now()}
	d, _, err := s.Push(ctx, files, meta, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tag(ctx, d, snapshot.TagLatest); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Fetch(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// cache writes snapshot.json of the local copy of target.
func (e *gateEnv) cache(t *testing.T, target string, snap *snapshot.Snapshot) {
	t.Helper()
	dir := Dir(e.paths, e.set.Name, target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteSnapshot(dir, snap.API(e.set.OCIPackage(target), "slurm", nil)); err != nil {
		t.Fatal(err)
	}
}

// The check of plan and build: same with one request per target, first,
// behind (stop, exit 13), unpushed and missing (warnings), and an
// unreachable registry (warning).
func TestGatePlan(t *testing.T) {
	ctx := context.Background()
	e := newGateEnv(t, "")
	g := e.gate()
	if !g.Enabled() {
		t.Fatal("a set with store.oci has the check")
	}

	// No snapshot anywhere: a first publish continues.
	fs := g.Check(ctx, []string{"el9", "el10"})
	if fs[0].Result != FreshFirst || fs[1].Result != FreshFirst {
		t.Fatalf("first: %+v", fs)
	}
	if ws, err := PlanOutcome(fs); err != nil || len(ws) != 0 {
		t.Fatalf("first: %v %v", ws, err)
	}

	// The local copy holds latest: same, with one read per target.
	s9 := e.publish(t, "el9", "p-20261103-1412-5e1", "d.klein")
	s10 := e.publish(t, "el10", "p-20261103-1412-5e1", "d.klein")
	e.cache(t, "el9", s9)
	e.cache(t, "el10", s10)
	e.requests.Store(0)
	fs = g.Check(ctx, []string{"el9", "el10"})
	if fs[0].Result != FreshSame || fs[1].Result != FreshSame || fs[0].LatestID() != "p-20261103-1412-5e1" {
		t.Fatalf("same: %+v", fs)
	}
	if n := e.requests.Load(); n != 2 {
		t.Errorf("same: %d manifest requests, want one per target", n)
	}

	// A pushable tree whose base is latest: unpushed warns.
	g.Pushable = func(target string) []Pushable {
		if target == "el9" {
			return []Pushable{{Publish: "p-20261104-0900-aaa", Base: s9.Digest(), New: "sha256:" + strings.Repeat("1", 64)}}
		}
		return nil
	}
	ws, err := PlanOutcome(g.Check(ctx, []string{"el9"}))
	if err != nil || len(ws) != 1 || ws[0].Code != api.PStateUnpushed {
		t.Fatalf("unpushed: %v %v", ws, err)
	}
	g.Pushable = nil

	// Another admin publishes el9: behind stops with state.behind.
	e.publish(t, "el9", "p-20261105-1000-b2c", "a.admin")
	fs = g.Check(ctx, []string{"el9", "el10"})
	_, err = PlanOutcome(fs)
	if ae, ok := api.AsError(err); !ok || ae.Code != api.PStateBehind || ae.ExitCode != api.ExitStateConflict || !strings.Contains(ae.Message, "p-20261105-1000-b2c") {
		t.Fatalf("behind: %v", err)
	}
	if pe, ok := api.AsError(fs[0].PlanErr()); !ok || pe.Hints[0].Run != "repoctl repo pull --target el9" {
		t.Errorf("behind: hint %+v", fs[0].PlanErr())
	}

	// Both behind: one error, one pull.
	e.publish(t, "el10", "p-20261105-1000-b2c", "a.admin")
	_, err = PlanOutcome(g.Check(ctx, []string{"el9", "el10"}))
	if ae, ok := api.AsError(err); !ok || !strings.Contains(ae.Message, "el9, el10") || ae.Hints[0].Run != "repoctl repo pull" {
		t.Fatalf("both behind: %v", err)
	}

	// The local copy without latest in the registry: missing warns.
	g.Open = func(target string) (*snapshot.Store, error) {
		return snapshot.Open(e.ociHost+"/gsi-hpc/other-"+target, snapshot.Options{Getenv: func(string) string { return "" }})
	}
	ws, err = PlanOutcome(g.Check(ctx, []string{"el9"}))
	if err != nil || len(ws) != 1 || ws[0].Code != api.PSnapshotMissing {
		t.Fatalf("missing: %v %v", ws, err)
	}

	// A registry that does not answer: a warning, plan uses the local copy.
	dead := httptest.NewServer(http.NotFoundHandler())
	host := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	g.Open = func(target string) (*snapshot.Store, error) {
		return snapshot.Open(host+"/gsi-hpc/slurm-26.05-"+target, snapshot.Options{Getenv: func(string) string { return "" }})
	}
	fs = g.Check(ctx, []string{"el9"})
	ws, err = PlanOutcome(fs)
	if err != nil || len(ws) != 1 || ws[0].Code != api.PRegistryUnreachable || !strings.Contains(ws[0].Message, "repoctl uses the local copy") {
		t.Fatalf("unreachable: %v %v", ws, err)
	}
	if fs[0].StatusLocal() != "same" {
		t.Errorf("unreachable: local %s", fs[0].StatusLocal())
	}

	// Without store.oci there is nothing to check.
	g.Set = &config.Set{Name: "x"}
	if g.Enabled() || g.Check(ctx, []string{"el9"}) != nil {
		t.Error("a set without store.oci has no check")
	}
}

// The served tree and the heartbeat of its mirror line, and the two rules
// of served.stale.
func TestServed(t *testing.T) {
	now := time.Date(2026, 11, 3, 14, 31, 0, 0, time.UTC)
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
	e := newGateEnv(t, srv.URL+"/slurm-26.05/")
	if u := ServedURL(e.set, "el10"); u != srv.URL+"/slurm-26.05/el10-x86_64" {
		t.Fatalf("served URL %s", u)
	}
	served := "sha256:" + strings.Repeat("c", 64)
	latest := "sha256:" + strings.Repeat("5", 64)
	put := func(path string, v any) {
		b, _ := json.Marshal(v)
		files["/slurm-26.05/"+path] = string(b)
	}
	put("el9/"+ServedFile, ServedSnapshot{Set: "slurm-26.05", Target: "el9", Digest: served, Publish: "p-20260912-1403-c21", Pulled: "2026-09-12T14:05:02Z"})
	since, failed := "2026-11-03T14:15:02Z", "gpgv: BAD signature"
	put("el9/"+MirrorFile, Heartbeat{Set: "slurm-26.05", Target: "el9", Checked: "2026-11-03T14:25:02Z", Result: "failed", Served: served, Latest: latest, BehindSince: &since, Error: &failed})

	ss := ReadServed(context.Background(), nil, e.set, []string{"el9", "el10"})
	el9, el10 := ss[0], ss[1]
	if el9.Err != nil || el9.Snapshot == nil || el9.Mirror == nil || el10.Snapshot != nil || el10.Mirror != nil || el10.Err != nil {
		t.Fatalf("read: %+v %+v", el9, el10)
	}
	if ServedState(&el9, latest) != "behind" || ServedState(&el9, served) != "same" || ServedState(&el10, latest) != "unknown" || ServedState(&el9, "") != "unknown" {
		t.Error("ServedState")
	}
	clock := func(t time.Time) string { return t.UTC().Format("15:04") }
	w := el9.Mirror.StaleWarning("el9", now, clock)
	if w == nil || w.Code != api.PServedStale || w.Message != "el9 is behind since 14:15. Last error of the mirror job: gpgv: BAD signature" ||
		w.Hint != "read the journal of the mirror job on the HTTP server. run: journalctl -u repoctl-mirror.service" {
		t.Fatalf("behind 15 min: %+v", w)
	}
	m := el9.Mirror.API(now)
	if m == nil || !m.Stale || m.Checked != "2026-11-03T14:25:02.000Z" || *m.BehindSince != "2026-11-03T14:15:02.000Z" {
		t.Fatalf("API: %+v", m)
	}
	b, _ := json.Marshal(api.StatusSnapshot{Target: "el9", Local: "same", Served: "behind", Mirror: m})
	if err := testkit.Validate("Status/properties/snapshots/items", b); err != nil {
		t.Fatalf("the mirror object does not agree with the contract: %v\n%s", err, b)
	}

	// Behind for less than 15 minutes: not stale.
	if el9.Mirror.Stale(time.Date(2026, 11, 3, 14, 28, 0, 0, time.UTC)) {
		t.Error("behind 13 min is not stale")
	}
	// The job does not run: checked is old, the tree is the same.
	old := Heartbeat{Checked: "2026-11-03T14:00:00Z", Result: "same"}
	if w := old.StaleWarning("el10", now, clock); w == nil || w.Message != "the mirror job has not checked el10 since 14:00." {
		t.Fatalf("old heartbeat: %+v", w)
	}
	fresh := Heartbeat{Checked: "2026-11-03T14:28:00Z", Result: "same"}
	if fresh.StaleWarning("el10", now, clock) != nil {
		t.Error("a fresh heartbeat is not stale")
	}
	if (&Heartbeat{Checked: "yesterday", Result: "same"}).API(now) != nil || (&Heartbeat{Checked: "2026-11-03T14:28:00Z", Result: "odd"}).API(now) != nil {
		t.Error("a heartbeat that is not valid has no mirror object")
	}

	// A server that does not answer.
	srv.Close()
	if s := ReadServed(context.Background(), nil, e.set, []string{"el9"}); s[0].Err == nil {
		t.Error("a closed server gives an error")
	}
}
