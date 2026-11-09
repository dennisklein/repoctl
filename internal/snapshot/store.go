package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/errcode"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// EnvPlainHTTP makes repoctl talk plain HTTP to the registry. The tests use
// it for an in-process registry; a loopback registry gets plain HTTP also
// without it.
const EnvPlainHTTP = "REPOCTL_TEST_REGISTRY_PLAIN_HTTP"

// Options are the settings of a registry client.
type Options struct {
	// Getenv reads the environment: HOME, XDG_RUNTIME_DIR, XDG_CONFIG_HOME,
	// REGISTRY_AUTH_FILE, DOCKER_CONFIG and EnvPlainHTTP.
	Getenv func(string) string
	// Credential overrides the container credential store, for tests.
	Credential auth.CredentialFunc
	// HTTPClient overrides the HTTP client, for tests.
	HTTPClient *http.Client
	// UserAgent is the user agent of the requests.
	UserAgent string
}

// Store is the OCI package of one set and target in the registry.
type Store struct {
	// Ref is the OCI package: STORE/SET-TARGET.
	Ref  string
	repo *remote.Repository
}

// LoginCommand is the command that logs in to registry for a push. podman
// comes with the repoctl RPM. The auth file in ~/.config stays after a
// reboot; the default file of podman in $XDG_RUNTIME_DIR does not. podman
// asks for the user name and the token (manual/install.html#registry-login).
func LoginCommand(registry string) string {
	return "podman login " + registry + " --authfile ~/.config/containers/auth.json"
}

// AuthFiles gives the container credential files that repoctl reads, in
// order: REGISTRY_AUTH_FILE, $XDG_RUNTIME_DIR/containers/auth.json,
// $XDG_CONFIG_HOME/containers/auth.json, $DOCKER_CONFIG/config.json and
// ~/.docker/config.json (publish.html#registry).
func AuthFiles(getenv func(string) string) []string {
	if getenv == nil {
		getenv = os.Getenv
	}
	home := getenv("HOME")
	var out []string
	add := func(p string) {
		if p != "" {
			out = append(out, p)
		}
	}
	add(getenv("REGISTRY_AUTH_FILE"))
	if d := getenv("XDG_RUNTIME_DIR"); d != "" {
		add(filepath.Join(d, "containers", "auth.json"))
	}
	if d := getenv("XDG_CONFIG_HOME"); d != "" {
		add(filepath.Join(d, "containers", "auth.json"))
	} else if home != "" {
		add(filepath.Join(home, ".config", "containers", "auth.json"))
	}
	if d := getenv("DOCKER_CONFIG"); d != "" {
		add(filepath.Join(d, "config.json"))
	}
	if home != "" {
		add(filepath.Join(home, ".docker", "config.json"))
	}
	return out
}

// CredentialStore gives the container credential store of the user: the
// auth files of AuthFiles, the first one first.
func CredentialStore(getenv func(string) string) (credentials.Store, error) {
	var stores []credentials.Store
	for _, f := range AuthFiles(getenv) {
		s, err := credentials.NewStore(f, credentials.StoreOptions{})
		if err != nil {
			return nil, api.Errorf(api.PRegistryDenied, "the container credential file %s cannot be read: %v", f, err).
				WithHint("log in again", LoginCommand("ghcr.io"))
		}
		stores = append(stores, s)
	}
	if len(stores) == 0 {
		return credentials.NewMemoryStore(), nil
	}
	return credentials.NewStoreWithFallbacks(stores[0], stores[1:]...), nil
}

// PlainHTTP reports whether a registry host gets plain HTTP: a loopback
// host, or EnvPlainHTTP=1.
func PlainHTTP(host string, getenv func(string) string) bool {
	if getenv != nil && getenv(EnvPlainHTTP) == "1" {
		return true
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Open gives the client of an OCI package STORE/SET-TARGET.
func Open(pkg string, o Options) (*Store, error) {
	if pkg == "" {
		return nil, api.Errorf(api.PConfigInvalid, "store.oci is not set in repoctl.yaml: publish needs it").
			WithHint("add the registry path, for example store: {oci: ghcr.io/ORG/REPO}", "repoctl schema config")
	}
	ref, err := registry.ParseReference(pkg)
	if err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "store.oci: the OCI package %s is not valid: %v", pkg, err)
	}
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	cred := o.Credential
	if cred == nil {
		cs, err := CredentialStore(getenv)
		if err != nil {
			return nil, err
		}
		cred = credentials.Credential(cs)
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = retry.DefaultClient
	}
	client := &auth.Client{Client: hc, Cache: auth.NewCache(), Credential: cred, Header: http.Header{}}
	ua := o.UserAgent
	if ua == "" {
		ua = "repoctl/" + config.Version
	}
	client.SetUserAgent(ua)
	if PlainHTTP(ref.Registry, getenv) {
		// A test registry on loopback: the bearer realm may be loopback too.
		client.BlockedRealmPrefixes = nil
	}
	repo := &remote.Repository{Client: client, Reference: ref, PlainHTTP: PlainHTTP(ref.Registry, getenv), ManifestMediaTypes: []string{ocispec.MediaTypeImageManifest}}
	return &Store{Ref: pkg, repo: repo}, nil
}

// Registry gives the host of the registry.
func (s *Store) Registry() string { return s.repo.Reference.Registry }

// classify turns a registry error into a problem of the contract.
func (s *Store) classify(err error, what string) error {
	if err == nil {
		return nil
	}
	if _, ok := api.AsError(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	var er *errcode.ErrorResponse
	if errors.As(err, &er) {
		switch er.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return api.Errorf(api.PRegistryDenied, "%s: the registry %s denies %s (HTTP %d); the token needs the scope write:packages", s.Ref, s.Registry(), what, er.StatusCode).
				WithCause(err).WithHint("log in with a token of the scope write:packages", LoginCommand(s.Registry()))
		}
		return api.Errorf(api.PSnapshotPushFailed, "%s: %s failed: %v", s.Ref, what, err).WithCause(err)
	}
	var ne net.Error
	var oe *net.OpError
	if errors.As(err, &ne) || errors.As(err, &oe) || strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "no such host") {
		return api.Errorf(api.PRegistryUnreachable, "the registry %s does not answer: %v", s.Registry(), err).WithCause(err).
			WithHint("try again when the registry answers; plan and build use the local copy", "")
	}
	return api.Errorf(api.PSnapshotPushFailed, "%s: %s failed: %v", s.Ref, what, err).WithCause(err)
}

// IsNotFound reports an error of a reference that names nothing.
func IsNotFound(err error) bool {
	if errors.Is(err, errdef.ErrNotFound) {
		return true
	}
	var er *errcode.ErrorResponse
	if errors.As(err, &er) && er.StatusCode == http.StatusNotFound {
		return true
	}
	return false
}

// Latest gives the descriptor of the tag latest, or ok false when the
// package has no snapshot (no tag latest).
func (s *Store) Latest(ctx context.Context) (ocispec.Descriptor, bool, error) {
	d, err := s.repo.Resolve(ctx, TagLatest)
	if err != nil {
		if IsNotFound(err) {
			return ocispec.Descriptor{}, false, nil
		}
		return ocispec.Descriptor{}, false, s.classify(err, "the read of latest")
	}
	return d, true, nil
}

// Resolve gives the descriptor of a reference: latest, a publish id or a
// digest. A reference that names nothing gives snapshot.not-found.
func (s *Store) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error) {
	if ref == "" {
		ref = TagLatest
	}
	d, err := s.repo.Resolve(ctx, ref)
	if err != nil {
		if IsNotFound(err) {
			return d, api.Errorf(api.PSnapshotNotFound, "%s names no snapshot of %s", ref, s.Ref).
				WithHint("list the snapshots", "repoctl repo snapshots --all")
		}
		return d, s.classify(err, "the read of "+ref)
	}
	return d, nil
}

// Fetch reads the snapshot manifest of a descriptor.
func (s *Store) Fetch(ctx context.Context, d ocispec.Descriptor) (*Snapshot, error) {
	if d.MediaType == "" {
		d.MediaType = ocispec.MediaTypeImageManifest
	}
	if d.Size == 0 {
		// A digest from an annotation: the registry gives the size.
		rd, err := s.Resolve(ctx, d.Digest.String())
		if err != nil {
			return nil, err
		}
		d = rd
	}
	raw, err := content.FetchAll(ctx, s.repo, d)
	if err != nil {
		if IsNotFound(err) {
			return nil, api.Errorf(api.PSnapshotNotFound, "%s has no manifest %s", s.Ref, d.Digest).
				WithHint("list the snapshots", "repoctl repo snapshots --all")
		}
		return nil, s.classify(err, "the read of the manifest "+d.Digest.String())
	}
	snap, err := Decode(raw, d)
	if err != nil {
		return nil, api.Errorf(api.PVerifyFailed, "%s@%s: %v", s.Ref, d.Digest, err)
	}
	return snap, nil
}

// Get resolves a reference and reads its snapshot manifest.
func (s *Store) Get(ctx context.Context, ref string) (*Snapshot, error) {
	d, err := s.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	return s.Fetch(ctx, d)
}

// PushStats counts the blobs of a push.
type PushStats struct {
	New   int
	Known int
	Bytes int64
}

// Push uploads the blobs that the registry does not have, then the
// manifest with the tag (a publish id). It gives the descriptor of the
// manifest. Each file needs Local.
func (s *Store) Push(ctx context.Context, files []File, meta Meta, tag string) (ocispec.Descriptor, PushStats, error) {
	var st PushStats
	empty := ocispec.DescriptorEmptyJSON
	if ok, err := s.repo.Exists(ctx, empty); err != nil {
		return ocispec.Descriptor{}, st, s.classify(err, "the push")
	} else if !ok {
		if err := s.repo.Push(ctx, empty, bytes.NewReader(empty.Data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return ocispec.Descriptor{}, st, s.classify(err, "the push of the config")
		}
	}
	seen := map[digest.Digest]bool{}
	for _, f := range files {
		if seen[f.Digest] {
			continue
		}
		seen[f.Digest] = true
		d := f.Descriptor()
		d.Annotations = nil
		ok, err := s.repo.Exists(ctx, d)
		if err != nil {
			return ocispec.Descriptor{}, st, s.classify(err, "the push of "+f.Path)
		}
		if ok {
			st.Known++
			continue
		}
		if err := s.pushFile(ctx, d, f.Local); err != nil {
			return ocispec.Descriptor{}, st, s.classify(err, "the push of "+f.Path)
		}
		st.New++
		st.Bytes += f.Size
	}
	raw, desc, err := Encode(NewManifest(files, meta))
	if err != nil {
		return ocispec.Descriptor{}, st, err
	}
	if err := s.repo.PushReference(ctx, desc, bytes.NewReader(raw), tag); err != nil {
		return ocispec.Descriptor{}, st, s.classify(err, "the push of the manifest")
	}
	return desc, st, nil
}

func (s *Store) pushFile(ctx context.Context, d ocispec.Descriptor, path string) error {
	if path == "" {
		return fmt.Errorf("blob %s has no local file", d.Digest)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	err = s.repo.Push(ctx, d, f)
	if errors.Is(err, errdef.ErrAlreadyExists) {
		return nil
	}
	return err
}

// Tag points a tag at a manifest.
func (s *Store) Tag(ctx context.Context, d ocispec.Descriptor, tag string) error {
	if d.MediaType == "" {
		d.MediaType = ocispec.MediaTypeImageManifest
	}
	if err := s.repo.Tag(ctx, d, tag); err != nil {
		return s.classify(err, "the tag write of "+tag)
	}
	return nil
}

// Tags gives the tags of the package, sorted. A package without a
// manifest has no tags.
func (s *Store) Tags(ctx context.Context) ([]string, error) {
	var out []string
	err := s.repo.Tags(ctx, "", func(tags []string) error {
		out = append(out, tags...)
		return nil
	})
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, s.classify(err, "the tag list")
	}
	sort.Strings(out)
	return out, nil
}

// Chain gives the snapshot of d and its parents, newest first, along the
// annotation io.repoctl.parent. It stops at the first snapshot without a
// parent, or at a parent that gc deleted.
func (s *Store) Chain(ctx context.Context, d ocispec.Descriptor, limit int) ([]*Snapshot, error) {
	var out []*Snapshot
	seen := map[digest.Digest]bool{}
	for {
		if seen[d.Digest] {
			return out, fmt.Errorf("%s: the parent chain has a loop at %s", s.Ref, d.Digest)
		}
		seen[d.Digest] = true
		snap, err := s.Fetch(ctx, d)
		if err != nil {
			if e, ok := api.AsError(err); ok && e.Code == api.PSnapshotNotFound && len(out) > 0 {
				return out, nil
			}
			return out, err
		}
		out = append(out, snap)
		if snap.Meta.Parent == "" || (limit > 0 && len(out) >= limit) {
			return out, nil
		}
		p, err := digest.Parse(snap.Meta.Parent)
		if err != nil {
			return out, fmt.Errorf("%s: the parent %q of %s is not a digest", s.Ref, snap.Meta.Parent, snap.Meta.Publish)
		}
		d = ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: p}
	}
}

// OnChain reports whether the chain from d holds the digest want.
func (s *Store) OnChain(ctx context.Context, d ocispec.Descriptor, want api.Digest) (bool, error) {
	chain, err := s.Chain(ctx, d, 0)
	for _, c := range chain {
		if c.Digest() == want {
			return true, nil
		}
	}
	return false, err
}

// Delete deletes a snapshot: its publish id tag, where the registry
// allows a tag delete, then its manifest.
func (s *Store) Delete(ctx context.Context, snap *Snapshot) error {
	if snap.Meta.Publish != "" {
		if d, err := s.repo.Resolve(ctx, snap.Meta.Publish); err == nil && d.Digest == snap.Desc.Digest {
			_ = s.deleteRef(ctx, snap.Meta.Publish)
		}
	}
	d := snap.Desc
	if d.MediaType == "" {
		d.MediaType = ocispec.MediaTypeImageManifest
	}
	if err := s.repo.Delete(ctx, d); err != nil && !IsNotFound(err) {
		return s.classify(err, "the delete of "+snap.Meta.Publish)
	}
	return nil
}

// deleteRef sends DELETE /v2/NAME/manifests/REF for a tag. The OCI
// distribution spec 1.1 allows it; a registry that refuses keeps the tag.
func (s *Store) deleteRef(ctx context.Context, ref string) error {
	scheme := "https"
	if s.repo.PlainHTTP {
		scheme = "http"
	}
	u := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, s.repo.Reference.Host(), s.repo.Reference.Repository, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.repo.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("DELETE %s: HTTP %d", ref, resp.StatusCode)
	}
	return nil
}

// FetchBlob reads the content of one file of a snapshot and checks its
// digest.
func (s *Store) FetchBlob(ctx context.Context, f File) ([]byte, error) {
	d := f.Descriptor()
	d.Annotations = nil
	b, err := content.FetchAll(ctx, s.repo, d)
	if err != nil {
		return nil, s.classify(err, "the download of "+f.Path)
	}
	return b, nil
}

// PullStats counts the blobs of a download.
type PullStats struct {
	New   int
	Known int
	Bytes int64
}

// Download writes the files of a snapshot into dir. A file whose digest
// have(digest) gives as a local path is linked from there (hard link, else
// a copy); every other file comes from the registry. Each download is
// checked against its digest.
func (s *Store) Download(ctx context.Context, snap *Snapshot, dir string, have func(digest.Digest) string) (PullStats, error) {
	var st PullStats
	for _, f := range snap.Files {
		dst := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return st, err
		}
		if have != nil {
			if src := have(f.Digest); src != "" {
				if err := LinkOrCopy(src, dst); err == nil {
					st.Known++
					continue
				}
			}
		}
		if err := s.downloadFile(ctx, f, dst); err != nil {
			return st, err
		}
		st.New++
		st.Bytes += f.Size
	}
	return st, nil
}

func (s *Store) downloadFile(ctx context.Context, f File, dst string) error {
	d := f.Descriptor()
	d.Annotations = nil
	rc, err := s.repo.Fetch(ctx, d)
	if err != nil {
		return s.classify(err, "the download of "+f.Path)
	}
	defer rc.Close()
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	vr := content.NewVerifyReader(rc, d)
	if _, err := io.Copy(out, vr); err != nil {
		out.Close()
		os.Remove(tmp)
		return api.Errorf(api.PVerifyFailed, "%s: the download of %s does not verify: %v", s.Ref, f.Path, err)
	}
	if err := vr.Verify(); err != nil {
		out.Close()
		os.Remove(tmp)
		return api.Errorf(api.PVerifyFailed, "%s: the download of %s does not have the sha256 %s: %v", s.Ref, f.Path, f.SHA256(), err)
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// LinkOrCopy makes dst a hard link of src, or a copy when a link does not
// work (another file system).
func LinkOrCopy(src, dst string) error {
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return CopyFile(src, dst)
}

// CopyFile copies src to dst.
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chtimes(tmp, st.ModTime(), st.ModTime()); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
