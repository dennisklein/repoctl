//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"

	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/snapshot"
	"github.com/dennisklein/repoctl/internal/testkit"
)

// tamper corrupts the responses of an HTTP handler whose path ends with a
// suffix: the last byte of the body flips.
type tamper struct {
	h      http.Handler
	mu     sync.Mutex
	suffix string
}

func (tp *tamper) set(suffix string) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.suffix = suffix
}

func (tp *tamper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tp.mu.Lock()
	suffix := tp.suffix
	tp.mu.Unlock()
	if suffix == "" || r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, suffix) {
		tp.h.ServeHTTP(w, r)
		return
	}
	rec := httptest.NewRecorder()
	tp.h.ServeHTTP(rec, r)
	body := rec.Body.Bytes()
	if len(body) > 0 {
		body[len(body)-1] ^= 0xff
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(body)
}

// mirrorFixture is the story with el9 published to a registry whose blobs
// the test can corrupt, a served directory behind repo.url and the config
// of the mirror job.
type mirrorFixture struct {
	s        *testkit.Story
	reg, web *tamper
	pkg      string
	srv      string
	served   string
	state    string
	conf     string
	keyring  string
	notified string
	digest   string
	publish  string
}

// newMirrorFixture publishes hello and slurm-release to el9 of the story.
func newMirrorFixture(t *testing.T) *mirrorFixture {
	t.Helper()
	needTools(t, "curl", "jq", "gpgv", "rpmkeys", "rpm", "sha256sum", "flock", "gzip")
	s := publishStory(t)
	f := &mirrorFixture{s: s}
	f.reg = &tamper{h: registry.New(registry.Logger(log.New(io.Discard, "", 0)))}
	reg := httptest.NewServer(f.reg)
	t.Cleanup(reg.Close)
	f.srv = filepath.Join(s.Root, "srv")
	if err := os.MkdirAll(f.srv, 0o755); err != nil {
		t.Fatal(err)
	}
	f.web = &tamper{h: http.FileServer(http.Dir(f.srv))}
	web := httptest.NewServer(f.web)
	t.Cleanup(web.Close)

	// repo.url and store.oci point at the servers of the fixture.
	yml := readFile(t, filepath.Join(s.SetDir, "repoctl.yaml"))
	yml = regexp.MustCompile(`url: http://127\.0\.0\.1:[0-9]+/slurm-26\.05`).ReplaceAllString(yml, "url: "+web.URL+"/slurm-26.05")
	host := strings.TrimPrefix(reg.URL, "http://")
	yml = regexp.MustCompile(`oci: 127\.0\.0\.1:[0-9]+/gsi-hpc`).ReplaceAllString(yml, "oci: "+host+"/gsi-hpc")
	writeFile(t, filepath.Join(s.SetDir, "repoctl.yaml"), yml)
	s.Git(s.SetDir, "commit", "-qam", "Serve the fixture")
	s.Git(s.SetDir, "push", "-q", "origin", testkit.SetBranch)
	f.pkg = host + "/gsi-hpc/" + testkit.SetName + "-el9"

	hello := fakeBuild(t, s, cloneBranch(t, s, "hello/slurm-26.05"), "hello.spec", filepath.Join(s.Root, "b", "hello"))
	rel := fakeBuild(t, s, filepath.Join(s.SetDir, "packages/slurm-release"), "slurm-release.spec", filepath.Join(s.Root, "b", "rel"))
	inTree := s.Git(s.SetDir, "log", "-1", "--format=%H", "--", "packages/slurm-release")
	stage(t, s, map[string]string{"hello": hello, "slurm-release": rel},
		map[string]string{"hello": s.BranchCommit("hello"), "slurm-release": inTree})
	s.Setenv(publishUnattended, "1")
	s.MustRun("publish", "--target", "el9", "--yes")
	env, _ := s.RunJSON("repo", "snapshots", "--target", "el9")
	latest := list(items(t, env)[0], "snapshots")[0].(map[string]any)
	f.digest, f.publish = latest["digest"].(string), latest["publish"].(string)

	f.served = filepath.Join(f.srv, testkit.SetName, "el9")
	f.state = filepath.Join(s.Root, "mirror-state")
	f.conf = filepath.Join(s.Root, "repoctl-mirror.conf")
	f.notified = filepath.Join(s.Root, "notified")
	f.keyring = filepath.Join(s.SetDir, "packages", "slurm-release", "RPM-GPG-KEY-slurm")
	notify := filepath.Join(s.Root, "notify")
	writeFile(t, notify, "#!/bin/sh\n{ echo \"$*\"; cat; echo; } >>"+f.notified+"\n")
	if err := os.Chmod(notify, 0o755); err != nil {
		t.Fatal(err)
	}
	f.config(t, f.keyring, true)
	return f
}

// config writes the config of the job: one line for el9 with the keyring,
// or no line.
func (f *mirrorFixture) config(t *testing.T, keyring string, line bool) {
	t.Helper()
	text := "# test config\nKEEP_TREES=1\nSTATE_DIR=" + f.state + "\nLOCK=" + filepath.Join(f.s.Root, "mirror.lock") +
		"\nNOTIFY=\"" + filepath.Join(f.s.Root, "notify") + "\"\n"
	if line {
		text += testkit.SetName + "  el9  " + f.pkg + "  " + f.served + "  " + keyring + "\n"
	}
	writeFile(t, f.conf, text)
}

// mirror runs the mirror job once.
func (f *mirrorFixture) mirror(t *testing.T) (int, string) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "contrib", "mirror", "mirror"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, f.conf)
	cmd.Env = append(os.Environ(), "MIRROR_TEST_PLAIN_HTTP=1", "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	t.Logf("mirror: exit %d\n%s", code, out.String())
	return code, out.String()
}

// heartbeat reads repoctl-mirror.json of the served tree.
func (f *mirrorFixture) heartbeat(t *testing.T) map[string]any {
	t.Helper()
	var hb map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(f.served, "repoctl-mirror.json"))), &hb); err != nil {
		t.Fatal(err)
	}
	return hb
}

// servedDigest gives the digest of the tree that the symlink of the served
// path names.
func (f *mirrorFixture) servedDigest(t *testing.T) string {
	t.Helper()
	target, err := os.Readlink(f.served)
	if err != nil {
		t.Fatalf("the served path is not a symlink: %v", err)
	}
	if filepath.Dir(target) != filepath.Join(f.state, ".trees", testkit.SetName+"-el9") {
		t.Fatalf("the served symlink names %s", target)
	}
	return "sha256:" + filepath.Base(target)
}

// trees gives the tree digests of the line.
func (f *mirrorFixture) trees(t *testing.T) []string {
	t.Helper()
	des, err := os.ReadDir(filepath.Join(f.state, ".trees", testkit.SetName+"-el9"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		if de.IsDir() && !strings.HasPrefix(de.Name(), ".") {
			out = append(out, de.Name())
		}
	}
	return out
}

// pushVariant pushes the files of the local copy of el9, changed by
// change, as a new snapshot and tags it latest. It gives its digest.
func (f *mirrorFixture) pushVariant(t *testing.T, id string, change func(dir string)) string {
	t.Helper()
	paths := config.NewPaths(os.Getenv)
	dir := filepath.Join(t.TempDir(), "tree")
	src := paths.LocalCopy(testkit.SetName, "el9")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, rel), 0o755)
		}
		return snapshot.CopyFile(p, filepath.Join(dir, rel))
	})
	if err != nil {
		t.Fatal(err)
	}
	if change != nil {
		change(dir)
	}
	files, err := snapshot.Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := snapshot.Open(f.pkg, snapshot.Options{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.Get(ctx, "latest")
	if err != nil {
		t.Fatal(err)
	}
	meta := snap.Meta
	meta.Publish, meta.Parent = id, snap.Digest()
	desc, _, err := st.Push(ctx, files, meta, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Tag(ctx, desc, snapshot.TagLatest); err != nil {
		t.Fatal(err)
	}
	return desc.Digest.String()
}

// TestMirrorJob runs contrib/mirror/mirror on the story: it serves latest
// of el9, keeps the old tree when a blob, a record, the keyring or the
// revision is wrong, and deletes the line that leaves the config.
func TestMirrorJob(t *testing.T) {
	f := newMirrorFixture(t)
	s := f.s

	// The first run serves latest.
	if code, out := f.mirror(t); code != 0 {
		t.Fatalf("mirror: exit %d\n%s", code, out)
	}
	if got := f.servedDigest(t); got != f.digest {
		t.Fatalf("served %s, want latest %s", got, f.digest)
	}
	var snap map[string]string
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(f.served, "repoctl-snapshot.json"))), &snap); err != nil {
		t.Fatal(err)
	}
	if snap["set"] != testkit.SetName || snap["target"] != "el9" || snap["digest"] != f.digest || snap["publish"] != f.publish {
		t.Errorf("repoctl-snapshot.json: %v", snap)
	}
	if _, err := time.Parse(time.RFC3339, snap["pulled"]); err != nil {
		t.Errorf("repoctl-snapshot.json: pulled %q: %v", snap["pulled"], err)
	}
	hb := f.heartbeat(t)
	if hb["result"] != "swapped" || hb["served"] != f.digest || hb["latest"] != f.digest || hb["behindSince"] != nil || hb["error"] != nil {
		t.Errorf("repoctl-mirror.json after the swap: %v", hb)
	}
	notified := readFile(t, f.notified)
	if !strings.Contains(notified, "served "+testkit.SetName+" el9\n") || !strings.Contains(notified, `"publish":"`+f.publish+`"`) {
		t.Errorf("NOTIFY served:\n%s", notified)
	}
	for _, p := range []string{"slurm/repodata/repomd.xml", "RPM-GPG-KEY-slurm", "slurm-release.rpm"} {
		if _, err := os.Stat(filepath.Join(f.served, p)); err != nil {
			t.Errorf("served tree: %v", err)
		}
	}

	// repo verify --served checks the tree that repo.url serves.
	r := s.Run("repo", "verify", "--served", "--target", "el9")
	t.Logf("repo verify --served:\n%s%s", r.Stdout, r.Stderr)
	if r.Code != 0 || !strings.Contains(r.Stdout, "el9 served") {
		t.Errorf("repo verify --served: exit %d\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	env, _ := s.RunJSON("repo", "verify", "--served", "--target", "el9")
	if items(t, env)[0]["worst"] != "ok" {
		t.Errorf("repo verify --served --json: %v", env)
	}
	// A changed RPM on the HTTP server fails it with exit 10.
	f.web.set(".rpm")
	if r := s.Run("repo", "verify", "--served", "--target", "el9"); r.Code != 10 {
		t.Errorf("repo verify --served of a changed RPM: exit %d\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	f.web.set("")

	// A second run finds the same digest.
	if code, _ := f.mirror(t); code != 0 {
		t.Fatalf("second mirror run: exit %d", code)
	}
	if hb := f.heartbeat(t); hb["result"] != "same" || hb["behindSince"] != nil {
		t.Errorf("repoctl-mirror.json after the second run: %v", hb)
	}

	// A snapshot with a changed record: first its blob breaks in the
	// download, then the record does not match the RepoManifest. The job
	// keeps the old tree.
	var record string
	bad := f.pushVariant(t, "p-20261103-1500-bad", func(dir string) {
		recs, _ := filepath.Glob(filepath.Join(dir, "slurm", "records", "hello-*", "provenance.json"))
		if len(recs) != 1 {
			t.Fatalf("records of hello: %v", recs)
		}
		writeFile(t, recs[0], readFile(t, recs[0])+"\n")
		sum, err := snapshot.FileOf(recs[0])
		if err != nil {
			t.Fatal(err)
		}
		record = sum.Digest.String()
	})
	f.reg.set("/blobs/" + record)
	code, out := f.mirror(t)
	f.reg.set("")
	if code != 1 || !strings.Contains(out, "step 3 blobs") || f.servedDigest(t) != f.digest {
		t.Fatalf("mirror of a broken blob: exit %d, served %s\n%s", code, f.servedDigest(t), out)
	}
	hb = f.heartbeat(t)
	since := hb["behindSince"]
	if hb["result"] != "failed" || hb["served"] != f.digest || hb["latest"] != bad || since == nil || !strings.Contains(hb["error"].(string), "sha256") {
		t.Errorf("repoctl-mirror.json after a broken blob: %v", hb)
	}
	code, out = f.mirror(t)
	if code != 1 || !strings.Contains(out, "step 5 verify") || !strings.Contains(out, "does not have the sha256 of the RepoManifest") || f.servedDigest(t) != f.digest {
		t.Fatalf("mirror of a changed record: exit %d\n%s", code, out)
	}
	if hb := f.heartbeat(t); hb["result"] != "failed" || hb["behindSince"] != since {
		t.Errorf("repoctl-mirror.json after a changed record: %v (behindSince %v)", hb, since)
	}

	// A keyring without the signer refuses the snapshot.
	good := f.pushVariant(t, "p-20261103-1510-a1b", nil)
	other := testkit.GenKey(t, "a.admin")
	otherKeyring := filepath.Join(s.Root, "other.asc")
	writeFile(t, otherKeyring, string(other.Public))
	f.config(t, otherKeyring, true)
	code, out = f.mirror(t)
	if code != 1 || !strings.Contains(out, "does not verify with the keyring") || f.servedDigest(t) != f.digest {
		t.Fatalf("mirror with a keyring without the signer: exit %d\n%s", code, out)
	}
	f.config(t, f.keyring, true)
	if code, out := f.mirror(t); code != 0 || f.servedDigest(t) != good {
		t.Fatalf("mirror with the keyring of the release: exit %d\n%s", code, out)
	}
	if got := f.trees(t); len(got) != 2 {
		t.Errorf("KEEP_TREES=1: trees %v, want the served one and one old tree", got)
	}

	// An older revision of repomd.xml, signed again, stays unserved.
	f.pushVariant(t, "p-20261103-1520-c2d", func(dir string) {
		md := filepath.Join(dir, "slurm", "repodata", "repomd.xml")
		text := readFile(t, md)
		m := regexp.MustCompile(`<revision>([0-9]+)</revision>`).FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("repomd.xml has no revision:\n%s", text)
		}
		rev, _ := strconv.Atoi(m[1])
		writeFile(t, md, strings.Replace(text, m[0], "<revision>"+strconv.Itoa(rev-100)+"</revision>", 1))
		sign := exec.Command("gpg", "--batch", "--yes", "--armor", "--local-user", "d.klein", "--detach-sign", "-o", md+".asc", md)
		sign.Env = s.Env()
		if b, err := sign.CombinedOutput(); err != nil {
			t.Fatalf("gpg: %v\n%s", err, b)
		}
	})
	code, out = f.mirror(t)
	if code != 1 || !strings.Contains(out, "step 6 revision") || f.servedDigest(t) != good {
		t.Fatalf("mirror of an older revision: exit %d\n%s", code, out)
	}

	// A line that leaves the config: the symlink, the trees and the objects
	// go.
	f.config(t, f.keyring, false)
	if code, out := f.mirror(t); code != 0 {
		t.Fatalf("mirror without the line: exit %d\n%s", code, out)
	}
	if _, err := os.Lstat(f.served); !os.IsNotExist(err) {
		t.Errorf("the served symlink stays: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.state, ".trees", testkit.SetName+"-el9")); !os.IsNotExist(err) {
		t.Errorf("the trees of the line stay: %v", err)
	}
	if objs, _ := os.ReadDir(filepath.Join(f.state, ".objects", "sha256")); len(objs) != 0 {
		t.Errorf("%d objects stay", len(objs))
	}
}
