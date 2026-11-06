// Package snapshot is the snapshot format and the registry client of
// repoctl (docs/design/publish.html#format, #registry).
//
// A snapshot is one OCI artifact per set and target, in the OCI package
// STORE/SET-TARGET (config.Set.OCIPackage). The manifest has the artifact
// type application/vnd.repoctl.repo.v1 and one layer per file of the tree:
// the annotation org.opencontainers.image.title of a layer is the path of
// the file in the tree. The annotations of the manifest say who made the
// snapshot and from what; the annotation io.repoctl.parent links each
// snapshot to its base, so the snapshots make a chain. The tag latest names
// the published snapshot, and each snapshot has the tag of its publish id.
package snapshot

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/dennisklein/repoctl/internal/api"
)

// ArtifactType is the artifact type of a snapshot manifest.
const ArtifactType = "application/vnd.repoctl.repo.v1"

// FileMediaType is the media type of a layer: one file of the tree.
const FileMediaType = "application/vnd.repoctl.file.v1"

// TagLatest is the tag of the published snapshot.
const TagLatest = "latest"

// The annotations of a snapshot manifest (publish.html#format).
const (
	AnnTitle  = ocispec.AnnotationTitle  // org.opencontainers.image.title of a layer
	AnnSource = ocispec.AnnotationSource // org.opencontainers.image.source

	AnnSet        = "io.repoctl.set"
	AnnTarget     = "io.repoctl.target"
	AnnPublish    = "io.repoctl.publish"
	AnnKind       = "io.repoctl.kind"
	AnnParent     = "io.repoctl.parent"
	AnnSetCommit  = "io.repoctl.set-commit"
	AnnSigner     = "io.repoctl.signer"
	AnnUser       = "io.repoctl.user"
	AnnHost       = "io.repoctl.host"
	AnnCreated    = "io.repoctl.created"
	AnnVersion    = "io.repoctl.repoctl-version"
	AnnAccepted   = "io.repoctl.advisories.accepted"
	AnnHeld       = "io.repoctl.held"
	AnnUnattended = "io.repoctl.unattended"
	// AnnTraceParent is the trace parent of an unattended publish.
	AnnTraceParent = "io.repoctl.trace-parent"
	// AnnOrigin is the origin of the publish: cli or ui.
	AnnOrigin = "io.repoctl.origin"
)

// The kinds of a snapshot.
const (
	KindPublish = "publish"
	KindRestore = "restore"
	KindPrune   = "prune"
	KindResign  = "resign"
	KindImport  = "import"
	KindReseed  = "reseed"
)

// Kinds lists the kinds of a snapshot.
var Kinds = []string{KindPublish, KindRestore, KindPrune, KindResign, KindImport, KindReseed}

// Meta is what the annotations of a snapshot manifest say.
type Meta struct {
	Set       string
	Target    string
	Publish   string
	Kind      string
	Parent    api.Digest
	SetCommit api.Commit
	// Signer is the full fingerprint of the key that signed the new files.
	Signer  string
	User    string
	Host    string
	Created time.Time
	Repoctl string
	// Source is the GitHub repository of the set.
	Source   string
	Accepted []api.SnapshotAcceptedItem
	Held     []api.SnapshotHeldItem
	// Unattended is set for a publish without a human
	// (REPOCTL_ALLOW_UNATTENDED_PUBLISH=1).
	Unattended  bool
	TraceParent string
	Origin      string
}

// Annotations gives the manifest annotations of the meta.
func (m Meta) Annotations() map[string]string {
	a := map[string]string{
		AnnSet:     m.Set,
		AnnTarget:  m.Target,
		AnnPublish: m.Publish,
		AnnKind:    m.Kind,
		AnnSigner:  m.Signer,
		AnnUser:    m.User,
		AnnHost:    m.Host,
		AnnVersion: m.Repoctl,
	}
	if !m.Created.IsZero() {
		a[AnnCreated] = api.FormatTime(m.Created)
		a[ocispec.AnnotationCreated] = m.Created.UTC().Format(time.RFC3339)
	}
	if m.Parent != "" {
		a[AnnParent] = m.Parent
	}
	if m.SetCommit != "" {
		a[AnnSetCommit] = m.SetCommit
	}
	if m.Source != "" {
		a[AnnSource] = m.Source
	}
	acc := m.Accepted
	if acc == nil {
		acc = []api.SnapshotAcceptedItem{}
	}
	b, _ := json.Marshal(acc)
	a[AnnAccepted] = string(b)
	if len(m.Held) > 0 {
		b, _ := json.Marshal(m.Held)
		a[AnnHeld] = string(b)
	}
	if m.Unattended {
		a[AnnUnattended] = "true"
	}
	if m.TraceParent != "" {
		a[AnnTraceParent] = m.TraceParent
	}
	if m.Origin != "" {
		a[AnnOrigin] = m.Origin
	}
	for k, v := range a {
		if v == "" {
			delete(a, k)
		}
	}
	return a
}

// MetaOf reads the annotations of a snapshot manifest.
func MetaOf(a map[string]string) (Meta, error) {
	m := Meta{
		Set: a[AnnSet], Target: a[AnnTarget], Publish: a[AnnPublish], Kind: a[AnnKind],
		Parent: a[AnnParent], SetCommit: a[AnnSetCommit], Signer: a[AnnSigner],
		User: a[AnnUser], Host: a[AnnHost], Repoctl: a[AnnVersion], Source: a[AnnSource],
		TraceParent: a[AnnTraceParent], Origin: a[AnnOrigin],
	}
	if a[AnnSet] == "" || a[AnnTarget] == "" || a[AnnPublish] == "" {
		return m, fmt.Errorf("the manifest is not a repoctl snapshot: annotations %s, %s or %s are missing", AnnSet, AnnTarget, AnnPublish)
	}
	if s := a[AnnCreated]; s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return m, fmt.Errorf("annotation %s: %w", AnnCreated, err)
		}
		m.Created = t
	}
	if s := a[AnnAccepted]; s != "" {
		if err := json.Unmarshal([]byte(s), &m.Accepted); err != nil {
			return m, fmt.Errorf("annotation %s: %w", AnnAccepted, err)
		}
	}
	if s := a[AnnHeld]; s != "" {
		if err := json.Unmarshal([]byte(s), &m.Held); err != nil {
			return m, fmt.Errorf("annotation %s: %w", AnnHeld, err)
		}
	}
	m.Unattended = a[AnnUnattended] == "true"
	return m, nil
}

// File is one file of a tree and one layer of its snapshot.
type File struct {
	// Path is the path in the tree with / separators: the title of the
	// layer.
	Path   string
	Digest digest.Digest
	Size   int64
	// Local is the file on disk, if any.
	Local string
}

// SHA256 gives the 64 hex digits of the digest.
func (f File) SHA256() string { return f.Digest.Encoded() }

// segmentRE is the rule of the mirror job for a segment of a title
// (publish.html#mirror-run).
var segmentRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

// ValidTitle checks a title the way the mirror job does: a relative path
// whose segments have only A-Z a-z 0-9 . _ + -, and no segment . or ..
func ValidTitle(title string) error {
	if title == "" || strings.HasPrefix(title, "/") {
		return fmt.Errorf("title %q is not a relative path", title)
	}
	for _, seg := range strings.Split(title, "/") {
		if seg == "." || seg == ".." || !segmentRE.MatchString(seg) {
			return fmt.Errorf("title %q: segment %q is not allowed", title, seg)
		}
	}
	return nil
}

// Skip reports the files of a tree directory that are not part of the
// snapshot: snapshot.json of the local copy and hidden work files.
func Skip(rel string) bool {
	base := filepath.Base(rel)
	return rel == "snapshot.json" || strings.HasPrefix(base, ".")
}

// Scan gives the files of a tree, sorted by path, with their digests.
func Scan(tree string) ([]File, error) {
	var out []File
	err := filepath.WalkDir(tree, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(tree, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if Skip(rel) {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		if err := ValidTitle(rel); err != nil {
			return err
		}
		f, err := FileOf(p)
		if err != nil {
			return err
		}
		f.Path = rel
		out = append(out, f)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// FileOf gives the digest and the size of one file.
func FileOf(path string) (File, error) {
	fh, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return File{}, err
	}
	d, err := digest.SHA256.FromReader(fh)
	if err != nil {
		return File{}, err
	}
	return File{Digest: d, Size: st.Size(), Local: path}, nil
}

// Descriptor gives the layer descriptor of a file.
func (f File) Descriptor() ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: FileMediaType, Digest: f.Digest, Size: f.Size, Annotations: map[string]string{AnnTitle: f.Path}}
}

// NewManifest gives the OCI manifest of a snapshot with these files.
func NewManifest(files []File, meta Meta) ocispec.Manifest {
	layers := make([]ocispec.Descriptor, len(files))
	for i, f := range files {
		layers[i] = f.Descriptor()
	}
	m := ocispec.Manifest{
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: ArtifactType,
		Config:       ocispec.DescriptorEmptyJSON,
		Layers:       layers,
		Annotations:  meta.Annotations(),
	}
	m.SchemaVersion = 2
	return m
}

// Encode gives the bytes and the descriptor of a manifest.
func Encode(m ocispec.Manifest) ([]byte, ocispec.Descriptor, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, ocispec.Descriptor{}, err
	}
	d := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, ArtifactType: ArtifactType, Digest: digest.FromBytes(b), Size: int64(len(b))}
	return b, d, nil
}

// Snapshot is one snapshot manifest as repoctl reads it.
type Snapshot struct {
	Desc ocispec.Descriptor
	Meta Meta
	// Files are the layers, sorted by path.
	Files []File
	// Raw is the manifest as the registry gave it.
	Raw []byte
}

// Digest gives the manifest digest, sha256:HEX.
func (s *Snapshot) Digest() api.Digest { return s.Desc.Digest.String() }

// File gives the layer of a path.
func (s *Snapshot) File(path string) (File, bool) {
	i := sort.Search(len(s.Files), func(i int) bool { return s.Files[i].Path >= path })
	if i < len(s.Files) && s.Files[i].Path == path {
		return s.Files[i], true
	}
	return File{}, false
}

// Decode reads a snapshot manifest.
func Decode(raw []byte, desc ocispec.Descriptor) (*Snapshot, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("the snapshot manifest is not JSON: %w", err)
	}
	if m.ArtifactType != ArtifactType {
		return nil, fmt.Errorf("the manifest has the artifact type %q, not %s", m.ArtifactType, ArtifactType)
	}
	meta, err := MetaOf(m.Annotations)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{Desc: desc, Meta: meta, Raw: raw}
	seen := map[string]bool{}
	for _, l := range m.Layers {
		title := l.Annotations[AnnTitle]
		if err := ValidTitle(title); err != nil {
			return nil, fmt.Errorf("layer %s: %w", l.Digest, err)
		}
		if seen[title] {
			return nil, fmt.Errorf("two layers have the title %s", title)
		}
		seen[title] = true
		if l.Digest.Algorithm() != digest.SHA256 {
			return nil, fmt.Errorf("layer %s: not a sha256 digest", title)
		}
		s.Files = append(s.Files, File{Path: title, Digest: l.Digest, Size: l.Size})
	}
	sort.Slice(s.Files, func(i, j int) bool { return s.Files[i].Path < s.Files[j].Path })
	return s, nil
}

// API gives the Snapshot item of the contract, without local, served and
// gc. repo is repo.name of the set: the binary repository whose RPMs count.
func (s *Snapshot) API(store, repo string, keyID func(fpr string) string) api.Snapshot {
	out := api.Snapshot{
		Target:     s.Meta.Target,
		Store:      store,
		Publish:    s.Meta.Publish,
		Digest:     s.Digest(),
		Parent:     s.Meta.Parent,
		Kind:       s.Meta.Kind,
		SetCommit:  s.Meta.SetCommit,
		User:       s.Meta.User,
		Host:       s.Meta.Host,
		Created:    api.FormatTime(s.Meta.Created),
		Repoctl:    s.Meta.Repoctl,
		RPMs:       len(s.RPMs(repo)),
		Unattended: s.Meta.Unattended,
		Accepted:   s.Meta.Accepted,
		Held:       s.Meta.Held,
	}
	if out.Accepted == nil {
		out.Accepted = []api.SnapshotAcceptedItem{}
	}
	if s.Meta.Signer != "" {
		out.Signer = &api.SnapshotSigner{Fingerprint: s.Meta.Signer}
		if keyID != nil {
			out.Signer.KeyID = keyID(s.Meta.Signer)
		}
	}
	return out
}

// RPMs gives the RPM files of the repositories of repo (repo.name): the
// files REPO/**.rpm, REPO-source/**.rpm and REPO-debuginfo/**.rpm. They
// are the RPMs that the counts of the design add up: 14 RPMs of ucx are
// its SRPM, binary and debuginfo RPMs, and "208 RPMs in 3 repositories".
func (s *Snapshot) RPMs(repo string) []File {
	var out []File
	for _, f := range s.Files {
		if !strings.HasSuffix(f.Path, ".rpm") {
			continue
		}
		for _, dir := range []string{repo, repo + "-source", repo + "-debuginfo"} {
			if strings.HasPrefix(f.Path, dir+"/") {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// Added counts the RPMs of s that parent does not hold.
func (s *Snapshot) Added(parent *Snapshot, repo string) int {
	n := 0
	for _, f := range s.RPMs(repo) {
		if parent == nil {
			n++
			continue
		}
		if pf, ok := parent.File(f.Path); !ok || pf.Digest != f.Digest {
			n++
		}
	}
	return n
}

// idRE is the form of a publish id: p-YYYYMMDD-HHMM-xxx, p-import-YYYYMMDD
// or p-reseed-YYYYMMDD (with an optional -N for a second one on a day).
var idRE = regexp.MustCompile(`^p-([0-9]{8}-[0-9]{4}-[0-9a-f]{3}|(import|reseed)-[0-9]{8}(-[0-9]+)?)$`)

// IsPublishID reports the form of a publish id.
func IsPublishID(s string) bool { return idRE.MatchString(s) }

// NewID gives a new publish id p-YYYYMMDD-HHMM-xxx for the time.
func NewID(now time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return "p-" + now.UTC().Format("20060102-1504") + "-" + hex.EncodeToString(b[:])[:3]
}

// DayID gives the publish id of an import or a re-seed: p-import-YYYYMMDD,
// p-reseed-YYYYMMDD; n > 1 adds -n for another one on the same day.
func DayID(kind string, now time.Time, n int) string {
	id := "p-" + kind + "-" + now.UTC().Format("20060102")
	if n > 1 {
		id += "-" + strconv.Itoa(n)
	}
	return id
}

// IDTime gives the time of a publish id, or the zero time.
func IDTime(id string) time.Time {
	parts := strings.Split(id, "-")
	if len(parts) >= 3 && len(parts[1]) == 8 && len(parts[2]) == 4 {
		t, _ := time.Parse("20060102-1504", parts[1]+"-"+parts[2])
		return t
	}
	if len(parts) >= 3 && len(parts[2]) == 8 {
		t, _ := time.Parse("20060102", parts[2])
		return t
	}
	return time.Time{}
}
