package tree

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/manifest"
)

// ServedSnapshotFile is the file that the mirror job writes into each
// served tree: it names the snapshot of the tree
// (publish.html#mirror-lines).
const ServedSnapshotFile = "repoctl-snapshot.json"

// ServedSnapshot is repoctl-snapshot.json of a served tree.
type ServedSnapshot struct {
	Set     string     `json:"set"`
	Target  string     `json:"target"`
	Digest  api.Digest `json:"digest"`
	Publish string     `json:"publish"`
	Pulled  string     `json:"pulled"`
}

// FetchOptions say what FetchServed downloads.
type FetchOptions struct {
	Layout Layout
	// Keyfile and Release name the root files: the key file and
	// RELEASE.rpm; "" skips them.
	Keyfile string
	Release string
	// Client is the HTTP client; nil is http.DefaultClient.
	Client *http.Client
	// Jobs is the number of parallel downloads (default 4).
	Jobs int
}

// Served is what FetchServed found at the URL of a target.
type Served struct {
	URL string
	// Snapshot is repoctl-snapshot.json, or nil when the served tree has
	// none.
	Snapshot *ServedSnapshot
	Files    int
	Bytes    int64
}

// FetchServed downloads the tree that a URL serves into dir, as a client
// reads it: per repository repomd.xml, its signature, the repodata files
// and the packages of primary.xml; the records that the RepoManifest lists;
// the root files; and repoctl-snapshot.json. Verify checks the result. A
// file that the server does not have stays missing, so that Verify names
// it; a binary repository without repomd.xml is an error.
func FetchServed(ctx context.Context, base, dir string, o FetchOptions) (*Served, error) {
	f := &fetcher{ctx: ctx, base: strings.TrimSuffix(base, "/"), dir: dir, client: o.Client}
	if f.client == nil {
		f.client = http.DefaultClient
	}
	out := &Served{URL: f.base}
	if ok, err := f.get(ServedSnapshotFile); err != nil {
		return nil, err
	} else if ok {
		var s ServedSnapshot
		b, err := os.ReadFile(filepath.Join(dir, ServedSnapshotFile))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, api.Errorf(api.PVerifyFailed, "%s/%s is not valid: %v", f.base, ServedSnapshotFile, err)
		}
		out.Snapshot = &s
	}
	for _, r := range o.Layout.Repos() {
		ok, err := f.get(r.Dir + "/repodata/repomd.xml")
		if err != nil {
			return nil, err
		}
		if !ok {
			if r.Kind == Binary {
				return nil, api.Errorf(api.PVerifyFailed, "%s/%s/repodata/repomd.xml: the server has no repodata", f.base, r.Dir)
			}
			continue
		}
		if _, err := f.get(r.Dir + "/repodata/repomd.xml.asc"); err != nil {
			return nil, err
		}
		rdir := filepath.Join(dir, r.Dir)
		md, err := ReadRepomd(rdir)
		if err != nil {
			return nil, err
		}
		var rels []string
		for _, d := range md.Data {
			rels = append(rels, r.Dir+"/"+d.Location.Href)
		}
		if err := f.getAll(rels, o.Jobs); err != nil {
			return nil, err
		}
		_, pkgs, err := ReadPrimary(ctx, rdir)
		if err != nil {
			// Verify names the broken repodata.
			continue
		}
		rels = rels[:0]
		for _, p := range pkgs {
			rels = append(rels, r.Dir+"/"+p.Location.Href)
		}
		if err := f.getAll(rels, o.Jobs); err != nil {
			return nil, err
		}
	}
	if m, err := manifest.ReadRepo(filepath.Join(dir, o.Layout.Name)); err == nil && m != nil {
		var rels []string
		add := func(rec *manifest.Record) {
			if rec != nil {
				rels = append(rels, o.Layout.Name+"/"+rec.Path)
			}
		}
		for _, b := range m.Builds {
			if b.Records != nil {
				add(b.Records.Provenance)
				add(b.Records.SBOM)
			}
		}
		if m.Records != nil {
			add(m.Records.SBOM)
		}
		if err := f.getAll(rels, o.Jobs); err != nil {
			return nil, err
		}
	}
	if o.Keyfile != "" && o.Release != "" {
		if err := f.getAll([]string{o.Keyfile, o.Release + ".rpm"}, o.Jobs); err != nil {
			return nil, err
		}
	}
	out.Files, out.Bytes = f.files, f.bytes
	return out, nil
}

// fetcher downloads files below a base URL into a directory.
type fetcher struct {
	ctx    context.Context
	client *http.Client
	base   string
	dir    string

	mu    sync.Mutex
	seen  map[string]bool
	files int
	bytes int64
}

// localPath checks a path of the server: relative, with slashes, with no
// . or .. segment.
func localPath(rel string) (string, error) {
	if rel == "" || strings.Contains(rel, `\`) || !filepath.IsLocal(rel) || path.Clean(rel) != rel {
		return "", fmt.Errorf("the served path %q is not a clean relative path", rel)
	}
	return filepath.FromSlash(rel), nil
}

// get downloads one file. A 404 gives false without an error.
func (f *fetcher) get(rel string) (bool, error) {
	lp, err := localPath(rel)
	if err != nil {
		return false, api.Errorf(api.PVerifyFailed, "%s: %v", f.base, err)
	}
	f.mu.Lock()
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[rel] {
		f.mu.Unlock()
		return true, nil
	}
	f.seen[rel] = true
	f.mu.Unlock()
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	u := f.base + "/" + strings.Join(segs, "/")
	req, err := http.NewRequestWithContext(f.ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return false, api.Errorf(api.PVerifyFailed, "the served tree cannot be read: %v", err).
			WithHint("check that the HTTP server of repo.url answers", "curl -I "+u)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return false, nil
	default:
		return false, api.Errorf(api.PVerifyFailed, "GET %s: HTTP %d", u, resp.StatusCode)
	}
	dst := filepath.Join(f.dir, lp)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	out, err := os.Create(dst) //nolint:gosec // G304: a checked relative path below the work directory
	if err != nil {
		return false, err
	}
	n, err := io.Copy(out, resp.Body)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, api.Errorf(api.PVerifyFailed, "GET %s: %v", u, err)
	}
	f.mu.Lock()
	f.files++
	f.bytes += n
	f.mu.Unlock()
	return true, nil
}

// getAll downloads files with jobs parallel downloads; a missing file
// stays missing.
func (f *fetcher) getAll(rels []string, jobs int) error {
	if jobs <= 0 {
		jobs = 4
	}
	work := make(chan string)
	errs := make(chan error, len(rels))
	var wg sync.WaitGroup
	for range min(jobs, len(rels)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rel := range work {
				if _, err := f.get(rel); err != nil {
					errs <- err
				}
			}
		}()
	}
	for _, rel := range rels {
		work <- rel
	}
	close(work)
	wg.Wait()
	close(errs)
	return <-errs
}
