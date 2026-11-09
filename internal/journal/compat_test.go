package journal_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dennisklein/repoctl/internal/advisory"
	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/journal"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// The journal of a check run that advisory.FileJournal writes folds and
// lists like a journal of this package (the JOURNAL HOOK of the check).
func TestReadCheckJournal(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOME": dir, "XDG_STATE_HOME": filepath.Join(dir, "state")}
	p := config.NewPaths(func(k string) string { return env[k] })
	now := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	j := &advisory.FileJournal{Paths: p, Origin: advisory.Origin{Via: "cli", User: "tester", Host: "host", PID: os.Getpid()},
		Repoctl: "test", Now: func() time.Time { return now }}
	run := "r-20261102-0600-c1k"
	if err := j.Started(run, "demo", now); err != nil {
		t.Fatal(err)
	}
	if err := j.Advisory(api.Advisory{ID: "closure.broken/pmix@el9", State: "open", Level: "required", FactsDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Gone("mock.drift/el9"); err != nil {
		t.Fatal(err)
	}
	if err := j.Finished(api.RunStateOk, 3*time.Second, 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.Journal("demo", run))
	if err != nil {
		t.Fatal(err)
	}
	testkit.ValidateJournal(t, data)

	es, err := journal.List(journal.FoldOptions{Paths: &p, Host: "host"}, "demo", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 {
		t.Fatalf("%d runs", len(es))
	}
	r := es[0].Run()
	if r.ID != run || r.Kind != journal.KindCheck || r.State != api.RunStateOk || r.Origin.User != "tester" {
		t.Fatalf("run %+v", r)
	}
	if e, err := journal.Find(journal.FoldOptions{Paths: &p, Host: "host"}, "demo", "latest", journal.KindCheck); err != nil || e.ID != run {
		t.Fatalf("find latest check: %v %v", e.ID, err)
	}
}
