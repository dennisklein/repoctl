package ui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
)

// A stale served tree is an item of Needs you (ui-build.html#needs-you).
func TestNeedsYouServedStale(t *testing.T) {
	src := fullSource(t)
	behind, failed := "2026-11-02T17:30:00.000Z", "gpgv: BAD signature"
	status := api.Status{
		Set:     api.StatusSet{Name: testSet, Config: "repoctl.yaml"},
		Targets: []api.Target{{ID: "el9", Mock: "rocky+epel-9-x86_64"}},
		Snapshots: []api.StatusSnapshot{{Target: "el9", Local: "same", Served: "behind",
			Latest: &api.StatusSnapshotLatest{Publish: "p-20261102-1712-5e1", Digest: "sha256:" + strings.Repeat("ab", 32)},
			Mirror: &api.StatusSnapshotMirror{Checked: "2026-11-02T17:55:00.000Z", Result: "failed", BehindSince: &behind, Error: &failed, Stale: true}}},
	}
	src.res["status"] = fakeEnvelope(t, api.KindStatus, status)
	e := newTestEnv(t, src)
	e.login()
	w := e.do(http.MethodGet, "/s/"+testSet+"/", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("overview: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"served.stale", "el9: the served tree is behind latest since", "Last error of the mirror job: gpgv: BAD signature", "journalctl -u repoctl-mirror.service"} {
		if !strings.Contains(body, want) {
			t.Errorf("Needs you has no %q", want)
		}
	}
}

// The open setup steps are the first items of Needs you
// (ui-build.html#needs-you, lifecycle.html#setup).
func TestNeedsYouSetup(t *testing.T) {
	src := fullSource(t)
	behind, failed := "2026-11-02T17:30:00.000Z", "gpgv: BAD signature"
	status := api.Status{
		Set:     api.StatusSet{Name: testSet, Config: "repoctl.yaml"},
		Targets: []api.Target{{ID: "el9", Mock: "rocky+epel-9-x86_64"}, {ID: "el10", Mock: "rocky+epel-10-x86_64"}},
		Snapshots: []api.StatusSnapshot{{Target: "el9", Local: "same", Served: "behind",
			Latest: &api.StatusSnapshotLatest{Publish: "p-20261102-1712-5e1", Digest: "sha256:" + strings.Repeat("ab", 32)},
			Mirror: &api.StatusSnapshotMirror{Checked: "2026-11-02T17:55:00.000Z", Result: "failed", BehindSince: &behind, Error: &failed, Stale: true}},
			{Target: "el10", Local: "none", Served: "unknown"}},
		Open: []api.StatusOpenItem{
			{Step: "First publish: el10 has no latest", Command: "repoctl publish --target el10"},
			{Step: "Check timer: the timer of repoctl check for " + testSet + " is not active on this host", Command: "repoctl check --install-timer"},
		},
	}
	src.res["status"] = fakeEnvelope(t, api.KindStatus, status)
	e := newTestEnv(t, src)
	e.login()
	w := e.do(http.MethodGet, "/s/"+testSet+"/", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("overview: %d", w.Code)
	}
	body := w.Body.String()
	setup := strings.Index(body, "First publish: el10 has no latest")
	stale := strings.Index(body, "el9: the served tree is behind latest since")
	if setup < 0 || stale < 0 || setup > stale {
		t.Fatalf("setup step at %d, served.stale at %d: the setup steps come first", setup, stale)
	}
	for _, want := range []string{"Run: repoctl publish --target el10", "Check timer: the timer of repoctl check", "Run: repoctl check --install-timer"} {
		if !strings.Contains(body, want) {
			t.Errorf("Needs you has no %q", want)
		}
	}
}
