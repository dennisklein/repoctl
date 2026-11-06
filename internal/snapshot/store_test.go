package snapshot

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// testRegistry starts an in-process OCI registry and gives its host.
func testRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, text := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func openStore(t *testing.T, host string) *Store {
	t.Helper()
	s, err := Open(host+"/gsi-hpc/slurm-repo/slurm-26.05-el9", Options{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func push(t *testing.T, s *Store, tree string, meta Meta) *Snapshot {
	t.Helper()
	ctx := context.Background()
	files, err := Scan(tree)
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := s.Push(ctx, files, meta, meta.Publish)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Fetch(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestPushPullTagChain(t *testing.T) {
	ctx := context.Background()
	host := testRegistry(t)
	s := openStore(t, host)
	if _, ok, err := s.Latest(ctx); err != nil || ok {
		t.Fatalf("latest of an empty package: ok %v err %v", ok, err)
	}
	if tags, err := s.Tags(ctx); err != nil || len(tags) != 0 {
		t.Fatalf("tags of an empty package: %v %v", tags, err)
	}

	root := t.TempDir()
	t1 := filepath.Join(root, "t1")
	writeTree(t, t1, map[string]string{
		"slurm/Packages/u/ucx-1.22.0-2.el9.x86_64.rpm":     "ucx 1.22.0",
		"slurm/repodata/repomd.xml":                        "<repomd/>",
		"slurm-source/Packages/u/ucx-1.22.0-2.el9.src.rpm": "src",
		"RPM-GPG-KEY-slurm":                                "key",
		"snapshot.json":                                    "not part of the tree",
	})
	created := time.Date(2026, 9, 12, 14, 5, 12, 0, time.UTC)
	m1 := Meta{Set: "slurm-26.05", Target: "el9", Publish: "p-20260912-1403-c21", Kind: KindPublish, User: "d.klein", Host: "hpc-a1",
		Signer: "4E7A0C93B85D21F69A0E3C47D1B266F041D80B6E", Created: created, Repoctl: "0.4.0", SetCommit: strings.Repeat("a", 40)}
	s1 := push(t, s, t1, m1)
	if len(s1.Files) != 4 {
		t.Fatalf("files: %+v", s1.Files)
	}
	if s1.Meta.Publish != m1.Publish || !s1.Meta.Created.Equal(created) || s1.Meta.Signer != m1.Signer {
		t.Fatalf("meta: %+v", s1.Meta)
	}
	if err := s.Tag(ctx, s1.Desc, TagLatest); err != nil {
		t.Fatal(err)
	}

	// The second snapshot shares three blobs and links to the first.
	t2 := filepath.Join(root, "t2")
	writeTree(t, t2, map[string]string{
		"slurm/Packages/u/ucx-1.22.0-2.el9.x86_64.rpm":     "ucx 1.22.0",
		"slurm/Packages/u/ucx-1.22.1-1.el9.x86_64.rpm":     "ucx 1.22.1",
		"slurm/repodata/repomd.xml":                        "<repomd revision=2/>",
		"slurm-source/Packages/u/ucx-1.22.0-2.el9.src.rpm": "src",
		"RPM-GPG-KEY-slurm":                                "key",
	})
	m2 := m1
	m2.Publish, m2.Parent, m2.Created = "p-20261103-1412-5e1", s1.Digest(), created.Add(time.Hour)
	m2.Held = []api.SnapshotHeldItem{{Node: "slurm@el9", NVR: "slurm-26.05.4-1.el9", Reason: "urgent fix of ucx"}}
	files, _ := Scan(t2)
	d2, st, err := s.Push(ctx, files, m2, m2.Publish)
	if err != nil {
		t.Fatal(err)
	}
	if st.New != 2 || st.Known != 3 {
		t.Fatalf("push stats: %+v", st)
	}
	if err := s.Tag(ctx, d2, TagLatest); err != nil {
		t.Fatal(err)
	}
	latest, ok, err := s.Latest(ctx)
	if err != nil || !ok || latest.Digest != d2.Digest {
		t.Fatalf("latest: %v %v %v", latest.Digest, ok, err)
	}
	chain, err := s.Chain(ctx, latest, 0)
	if err != nil || len(chain) != 2 || chain[1].Digest() != s1.Digest() {
		t.Fatalf("chain: %v %v", chain, err)
	}
	if chain[0].Meta.Held[0] != m2.Held[0] {
		t.Fatalf("held: %+v", chain[0].Meta.Held)
	}
	if n := chain[0].Added(chain[1], "slurm"); n != 1 {
		t.Fatalf("added: %d", n)
	}
	tags, err := s.Tags(ctx)
	if err != nil || strings.Join(tags, " ") != "latest p-20260912-1403-c21 p-20261103-1412-5e1" {
		t.Fatalf("tags: %v %v", tags, err)
	}
	if _, err := s.Resolve(ctx, "p-20990101-0000-000"); err == nil {
		t.Fatal("resolve of an unknown id")
	} else if e, ok := api.AsError(err); !ok || e.Code != api.PSnapshotNotFound {
		t.Fatalf("unknown id: %v", err)
	}
	byDigest, err := s.Get(ctx, s1.Digest())
	if err != nil || byDigest.Meta.Publish != m1.Publish {
		t.Fatalf("get by digest: %v", err)
	}

	// Pull: one file from the old tree, the others from the registry.
	pulled := filepath.Join(root, "pulled")
	have := func(d digest.Digest) string {
		if f, ok := s1.File("RPM-GPG-KEY-slurm"); ok && f.Digest == d {
			return filepath.Join(t1, "RPM-GPG-KEY-slurm")
		}
		return ""
	}
	pst, err := s.Download(ctx, chain[0], pulled, have)
	if err != nil {
		t.Fatal(err)
	}
	if pst.Known != 1 || pst.New != 4 {
		t.Fatalf("pull stats: %+v", pst)
	}
	got, err := Scan(pulled)
	if err != nil || len(got) != len(files) {
		t.Fatalf("pulled tree: %v %v", got, err)
	}
	for i := range got {
		if got[i].Path != files[i].Path || got[i].Digest != files[i].Digest {
			t.Fatalf("pulled %s %s, want %s %s", got[i].Path, got[i].Digest, files[i].Path, files[i].Digest)
		}
	}
	b, err := s.FetchBlob(ctx, files[0])
	if err != nil || len(b) == 0 {
		t.Fatalf("blob: %v", err)
	}

	// The contract item.
	item := chain[0].API(s.Ref, "slurm", func(string) string { return "d.klein" })
	item.Tags = []string{"latest", m2.Publish}
	if err := testkit.Validate("Snapshot", testkit.MarshalItem(t, item)); err != nil {
		t.Fatal(err)
	}
	// Two binary RPMs and the SRPM of slurm-source: the RPMs of all
	// repositories count.
	if item.RPMs != 3 || item.Signer.KeyID != "d.klein" {
		t.Fatalf("item: %+v", item)
	}
}

func TestDeleteAndGCPlan(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, testRegistry(t))
	root := t.TempDir()
	now := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	var snaps []*Snapshot
	parent := ""
	for i := 0; i < 4; i++ {
		tree := filepath.Join(root, string(rune('a'+i)))
		writeTree(t, tree, map[string]string{
			"slurm/Packages/a/a-1-" + string(rune('1'+i)) + ".el9.x86_64.rpm": "rpm " + string(rune('1'+i)),
			"slurm/repodata/repomd.xml":                                       "<repomd " + string(rune('1'+i)) + "/>",
		})
		m := Meta{Set: "slurm-26.05", Target: "el9", Publish: "p-2026010" + string(rune('1'+i)) + "-1000-aaa", Kind: KindPublish,
			Parent: parent, Created: now.Add(-time.Duration(400-i*100) * 24 * time.Hour)}
		snap := push(t, s, tree, m)
		parent = snap.Digest()
		snaps = append([]*Snapshot{snap}, snaps...)
	}
	if err := s.Tag(ctx, snaps[0].Desc, TagLatest); err != nil {
		t.Fatal(err)
	}
	// The oldest snapshot holds a pruned file that clients can still have.
	removed := []RemovedEntry{{NVR: "a-1-1.el9", SHA256: []string{snaps[3].Files[0].SHA256()}, Removed: now.AddDate(0, -1, 0)}}
	plan := GCPlan(snaps, nil, removed, Retention{Keep: 2, KeepDays: 90 * 24 * time.Hour, KeepRemoved: 365 * 24 * time.Hour}, now)
	var del []string
	for _, d := range plan {
		if d.Delete {
			del = append(del, d.Snapshot.Meta.Publish)
		}
	}
	// a-1-1 is only in snaps[3]; snaps[2] is deleted.
	if strings.Join(del, " ") != snaps[2].Meta.Publish {
		t.Fatalf("delete %v; plan %+v", del, plan)
	}
	if err := (OCIDeleter{}).Delete(ctx, s, snaps[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fetch(ctx, snaps[2].Desc); err == nil {
		t.Fatal("deleted manifest still there")
	}
	// The chain stops at the gap.
	chain, err := s.Chain(ctx, snaps[0].Desc, 0)
	if err != nil || len(chain) != 2 {
		t.Fatalf("chain after gc: %d %v", len(chain), err)
	}
}

func TestTitlesAndIDs(t *testing.T) {
	for _, ok := range []string{"slurm/Packages/u/ucx-1.22.1-1.el9.x86_64.rpm", "RPM-GPG-KEY-slurm", "slurm/repodata/abc-repoctl.json.gz"} {
		if err := ValidTitle(ok); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{"", "/abs", "a/../b", "a/./b", "a b", "x~1.rpm"} {
		if ValidTitle(bad) == nil {
			t.Fatalf("title %q accepted", bad)
		}
	}
	id := NewID(time.Date(2026, 11, 3, 14, 12, 0, 0, time.UTC))
	if !strings.HasPrefix(id, "p-20261103-1412-") || !IsPublishID(id) {
		t.Fatal(id)
	}
	if !IsPublishID("p-import-20260828") || IsPublishID("r-20261102-1750-a3f") {
		t.Fatal("id forms")
	}
	if !IDTime("p-import-20260828").Equal(time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)) {
		t.Fatal(IDTime("p-import-20260828"))
	}
	if !PlainHTTP("127.0.0.1:5000", nil) || PlainHTTP("ghcr.io", nil) {
		t.Fatal("plain http")
	}
	files := AuthFiles(func(k string) string {
		return map[string]string{"HOME": "/h", "XDG_RUNTIME_DIR": "/run/user/1"}[k]
	})
	if files[0] != "/run/user/1/containers/auth.json" || files[len(files)-1] != "/h/.docker/config.json" {
		t.Fatal(files)
	}
}
