package srpm

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
)

func sum(s string) string {
	h := sha512.Sum512([]byte(s))
	return hex.EncodeToString(h[:])
}

func code(t *testing.T, err error) api.ProblemCode {
	t.Helper()
	e, ok := api.AsError(err)
	if !ok {
		t.Fatalf("not an api error: %v", err)
	}
	return e.Code
}

func TestParseSourcesFile(t *testing.T) {
	data := "SHA512 (ucx-1.22.0.tar.gz) = " + strings.ToUpper(sum("a")) + "\n" +
		"d41d8cd98f00b204e9800998ecf8427e  old.tar.gz\n" +
		"  SHA512 (extra.patch) = " + sum("b") + "  \n"
	got := ParseSourcesFile([]byte(data))
	if len(got) != 2 || got["ucx-1.22.0.tar.gz"] != sum("a") || got["extra.patch"] != sum("b") {
		t.Fatalf("got %v", got)
	}
}

func TestParseSpecSources(t *testing.T) {
	parsed := `Name: ucx
Version: 1.22.0
Source0: https://github.com/openucx/ucx/releases/download/v1.22.0/ucx-1.22.0.tar.gz
Source1: https://example.org/archive/v1.22.0.tar.gz#/ucx-extra-1.22.0.tar.gz
source2:   ucx.conf
Patch0: fix-build.patch
%description
The UCX library.
%sourcelist
https://example.org/more-1.0.tar.gz
# a comment

%patchlist
second.patch
%prep
`
	got := ParseSpecSources(parsed)
	var files []string
	for _, s := range got {
		files = append(files, s.Tag+"="+s.File)
	}
	want := "Source0=ucx-1.22.0.tar.gz Source1=ucx-extra-1.22.0.tar.gz source2=ucx.conf Patch0=fix-build.patch " +
		"sourcelist=more-1.0.tar.gz patchlist=second.patch"
	if strings.Join(files, " ") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(files, " "), want)
	}
	if got[0].URL == "" || got[2].URL != "" {
		t.Fatalf("URLs: %+v", got)
	}
}

func TestFetcherGet(t *testing.T) {
	good, bad := "the archive", "another archive"
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/lookaside/ucx/ucx.tar.gz/sha512/" + sum(good) + "/ucx.tar.gz", "/up/ucx.tar.gz":
			_, _ = w.Write([]byte(good))
		case "/up/wrong.tar.gz":
			_, _ = w.Write([]byte(bad))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	f := NewFetcher(t.TempDir())
	f.Lookaside = srv.URL + "/lookaside"

	// The lookaside cache first.
	p, err := f.Get(ctx, "ucx.tar.gz", sum(good), "ucx", srv.URL+"/up/ucx.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != good || p != f.CachePath(sum(good), "ucx.tar.gz") {
		t.Fatalf("%s: %q", p, b)
	}
	if len(hits) != 1 || !strings.HasPrefix(hits[0], "/lookaside/") {
		t.Fatalf("hits %v", hits)
	}
	// The cache next time, without a request.
	hits = nil
	if _, err := f.Get(ctx, "ucx.tar.gz", sum(good), "ucx", srv.URL+"/up/ucx.tar.gz"); err != nil || len(hits) != 0 {
		t.Fatalf("cache: %v, hits %v", err, hits)
	}
	// Without upstream.fedora: the URL of the Source line.
	f2 := NewFetcher(t.TempDir())
	f2.Lookaside = srv.URL + "/lookaside"
	hits = nil
	if _, err := f2.Get(ctx, "ucx.tar.gz", sum(good), "", srv.URL+"/up/ucx.tar.gz"); err != nil || len(hits) != 1 || hits[0] != "/up/ucx.tar.gz" {
		t.Fatalf("url: %v, hits %v", err, hits)
	}
	// Another SHA-512: source.mismatch, and nothing in the cache.
	_, err = f2.Get(ctx, "wrong.tar.gz", sum(good), "", srv.URL+"/up/wrong.tar.gz")
	if code(t, err) != api.PSourceMismatch || !strings.Contains(err.Error(), sum(bad)[:12]) {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err := os.Stat(f2.CachePath(sum(good), "wrong.tar.gz")); err == nil {
		t.Fatal("a mismatching download is in the cache")
	}
	// Nowhere: source.unreachable.
	_, err = f2.Get(ctx, "gone.tar.gz", sum(good), "gone", srv.URL+"/up/gone.tar.gz")
	if code(t, err) != api.PSourceUnreachable {
		t.Fatalf("unreachable: %v", err)
	}
	_, err = f2.Get(ctx, "gone.tar.gz", sum(good), "", "")
	if code(t, err) != api.PSourceUnreachable {
		t.Fatalf("no URL: %v", err)
	}
	f2.Offline = true
	if _, err := f2.Get(ctx, "ucx.tar.gz", sum(good), "", ""); err != nil {
		t.Fatalf("offline cache hit: %v", err)
	}
	if _, err := f2.Get(ctx, "x.tar.gz", sum(good), "", srv.URL+"/up/ucx.tar.gz"); code(t, err) != api.PSourceUnreachable {
		t.Fatalf("offline: %v", err)
	}
}

func TestLookasideURL(t *testing.T) {
	f := NewFetcher("/cache")
	got := f.LookasideURL("munge", "munge-0.5.18.tar.xz", "ABC")
	if got != FedoraLookaside+"/munge/munge-0.5.18.tar.xz/sha512/abc/munge-0.5.18.tar.xz" {
		t.Fatal(got)
	}
}

func TestPutSources(t *testing.T) {
	archive := "tarball"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(archive))
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SourcesFile), []byte("SHA512 (a-1.0.tar.gz) = "+sum(archive)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.conf"), []byte("in git"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := NewFetcher(t.TempDir())
	f.Lookaside = ""
	srcs := ParseSpecSources("Source0: " + srv.URL + "/a-1.0.tar.gz\nSource1: a.conf\nSource2: generated.txt\n")
	if err := f.PutSources(context.Background(), dir, srcs, ""); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "a-1.0.tar.gz")); err != nil || string(b) != archive {
		t.Fatalf("source: %q %v", b, err)
	}
	d := sourceDigests(dir, srcs)
	if len(d) != 2 || d["a.conf"] == "" || d["a-1.0.tar.gz"] == "" {
		t.Fatalf("digests %v", d)
	}
	// A URL without a SHA512 line: source.unlisted.
	srcs = ParseSpecSources("Source0: " + srv.URL + "/b-1.0.tar.gz\n")
	err := f.PutSources(context.Background(), dir, srcs, "")
	if code(t, err) != api.PSourceUnlisted {
		t.Fatalf("unlisted: %v", err)
	}
}

func TestChrootMacros(t *testing.T) {
	out := `config_opts['root'] = 'rocky+epel-9-x86_64'
config_opts['dist'] = 'el9'
config_opts['releasever'] = '9'
config_opts['macros']['%_host'] = 'x86_64-redhat-linux-gnu'
config_opts['macros']['%vendor'] = 'Rocky'
`
	dist, rhel, macros := chrootMacros(out)
	if dist != "el9" || rhel != "9" {
		t.Fatalf("dist %s rhel %s", dist, rhel)
	}
	want := [][2]string{{"dist", ".el9"}, {"el9", "1"}, {"rhel", "9"}, {"_host", "x86_64-redhat-linux-gnu"}, {"vendor", "Rocky"}}
	if len(macros) != len(want) {
		t.Fatalf("macros %v", macros)
	}
	for i := range want {
		if macros[i] != want[i] {
			t.Fatalf("macros %v, want %v", macros, want)
		}
	}
	// The dict form, and %rhel from the macros.
	out = `config_opts['dist'] = '.el10_1'
config_opts['macros'] = {'%rhel': '10', '%_vendor': 'redhat'}
`
	dist, rhel, macros = chrootMacros(out)
	if dist != "el10_1" || rhel != "10" || macros[0] != [2]string{"dist", ".el10_1"} || macros[1] != [2]string{"el10_1", "1"} {
		t.Fatalf("dist %s rhel %s macros %v", dist, rhel, macros)
	}
}

func TestUsesAutospec(t *testing.T) {
	for spec, want := range map[string]bool{
		"Release: %autorelease\n":         true,
		"Release: %{autorelease}\n":       true,
		"%changelog\n%{?autochangelog}\n": true,
		"Release: 1%{?dist}\n":            false,
		"Release: %autoreleases\n":        false,
	} {
		if UsesAutospec([]byte(spec)) != want {
			t.Errorf("%q: want %v", spec, want)
		}
	}
}

func TestFindSpec(t *testing.T) {
	dir := t.TempDir()
	if _, err := FindSpec(dir, "a"); err == nil {
		t.Fatal("no spec: no error")
	}
	write := func(n string) {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("x.spec")
	if s, err := FindSpec(dir, "a"); err != nil || filepath.Base(s) != "x.spec" {
		t.Fatalf("one spec: %s %v", s, err)
	}
	write("y.spec")
	if _, err := FindSpec(dir, "a"); err == nil {
		t.Fatal("two specs without a.spec: no error")
	}
	write("a.spec")
	if s, err := FindSpec(dir, "a"); err != nil || filepath.Base(s) != "a.spec" {
		t.Fatalf("PKG.spec: %s %v", s, err)
	}
}

func TestResultNVR(t *testing.T) {
	r := &Result{Name: "munge", Epoch: "0", Version: "0.5.18", Release: "1.el9"}
	if r.NVR() != "munge-0.5.18-1.el9" || r.EVR() != "0.5.18-1.el9" {
		t.Fatal(r.NVR(), r.EVR())
	}
	r.Epoch = "2"
	if r.EVR() != "2:0.5.18-1.el9" {
		t.Fatal(r.EVR())
	}
}
